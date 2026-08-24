// Copyright 2026 Henry Zektser.

package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// DigestPrefix namespaces digests so a bare hex string can never be mistaken
// for one, and so a future hash change is visible in stored data.
const DigestPrefix = "sha256:"

// Digest is a content address: "sha256:" followed by 64 lowercase hex digits.
type Digest string

// String implements fmt.Stringer.
func (d Digest) String() string { return string(d) }

// Valid reports whether d is well-formed.
func (d Digest) Valid() bool {
	s := string(d)
	if !strings.HasPrefix(s, DigestPrefix) {
		return false
	}
	hexPart := s[len(DigestPrefix):]
	if len(hexPart) != 64 {
		return false
	}
	_, err := hex.DecodeString(hexPart)
	return err == nil
}

// Short returns the first 12 hex characters, for logs and UI. Never use it as
// an identity — collisions at 48 bits are reachable by an adversary.
func (d Digest) Short() string {
	s := string(d)
	if len(s) < len(DigestPrefix)+12 {
		return s
	}
	return s[len(DigestPrefix) : len(DigestPrefix)+12]
}

// DigestBytes hashes raw bytes. Callers should pass canonical bytes.
func DigestBytes(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest(DigestPrefix + hex.EncodeToString(sum[:]))
}

// DigestOf canonicalizes v and returns its digest.
func DigestOf(v any) (Digest, error) {
	b, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

// ProseFields are the tool-definition fields that carry human prose rather
// than machine-checkable structure.
//
// They are excluded from the *semantic* digest so drift detection can tell a
// reworded description (cosmetic) from a changed schema (structural) by
// comparing two digests rather than by diffing field by field. A reworded
// description still changes the full digest — it is a new definition and must
// be re-admitted — but it is classified differently, and the LLM guard cares
// about exactly this distinction because prose is where injection hides.
var ProseFields = map[string]bool{
	"description": true,
	"title":       true,
	"examples":    true,
	"$comment":    true,
	"deprecated":  false, // structural: affects client behaviour.
}

// ToolDefinition is the canonical, transport-independent shape of a tool as
// MCPDoll stores and hashes it.
//
// It is deliberately *not* the SDK's mcp.Tool: the SDK type tracks the wire
// protocol and gains fields as the spec moves, and a new optional field
// appearing there must not silently change every stored digest. Conversion
// lives in internal/mcp.
type ToolDefinition struct {
	Name         string         `json:"name"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description,omitempty"`
	InputSchema  any            `json:"inputSchema,omitempty"`
	OutputSchema any            `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
	Meta         map[string]any `json:"_meta,omitempty"`
}

// CanonicalForm returns the canonical bytes for the definition, with schemas
// `$defs`-resolved.
func (t *ToolDefinition) CanonicalForm() ([]byte, error) {
	normalized, err := t.normalized()
	if err != nil {
		return nil, err
	}
	return marshalCanonicalValue(normalized)
}

// Digest is the full content address of the definition.
func (t *ToolDefinition) Digest() (Digest, error) {
	b, err := t.CanonicalForm()
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

// SemanticDigest is the content address of the definition with prose removed.
//
// Equal semantic digests plus differing full digests means "only the wording
// changed" — the cosmetic drift class.
func (t *ToolDefinition) SemanticDigest() (Digest, error) {
	normalized, err := t.normalized()
	if err != nil {
		return "", err
	}
	stripped := stripProse(normalized)
	b, err := marshalCanonicalValue(stripped)
	if err != nil {
		return "", err
	}
	return DigestBytes(b), nil
}

// normalized returns the definition as a generic value with both schemas
// resolved, ready for canonical encoding.
func (t *ToolDefinition) normalized() (any, error) {
	if t.Name == "" {
		return nil, fmt.Errorf("canonical: tool definition has no name")
	}
	out := map[string]any{"name": t.Name}
	if t.Title != "" {
		out["title"] = t.Title
	}
	if t.Description != "" {
		out["description"] = t.Description
	}
	if t.InputSchema != nil {
		resolved, err := resolveAnySchema(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("canonical: inputSchema: %w", err)
		}
		out["inputSchema"] = resolved
	}
	if t.OutputSchema != nil {
		resolved, err := resolveAnySchema(t.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("canonical: outputSchema: %w", err)
		}
		out["outputSchema"] = resolved
	}
	if len(t.Annotations) > 0 {
		out["annotations"] = t.Annotations
	}
	if len(t.Meta) > 0 {
		out["_meta"] = t.Meta
	}
	return out, nil
}

func resolveAnySchema(schema any) (any, error) {
	raw, err := marshalCanonicalValue(schema)
	if err != nil {
		return nil, err
	}
	return ResolveSchema(raw)
}

// stripProse removes prose-only keys recursively.
func stripProse(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if ProseFields[k] {
				continue
			}
			out[k] = stripProse(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, elem := range t {
			out[i] = stripProse(elem)
		}
		return out
	default:
		return v
	}
}
