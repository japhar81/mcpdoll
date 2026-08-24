// Copyright 2026 The MCPDoll Authors.

package edge_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// The gateway authenticates against the snapshot, with no database and no call
// to the control plane (ADR 0021). What is worth testing is the failure side:
// every way of getting it wrong must be indistinguishable from every other, and
// none of them may produce a principal.

func digestOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// keyedView builds a snapshot carrying one principal reachable by one key.
func keyedView(t *testing.T, prefix, secret string) *snapshot.View {
	t.Helper()

	b := snapshot.NewBuilder(7)
	b.AddTenant(&snapshotpb.Tenant{
		Id: "tn_acme", Slug: "acme", Name: "Acme", Status: "active",
	})
	b.AddNamespace(&snapshotpb.Namespace{Id: "ns_crm", Name: "crm", Prefix: "crm"})
	b.AddToolset(&snapshotpb.Toolset{Id: "ts_support", Name: "support", Priority: 10})
	b.AddServer(&snapshotpb.Server{
		Id: "srv_crm", Name: "crm", NamespaceId: "ns_crm",
		Bindings: []*snapshotpb.Binding{{TenantId: "tn_acme", Primary: "http://127.0.0.1:1"}},
	})
	b.AddTool(snapshot.ToolInput{
		ServerID: "srv_crm", NamespaceID: "ns_crm", TenantID: "tn_acme",
		ToolsetID: "ts_support", Prefix: "crm", Name: "lookup",
		InputSchema: []byte(`{"type":"object"}`),
		EffectClass: snapshotpb.EffectClass_EFFECT_CLASS_READ,
	})
	b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{{
		Id: "key_1", TenantId: "tn_acme", Subject: "agent@acme.example",
		Grants: []*snapshotpb.Grant{
			{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "support")},
		},
		KeyPrefix: prefix, KeySecretSha256: digestOf(secret),
	}})

	snap, err := b.Build()
	require.NoError(t, err)
	view, err := snapshot.Build(snap)
	require.NoError(t, err)
	return view
}

func resolverOver(t *testing.T, view *snapshot.View) edge.IdentityResolver {
	t.Helper()
	r, err := edge.NewAPIKeyIdentityResolver(func() *snapshot.View { return view })
	require.NoError(t, err)
	return r
}

func bearer(value string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+value)
	return h
}

func TestAValidKeyResolvesToItsPrincipalAndTenant(t *testing.T) {
	t.Parallel()
	view := keyedView(t, "abc12345", "s3cr3t-value")

	principal, err := resolverOver(t, view).Resolve(bearer("mcpd.abc12345.s3cr3t-value"))
	require.NoError(t, err)

	// The id is what authorization keys on; the tenant comes from the
	// credential rather than from the path (ADR 0019).
	require.Equal(t, "key_1", principal.ID)
	require.Equal(t, "agent@acme.example", principal.Subject)
	require.Equal(t, "acme", principal.Tenant)
}

func TestAKeyNeverCarriesGroupsOrClaims(t *testing.T) {
	t.Parallel()
	view := keyedView(t, "abc12345", "s3cr3t-value")

	h := bearer("mcpd.abc12345.s3cr3t-value")
	h.Set(edge.HeaderGroups, "data-restricted,eng-platform")
	h.Set(edge.HeaderClaim+"Department", "finance")

	principal, err := resolverOver(t, view).Resolve(h)
	require.NoError(t, err)

	// A key that could assert group membership would let a credential grant
	// itself whatever a group-conditioned policy allows. The headers are
	// present and must be ignored.
	require.Empty(t, principal.Groups)
	require.Empty(t, principal.Claims)
}

func TestEveryWrongCredentialFailsIdentically(t *testing.T) {
	t.Parallel()
	view := keyedView(t, "abc12345", "s3cr3t-value")
	resolver := resolverOver(t, view)

	for name, presented := range map[string]string{
		"wrong secret":       "mcpd.abc12345.wrong",
		"unknown prefix":     "mcpd.zzzzzzzz.s3cr3t-value",
		"wrong marker":       "xxxx.abc12345.s3cr3t-value",
		"two segments":       "mcpd.abc12345",
		"four segments":      "mcpd.abc12345.s3cr3t.extra",
		"empty secret":       "mcpd.abc12345.",
		"not a key at all":   "hunter2",
		"prefix as the key":  "abc12345",
		"the digest itself":  digestOf("s3cr3t-value"),
		"a plausible prefix": "mcpd.abc12345.s3cr3t-valuf",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := resolver.Resolve(bearer(presented))
			// One error for every failure. A caller learns whether their
			// credential worked and nothing else.
			require.ErrorIs(t, err, edge.ErrUnauthenticated)
		})
	}
}

