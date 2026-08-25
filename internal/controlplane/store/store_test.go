// Copyright 2026 Henry Zektser.

package store_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// These run against a real Postgres, not a mock.
//
// The behaviour worth testing here is almost entirely the database's: a unique
// constraint firing, a cascade deleting the right rows, a check constraint
// refusing a status. A mock would assert that this package calls the queries it
// was written to call, which is a tautology — and every bug this suite has
// actually caught was in SQL.
//
// Skipped rather than failed when no database is reachable, so `go test ./...`
// stays green on a machine with nothing running. `make up` starts one; CI
// starts one as a service.

func testStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("MCPDOLL_TEST_DATABASE_URL")
	if dsn == "" {
		// mcpdoll_test, not mcpdoll. The suite must not write into the database
		// the local stack is serving from: test tenants would accumulate there,
		// and the platform-admin seeding would refuse to run because tenants
		// already existed — leaving the stack with no way to log in.
		dsn = "postgres://mcpdoll:mcpdoll@localhost:5432/mcpdoll_test?sslmode=disable"
	}

	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("no database at %s (%v); start one with `make up`", dsn, err)
	}
	t.Cleanup(s.Close)

	require.NoError(t, s.Migrate(ctx), "migrations failed")
	return s
}

// uniqueSlug keeps parallel tests from colliding on the unique constraint
// without needing a fresh database per test.
func uniqueSlug(t *testing.T) string {
	t.Helper()
	return "test-" + uuid.NewString()[:8]
}

func newTenant(t *testing.T, s *store.Store) store.Tenant {
	t.Helper()
	tenant, err := s.CreateTenant(context.Background(), uniqueSlug(t), "Test Tenant")
	require.NoError(t, err)
	return tenant
}

// ------------------------------------------------------------------ tenants --

func TestTenantSlugsMustBeScopeSafe(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	// A slug goes inside `t/<slug>/ts/...`. One containing a slash would change
	// what every scope built from it means — a tenant named `a/ts/x` makes
	// `t/a/ts/x` ambiguous with a toolset scope. That is a privilege boundary.
	for _, bad := range []string{"has/slash", "has space", "UPPER", "*", "-leading", "trailing-"} {
		_, err := s.CreateTenant(ctx, bad, "Bad")
		require.ErrorIsf(t, err, store.ErrInvalid, "slug %q should have been refused", bad)
	}
}

func TestDuplicateTenantSlugIsAConflict(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	slug := uniqueSlug(t)
	_, err := s.CreateTenant(ctx, slug, "First")
	require.NoError(t, err)

	_, err = s.CreateTenant(ctx, slug, "Second")
	// Mapped to this package's sentinel rather than leaking a pgx error, so
	// callers can choose a 409 without importing the driver.
	require.ErrorIs(t, err, store.ErrConflict)
}

// -------------------------------------------------------------------- users --

func TestAnEmailIdentifiesOnePerson(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	// It used to be per tenant, and one person could hold two accounts in one
	// deployment. That forced signing in *to a tenant*, and made a moved
	// account fail exactly like a wrong password. A user is a person now, and
	// which tenants they reach is what their grants say.
	email := uniqueSlug(t) + "@example.com"
	_, err := s.CreateUser(ctx, email, "Alice", "")
	require.NoError(t, err)

	_, err = s.CreateUser(ctx, email, "Alice again", "")
	require.ErrorIs(t, err, store.ErrConflict)
}

func TestPasswordHashesNeverLeaveTheStore(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	bob := uniqueSlug(t) + "-bob@example.com"
	user, err := s.CreateUser(ctx, bob, "Bob", "correct horse battery")
	require.NoError(t, err)

	// The domain type carries a boolean, not the hash. There is no field on
	// which a hash could be accidentally serialized into an API response.
	require.True(t, user.HasPassword)

	verified, err := s.VerifyPassword(ctx, bob, "correct horse battery")
	require.NoError(t, err)
	require.Equal(t, user.ID, verified.ID)

	_, err = s.VerifyPassword(ctx, bob, "wrong")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestAnUnknownUserAndAWrongPasswordAreIndistinguishable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	real := uniqueSlug(t) + "-real@example.com"
	_, err := s.CreateUser(ctx, real, "Real", "hunter2")
	require.NoError(t, err)

	_, errWrong := s.VerifyPassword(ctx, real, "nope")
	_, errUnknown := s.VerifyPassword(ctx, "ghost-"+real, "nope")

	// Same sentinel for both, so a caller cannot accidentally build a
	// user-enumeration oracle by branching on the error.
	require.ErrorIs(t, errWrong, store.ErrNotFound)
	require.ErrorIs(t, errUnknown, store.ErrNotFound)
}

