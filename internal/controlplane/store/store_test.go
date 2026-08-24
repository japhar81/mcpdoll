// Copyright 2026 The MCPDoll Authors.

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
		dsn = "postgres://mcpdoll:mcpdoll@localhost:5432/mcpdoll?sslmode=disable"
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

func TestTheSameEmailMayExistInTwoTenants(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	a := newTenant(t, s)
	b := newTenant(t, s)

	// Uniqueness is per tenant. The same person holding accounts in two
	// tenants of one deployment is normal, and they are different principals.
	_, err := s.CreateUser(ctx, a.ID, "alice@example.com", "Alice", "")
	require.NoError(t, err)
	_, err = s.CreateUser(ctx, b.ID, "alice@example.com", "Alice", "")
	require.NoError(t, err)

	_, err = s.CreateUser(ctx, a.ID, "alice@example.com", "Alice again", "")
	require.ErrorIs(t, err, store.ErrConflict)
}

func TestPasswordHashesNeverLeaveTheStore(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, tenant.ID, "bob@example.com", "Bob", "correct horse battery")
	require.NoError(t, err)

	// The domain type carries a boolean, not the hash. There is no field on
	// which a hash could be accidentally serialized into an API response.
	require.True(t, user.HasPassword)

	verified, err := s.VerifyPassword(ctx, tenant.ID, "bob@example.com", "correct horse battery")
	require.NoError(t, err)
	require.Equal(t, user.ID, verified.ID)

	_, err = s.VerifyPassword(ctx, tenant.ID, "bob@example.com", "wrong")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestAnUnknownUserAndAWrongPasswordAreIndistinguishable(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	_, err := s.CreateUser(ctx, tenant.ID, "real@example.com", "Real", "hunter2")
	require.NoError(t, err)

	wrongPassword := s.VerifyPassword
	_, errWrong := wrongPassword(ctx, tenant.ID, "real@example.com", "nope")
	_, errUnknown := s.VerifyPassword(ctx, tenant.ID, "ghost@example.com", "nope")

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
	user, err := s.CreateUser(ctx, tenant.ID, "carol@example.com", "Carol", "")
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
	user, err := s.CreateUser(ctx, tenant.ID, "dave@example.com", "Dave", "")
	require.NoError(t, err)

	grant := authz.Grant{Role: authz.RoleViewer, Scope: authz.TenantScope(tenant.Slug)}
	require.NoError(t, s.Grant(ctx, user.ID, grant, nil))
	require.NoError(t, s.Grant(ctx, user.ID, grant, nil))

	grants, err := s.GrantsForUser(ctx, user.ID)
	require.NoError(t, err)
	require.Len(t, grants, 1, "granting the same thing twice should not duplicate it")
}

func TestDeletingATenantCascades(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, tenant.ID, "erin@example.com", "Erin", "")
	require.NoError(t, err)
	require.NoError(t, s.Grant(ctx, user.ID,
		authz.Grant{Role: authz.RoleViewer, Scope: authz.TenantScope(tenant.Slug)}, nil))
	_, _, err = s.MintAPIKey(ctx, user.ID, "k", nil, nil)
	require.NoError(t, err)

	require.NoError(t, s.DeleteTenant(ctx, tenant.ID))

	// Ownership rather than a filter: deleting a tenant removes its users,
	// their grants, and their keys, without anybody writing the cleanup.
	_, err = s.GetUser(ctx, user.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// ----------------------------------------------------------------- api keys --

func TestAPIKeyPlaintextIsShownOnceAndNeverStored(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, tenant.ID, "frank@example.com", "Frank", "")
	require.NoError(t, err)

	key, plaintext, err := s.MintAPIKey(ctx, user.ID, "agent", nil, nil)
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
	user, err := s.CreateUser(ctx, tenant.ID, "grace@example.com", "Grace", "")
	require.NoError(t, err)

	// The owner holds one toolset.
	require.NoError(t, s.Grant(ctx, user.ID, authz.Grant{
		Role: authz.RoleToolUser, Scope: authz.ToolsetScope(tenant.Slug, "crm"),
	}, nil))

	// The key declares one tool inside it, and one toolset outside it.
	_, plaintext, err := s.MintAPIKey(ctx, user.ID, "bot", []authz.Grant{
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
	user, err := s.CreateUser(ctx, tenant.ID, "heidi@example.com", "Heidi", "")
	require.NoError(t, err)

	ownerGrant := authz.Grant{
		Role: authz.RoleToolUser, Scope: authz.ToolsetScope(tenant.Slug, "crm"),
	}
	require.NoError(t, s.Grant(ctx, user.ID, ownerGrant, nil))

	_, plaintext, err := s.MintAPIKey(ctx, user.ID, "bot", []authz.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolScope(tenant.Slug, "crm", "lookup")},
	}, nil)
	require.NoError(t, err)

	resolved, err := s.ResolveAPIKey(ctx, plaintext)
	require.NoError(t, err)
	require.NotEmpty(t, resolved.Grants)

	// The admin revokes the *user's* grant. Nobody touches the key.
	require.NoError(t, s.Revoke(ctx, user.ID, ownerGrant))

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
	user, err := s.CreateUser(ctx, tenant.ID, "ivan@example.com", "Ivan", "")
	require.NoError(t, err)

	revoked, plaintextRevoked, err := s.MintAPIKey(ctx, user.ID, "revoked", nil, nil)
	require.NoError(t, err)
	require.NoError(t, s.RevokeAPIKey(ctx, revoked.ID))

	_, err = s.ResolveAPIKey(ctx, plaintextRevoked)
	require.ErrorIs(t, err, store.ErrInvalid)

	past := time.Now().Add(-time.Hour)
	_, plaintextExpired, err := s.MintAPIKey(ctx, user.ID, "expired", nil, &past)
	require.NoError(t, err)

	_, err = s.ResolveAPIKey(ctx, plaintextExpired)
	require.ErrorIs(t, err, store.ErrInvalid)
}

func TestAKeyForADisabledUserDoesNotResolve(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()

	tenant := newTenant(t, s)
	user, err := s.CreateUser(ctx, tenant.ID, "judy@example.com", "Judy", "")
	require.NoError(t, err)

	_, plaintext, err := s.MintAPIKey(ctx, user.ID, "bot", nil, nil)
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