func TestNoCredentialIsNotAnAnonymousPrincipal(t *testing.T) {
	t.Parallel()
	view := keyedView(t, "abc12345", "s3cr3t-value")

	_, err := resolverOver(t, view).Resolve(http.Header{})
	require.ErrorIs(t, err, edge.ErrUnauthenticated)
}

func TestNoSnapshotAuthenticatesNobody(t *testing.T) {
	t.Parallel()

	// Before the first snapshot lands there is nothing to verify against.
	// Failing closed is the only correct answer: a gateway that authenticated
	// during its own startup window would be a way to get in by timing.
	r, err := edge.NewAPIKeyIdentityResolver(func() *snapshot.View { return nil })
	require.NoError(t, err)

	_, err = r.Resolve(bearer("mcpd.abc12345.s3cr3t-value"))
	require.ErrorIs(t, err, edge.ErrUnauthenticated)
}

func TestTwoPrincipalsCannotShareAKeyPrefix(t *testing.T) {
	t.Parallel()

	b := snapshot.NewBuilder(7)
	b.AddTenant(&snapshotpb.Tenant{Id: "tn_acme", Slug: "acme", Name: "Acme", Status: "active"})
	b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{
		{Id: "key_1", TenantId: "tn_acme", Subject: "a@x", KeyPrefix: "dup", KeySecretSha256: digestOf("one")},
		{Id: "key_2", TenantId: "tn_acme", Subject: "b@x", KeyPrefix: "dup", KeySecretSha256: digestOf("two")},
	})

	// Not a duplicate id — an ambiguity about who is calling, whose answer
	// would depend on map iteration order. Refused by the builder, so the
	// snapshot never reaches a file, let alone a data plane.
	_, err := b.Build()
	require.ErrorContains(t, err, "claimed by two principals")
}

func TestAPrefixWithNoDigestIsRefusedAtLoad(t *testing.T) {
	t.Parallel()

	b := snapshot.NewBuilder(7)
	b.AddTenant(&snapshotpb.Tenant{Id: "tn_acme", Slug: "acme", Name: "Acme", Status: "active"})
	b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{
		{Id: "key_1", TenantId: "tn_acme", Subject: "a@x", KeyPrefix: "abc"},
	})

	// An empty digest would compare equal to nothing, but the failure mode is
	// worth refusing outright: it is one typo away from a principal any secret
	// authenticates as.
	_, err := b.Build()
	require.ErrorContains(t, err, "any secret would verify")
}

func TestTheChainPrefersARealCredentialOverAClaimedSubject(t *testing.T) {
	t.Parallel()
	view := keyedView(t, "abc12345", "s3cr3t-value")

	keys, err := edge.NewAPIKeyIdentityResolver(func() *snapshot.View { return view })
	require.NoError(t, err)
	headers, err := edge.NewHeaderIdentityResolver("development", "", nil)
	require.NoError(t, err)

	h := bearer("mcpd.abc12345.s3cr3t-value")
	h.Set(edge.HeaderSubject, "somebody-else")

	// Order is the security property. If the header resolver ran first, a
	// client could present a valid key *and* a forged subject and have the
	// forgery win.
	principal, err := edge.ChainIdentityResolvers(keys, headers).Resolve(h)
	require.NoError(t, err)
	require.Equal(t, "key_1", principal.ID)
}

func TestAWrongKeyDoesNotFallThroughToTheHeaderResolver(t *testing.T) {
	t.Parallel()
	view := keyedView(t, "abc12345", "s3cr3t-value")

	keys, err := edge.NewAPIKeyIdentityResolver(func() *snapshot.View { return view })
	require.NoError(t, err)
	// Empty default subject: with one, every failed authentication would
	// silently succeed as that subject — including a request that presented a
	// wrong key, which is the case this test exists for.
	headers, err := edge.NewHeaderIdentityResolver("development", "", nil)
	require.NoError(t, err)

	_, err = edge.ChainIdentityResolvers(keys, headers).Resolve(bearer("mcpd.abc12345.wrong"))
	require.ErrorIs(t, err, edge.ErrUnauthenticated)
}

func TestTheHeaderResolverStillRefusesProduction(t *testing.T) {
	t.Parallel()

	_, err := edge.NewHeaderIdentityResolver("production", "", nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "must never run in")
}