// --------------------------------------------------------------------- rbac --

func TestCatalogFallsBackToDefaultsWhenUnseeded(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	catalog, err := s.Catalog(context.Background())
	require.NoError(t, err)

	// An empty catalog would be default-deny for everyone — including the
	// administrator who would need to fix it. A fresh install must authorize.
	require.NotEmpty(t, catalog)
	require.Contains(t, catalog, authz.RolePlatformAdmin)
}

func TestGrantsRoundTripAndRejectMalformedScopes(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-carol@example.com", "Carol", "")
	require.NoError(t, err)

	good := authz.Grant{Role: authz.RoleToolUser, Scope: authz.ToolsetScope(tenant.Slug, "crm")}
	require.NoError(t, s.Grant(ctx, user.ID, good, nil))

	grants, err := s.GrantsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, []authz.Grant{good}, grants)

	// A malformed scope is refused before it reaches the database, because a
	// scope that cannot be parsed cannot be reasoned about later.
	err = s.Grant(ctx, user.ID, authz.Grant{Role: authz.RoleViewer, Scope: "t/"}, nil)
	require.ErrorIs(t, err, store.ErrInvalid)
}

func TestGrantingTwiceIsIdempotent(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-dave@example.com", "Dave", "")
	require.NoError(t, err)

	grant := authz.Grant{Role: authz.RoleViewer, Scope: authz.TenantScope(tenant.Slug)}
	require.NoError(t, s.Grant(ctx, user.ID, grant, nil))
	require.NoError(t, s.Grant(ctx, user.ID, grant, nil))

	grants, err := s.GrantsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Len(t, grants, 1, "granting the same thing twice should not duplicate it")
}

func TestDeletingATenantTakesItsGrantsAndKeys(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-erin@example.com", "Erin", "")
	require.NoError(t, err)
	require.NoError(t, s.Grant(ctx, user.ID,
		authz.Grant{Role: authz.RoleViewer, Scope: authz.TenantScope(tenant.Slug)}, nil))
	_, _, err = s.MintAPIKey(ctx, user.ID, &tenant.ID, "k", nil, nil)
	require.NoError(t, err)

	require.NoError(t, s.DeleteTenant(ctx, tenant.ID))

	// The user survives — they belong to no tenant. What must not survive is
	// their grant into it: a tenant recreated with the same slug would
	// otherwise re-authorize them silently.
	_, err = s.GetUser(ctx, user.ID)
	require.NoError(t, err)

	held, err := s.GrantsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Empty(t, held, "a grant survived its tenant")

	// And the key, which named the tenant, went with it.
	keys, err := s.ListAPIKeysByUser(ctx, user.ID)
	require.NoError(t, err)
	require.Empty(t, keys)
}

// ----------------------------------------------------------------- api keys --

func TestAPIKeyPlaintextIsShownOnceAndNeverStored(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-frank@example.com", "Frank", "")
	require.NoError(t, err)

	key, plaintext, err := s.MintAPIKey(ctx, user.ID, &tenant.ID, "agent", nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, plaintext)

	// The marker exists so a secret scanner can recognise a leaked MCPDoll
	// credential in a repository or a log.
	require.True(t, len(plaintext) > 10 && plaintext[:5] == "mcpd.")

	// The separator must not appear inside the encoded fields, or a key is
	// unparseable exactly when its random bytes happen to encode one.
	require.Equal(t, 3, len(strings.Split(plaintext, ".")),
		"the key split into the wrong number of fields")

	// Listing must never return it.
	keys, err := s.ListAPIKeysByUser(ctx, user.ID)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, key.Prefix, keys[0].Prefix)
	for _, k := range keys {
		require.NotEqual(t, plaintext, k.Prefix)
	}
}

