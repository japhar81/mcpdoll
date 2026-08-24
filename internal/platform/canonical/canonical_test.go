// Copyright 2026 Henry Zektser.

package canonical

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty object", `{}`, `{}`},
		{"empty array", `[]`, `[]`},
		{"null", `null`, `null`},
		{"true", `true`, `true`},
		{"whitespace stripped", "{\n  \"a\" : 1 ,\n\t\"b\":2\n}", `{"a":1,"b":2}`},
		{"keys sorted", `{"c":3,"a":1,"b":2}`, `{"a":1,"b":2,"c":3}`},
		{"nested keys sorted", `{"z":{"y":1,"x":2}}`, `{"z":{"x":2,"y":1}}`},
		{"array order preserved", `[3,1,2]`, `[3,1,2]`},
		{"uppercase sorts before lowercase", `{"a":1,"A":2}`, `{"A":2,"a":1}`},
		{"digits sort before letters", `{"a":1,"1":2}`, `{"1":2,"a":1}`},
		{"empty key", `{"":1,"a":2}`, `{"":1,"a":2}`},

		// RFC 8785 section 3.2.2.3 -- numbers.
		{"integer", `1`, `1`},
		{"negative zero", `-0`, `0`},
		{"negative zero float", `-0.0`, `0`},
		{"trailing zero dropped", `1.0`, `1`},
		{"exponent normalized", `1e2`, `100`},
		{"leading plus exponent", `1E+2`, `100`},
		{"fraction", `1.5`, `1.5`},
		{"negative fraction", `-1.5`, `-1.5`},
		{"small decimal kept in full", `0.000001`, `0.000001`},
		{"smaller decimal goes exponential", `0.0000001`, `1e-7`},
		{"large integer stays plain", `1e20`, `100000000000000000000`},
		{"very large goes exponential", `1e21`, `1e+21`},
		{"very large with mantissa", `1.5e21`, `1.5e+21`},
		{"max safe integer", `9007199254740991`, `9007199254740991`},
		{"repeating fraction", `0.1`, `0.1`},
		{"sum artifact", `1.0000000000000002`, `1.0000000000000002`},

		// RFC 8785 section 3.2.2.2 -- strings.
		{"quote escaped", `"a\"b"`, `"a\"b"`},
		{"backslash escaped", `"a\\b"`, `"a\\b"`},
		{"newline short escape", `"a\nb"`, `"a\nb"`},
		{"tab short escape", `"a\tb"`, `"a\tb"`},
		{"backspace short escape", `"a\bb"`, `"a\bb"`},
		{"formfeed short escape", `"a\fb"`, `"a\fb"`},
		{"carriage return short escape", `"a\rb"`, `"a\rb"`},
		{"other control uses uXXXX", `"\u0001ab"`, `"\u0001ab"`},
		{"control 0x1f", `"\u001f"`, `"\u001f"`},
		{"uppercase escape normalizes to lowercase hex", `"\u001F"`, `"\u001f"`},
		{"escaped unicode becomes literal", `"\u00e9"`, "\"\u00e9\""},
		{"forward slash not escaped", `"a/b"`, `"a/b"`},
		{"escaped solidus unescaped", `"a\/b"`, `"a/b"`},
		{"del is literal", `"\u007f"`, "\"\u007f\""},
		{"surrogate pair becomes literal", `"\ud83c\udf89"`, "\"\U0001F389\""},
		{"non-ascii literal", `"café"`, "\"café\""},
		{"emoji literal", `"🎉"`, "\"\U0001F389\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonicalize([]byte(tc.in))
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
		})
	}
}

// TestCanonicalizeIdempotent asserts canonical output is a fixed point:
// re-canonicalizing must not change the bytes, or digests would depend on how
// many times a value happened to pass through the pipeline.
func TestCanonicalizeIdempotent(t *testing.T) {
	inputs := []string{
		`{"b":[1,2,{"z":null,"a":true}],"a":"x"}`,
		`{"n":1e21,"m":0.0000001,"k":-0}`,
		`{"s":"tab\there\nand\u0001control"}`,
		`[[[[1]]]]`,
	}
	for _, in := range inputs {
		once, err := Canonicalize([]byte(in))
		require.NoError(t, err)
		twice, err := Canonicalize(once)
		require.NoError(t, err)
		require.Equal(t, string(once), string(twice), "not a fixed point for %s", in)
	}
}

// TestCanonicalizeKeyOrderIndependent is the property that makes the digest a
// content address: the same logical value must canonicalize identically no
// matter what order its keys arrived in.
func TestCanonicalizeKeyOrderIndependent(t *testing.T) {
	a := `{"alpha":1,"beta":{"x":[1,2],"y":"z"},"gamma":null}`
	b := `{"gamma":null,"beta":{"y":"z","x":[1,2]},"alpha":1}`
	ca, err := Canonicalize([]byte(a))
	require.NoError(t, err)
	cb, err := Canonicalize([]byte(b))
	require.NoError(t, err)
	require.Equal(t, string(ca), string(cb))
}

