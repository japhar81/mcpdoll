// Copyright 2026 The MCPDoll Authors.

package pipeline_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/pipeline"
)

func TestApplyPatchOperations(t *testing.T) {
	tests := []struct {
		name   string
		doc    string
		patch  string
		scopes []string
		want   string
	}{
		{
			name:   "replace a scalar",
			doc:    `{"a":{"b":"old"}}`,
			patch:  `[{"op":"replace","path":"/a/b","value":"new"}]`,
			scopes: []string{"a"},
			want:   `{"a":{"b":"new"}}`,
		},
		{
			name:   "add a new key",
			doc:    `{"a":{}}`,
			patch:  `[{"op":"add","path":"/a/b","value":1}]`,
			scopes: []string{"a"},
			want:   `{"a":{"b":1}}`,
		},
		{
			name:   "remove a key",
			doc:    `{"a":{"b":1,"c":2}}`,
			patch:  `[{"op":"remove","path":"/a/b"}]`,
			scopes: []string{"a"},
			want:   `{"a":{"c":2}}`,
		},
		{
			name:   "replace an array element",
			doc:    `{"list":["a","b","c"]}`,
			patch:  `[{"op":"replace","path":"/list/1","value":"B"}]`,
			scopes: []string{"list"},
			want:   `{"list":["a","B","c"]}`,
		},
		{
			name:   "remove an array element",
			doc:    `{"list":["a","b","c"]}`,
			patch:  `[{"op":"remove","path":"/list/1"}]`,
			scopes: []string{"list"},
			want:   `{"list":["a","c"]}`,
		},
		{
			name:   "insert into an array",
			doc:    `{"list":["a","c"]}`,
			patch:  `[{"op":"add","path":"/list/1","value":"b"}]`,
			scopes: []string{"list"},
			want:   `{"list":["a","b","c"]}`,
		},
		{
			name:   "append to an array",
			doc:    `{"list":["a"]}`,
			patch:  `[{"op":"add","path":"/list/-","value":"b"}]`,
			scopes: []string{"list"},
			want:   `{"list":["a","b"]}`,
		},
		{
			name:   "several operations apply in order",
			doc:    `{"a":1,"b":2}`,
			patch:  `[{"op":"replace","path":"/a","value":10},{"op":"remove","path":"/b"}]`,
			scopes: []string{"a", "b"},
			want:   `{"a":10}`,
		},
		{
			name:   "test that passes",
			doc:    `{"a":"x"}`,
			patch:  `[{"op":"test","path":"/a","value":"x"},{"op":"replace","path":"/a","value":"y"}]`,
			scopes: []string{"a"},
			want:   `{"a":"y"}`,
		},
		{
			name:   "empty patch is a no-op",
			doc:    `{"a":1}`,
			patch:  `[]`,
			scopes: []string{"a"},
			want:   `{"a":1}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pipeline.ApplyPatch([]byte(tc.doc), []byte(tc.patch), tc.scopes)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(got))
		})
	}
}

// TestApplyPatchOutputIsCanonical: a patch must not be able to smuggle in key
// ordering or duplicate keys that would change a downstream digest.
func TestApplyPatchOutputIsCanonical(t *testing.T) {
	got, err := pipeline.ApplyPatch(
		[]byte(`{"z":1,"a":2}`),
		[]byte(`[{"op":"add","path":"/m","value":{"y":1,"b":2}}]`),
		[]string{"m"})
	require.NoError(t, err)
	require.Equal(t, `{"a":2,"m":{"b":2,"y":1},"z":1}`, string(got),
		"the patched document must come back in canonical form")
}

// TestApplyPatchEnforcesWriteScopes is what makes a manifest a contract rather
// than documentation.
func TestApplyPatchEnforcesWriteScopes(t *testing.T) {
	doc := `{"principal":{"groups":["users"]},"result":{"content":[{"text":"hi"}]},"arguments":{"id":"x"}}`

	t.Run("in scope", func(t *testing.T) {
		_, err := pipeline.ApplyPatch([]byte(doc),
			[]byte(`[{"op":"replace","path":"/result/content/0/text","value":"redacted"}]`),
			[]string{"result.content"})
		require.NoError(t, err)
	})

	t.Run("a sibling is out of scope", func(t *testing.T) {
		_, err := pipeline.ApplyPatch([]byte(doc),
			[]byte(`[{"op":"replace","path":"/principal/groups","value":["admins"]}]`),
			[]string{"result.content"})
		var violation *pipeline.ErrScopeViolation
		require.ErrorAs(t, err, &violation)
		require.Equal(t, "/principal/groups", violation.Path)
	})

	t.Run("a parent is out of scope", func(t *testing.T) {
		// `result.content` does not authorize replacing `result` wholesale, which
		// would let a plugin discard `isError` along with the content.
		_, err := pipeline.ApplyPatch([]byte(doc),
			[]byte(`[{"op":"replace","path":"/result","value":{}}]`),
			[]string{"result.content"})
		require.Error(t, err)
	})

	t.Run("no declared writes means nothing is writable", func(t *testing.T) {
		_, err := pipeline.ApplyPatch([]byte(doc),
			[]byte(`[{"op":"replace","path":"/arguments/id","value":"y"}]`),
			nil)
		require.Error(t, err)
	})

	t.Run("array indices are not scope boundaries", func(t *testing.T) {
		// A plugin authorized to rewrite result content cannot know the indices
		// in advance, so `result.content` must cover `/result/content/0/text`.
		_, err := pipeline.ApplyPatch([]byte(doc),
			[]byte(`[{"op":"replace","path":"/result/content/0/text","value":"x"}]`),
			[]string{"result.content"})
		require.NoError(t, err)
	})

	t.Run("the wildcard scope is an explicit escape hatch", func(t *testing.T) {
		_, err := pipeline.ApplyPatch([]byte(doc),
			[]byte(`[{"op":"replace","path":"/principal/groups","value":["admins"]}]`),
			[]string{"*"})
		require.NoError(t, err)
	})
}

// TestApplyPatchIsAllOrNothing: a patch that is partly in scope is rejected
// whole. Applying the permitted half would leave the payload in a state no
// plugin asked for.
func TestApplyPatchIsAllOrNothing(t *testing.T) {
	doc := `{"result":{"content":[{"text":"hi"}]},"principal":{"groups":["users"]}}`
	patch := `[
		{"op":"replace","path":"/result/content/0/text","value":"redacted"},
		{"op":"replace","path":"/principal/groups","value":["admins"]}
	]`

	got, err := pipeline.ApplyPatch([]byte(doc), []byte(patch), []string{"result.content"})
	require.Error(t, err)
	require.Nil(t, got, "no partially-applied document should be returned")
}

// TestApplyPatchChecksMoveSource: move and copy read from `from`, so the source
// has to be in scope too — otherwise a plugin scoped to the result could copy the
// principal's groups into it and exfiltrate them.
func TestApplyPatchChecksMoveSource(t *testing.T) {
	doc := `{"result":{"content":[]},"principal":{"groups":["secret-group"]}}`
	patch := `[{"op":"move","from":"/principal/groups","path":"/result/leaked"}]`

	_, err := pipeline.ApplyPatch([]byte(doc), []byte(patch), []string{"result"})
	require.Error(t, err,
		"the source of a move must be scope-checked, not just the destination")
	var violation *pipeline.ErrScopeViolation
	require.ErrorAs(t, err, &violation)
	require.Equal(t, "/principal/groups", violation.Path)
}

// TestApplyPatchRejectsMoveAndCopy: they are expressible as remove+add and are
// omitted rather than implemented half-correctly.
func TestApplyPatchRejectsMoveAndCopy(t *testing.T) {
	for _, op := range []string{"move", "copy"} {
		patch := fmt.Sprintf(`[{"op":%q,"from":"/a","path":"/b"}]`, op)
		_, err := pipeline.ApplyPatch([]byte(`{"a":1}`), []byte(patch), []string{"*"})
		require.ErrorContains(t, err, "not supported")
		require.ErrorContains(t, err, "remove plus an add")
	}
}

func TestApplyPatchRejects(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		patch   string
		wantErr string
	}{
		{
			name:    "not a patch document",
			doc:     `{"a":1}`,
			patch:   `{"op":"replace"}`,
			wantErr: "RFC 6902",
		},
		{
			name:    "unknown op",
			doc:     `{"a":1}`,
			patch:   `[{"op":"frobnicate","path":"/a"}]`,
			wantErr: "unknown op",
		},
		{
			name:    "replacing the whole document",
			doc:     `{"a":1}`,
			patch:   `[{"op":"replace","path":"","value":{}}]`,
			wantErr: "whole document",
		},
		{
			name:    "removing the whole document",
			doc:     `{"a":1}`,
			patch:   `[{"op":"remove","path":""}]`,
			wantErr: "whole document",
		},
		{
			name:    "replace requires an existing value",
			doc:     `{"a":{}}`,
			patch:   `[{"op":"replace","path":"/a/missing","value":1}]`,
			wantErr: "requires an existing value",
		},
		{
			name:    "remove requires an existing value",
			doc:     `{"a":{}}`,
			patch:   `[{"op":"remove","path":"/a/missing"}]`,
			wantErr: "to remove",
		},
		{
			name:    "array index out of range",
			doc:     `{"list":["a"]}`,
			patch:   `[{"op":"replace","path":"/list/5","value":"x"}]`,
			wantErr: "out of range",
		},
		{
			name:    "test that fails",
			doc:     `{"a":"x"}`,
			patch:   `[{"op":"test","path":"/a","value":"y"}]`,
			wantErr: "test failed",
		},
		{
			name:    "pointer without a leading slash",
			doc:     `{"a":1}`,
			patch:   `[{"op":"replace","path":"a","value":2}]`,
			wantErr: "must start with /",
		},
		{
			name:    "payload is not JSON",
			doc:     `not json`,
			patch:   `[{"op":"replace","path":"/a","value":1}]`,
			wantErr: "not JSON",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pipeline.ApplyPatch([]byte(tc.doc), []byte(tc.patch), []string{"*"})
			require.Error(t, err)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestApplyPatchBoundsOperationCount: a plugin is a third-party artifact, so an
// enormous patch is a denial of service against the request it rides on.
func TestApplyPatchBoundsOperationCount(t *testing.T) {
	ops := make([]map[string]any, pipeline.MaxPatchOperations+1)
	for i := range ops {
		ops[i] = map[string]any{"op": "add", "path": "/x", "value": i}
	}
	patch, err := json.Marshal(ops)
	require.NoError(t, err)

	_, err = pipeline.ApplyPatch([]byte(`{}`), patch, []string{"*"})
	require.ErrorContains(t, err, "over the limit")
}

// TestApplyPatchEscapedPointers covers RFC 6901 escaping, since a key containing
// a slash is legal JSON and would otherwise be mis-parsed as a path segment.
func TestApplyPatchEscapedPointers(t *testing.T) {
	got, err := pipeline.ApplyPatch(
		[]byte(`{"a/b":1,"c~d":2}`),
		[]byte(`[{"op":"replace","path":"/a~1b","value":10},{"op":"replace","path":"/c~0d","value":20}]`),
		[]string{"*"})
	require.NoError(t, err)
	require.JSONEq(t, `{"a/b":10,"c~d":20}`, string(got))
}

func TestErrScopeViolationMessage(t *testing.T) {
	err := &pipeline.ErrScopeViolation{Path: "/principal/groups", Scopes: []string{"result.content"}}
	msg := err.Error()
	require.Contains(t, msg, "/principal/groups")
	require.Contains(t, msg, "result.content")
	require.True(t, strings.Contains(msg, "declared writes"),
		"the message should name the manifest field the plugin violated")
}
