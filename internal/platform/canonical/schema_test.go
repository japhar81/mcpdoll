// Copyright 2026 Henry Zektser.

package canonical

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveSchemaInlinesDefs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no defs is unchanged",
			in:   `{"type":"object","properties":{"a":{"type":"string"}}}`,
			want: `{"properties":{"a":{"type":"string"}},"type":"object"}`,
		},
		{
			name: "single ref inlined",
			in: `{"type":"object","properties":{"a":{"$ref":"#/$defs/Name"}},
			      "$defs":{"Name":{"type":"string","minLength":1}}}`,
			want: `{"properties":{"a":{"minLength":1,"type":"string"}},"type":"object"}`,
		},
		{
			name: "ref used twice expands at both sites",
			in: `{"properties":{"a":{"$ref":"#/$defs/N"},"b":{"$ref":"#/$defs/N"}},
			      "$defs":{"N":{"type":"integer"}}}`,
			want: `{"properties":{"a":{"type":"integer"},"b":{"type":"integer"}}}`,
		},
		{
			name: "legacy definitions keyword also inlined",
			in: `{"properties":{"a":{"$ref":"#/definitions/N"}},
			      "definitions":{"N":{"type":"boolean"}}}`,
			want: `{"properties":{"a":{"type":"boolean"}}}`,
		},
		{
			name: "unused defs are dropped",
			in:   `{"type":"string","$defs":{"Unused":{"type":"integer"}}}`,
			want: `{"type":"string"}`,
		},
		{
			name: "nested defs resolved transitively",
			in: `{"properties":{"a":{"$ref":"#/$defs/Outer"}},
			      "$defs":{"Outer":{"type":"object","properties":{"i":{"$ref":"#/$defs/Inner"}}},
			               "Inner":{"type":"number"}}}`,
			want: `{"properties":{"a":{"properties":{"i":{"type":"number"}},"type":"object"}}}`,
		},
		{
			name: "ref into an array element",
			in: `{"properties":{"a":{"$ref":"#/$defs/L/0"}},
			      "$defs":{"L":[{"type":"string"}]}}`,
			want: `{"properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "root ref",
			in:   `{"type":"object","properties":{"self":{"$ref":"#"}}}`,
			// Recursion: the root is already being expanded, so the reference
			// survives rather than looping.
			want: `{"properties":{"self":{"$ref":"#"}},"type":"object"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalizeSchema([]byte(tc.in))
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
		})
	}
}

// TestResolveSchemaFactoringIsInvisible is the whole point of inlining: a
// schema and its refactored-into-$defs twin must be byte-identical afterwards,
// so hoisting a repeated subschema is not a breaking republish.
func TestResolveSchemaFactoringIsInvisible(t *testing.T) {
	inline := `{
	  "type":"object",
	  "properties":{
	    "from":{"type":"string","format":"email"},
	    "to":{"type":"string","format":"email"}
	  }
	}`
	factored := `{
	  "type":"object",
	  "properties":{
	    "from":{"$ref":"#/$defs/Email"},
	    "to":{"$ref":"#/$defs/Email"}
	  },
	  "$defs":{"Email":{"type":"string","format":"email"}}
	}`
	a, err := CanonicalizeSchema([]byte(inline))
	require.NoError(t, err)
	b, err := CanonicalizeSchema([]byte(factored))
	require.NoError(t, err)
	require.Equal(t, string(a), string(b))
}

// TestResolveSchemaSiblingKeywords covers the 2020-12 change that `$ref` no
// longer suppresses its siblings: they must survive and win over the target.
func TestResolveSchemaSiblingKeywords(t *testing.T) {
	in := `{"properties":{"a":{"$ref":"#/$defs/N","description":"the a","minimum":5}},
	        "$defs":{"N":{"type":"integer","minimum":0}}}`
	got, err := CanonicalizeSchema([]byte(in))
	require.NoError(t, err)
	require.Equal(t,
		`{"properties":{"a":{"description":"the a","minimum":5,"type":"integer"}}}`,
		string(got),
		"sibling keywords must override the referenced schema's")
}

