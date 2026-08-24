// Copyright 2026 Henry Zektser.

package ids

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewFormat(t *testing.T) {
	id := New(KindServer)
	require.True(t, strings.HasPrefix(id, "srv_"), "got %q", id)
	kind, body, err := Parse(id)
	require.NoError(t, err)
	require.Equal(t, KindServer, kind)
	require.Len(t, body, 26)
}

func TestNewIsUnique(t *testing.T) {
	const n = 20000
	seen := make(map[string]bool, n)
	for range n {
		id := New(KindRequest)
		require.False(t, seen[id], "duplicate id %q", id)
		seen[id] = true
	}
}

// TestNewIsSortable is the property that lets an index on the id column double
// as a time index: ids minted later must sort later as plain strings.
func TestNewIsSortable(t *testing.T) {
	var got []string
	for range 5 {
		got = append(got, New(KindAudit))
		// Cross a millisecond boundary so the timestamp field actually moves.
		time.Sleep(2 * time.Millisecond)
	}
	want := append([]string(nil), got...)
	sort.Strings(want)
	require.Equal(t, want, got, "ids must already be in sorted order")
}

func TestTimestampRoundTrip(t *testing.T) {
	before := time.Now().UTC().Truncate(time.Millisecond)
	id := New(KindSnapshot)
	after := time.Now().UTC()

	ts, err := Timestamp(id)
	require.NoError(t, err)
	require.False(t, ts.Before(before), "%v is before %v", ts, before)
	require.False(t, ts.After(after.Add(time.Millisecond)), "%v is after %v", ts, after)
}

func TestParseRejects(t *testing.T) {
	tests := []struct{ name, in string }{
		{"empty", ""},
		{"no separator", "srv0123456789ABCDEFGHJKMNPQ"},
		{"no prefix", "_0123456789ABCDEFGHJKMNPQRS"},
		{"no body", "srv_"},
		{"body too short", "srv_0123456789"},
		{"body too long", "srv_0123456789ABCDEFGHJKMNPQRSTV"},
		{"letter I is excluded", "srv_I123456789ABCDEFGHJKMNPQ"},
		{"letter L is excluded", "srv_L123456789ABCDEFGHJKMNPQ"},
		{"letter O is excluded", "srv_O123456789ABCDEFGHJKMNPQ"},
		{"letter U is excluded", "srv_U123456789ABCDEFGHJKMNPQ"},
		{"lowercase is not the alphabet", "srv_abcdefghijklmnopqrstuvwxyz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse(tc.in)
			require.Error(t, err, "Parse(%q) should have failed", tc.in)
		})
	}
}

// TestIsRejectsWrongKind is the check every handler relies on to stop a
// well-formed id for the wrong entity reaching a query.
func TestIsRejectsWrongKind(t *testing.T) {
	server := New(KindServer)
	require.True(t, Is(server, KindServer))
	require.False(t, Is(server, KindToolset))
	require.False(t, Is("not-an-id", KindServer))
	require.False(t, Is("", KindServer))
}

// TestParseAuditEventKind: "aud_ev" contains the separator, so a naive first
// cut yields the meaningless prefix "aud". It must round-trip anyway.
func TestParseAuditEventKind(t *testing.T) {
	audit := New(KindAudit)
	kind, _, err := Parse(audit)
	require.NoError(t, err)
	require.Equal(t, KindAudit, kind)
	require.True(t, Is(audit, KindAudit))
	require.False(t, Is(audit, KindTenant), "an audit id must not read as a tenant id")

	tenant := New(KindTenant)
	require.True(t, Is(tenant, KindTenant))
	require.False(t, Is(tenant, KindAudit))
}

// TestKindPrefixesAreDistinct guards against two entity kinds sharing a
// prefix, which would silently defeat every Is() check for both.
func TestKindPrefixesAreDistinct(t *testing.T) {
	all := []Kind{
		KindOrg, KindProject, KindTeam, KindNamespace, KindServer, KindVersion,
		KindToolDef, KindToolset, KindTenant, KindPolicy, KindPlugin,
		KindSnapshot, KindAdmission, KindDrift, KindAudit, KindRequest,
		KindPrincipal, KindGrant, KindProbe, KindRequestState,
	}
	seen := map[Kind]bool{}
	for _, k := range all {
		require.False(t, seen[k], "duplicate kind prefix %q", k)
		seen[k] = true
		require.NotEmpty(t, string(k))
	}
	// Every kind must survive a mint/parse round trip.
	for _, k := range all {
		require.True(t, Is(New(k), k), "kind %q does not round-trip", k)
	}
}

func TestEncodeIsMonotonicInTimestamp(t *testing.T) {
	// Two ids whose only difference is the timestamp field must sort by it.
	var lo, hi [16]byte
	lo[5] = 1
	hi[5] = 2
	require.Less(t, encode(lo), encode(hi))

	// And the randomness field must not perturb that ordering.
	lo[15] = 0xFF
	require.Less(t, encode(lo), encode(hi))
}
