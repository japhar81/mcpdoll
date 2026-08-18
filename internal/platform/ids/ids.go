// Copyright 2026 The MCPDoll Authors.

// Package ids mints the identifiers MCPDoll uses in URLs, logs, and audit
// records.
//
// Identifiers are prefixed and k-sortable. The prefix makes a stray id
// self-describing in a log line or a bug report ("srv_…" is a server, not a
// snapshot), and catches the class of bug where the right-shaped id for the
// wrong entity is passed to a handler. K-sortability means an index on the id
// column is also roughly an index on creation time, so audit queries that page
// backwards through recent events do not need a separate sort.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// Kind is an entity type. The zero value is invalid.
type Kind string

// The complete set of identifier kinds. Adding one here is the only place a
// new prefix should ever be introduced.
const (
	KindOrg          Kind = "org"
	KindProject      Kind = "prj"
	KindTeam         Kind = "team"
	KindNamespace    Kind = "ns"
	KindServer       Kind = "srv"
	KindVersion      Kind = "ver"
	KindToolDef      Kind = "tool"
	KindBundle       Kind = "bnd"
	KindAudience     Kind = "aud"
	KindPolicy       Kind = "pol"
	KindPlugin       Kind = "plg"
	KindSnapshot     Kind = "snap"
	KindAdmission    Kind = "adm"
	KindDrift        Kind = "drift"
	KindAudit        Kind = "aud_ev"
	KindRequest      Kind = "req"
	KindPrincipal    Kind = "prin"
	KindGrant        Kind = "grant"
	KindProbe        Kind = "probe"
	KindRequestState Kind = "rs"
)

// crockford is Crockford's base32 alphabet: no I, L, O, or U, so an id read
// aloud or retyped from a screenshot does not turn into a different id.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New mints an identifier of the given kind.
//
// The layout is `<prefix>_<48-bit ms timestamp><80 bits of randomness>`,
// encoded as 26 Crockford base32 characters — the ULID layout. Two ids minted
// in the same millisecond sort arbitrarily relative to each other but still
// sort correctly against any other millisecond.
func New(kind Kind) string {
	var raw [16]byte
	ms := uint64(time.Now().UTC().UnixMilli())
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	// rand.Read from crypto/rand cannot fail on any supported platform; it
	// panics internally rather than returning an error worth handling.
	if _, err := rand.Read(raw[6:]); err != nil {
		panic(fmt.Sprintf("ids: crypto/rand failed: %v", err))
	}
	return string(kind) + "_" + encode(raw)
}

// encode writes 16 bytes as 26 Crockford base32 characters (130 bits of
// capacity for 128 bits of data; the top two bits are always zero).
func encode(raw [16]byte) string {
	var out [26]byte
	// Process as two 64-bit halves plus the leading bits, most-significant
	// first, so the encoding preserves byte order and therefore sort order.
	hi := binary.BigEndian.Uint64(raw[0:8])
	lo := binary.BigEndian.Uint64(raw[8:16])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&0x1F]
		lo >>= 5
		// Carry the low 5 bits of hi down into lo as it empties.
		lo |= (hi & 0x1F) << 59
		hi >>= 5
	}
	return string(out[:])
}

// Parse splits an identifier into its kind and body, and validates the body's
// alphabet. It does not check that the kind is one this build knows about —
// use [Is] for that — so a rolling upgrade can read ids a newer peer wrote.
func Parse(id string) (Kind, string, error) {
	prefix, body, ok := strings.Cut(id, "_")
	if !ok || prefix == "" || body == "" {
		return "", "", fmt.Errorf("ids: %q is not a prefixed identifier", id)
	}
	// The audit-event kind contains an underscore, so the first cut is wrong
	// for it; re-cut from the right when the leading segment is ambiguous.
	if prefix == "aud" && strings.HasPrefix(body, "ev_") {
		prefix, body = "aud_ev", strings.TrimPrefix(body, "ev_")
	}
	if len(body) != 26 {
		return "", "", fmt.Errorf("ids: %q has a %d-character body, want 26", id, len(body))
	}
	for i := 0; i < len(body); i++ {
		if !strings.ContainsRune(crockford, rune(body[i])) {
			return "", "", fmt.Errorf("ids: %q contains %q, which is not in the Crockford alphabet", id, body[i])
		}
	}
	return Kind(prefix), body, nil
}

// Is reports whether id is well-formed and of the given kind. Handlers should
// call this on every path parameter: it is the cheap check that stops a bundle
// id reaching a query that expects a server id.
func Is(id string, kind Kind) bool {
	got, _, err := Parse(id)
	return err == nil && got == kind
}

// Timestamp recovers the creation time encoded in an identifier. Useful for
// audit display and for retention sweeps that would otherwise need a join.
func Timestamp(id string) (time.Time, error) {
	_, body, err := Parse(id)
	if err != nil {
		return time.Time{}, err
	}
	var ms uint64
	// The first 10 base32 characters carry the 48-bit millisecond field
	// (10 × 5 = 50 bits; the top 2 are zero).
	for i := 0; i < 10; i++ {
		idx := strings.IndexByte(crockford, body[i])
		if idx < 0 {
			return time.Time{}, fmt.Errorf("ids: %q has a corrupt timestamp field", id)
		}
		ms = ms<<5 | uint64(idx)
	}
	return time.UnixMilli(int64(ms)).UTC(), nil
}
