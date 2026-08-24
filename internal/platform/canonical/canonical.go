// Copyright 2026 Henry Zektser.

// Package canonical produces byte-stable serializations of JSON values and the
// content-addressed digests MCPDoll derives from them.
//
// Canonicalization is load-bearing for the whole platform: a tool definition's
// identity *is* the digest of its canonical form. Registry immutability, drift
// detection, snapshot reproducibility, and the LLM-guard verdict cache all key
// off it, so the encoding must be exactly reproducible across processes and
// releases.
//
// The encoding is RFC 8785 (JSON Canonicalization Scheme):
//
//   - object keys sorted by their UTF-16 code units,
//   - no insignificant whitespace,
//   - ECMAScript `Number::toString` number formatting (see number.go),
//   - minimal string escaping, non-ASCII left as literal UTF-8.
//
// On top of plain JCS, [CanonicalizeSchema] resolves a JSON Schema's internal
// `$defs` so that two schemas which differ only in whether a subschema was
// factored out hash identically. See docs/adr/0005-content-addressed-tool-definitions.md.
package canonical

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"unicode/utf16"
)

// Marshal returns the RFC 8785 canonical JSON encoding of v.
//
// v is first round-tripped through encoding/json, so it accepts anything
// json.Marshal accepts. Values that JSON cannot represent canonically (NaN,
// ±Inf) are rejected rather than silently mangled.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical: marshal input: %w", err)
	}
	return Canonicalize(raw)
}

// Canonicalize rewrites already-encoded JSON into its canonical form.
func Canonicalize(raw []byte) ([]byte, error) {
	parsed, err := parse(raw)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := write(&buf, parsed); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// parse decodes JSON into the canonical value model: nil, bool, string,
// float64, []any, map[string]any.
//
// Unlike encoding/json's default behaviour it rejects duplicate object keys.
// A duplicate key makes a document's meaning parser-dependent, which is exactly
// the sort of ambiguity a content-addressed identity scheme cannot tolerate —
// two gateways could legitimately disagree about what they just hashed.
func parse(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	// Reject trailing content: "1 2" must not silently canonicalize to "1".
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("canonical: trailing content after top-level JSON value")
		}
		return nil, fmt.Errorf("canonical: %w", err)
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("canonical: %w", err)
	}
	return parseFromToken(dec, tok)
}

func parseFromToken(dec *json.Decoder, tok json.Token) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return parseObject(dec)
		case '[':
			return parseArray(dec)
		default:
			return nil, fmt.Errorf("canonical: unexpected delimiter %q", t)
		}
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return nil, fmt.Errorf("canonical: number %s: %w", t.String(), err)
		}
		return f, nil
	case string, bool, nil:
		return t, nil
	default:
		return nil, fmt.Errorf("canonical: unexpected token %T", tok)
	}
}

func parseObject(dec *json.Decoder) (any, error) {
	out := map[string]any{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("canonical: %w", err)
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return out, nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("canonical: object key is %T, want string", tok)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("canonical: duplicate object key %q", key)
		}
		val, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		out[key] = val
	}
}

func parseArray(dec *json.Decoder) (any, error) {
	// Non-nil empty slice so an empty array encodes as "[]", not "null".
	out := []any{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("canonical: %w", err)
		}
		if d, ok := tok.(json.Delim); ok && d == ']' {
			return out, nil
		}
		val, err := parseFromToken(dec, tok)
		if err != nil {
			return nil, err
		}
		out = append(out, val)
	}
}

func write(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64:
		s, err := formatNumber(t)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case string:
		writeString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, elem := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := write(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortKeys(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := write(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonical: unsupported value type %T", v)
	}
	return nil
}

// sortKeys orders keys by their UTF-16 code units, as RFC 8785 §3.2.3 requires.
//
// This is *not* the same as Go's byte-wise string comparison. UTF-8 sorts by
// code point, but UTF-16 encodes anything above the BMP as a surrogate pair in
// 0xD800–0xDFFF, which places supplementary characters (U+10000 and up) *below*
// U+E000–U+FFFF rather than above them. An emoji key next to a private-use key
// is enough to expose the difference.
func sortKeys(keys []string) {
	sort.Slice(keys, func(i, j int) bool {
		return lessUTF16(keys[i], keys[j])
	})
}

func lessUTF16(a, b string) bool {
	// ASCII-only keys — overwhelmingly the common case — compare identically
	// under UTF-8 bytes and UTF-16 code units, so skip the conversion.
	if isASCII(a) && isASCII(b) {
		return a < b
	}
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

const hexDigits = "0123456789abcdef"

// writeString emits an RFC 8785 string literal: the two mandatory escapes, the
// five short control-character escapes, \u00xx for the remaining C0 controls,
// and literal UTF-8 for everything else (including non-ASCII).
func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c == '\b':
			buf.WriteString(`\b`)
		case c == '\f':
			buf.WriteString(`\f`)
		case c == '\n':
			buf.WriteString(`\n`)
		case c == '\r':
			buf.WriteString(`\r`)
		case c == '\t':
			buf.WriteString(`\t`)
		case c < 0x20:
			buf.WriteString(`\u00`)
			buf.WriteByte(hexDigits[c>>4])
			buf.WriteByte(hexDigits[c&0xF])
		default:
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
}

// MustMarshalString is a test/fixture helper that panics on failure.
func MustMarshalString(v any) string {
	b, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Equal reports whether two JSON documents have the same canonical form.
func Equal(a, b []byte) (bool, error) {
	ca, err := Canonicalize(a)
	if err != nil {
		return false, err
	}
	cb, err := Canonicalize(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ca, cb), nil
}