// TestResolveSchemaRecursive proves a self-referential schema terminates. A
// naive inliner would expand forever here.
func TestResolveSchemaRecursive(t *testing.T) {
	in := `{
	  "$defs":{"Node":{"type":"object","properties":{"child":{"$ref":"#/$defs/Node"}}}},
	  "$ref":"#/$defs/Node"
	}`
	got, err := CanonicalizeSchema([]byte(in))
	require.NoError(t, err)
	require.Equal(t,
		`{"properties":{"child":{"$ref":"#/$defs/Node"}},"type":"object"}`,
		string(got))
}

func TestResolveSchemaMutualRecursion(t *testing.T) {
	in := `{
	  "$ref":"#/$defs/A",
	  "$defs":{
	    "A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"}}},
	    "B":{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}}}
	  }
	}`
	got, err := CanonicalizeSchema([]byte(in))
	require.NoError(t, err)
	require.Equal(t,
		`{"properties":{"b":{"properties":{"a":{"$ref":"#/$defs/A"}},"type":"object"}},"type":"object"}`,
		string(got))
}

// TestResolveSchemaRejectsExternalRef is a security control, not a nicety:
// dereferencing an external $ref would let a registered backend point the
// gateway at an attacker-chosen URL, and would untether a signed definition's
// meaning from a document we control.
func TestResolveSchemaRejectsExternalRef(t *testing.T) {
	external := []string{
		`{"$ref":"https://evil.example/schema.json"}`,
		`{"$ref":"http://169.254.169.254/latest/meta-data/"}`,
		`{"$ref":"file:///etc/passwd"}`,
		`{"$ref":"other.json#/$defs/X"}`,
		`{"properties":{"a":{"$ref":"//evil.example/s.json"}}}`,
		`{"$defs":{"X":{"$ref":"https://evil.example/s.json"}},"$ref":"#/$defs/X"}`,
	}
	for _, in := range external {
		t.Run(in, func(t *testing.T) {
			_, err := ResolveSchema([]byte(in))
			require.Error(t, err)
			var ext *ErrExternalRef
			require.ErrorAs(t, err, &ext, "want a typed ErrExternalRef, got %v", err)
		})
	}
}

func TestResolveSchemaRejectsDanglingRef(t *testing.T) {
	_, err := ResolveSchema([]byte(`{"$ref":"#/$defs/Missing"}`))
	require.ErrorContains(t, err, "not found")
}

func TestResolveSchemaRejectsAnchorFragment(t *testing.T) {
	// $anchor resolution is deliberately unimplemented; failing loudly beats
	// silently hashing a schema we did not actually resolve.
	_, err := ResolveSchema([]byte(`{"$ref":"#SomeAnchor"}`))
	require.ErrorContains(t, err, "anchors are not resolved")
}

func TestResolveSchemaDepthLimit(t *testing.T) {
	deep := strings.Repeat(`{"items":`, MaxSchemaDepth+5) + `{}` + strings.Repeat(`}`, MaxSchemaDepth+5)
	_, err := ResolveSchema([]byte(deep))
	require.Error(t, err)
	var tooComplex *ErrSchemaTooComplex
	require.ErrorAs(t, err, &tooComplex)
	require.Contains(t, tooComplex.Reason, "max depth")
}

// TestResolveSchemaNodeLimit covers the expansion bomb the depth limit misses:
// N definitions each referenced twice expand as 2^N while staying shallow.
func TestResolveSchemaNodeLimit(t *testing.T) {
	var defs []string
	defs = append(defs, `"D0":{"type":"string"}`)
	for i := 1; i <= 16; i++ {
		defs = append(defs, fmt.Sprintf(
			`"D%d":{"type":"object","properties":{"a":{"$ref":"#/$defs/D%d"},"b":{"$ref":"#/$defs/D%d"}}}`,
			i, i-1, i-1))
	}
	bomb := `{"$ref":"#/$defs/D16","$defs":{` + strings.Join(defs, ",") + `}}`

	_, err := ResolveSchema([]byte(bomb))
	require.Error(t, err, "an exponentially expanding schema must be rejected")
	var tooComplex *ErrSchemaTooComplex
	require.ErrorAs(t, err, &tooComplex)
	require.Contains(t, tooComplex.Reason, "node count")
}

