// Copyright 2026 The MCPDoll Authors.

package canonical

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// MaxSchemaDepth bounds how deep ResolveSchema will descend. JSON Schema
// 2020-12 permits arbitrary nesting, and a hostile backend can publish a
// deeply-nested schema purely to burn admission CPU or blow the stack. The
// limit is generous relative to hand-written schemas and cheap to enforce.
const MaxSchemaDepth = 64

// MaxSchemaNodes bounds the total number of nodes produced by inlining. A
// schema with N `$defs` each referenced twice expands as 2^N; the depth limit
// alone does not catch that, because the expansion is wide rather than deep.
const MaxSchemaNodes = 20000

// ErrExternalRef reports a `$ref` that points outside the document.
//
// MCPDoll never dereferences an external `$ref`: doing so would let a
// registered backend make the gateway fetch an attacker-chosen URL at admission
// time, and would make a tool definition's meaning depend on a remote document
// that can change after the definition was signed. External refs are rejected
// at canonicalization, which is upstream of both admission and snapshot build.
type ErrExternalRef struct {
	Ref  string
	Path string
}

func (e *ErrExternalRef) Error() string {
	return fmt.Sprintf("canonical: external $ref %q at %s is not permitted", e.Ref, e.Path)
}

// ErrSchemaTooComplex reports a schema that exceeded the depth or node budget.
type ErrSchemaTooComplex struct {
	Reason string
	Path   string
}

func (e *ErrSchemaTooComplex) Error() string {
	return fmt.Sprintf("canonical: schema too complex at %s: %s", e.Path, e.Reason)
}