func TestResolvingAKeyIntersectsWithItsOwner(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-grace@example.com", "Grace", "")
	require.NoError(t, err)

	// The owner holds one toolset.
	require.NoError(t, s.Grant(ctx, user.ID, authz.Grant{
		Role: authz.RoleToolUser, Scope: authz.ToolsetScope(tenant.Slug, "crm"),
	}, nil))

	// The key declares one tool inside it, and one toolset outside it.
	_, plaintext, err := s.MintAPIKey(ctx, user.ID, &tenant.ID, "bot", []authz.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolScope(tenant.Slug, "crm", "lookup")},
		{Role: authz.RoleToolUser, Scope: authz.ToolsetScope(tenant.Slug, "billing")},
	}, nil)
	require.NoError(t, err)

	resolved, err := s.ResolveAPIKey(ctx, plaintext)
	require.NoError(t, err)
	require.Equal(t, user.ID, resolved.User.ID)
	require.Equal(t, tenant.ID, resolved.Tenant.ID)

	// Narrowed, not widened: the billing grant was never the owner's to give.
	require.Equal(t, []authz.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolScope(tenant.Slug, "crm", "lookup")},
	}, resolved.Grants)
}

func TestRevokingTheUsersGrantKillsTheKeyWithoutTouchingIt(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-heidi@example.com", "Heidi", "")
	require.NoError(t, err)

	ownerGrant := authz.Grant{
		Role: authz.RoleToolUser, Scope: authz.ToolsetScope(tenant.Slug, "crm"),
	}
	require.NoError(t, s.Grant(ctx, user.ID, ownerGrant, nil))

	_, plaintext, err := s.MintAPIKey(ctx, user.ID, &tenant.ID, "bot", []authz.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolScope(tenant.Slug, "crm", "lookup")},
	}, nil)
	require.NoError(t, err)

	resolved, err := s.ResolveAPIKey(ctx, plaintext)
	require.NoError(t, err)
	require.NotEmpty(t, resolved.Grants)

	// The admin revokes the *user's* grant. Nobody touches the key.
	require.NoError(t, s.RevokeGrant(ctx, user.ID, ownerGrant))

	resolved, err = s.ResolveAPIKey(ctx, plaintext)
	require.NoError(t, err, "the key still authenticates — it just carries nothing")
	require.Empty(t, resolved.Grants,
		"revoking the owner must revoke the key, with no key-by-key cleanup")
}

func TestRevokedAndExpiredKeysDoNotResolve(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-ivan@example.com", "Ivan", "")
	require.NoError(t, err)

	revoked, plaintextRevoked, err := s.MintAPIKey(ctx, user.ID, &tenant.ID, "revoked", nil, nil)
	require.NoError(t, err)
	require.NoError(t, s.RevokeAPIKey(ctx, revoked.ID))

	_, err = s.ResolveAPIKey(ctx, plaintextRevoked)
	require.ErrorIs(t, err, store.ErrInvalid)

	past := time.Now().Add(-time.Hour)
	_, plaintextExpired, err := s.MintAPIKey(ctx, user.ID, &tenant.ID, "expired", nil, &past)
	require.NoError(t, err)

	_, err = s.ResolveAPIKey(ctx, plaintextExpired)
	require.ErrorIs(t, err, store.ErrInvalid)
}

func TestAKeyForADisabledUserDoesNotResolve(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-judy@example.com", "Judy", "")
	require.NoError(t, err)

	_, plaintext, err := s.MintAPIKey(ctx, user.ID, &tenant.ID, "bot", nil, nil)
	require.NoError(t, err)

	require.NoError(t, s.SetUserStatus(ctx, user.ID, "disabled"))

	// Disabling a person must stop their agents too. Checking only the key
	// would leave an offboarded employee's automation running.
	_, err = s.ResolveAPIKey(ctx, plaintext)
	require.ErrorIs(t, err, store.ErrInvalid)
}

