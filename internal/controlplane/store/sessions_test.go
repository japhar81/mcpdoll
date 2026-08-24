// Copyright 2026 Henry Zektser.

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// Sessions and revocations.
//
// A local password is a principal (ADR 0022), and a revoked credential must
// stop working without waiting for a snapshot (ADR 0023). What is worth testing
// is the part that is easy to get subtly wrong: whether a change to a user
// reaches the credentials they already hold.

func TestSignInProducesAWorkingSession(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	_, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "Alice", "hunter2")
	require.NoError(t, err)

	session, token, user, err := s.SignIn(ctx, tenant.Slug, "alice@example.com", "hunter2", "", "")
	require.NoError(t, err)
	require.Equal(t, "alice@example.com", user.Email)
	require.NotEmpty(t, token)
	require.True(t, session.Active(session.CreatedAt))

	resolved, _, err := s.ResolveSession(ctx, token)
	require.NoError(t, err)
	require.Equal(t, user.ID, resolved.User.ID)
	require.Equal(t, tenant.Slug, resolved.Tenant.Slug)
}

func TestTheSameEmailInTwoTenantsIsTwoPeople(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	one, two := newTenant(t, s), newTenant(t, s)

	_, err := s.CreateUser(ctx, one.ID, "alice@example.com", "", "password-one")
	require.NoError(t, err)
	_, err = s.CreateUser(ctx, two.ID, "alice@example.com", "", "password-two")
	require.NoError(t, err)

	// The tenant is part of the identity, not a lookup hint. One tenant's
	// password must not sign anybody in to another's.
	_, _, _, err = s.SignIn(ctx, one.Slug, "alice@example.com", "password-two", "", "")
	require.ErrorIs(t, err, store.ErrNotFound)

	_, _, user, err := s.SignIn(ctx, one.Slug, "alice@example.com", "password-one", "", "")
	require.NoError(t, err)
	require.Equal(t, one.ID, user.TenantID)
}

func TestAWrongTenantAndAWrongPasswordAreIndistinguishable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	_, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "", "hunter2")
	require.NoError(t, err)

	// Returning early on an unknown tenant would make it measurably faster than
	// a wrong password, which enumerates tenants.
	_, _, _, err = s.SignIn(ctx, "no-such-tenant", "alice@example.com", "hunter2", "", "")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, _, _, err = s.SignIn(ctx, tenant.Slug, "alice@example.com", "wrong", "", "")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestASessionReadsGrantsFresh(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "", "hunter2")
	require.NoError(t, err)
	grant := authz.Grant{Role: authz.RoleToolUser, Scope: "t/" + tenant.Slug}
	require.NoError(t, s.SetGrants(ctx, user.ID, []authz.Grant{grant}, nil))

	_, token, _, err := s.SignIn(ctx, tenant.Slug, "alice@example.com", "hunter2", "", "")
	require.NoError(t, err)

	resolved, _, err := s.ResolveSession(ctx, token)
	require.NoError(t, err)
	require.Equal(t, []authz.Grant{grant}, resolved.Grants)

	// A session that kept the grants it was minted with would keep them after
	// they were taken away — the same mistake ADR 0014 refuses for API keys.
	require.NoError(t, s.SetGrants(ctx, user.ID, nil, nil))
	resolved, _, err = s.ResolveSession(ctx, token)
	require.NoError(t, err)
	require.Empty(t, resolved.Grants)
}

func TestDisablingAUserStopsTheirSession(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "", "hunter2")
	require.NoError(t, err)
	_, token, _, err := s.SignIn(ctx, tenant.Slug, "alice@example.com", "hunter2", "", "")
	require.NoError(t, err)

	_, err = s.UpdateUser(ctx, user.ID, "", "disabled")
	require.NoError(t, err)

	// Checking the session row alone would leave an offboarded employee signed
	// in until it expired.
	_, _, err = s.ResolveSession(ctx, token)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestSigningOutStopsTheSession(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	_, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "", "hunter2")
	require.NoError(t, err)
	_, token, _, err := s.SignIn(ctx, tenant.Slug, "alice@example.com", "hunter2", "", "")
	require.NoError(t, err)

	_, session, err := s.ResolveSession(ctx, token)
	require.NoError(t, err)
	require.NoError(t, s.SignOut(ctx, session.ID))

	_, _, err = s.ResolveSession(ctx, token)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// ------------------------------------------------------------ revocations --

func TestRevokingAUserCoversEveryCredentialTheyHold(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "", "hunter2")
	require.NoError(t, err)
	keyOne, _, err := s.MintAPIKey(ctx, user.ID, "one", nil, nil)
	require.NoError(t, err)
	keyTwo, _, err := s.MintAPIKey(ctx, user.ID, "two", nil, nil)
	require.NoError(t, err)
	_, _, _, err = s.SignIn(ctx, tenant.Slug, "alice@example.com", "hunter2", "", "")
	require.NoError(t, err)

	state, err := s.RevokeUser(ctx, user.ID, "offboarded")
	require.NoError(t, err)
	require.Greater(t, state.Version, int64(0))

	entries, err := s.ListRevocations(ctx)
	require.NoError(t, err)

	refused := map[string]bool{}
	for _, e := range entries {
		refused[e.PrincipalID.String()] = true
	}
	// Both keys and the session. Revoking only the sessions would leave the
	// person gone from the console with their automation still running, which
	// is the failure an offboarding checklist cannot see.
	require.True(t, refused[keyOne.ID.String()])
	require.True(t, refused[keyTwo.ID.String()])
	require.GreaterOrEqual(t, len(entries), 3)
}