func TestPointerEscaping(t *testing.T) {
	// RFC 6901: "~" is ~0 and "/" is ~1, and ~01 must decode to "~1", not "/".
	tests := []struct{ raw, escaped string }{
		{"plain", "plain"},
		{"a/b", "a~1b"},
		{"a~b", "a~0b"},
		{"~1", "~01"},
		{"a~/b", "a~0~1b"},
	}
	for _, tc := range tests {
		require.Equal(t, tc.escaped, escapePointer(tc.raw))
		require.Equal(t, tc.raw, unescapePointer(tc.escaped))
	}
}

func TestResolvePointerIntoEscapedKey(t *testing.T) {
	in := `{"$ref":"#/$defs/a~1b","$defs":{"a/b":{"type":"null"}}}`
	got, err := CanonicalizeSchema([]byte(in))
	require.NoError(t, err)
	require.Equal(t, `{"type":"null"}`, string(got))
}

// TestResolveSchema2020_12Constructs exercises the keywords a modern schema
// actually uses, to prove resolution walks into all of them rather than only
// into `properties`.
func TestResolveSchema2020_12Constructs(t *testing.T) {
	in := `{
	  "$schema":"https://json-schema.org/draft/2020-12/schema",
	  "type":"object",
	  "properties":{"kind":{"$ref":"#/$defs/Kind"}},
	  "patternProperties":{"^x-":{"$ref":"#/$defs/Kind"}},
	  "prefixItems":[{"$ref":"#/$defs/Kind"}],
	  "items":{"$ref":"#/$defs/Kind"},
	  "additionalProperties":{"$ref":"#/$defs/Kind"},
	  "propertyNames":{"$ref":"#/$defs/Kind"},
	  "contains":{"$ref":"#/$defs/Kind"},
	  "unevaluatedProperties":{"$ref":"#/$defs/Kind"},
	  "allOf":[{"$ref":"#/$defs/Kind"}],
	  "anyOf":[{"$ref":"#/$defs/Kind"}],
	  "oneOf":[{"$ref":"#/$defs/Kind"}],
	  "not":{"$ref":"#/$defs/Kind"},
	  "if":{"$ref":"#/$defs/Kind"},
	  "then":{"$ref":"#/$defs/Kind"},
	  "else":{"$ref":"#/$defs/Kind"},
	  "dependentSchemas":{"kind":{"$ref":"#/$defs/Kind"}},
	  "$defs":{"Kind":{"const":"k"}}
	}`
	got, err := CanonicalizeSchema([]byte(in))
	require.NoError(t, err)
	out := string(got)
	require.NotContains(t, out, `"$ref"`, "every reference should have been inlined")
	require.NotContains(t, out, `"$defs"`, "the container should have been dropped")
	// The const must appear once per reference site (16 of them).
	require.Equal(t, 16, strings.Count(out, `"const":"k"`))
}

// TestResolveSchemaPreservesUnknownKeywords: vocabulary extensions and
// annotations we do not understand must survive, because the digest has to
// cover everything a backend published, not just the parts we recognise.
func TestResolveSchemaPreservesUnknownKeywords(t *testing.T) {
	in := `{"type":"string","x-mcpdoll-effect":"read","x-vendor":{"deep":[1,2]}}`
	got, err := CanonicalizeSchema([]byte(in))
	require.NoError(t, err)
	require.Equal(t, `{"type":"string","x-mcpdoll-effect":"read","x-vendor":{"deep":[1,2]}}`, string(got))
}
