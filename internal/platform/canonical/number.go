// Copyright 2026 Henry Zektser.

package canonical

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// formatNumber renders f using the ECMAScript `Number::toString` algorithm,
// which is what RFC 8785 (JSON Canonicalization Scheme) mandates for numbers.
//
// Go's own float formatting is *shortest-round-trip* like ECMAScript's, but it
// picks different thresholds for switching into exponent notation, so we cannot
// use strconv.FormatFloat(f, 'g', -1, 64) directly. Instead we take the
// shortest round-trip digits from strconv in 'e' form and re-assemble them
// under the ECMAScript rules (ECMA-262 §6.1.6.1.20).
//
// The variables below mirror the spec's: `s` is the significand's decimal
// digits, `k` is how many there are, and `n` is the position of the decimal
// point relative to those digits (value == 0.s * 10^n).
func formatNumber(f float64) (string, error) {
	if math.IsNaN(f) {
		return "", fmt.Errorf("canonical: NaN is not representable in JSON")
	}
	if math.IsInf(f, 0) {
		return "", fmt.Errorf("canonical: %v is not representable in JSON", f)
	}
	// RFC 8785 §3.2.2.3: -0 canonicalizes to "0".
	if f == 0 {
		return "0", nil
	}
	if f < 0 {
		rest, err := formatNumber(-f)
		if err != nil {
			return "", err
		}
		return "-" + rest, nil
	}

	// strconv 'e' with precision -1 yields the shortest digit string that
	// round-trips, in the form "d.dddde±dd" (or "de±dd" for a single digit).
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, expPart, ok := strings.Cut(e, "e")
	if !ok {
		return "", fmt.Errorf("canonical: unexpected float encoding %q", e)
	}
	exp, err := strconv.Atoi(expPart)
	if err != nil {
		return "", fmt.Errorf("canonical: unexpected exponent in %q: %w", e, err)
	}
	s := strings.Replace(mantissa, ".", "", 1)
	k := len(s)
	// strconv writes `d.ddd e exp` meaning d.ddd * 10^exp, so the decimal point
	// sits one digit in; ECMAScript's n counts from the left of all digits.
	n := exp + 1

	var b strings.Builder
	switch {
	case k <= n && n <= 21:
		// Integer with trailing zeros: "100", "1e21" is excluded by n <= 21.
		b.WriteString(s)
		b.WriteString(strings.Repeat("0", n-k))
	case 0 < n && n <= 21:
		// Decimal point falls inside the digits: "1.5", "123.45".
		b.WriteString(s[:n])
		b.WriteByte('.')
		b.WriteString(s[n:])
	case -6 < n && n <= 0:
		// Small magnitude, still written in full: "0.001".
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -n))
		b.WriteString(s)
	default:
		// Exponent notation: "1e+21", "1.5e-7".
		b.WriteString(s[:1])
		if k > 1 {
			b.WriteByte('.')
			b.WriteString(s[1:])
		}
		b.WriteByte('e')
		if n-1 >= 0 {
			b.WriteByte('+')
		} else {
			b.WriteByte('-')
		}
		b.WriteString(strconv.Itoa(abs(n - 1)))
	}
	return b.String(), nil
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}