// TestSortKeysUTF16 covers the case that separates UTF-16 ordering from
// code-point ordering: a supplementary character (encoded as a surrogate pair
// starting at U+D800) must sort *below* a BMP character above U+E000, even
// though its code point is far higher.
func TestSortKeysUTF16(t *testing.T) {
	// U+FB33 (Hebrew, BMP) vs U+1F600 (emoji, supplementary).
	// Code point order:  U+FB33 < U+1F600
	// UTF-16 unit order: 0xD83D (emoji's lead surrogate) < 0xFB33
	const bmp = "דּ"
	const supp = "\U0001F600"

	// Precondition: Go's byte-wise comparison orders them the other way, so
	// this test fails if sortKeys ever regresses to a plain `<`.
	require.True(t, bmp < supp, "precondition: UTF-8 byte order puts BMP first")
	require.True(t, lessUTF16(supp, bmp), "UTF-16 order must put the surrogate pair first")

	got, err := Marshal(map[string]any{bmp: 1, supp: 2})
	require.NoError(t, err)
	require.Equal(t, "{\""+supp+"\":2,\""+bmp+"\":1}", string(got))
}

func TestCanonicalizeRejects(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"duplicate key", `{"a":1,"a":2}`, "duplicate object key"},
		{"trailing content", `1 2`, "trailing content"},
		{"trailing object", `{} {}`, "trailing content"},
		{"malformed", `{`, "EOF"},
		{"bare word", `undefined`, "invalid character"},
		{"nested duplicate", `{"a":{"b":1,"b":2}}`, "duplicate object key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Canonicalize([]byte(tc.in))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestFormatNumberRejectsNonFinite(t *testing.T) {
	// encoding/json refuses NaN/Inf before we ever see them, so exercise our
	// own guard directly.
	_, err := formatNumber(math.NaN())
	require.ErrorContains(t, err, "NaN")
	_, err = formatNumber(math.Inf(1))
	require.ErrorContains(t, err, "not representable")
	_, err = formatNumber(math.Inf(-1))
	require.ErrorContains(t, err, "not representable")
}

// TestFormatNumberRoundTrip asserts every canonical number parses back to the
// exact same float64. If this ever fails, digests stop being stable across a
// store/load cycle.
func TestFormatNumberRoundTrip(t *testing.T) {
	values := []float64{
		0, 1, -1, 0.5, -0.5, 1e-7, 1e-6, 1e20, 1e21, 1e-321,
		math.MaxFloat64, math.SmallestNonzeroFloat64,
		9007199254740991, -9007199254740991,
		3.141592653589793, 2.718281828459045,
		1.0000000000000002, 0.1, 0.2, 0.3,
		123456789.123456789, -0.000001234,
	}
	for _, v := range values {
		s, err := formatNumber(v)
		require.NoError(t, err, "value %v", v)
		var back float64
		require.NoError(t, json.Unmarshal([]byte(s), &back), "reparse %q", s)
		require.Equal(t, v, back, "round-trip of %v via %q", v, s)
	}
}

func TestFormatNumberECMAScriptForms(t *testing.T) {
	// Exact strings the ECMAScript Number::toString algorithm produces. These
	// are the cases where Go's own %g formatting would disagree.
	tests := []struct {
		in   float64
		want string
	}{
		{1e21, "1e+21"},
		{1e20, "100000000000000000000"},
		{1e-6, "0.000001"},
		{1e-7, "1e-7"},
		{1.5e-7, "1.5e-7"},
		{-1e21, "-1e+21"},
		{5e-324, "5e-324"},
		{100, "100"},
		{1.5, "1.5"},
	}
	for _, tc := range tests {
		got, err := formatNumber(tc.in)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "formatNumber(%v)", tc.in)
	}
}

func TestEqual(t *testing.T) {
	same, err := Equal([]byte(`{"a":1,"b":2}`), []byte(`{"b":2,"a":1}`))
	require.NoError(t, err)
	require.True(t, same)

	same, err = Equal([]byte(`{"a":1}`), []byte(`{"a":2}`))
	require.NoError(t, err)
	require.False(t, same)

	_, err = Equal([]byte(`{`), []byte(`{}`))
	require.Error(t, err)
}

func TestMarshalGoValues(t *testing.T) {
	type nested struct {
		Z int `json:"z"`
		A int `json:"a"`
	}
	got, err := Marshal(struct {
		B nested `json:"b"`
		A string `json:"a"`
	}{B: nested{Z: 1, A: 2}, A: "x"})
	require.NoError(t, err)
	require.Equal(t, `{"a":"x","b":{"a":2,"z":1}}`, string(got))
}

func TestMustMarshalString(t *testing.T) {
	require.Panics(t, func() { MustMarshalString(make(chan int)) })
	require.Equal(t, `{"a":1}`, MustMarshalString(map[string]int{"a": 1}))
}

// TestCanonicalizeDeepNesting proves the encoder handles structures as deep as
// admission permits without blowing the stack.
func TestCanonicalizeDeepNesting(t *testing.T) {
	depth := 200
	in := strings.Repeat(`{"a":`, depth) + `1` + strings.Repeat(`}`, depth)
	got, err := Canonicalize([]byte(in))
	require.NoError(t, err)
	require.Equal(t, in, string(got))
}