func TestAGarbageCredentialIsNotFound(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	for _, bad := range []string{"", "nonsense", "mcpd.onlytwo", "wrong.prefix.secret"} {
		_, err := s.ResolveAPIKey(ctx, bad)
		require.ErrorIsf(t, err, store.ErrNotFound, "credential %q", bad)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	// testStore already migrated. Running again must be a no-op rather than
	// an error, because every process start calls this.
	require.NoError(t, s.Migrate(context.Background()))
	require.NoError(t, s.Migrate(context.Background()))
}

// --------------------------------------------------------- declarative sets --

func TestSetGrantsRevokesWhatIsNotInTheSet(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-alice@example.com", "", "")
	require.NoError(t, err)

	support := authz.Grant{Role: authz.RoleToolUser, Scope: "t/" + tenant.Slug + "/ts/support"}
	people := authz.Grant{Role: authz.RoleToolUser, Scope: "t/" + tenant.Slug + "/ts/people"}

	require.NoError(t, s.SetGrants(ctx, user.ID, []authz.Grant{support, people}, nil))
	held, err := s.GrantsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Len(t, held, 2)

	// The whole point of the declarative form: dropping one from the submitted
	// set revokes it. Expressing this as a sequence of deltas is how a
	// revocation gets forgotten.
	require.NoError(t, s.SetGrants(ctx, user.ID, []authz.Grant{support}, nil))
	held, err = s.GrantsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, []authz.Grant{support}, held)

	require.NoError(t, s.SetGrants(ctx, user.ID, nil, nil))
	held, err = s.GrantsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Empty(t, held, "an empty set strips the account without deleting it")
}

func TestSetGrantsRefusesTheWholeSetOnOneMalformedScope(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-bob@example.com", "", "")
	require.NoError(t, err)

	good := authz.Grant{Role: authz.RoleToolUser, Scope: "t/" + tenant.Slug}
	require.NoError(t, s.SetGrants(ctx, user.ID, []authz.Grant{good}, nil))

	// Validated before anything is written. A partial apply would leave the
	// user holding some of what was asked for and none of the rest, which is a
	// state nobody intended and nobody can see.
	err = s.SetGrants(ctx, user.ID, []authz.Grant{
		good,
		{Role: authz.RoleToolUser, Scope: "not-a-scope"},
	}, nil)
	require.ErrorIs(t, err, store.ErrInvalid)

	held, err := s.GrantsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, []authz.Grant{good}, held, "nothing changed")
}

func TestSetGrantsLeavesUnchangedGrantsAlone(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	tenant := newTenant(t, s)

	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-carol@example.com", "", "")
	require.NoError(t, err)

	want := []authz.Grant{{Role: authz.RoleToolUser, Scope: "t/" + tenant.Slug}}
	require.NoError(t, s.SetGrants(ctx, user.ID, want, nil))
	require.NoError(t, s.SetGrants(ctx, user.ID, want, nil))

	held, err := s.GrantsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, want, held, "re-submitting an unchanged set is a no-op")
}

func TestCountUsersByTenantCountsFromGrants(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	one, two := newTenant(t, s), newTenant(t, s)

	// The baseline first, and the assertions below are on the delta.
	//
	// A count of exactly two would be asserting isolation this suite does not
	// have: a grant at the global scope reaches *every* tenant, so any test
	// anywhere that creates a platform admin lands in this tenant's count. The
	// count is right; expecting to be alone in the database was not.
	before, err := s.CountUsersByTenant(ctx)
	require.NoError(t, err)

	// Counted from grants, because a user belongs to no tenant. Creating one
	// puts them in no tenant at all; granting is what puts them somewhere.
	for range 2 {
		u, err := s.CreateUser(ctx, uniqueSlug(t)+"@example.com", "", "")
		require.NoError(t, err)
		require.NoError(t, s.SetGrants(ctx, u.ID,
			[]authz.Grant{{Role: authz.RoleToolUser, Scope: authz.TenantScope(one.Slug)}}, nil))
	}
	u, err := s.CreateUser(ctx, uniqueSlug(t)+"@example.com", "", "")
	require.NoError(t, err)
	require.NoError(t, s.SetGrants(ctx, u.ID,
		[]authz.Grant{{Role: authz.RoleToolUser, Scope: authz.TenantScope(two.Slug)}}, nil))

	counts, err := s.CountUsersByTenant(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, counts[one.ID]-before[one.ID])
	require.Equal(t, 1, counts[two.ID]-before[two.ID])
}

func TestUpdateUserRefusesAnUnknownStatus(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	user, err := s.CreateUser(ctx, uniqueSlug(t)+"-dave@example.com", "Dave", "")
	require.NoError(t, err)

	// A typo that silently stored "disable" would leave an account the operator
	// believes is shut off still authenticating.
	_, err = s.UpdateUser(ctx, user.ID, "Dave", "disable")
	require.ErrorIs(t, err, store.ErrInvalid)

	after, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, "active", after.Status)
}
