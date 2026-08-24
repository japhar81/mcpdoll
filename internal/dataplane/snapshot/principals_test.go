// Copyright 2026 Henry Zektser.

package snapshot

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// The point of ADR 0024, as a test: a principal change reaches a catalog
// without the snapshot moving.

func principalSet(version int64, grants ...*snapshotpb.Grant) *Principals {
	set := EmptyPrincipals()
	set.Version = version
	set.catalog = authz.DefaultCatalog()
	set.byID["usr_alice"] = &snapshotpb.Principal{
		Id: "usr_alice", TenantId: "tn_acme", Subject: "alice@example.com",
		Grants: grants,
	}
	return set
}

func TestAGrantChangeReachesTheCatalogWithoutTheSnapshotMoving(t *testing.T) {
	t.Parallel()
	f := defaultFixture(1)
	v := f.build(t)
	store := v.fixtureStore
	before := store.Version()

	// The whole toolset.
	require.NoError(t, store.ApplyPrincipals(principalSet(2,
		&snapshotpb.Grant{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "support")})))
	wide, err := store.PrincipalView(context.Background(), "usr_alice")
	require.NoError(t, err)
	require.Len(t, wide.Tools, 4)

	// Narrowed to one tool. No new snapshot — that is the claim.
	require.NoError(t, store.ApplyPrincipals(principalSet(3,
		&snapshotpb.Grant{
			Role:  authz.RoleToolUser,
			Scope: authz.ToolScope("acme", "support", "lookup_customer"),
		})))
	narrow, err := store.PrincipalView(context.Background(), "usr_alice")
	require.NoError(t, err)
	require.Len(t, narrow.Tools, 1)
	require.Equal(t, "crm.lookup_customer", narrow.Tools[0].Def.QualifiedName)

	require.Equal(t, before, store.Version(), "the snapshot moved")
}

func TestTheComposedViewIsKeyedOnBothVersions(t *testing.T) {
	t.Parallel()
	f := defaultFixture(1)
	store := f.build(t).fixtureStore

	require.NoError(t, store.ApplyPrincipals(principalSet(2,
		&snapshotpb.Grant{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "support")})))
	first, err := store.PrincipalView(context.Background(), "usr_alice")
	require.NoError(t, err)
	require.Len(t, first.Tools, 4)

	// Same principal id, new set. A cache keyed on the snapshot version alone
	// would hand back `first` and serve grants that no longer exist — which is
	// the coupling ADR 0024 removes, reintroduced in the cache.
	require.NoError(t, store.ApplyPrincipals(principalSet(3)))
	second, err := store.PrincipalView(context.Background(), "usr_alice")
	require.NoError(t, err)
	require.Empty(t, second.Tools, "a stale composed view was served")
}

func TestAStalePrincipalSetIsRefused(t *testing.T) {
	t.Parallel()
	store := NewStore(3)

	require.NoError(t, store.ApplyPrincipals(principalSet(5)))
	// Same monotonicity rule as the snapshot: a replayed older artifact must
	// not roll authorization backwards.
	require.ErrorIs(t, store.ApplyPrincipals(principalSet(5)), ErrStalePrincipals)
	require.ErrorIs(t, store.ApplyPrincipals(principalSet(4)), ErrStalePrincipals)
	require.NoError(t, store.ApplyPrincipals(principalSet(6)))
}

func TestObserversFireOnAPrincipalSwap(t *testing.T) {
	t.Parallel()
	store := NewStore(3)

	// The edge caches a built MCP server per principal and has to drop it, or
	// it keeps serving a catalog composed against grants that have changed.
	fired := 0
	store.ObservePrincipals(func(*Principals) { fired++ })

	require.NoError(t, store.ApplyPrincipals(principalSet(1)))
	require.NoError(t, store.ApplyPrincipals(principalSet(2)))
	require.Equal(t, 2, fired)

	require.Error(t, store.ApplyPrincipals(principalSet(2)))
	require.Equal(t, 2, fired, "a refused set must not notify")
}
