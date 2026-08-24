// Copyright 2026 Henry Zektser.

package pipeline

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mcpdoll/mcpdoll/internal/platform/canonical"
)

// MaxPatchOperations bounds a single patch document.
//
// A plugin is a third-party artifact. Without a bound, a patch with a million
// operations is a denial of service against the request it is attached to — and
// against every request queued behind it.
const MaxPatchOperations = 256

// ErrScopeViolation reports a patch operation outside a plugin's declared writes.
//
// This is the enforcement that makes a manifest a contract rather than
// documentation. A plugin that declares `writes: [result.content]` and then
// patches `/principal/groups` is either broken or hostile; either way the
// gateway must not apply it.
type ErrScopeViolation struct {
	Path   string
	Scopes []string
}

func (e *ErrScopeViolation) Error() string {
	return fmt.Sprintf(
		"pipeline: patch touches %q, which is outside the plugin's declared writes %v",
		e.Path, e.Scopes)
}

// PatchOp is one RFC 6902 operation.
type PatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	From  string          `json:"from,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// ApplyPatch applies an RFC 6902 patch, refusing anything outside `scopes`.
//
// Scopes are dotted paths matching the payload's structure (`result.content`),
// and a scope authorizes that path and everything under it. The check runs before
// any operation is applied, so a patch that is partly in scope is rejected whole
// — applying the permitted half of a rejected patch would leave the payload in a
// state no plugin asked for.
//
// The result is re-canonicalized, so a patch cannot smuggle in key ordering or
// duplicate keys that would change a downstream digest.
func ApplyPatch(payload []byte, patch []byte, scopes []string) ([]byte, error) {
	if len(patch) == 0 {
		return payload, nil
	}

	var ops []PatchOp
	if err := json.Unmarshal(patch, &ops); err != nil {
		return nil, fmt.Errorf("pipeline: patch is not an RFC 6902 document: %w", err)
	}
	if len(ops) == 0 {
		return payload, nil
	}
	if len(ops) > MaxPatchOperations {
		return nil, fmt.Errorf("pipeline: patch has %d operations, over the limit of %d",
			len(ops), MaxPatchOperations)
	}

	// Scope-check everything first.
	for _, op := range ops {
		if err := checkScope(op.Path, scopes); err != nil {
			return nil, err
		}
		// `move` and `copy` read from `from` and write to `path`. The source has
		// to be in scope too: otherwise a plugin scoped to the result could copy
		// the principal's groups into it and exfiltrate them.
		if op.From != "" {
			if err := checkScope(op.From, scopes); err != nil {
				return nil, err
			}
		}
	}

	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, fmt.Errorf("pipeline: payload is not JSON: %w", err)
	}

	for i, op := range ops {
		var err error
		doc, err = applyOp(doc, op)
		if err != nil {
			return nil, fmt.Errorf("pipeline: patch operation %d (%s %s): %w", i, op.Op, op.Path, err)
		}
	}

	// Re-canonicalize so the patched payload cannot differ from an equivalent
	// unpatched one in key order or whitespace.
	return canonical.Marshal(doc)
}

// checkScope reports whether a JSON Pointer falls within the declared writes.
func checkScope(pointer string, scopes []string) error {
	if len(scopes) == 0 {
		return &ErrScopeViolation{Path: pointer, Scopes: scopes}
	}
	dotted := pointerToDotted(pointer)
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		// A scope of "*" is a deliberate escape hatch for a plugin the operator
		// has decided to trust completely. It has to be written explicitly.
		if scope == "*" {
			return nil
		}
		if dotted == scope || strings.HasPrefix(dotted, scope+".") {
			return nil
		}
	}
	return &ErrScopeViolation{Path: pointer, Scopes: scopes}
}

// pointerToDotted converts an RFC 6901 pointer to the dotted form scopes use.
//
// Array indices become "*" so a scope of `result.content` covers
// `/result/content/0/text` — a plugin authorized to rewrite result content should
// not have to enumerate indices it cannot know in advance.
func pointerToDotted(pointer string) string {
	trimmed := strings.TrimPrefix(pointer, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
		if _, err := strconv.Atoi(part); err == nil {
			continue // array index: not a scope boundary
		}
		out = append(out, part)
	}
	return strings.Join(out, ".")
}

// applyOp applies one operation.
//
// A deliberately small subset of RFC 6902: add, remove, replace, and test.
// `move` and `copy` are expressible as remove+add and are omitted rather than
// implemented half-correctly; a plugin that needs them can emit the pair.
func applyOp(doc any, op PatchOp) (any, error) {
	switch op.Op {
	case "add", "replace":
		var value any
		if err := json.Unmarshal(op.Value, &value); err != nil {
			return nil, fmt.Errorf("value is not JSON: %w", err)
		}
		return setPointer(doc, op.Path, value, op.Op == "add")
	case "remove":
		return removePointer(doc, op.Path)
	case "test":
		var want any
		if err := json.Unmarshal(op.Value, &want); err != nil {
			return nil, fmt.Errorf("value is not JSON: %w", err)
		}
		got, err := getPointer(doc, op.Path)
		if err != nil {
			return nil, err
		}
		same, err := jsonEqual(got, want)
		if err != nil {
			return nil, err
		}
		if !same {
			return nil, fmt.Errorf("test failed: the value at %s is not what the patch expected", op.Path)
		}
		return doc, nil
	case "move", "copy":
		return nil, fmt.Errorf(
			"op %q is not supported; express it as a remove plus an add", op.Op)
	default:
		return nil, fmt.Errorf("unknown op %q", op.Op)
	}
}

func splitPointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("pointer %q must start with /", pointer)
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, part := range parts {
		parts[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
	}
	return parts, nil
}

func getPointer(doc any, pointer string) (any, error) {
	parts, err := splitPointer(pointer)
	if err != nil {
		return nil, err
	}
	cur := doc
	for _, part := range parts {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[part]
			if !ok {
				return nil, fmt.Errorf("no value at %q", part)
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(part)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("index %q is out of range", part)
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("cannot descend into %T at %q", cur, part)
		}
	}
	return cur, nil
}

func setPointer(doc any, pointer string, value any, isAdd bool) (any, error) {
	parts, err := splitPointer(pointer)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		// Replacing the whole document is legal RFC 6902 but would let a plugin
		// discard everything, including the parts outside its scope.
		return nil, fmt.Errorf("replacing the whole document is not permitted")
	}

	parent, err := getPointerParts(doc, parts[:len(parts)-1])
	if err != nil {
		return nil, err
	}
	last := parts[len(parts)-1]

	switch node := parent.(type) {
	case map[string]any:
		if !isAdd {
			if _, ok := node[last]; !ok {
				return nil, fmt.Errorf("replace requires an existing value at %q", last)
			}
		}
		node[last] = value
		return doc, nil
	case []any:
		if last == "-" {
			if !isAdd {
				return nil, fmt.Errorf("replace cannot target the array append position")
			}
			// Appending needs the parent's parent to hold the new slice, since
			// append may reallocate.
			return replaceSlice(doc, parts[:len(parts)-1], append(node, value))
		}
		idx, err := strconv.Atoi(last)
		if err != nil {
			return nil, fmt.Errorf("array index %q is not a number", last)
		}
		if isAdd {
			if idx < 0 || idx > len(node) {
				return nil, fmt.Errorf("index %d is out of range for insertion", idx)
			}
			grown := make([]any, 0, len(node)+1)
			grown = append(grown, node[:idx]...)
			grown = append(grown, value)
			grown = append(grown, node[idx:]...)
			return replaceSlice(doc, parts[:len(parts)-1], grown)
		}
		if idx < 0 || idx >= len(node) {
			return nil, fmt.Errorf("index %d is out of range", idx)
		}
		node[idx] = value
		return doc, nil
	default:
		return nil, fmt.Errorf("cannot set a member of %T", parent)
	}
}

func removePointer(doc any, pointer string) (any, error) {
	parts, err := splitPointer(pointer)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("removing the whole document is not permitted")
	}

	parent, err := getPointerParts(doc, parts[:len(parts)-1])
	if err != nil {
		return nil, err
	}
	last := parts[len(parts)-1]

	switch node := parent.(type) {
	case map[string]any:
		if _, ok := node[last]; !ok {
			return nil, fmt.Errorf("no value at %q to remove", last)
		}
		delete(node, last)
		return doc, nil
	case []any:
		idx, err := strconv.Atoi(last)
		if err != nil || idx < 0 || idx >= len(node) {
			return nil, fmt.Errorf("index %q is out of range", last)
		}
		shrunk := make([]any, 0, len(node)-1)
		shrunk = append(shrunk, node[:idx]...)
		shrunk = append(shrunk, node[idx+1:]...)
		return replaceSlice(doc, parts[:len(parts)-1], shrunk)
	default:
		return nil, fmt.Errorf("cannot remove a member of %T", parent)
	}
}

func getPointerParts(doc any, parts []string) (any, error) {
	cur := doc
	for _, part := range parts {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[part]
			if !ok {
				return nil, fmt.Errorf("no value at %q", part)
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(part)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("index %q is out of range", part)
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("cannot descend into %T at %q", cur, part)
		}
	}
	return cur, nil
}

// replaceSlice re-attaches a rebuilt slice to its parent, because append and
// splice may reallocate and the old backing array is no longer the document's.
func replaceSlice(doc any, parts []string, value []any) (any, error) {
	if len(parts) == 0 {
		return value, nil
	}
	parent, err := getPointerParts(doc, parts[:len(parts)-1])
	if err != nil {
		return nil, err
	}
	last := parts[len(parts)-1]
	switch node := parent.(type) {
	case map[string]any:
		node[last] = value
		return doc, nil
	case []any:
		idx, err := strconv.Atoi(last)
		if err != nil || idx < 0 || idx >= len(node) {
			return nil, fmt.Errorf("index %q is out of range", last)
		}
		node[idx] = value
		return doc, nil
	default:
		return nil, fmt.Errorf("cannot attach a slice to %T", parent)
	}
}

func jsonEqual(a, b any) (bool, error) {
	ca, err := canonical.Marshal(a)
	if err != nil {
		return false, err
	}
	cb, err := canonical.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(ca) == string(cb), nil
}