func TestRevokingTwiceIsIdempotentAndStillAdvancesTheVersion(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "", "")
	require.NoError(t, err)
	key, _, err := s.MintAPIKey(ctx, user.ID, "one", nil, nil)
	require.NoError(t, err)

	first, err := s.Revoke(ctx, key.ID, "api_key", &user.ID, "leaked")
	require.NoError(t, err)
	second, err := s.Revoke(ctx, key.ID, "api_key", &user.ID, "leaked again")
	require.NoError(t, err)

	// One entry, because a principal is either refused or not. But the version
	// still moves: a data plane refuses a list no newer than the one it holds,
	// so a re-revoke that did not bump would publish a list nobody applies.
	require.Greater(t, second.Version, first.Version)

	entries, err := s.ListRevocations(ctx)
	require.NoError(t, err)
	count := 0
	for _, e := range entries {
		if e.PrincipalID == key.ID {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func TestPruningDropsWhatASnapshotAlreadyReflects(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "", "")
	require.NoError(t, err)
	old, _, err := s.MintAPIKey(ctx, user.ID, "old", nil, nil)
	require.NoError(t, err)
	_, err = s.Revoke(ctx, old.ID, "api_key", &user.ID, "before the build")
	require.NoError(t, err)

	// A build reads the database, then publishes. Everything revoked before
	// that read is already absent from the snapshot.
	readAt := timeNow(t, s, ctx)

	recent, _, err := s.MintAPIKey(ctx, user.ID, "recent", nil, nil)
	require.NoError(t, err)
	_, err = s.Revoke(ctx, recent.ID, "api_key", &user.ID, "after the build")
	require.NoError(t, err)

	state, err := s.PruneRevocations(ctx, 42, readAt)
	require.NoError(t, err)
	require.Equal(t, int64(42), state.PrunedThrough)

	entries, err := s.ListRevocations(ctx)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.PrincipalID.String()] = true
	}
	require.False(t, ids[old.ID.String()], "an entry the snapshot reflects should be pruned")
	// The one revoked *after* the read is not in that snapshot, so pruning it
	// would silently un-revoke a credential.
	require.True(t, ids[recent.ID.String()], "an entry revoked after the read must survive")
}

func TestAnEmptyListStillPublishesAVersion(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	list, err := s.RevocationList(ctx)
	require.NoError(t, err)
	// Version zero would be refused by a data plane as not newer than its
	// starting state. A list that exists and is empty proves the pipeline works
	// before anybody needs it to.
	require.GreaterOrEqual(t, list.Version, int64(1))
}

// timeNow reads the database's clock, not the test process's.
//
// Pruning compares against `revoked_at`, which Postgres sets. Comparing those
// to a Go timestamp would make this test fail on any skew between the two,
// which is a property of the machine rather than of the code.
func timeNow(t *testing.T, s *store.Store, ctx context.Context) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, s.Pool().QueryRow(ctx, "SELECT now()").Scan(&now))
	return now
}

func TestRevokingAKeyLeavesTheConsoleAlone(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "", "hunter2")
	require.NoError(t, err)
	key, secret, err := s.MintAPIKey(ctx, user.ID, "agent", nil, nil)
	require.NoError(t, err)
	_, token, _, err := s.SignIn(ctx, tenant.Slug, "alice@example.com", "hunter2", "", "")
	require.NoError(t, err)

	// The two axes are independent, and each revocation should hit exactly one.
	require.NoError(t, s.RevokeAPIKey(ctx, key.ID))
	_, err = s.ResolveAPIKey(ctx, secret)
	require.Error(t, err, "the agent credential should be dead")

	_, _, err = s.ResolveSession(ctx, token)
	require.NoError(t, err,
		"revoking an agent key must not sign the person out of the console")
}

func TestDisablingAUserKillsBothAxes(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, tenant.ID, "alice@example.com", "", "hunter2")
	require.NoError(t, err)
	_, secret, err := s.MintAPIKey(ctx, user.ID, "agent", nil, nil)
	require.NoError(t, err)
	_, token, _, err := s.SignIn(ctx, tenant.Slug, "alice@example.com", "hunter2", "", "")
	require.NoError(t, err)

	_, err = s.UpdateUser(ctx, user.ID, "", "disabled")
	require.NoError(t, err)
	_, err = s.RevokeUser(ctx, user.ID, "offboarded")
	require.NoError(t, err)

	// The console half is caught by ResolveSession re-reading the owner's
	// status, and the agent half by the key resolving against the same owner.
	_, _, err = s.ResolveSession(ctx, token)
	require.Error(t, err, "a disabled user must not stay signed in")
	_, err = s.ResolveAPIKey(ctx, secret)
	require.Error(t, err, "a disabled user's agents must stop")

	// And the key is in the published list, which is what makes the *data
	// plane* refuse it within a second rather than at the next snapshot. The
	// session does not need to be there — session resolution already caught it
	// — but it is, and listing it costs nothing.
	entries, err := s.ListRevocations(ctx)
	require.NoError(t, err)
	kinds := map[string]int{}
	for _, e := range entries {
		if e.UserID != nil && *e.UserID == user.ID {
			kinds[e.Kind]++
		}
	}
	require.Equal(t, 1, kinds["api_key"])
	require.Equal(t, 1, kinds["session"])
}