// ResolveSchema inlines a JSON Schema's internal `$defs` (and the legacy
// `definitions` keyword) so that logically identical schemas share a canonical
// form regardless of how the author chose to factor them.
//
// Two schemas that describe the same data must produce the same digest, or the
// registry would treat a purely editorial refactor — hoisting a repeated
// subschema into `$defs` — as a breaking republish. Inlining is what makes the
// digest describe *meaning* rather than *layout*.
//
// Recursive schemas cannot be fully inlined. When a `$ref` reappears while its
// own expansion is still in progress, the reference is left in place (with its
// pointer normalized), which keeps the encoding finite and still deterministic.
//
// External refs are rejected; see [ErrExternalRef].
func ResolveSchema(raw []byte) (any, error) {
	root, err := parse(raw)
	if err != nil {
		return nil, err
	}
	r := &resolver{root: root}
	// The root is "in progress" from the moment we start, so a `$ref: "#"`
	// anywhere inside it is recursion and must be left in place. Without this
	// seed the root would be inlined into itself exactly once before the cycle
	// check noticed — producing a stable but needlessly duplicated encoding.
	active := map[string]bool{"": true}
	out, err := r.resolve(root, "#", active, 0)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CanonicalizeSchema returns the canonical bytes of a `$defs`-resolved schema.
func CanonicalizeSchema(raw []byte) ([]byte, error) {
	resolved, err := ResolveSchema(raw)
	if err != nil {
		return nil, err
	}
	return marshalCanonicalValue(resolved)
}

type resolver struct {
	root  any
	nodes int
}

// resolve walks the schema, replacing internal `$ref`s with the subschema they
// point at. `active` holds the pointers currently being expanded, which is how
// recursion is detected.
func (r *resolver) resolve(v any, path string, active map[string]bool, depth int) (any, error) {
	if depth > MaxSchemaDepth {
		return nil, &ErrSchemaTooComplex{Reason: fmt.Sprintf("exceeded max depth %d", MaxSchemaDepth), Path: path}
	}
	r.nodes++
	if r.nodes > MaxSchemaNodes {
		return nil, &ErrSchemaTooComplex{Reason: fmt.Sprintf("exceeded max node count %d", MaxSchemaNodes), Path: path}
	}

	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))
		for i, elem := range t {
			resolved, err := r.resolve(elem, path+"/"+strconv.Itoa(i), active, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil

	case map[string]any:
		if refRaw, ok := t["$ref"]; ok {
			ref, isStr := refRaw.(string)
			if !isStr {
				return nil, fmt.Errorf("canonical: $ref at %s is %T, want string", path, refRaw)
			}
			return r.resolveRef(t, ref, path, active, depth)
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			// `$defs`/`definitions` are pure containers: once every reference
			// to them has been inlined they carry no meaning, and keeping them
			// would make an unused definition change the digest.
			if k == "$defs" || k == "definitions" {
				continue
			}
			resolved, err := r.resolve(val, path+"/"+escapePointer(k), active, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil

	default:
		return v, nil
	}
}

func (r *resolver) resolveRef(node map[string]any, ref, path string, active map[string]bool, depth int) (any, error) {
	if !strings.HasPrefix(ref, "#") {
		return nil, &ErrExternalRef{Ref: ref, Path: path}
	}
	pointer := strings.TrimPrefix(ref, "#")

	// A ref already being expanded means the schema is recursive. Leave the
	// reference in place rather than looping forever; the pointer is kept
	// verbatim so the encoding stays deterministic.
	if active[pointer] {
		out := map[string]any{"$ref": "#" + pointer}
		// Sibling keywords are meaningful in 2020-12 ($ref no longer
		// suppresses them), so carry them through.
		for k, val := range node {
			if k == "$ref" || k == "$defs" || k == "definitions" {
				continue
			}
			resolved, err := r.resolve(val, path+"/"+escapePointer(k), active, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil
	}

	target, err := resolvePointer(r.root, pointer)
	if err != nil {
		return nil, fmt.Errorf("canonical: $ref %q at %s: %w", ref, path, err)
	}

	active[pointer] = true
	resolvedTarget, err := r.resolve(target, "#"+pointer, active, depth+1)
	delete(active, pointer)
	if err != nil {
		return nil, err
	}

	// In 2020-12 a `$ref` may sit alongside other keywords, and they apply in
	// addition to the referenced schema. Merge them over the inlined target.
	targetMap, isMap := resolvedTarget.(map[string]any)
	if !isMap {
		if len(node) == 1 {
			return resolvedTarget, nil
		}
		return nil, fmt.Errorf("canonical: $ref %q at %s resolves to a non-object but has sibling keywords", ref, path)
	}
	out := make(map[string]any, len(targetMap)+len(node))
	maps.Copy(out, targetMap)
	for k, val := range node {
		if k == "$ref" || k == "$defs" || k == "definitions" {
			continue
		}
		resolved, err := r.resolve(val, path+"/"+escapePointer(k), active, depth+1)
		if err != nil {
			return nil, err
		}
		out[k] = resolved
	}
	return out, nil
}

// resolvePointer walks an RFC 6901 JSON Pointer against the document root.
func resolvePointer(root any, pointer string) (any, error) {
	if pointer == "" || pointer == "/" {
		return root, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		// A plain-name fragment ("#Foo") refers to an `$anchor`, which we do
		// not index. Treat it as unresolvable rather than guessing.
		return nil, fmt.Errorf("unsupported fragment (anchors are not resolved)")
	}
	cur := root
	for tokenRaw := range strings.SplitSeq(strings.TrimPrefix(pointer, "/"), "/") {
		token := unescapePointer(tokenRaw)
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[token]
			if !ok {
				return nil, fmt.Errorf("pointer segment %q not found", token)
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(token)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("pointer segment %q is not a valid index", token)
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("pointer segment %q descends into a %T", token, cur)
		}
	}
	return cur, nil
}

// escapePointer / unescapePointer implement RFC 6901 token escaping.
func escapePointer(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func unescapePointer(s string) string {
	// Order matters: ~01 must decode to "~1", not "/".
	return strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
}

// marshalCanonicalValue writes an already-parsed canonical value.
func marshalCanonicalValue(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical: %w", err)
	}
	return Canonicalize(raw)
}
