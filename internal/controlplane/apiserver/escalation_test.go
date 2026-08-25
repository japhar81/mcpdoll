// Copyright 2026 Henry Zektser.

package apiserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/apiserver"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// You cannot grant what you do not hold.
//
// This is the check that stops the permission set being decoration: without it
// a tenant admin could grant themselves platform_admin at `*` and the whole
// structure would be one request away from meaningless.
//
// Against a real database, because the thing being tested is a decision made
// from grants read out of one. Skipped when none is reachable, like the rest of
// the store suite.

func liveServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()

	dsn := os.Getenv("MCPDOLL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://mcpdoll:mcpdoll@localhost:5432/mcpdoll_test?sslmode=disable"
	}
	ctx := context.Background()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("no database at %s (%v); start one with `make up`", dsn, err)
	}
	t.Cleanup(db.Close)
	require.NoError(t, db.Migrate(ctx))
	require.NoError(t, db.SeedCatalog(ctx))

	h, err := apiserver.New(apiserver.Config{
		RegistryPath: writeRegistry(t, registryYAML),
		Token:        testToken,
		Version:      "test",
		Store:        db,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return h, db
}

// uniqueTenantSlug keeps parallel runs and repeated runs from colliding on the
// unique constraint. These tests are not parallel — they share one database and
// assert on cross-tenant visibility — but they do run repeatedly.
func uniqueTenantSlug(t *testing.T) string {
	t.Helper()
	return "authz-" + uuid.NewString()[:8]
}

// asUser creates a user with the given grants and returns a session token.
func asUser(t *testing.T, db *store.Store, tenantSlug, email string, grants ...authz.Grant) string {
	t.Helper()
	ctx := context.Background()

	if _, err := db.GetTenantBySlug(ctx, tenantSlug); err != nil {
		_, err = db.CreateTenant(ctx, tenantSlug, tenantSlug)
		require.NoError(t, err)
	}
	user, err := db.CreateUser(ctx, email, "", "hunter2")
	require.NoError(t, err)
	require.NoError(t, db.SetGrants(ctx, user.ID, grants, nil))

	_, token, _, err := db.SignIn(ctx, email, "hunter2", "", "")
	require.NoError(t, err)
	return token
}

func asToken(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func TestATenantAdminCannotGrantThemselvesPlatformAdmin(t *testing.T) {
	h, db := liveServer(t)
	ctx := context.Background()

	slug := uniqueTenantSlug(t)
	token := asUser(t, db, slug, "admin@"+slug+".example",
		authz.Grant{Role: authz.RoleTenantAdmin, Scope: authz.TenantScope(slug)})

	me := userByEmail(t, db, "admin@"+slug+".example")
	self := me.ID.String()

	// The route check passes: they hold role:manage at their own tenant, and
	// the target user is in it. The escalation check is a *different* question
	// — do they hold role:manage at the scope of the grant being issued — and
	// it is the only thing standing between this and total authority.
	rec := do(t, h, http.MethodPut, "/api/v1/users/"+self+"/grants",
		apiserver.PutGrantsRequest{Grants: []api.Grant{
			{Role: authz.RolePlatformAdmin, Scope: authz.GlobalScope},
		}}, asToken(token))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "you cannot grant")

	held, err := db.GrantsForUser(ctx, me.ID)
	require.NoError(t, err)
	require.Len(t, held, 1, "nothing was written")
}

func TestATenantAdminCannotGrantIntoAnotherTenant(t *testing.T) {
	h, db := liveServer(t)
	ctx := context.Background()

	mine, theirs := uniqueTenantSlug(t), uniqueTenantSlug(t)
	token := asUser(t, db, mine, "admin@"+mine+".example",
		authz.Grant{Role: authz.RoleTenantAdmin, Scope: authz.TenantScope(mine)})

	_, err := db.CreateTenant(ctx, theirs, theirs)
	require.NoError(t, err)
	victim, err := db.CreateUser(ctx, "victim@"+theirs+".example", "", "")
	require.NoError(t, err)

	// Refused at the route, before the handler: the scope of the operation is
	// the *target user's* tenant, which this caller holds nothing at.
	rec := do(t, h, http.MethodPut, "/api/v1/users/"+victim.ID.String()+"/grants",
		apiserver.PutGrantsRequest{Grants: []api.Grant{
			{Role: authz.RoleToolUser, Scope: authz.TenantScope(theirs)},
		}}, asToken(token))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestATenantAdminSeesOnlyTheirOwnTenant(t *testing.T) {
	h, db := liveServer(t)

	mine, theirs := uniqueTenantSlug(t), uniqueTenantSlug(t)
	token := asUser(t, db, mine, "admin@"+mine+".example",
		authz.Grant{Role: authz.RoleTenantAdmin, Scope: authz.TenantScope(mine)})
	_, err := db.CreateTenant(context.Background(), theirs, theirs)
	require.NoError(t, err)

	// Filtered, not refused. A control plane that answers "forbidden" to a
	// question the caller is partly entitled to ask is useless to anybody who
	// is not a platform administrator.
	rec := do(t, h, http.MethodGet, "/api/v1/tenants", nil, asToken(token))
	require.Equal(t, http.StatusOK, rec.Code)

	var list api.TenantList
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))

	slugs := map[string]bool{}
	for _, tn := range list.Registered {
		slugs[tn.Slug] = true
	}
	require.True(t, slugs[mine])
	require.False(t, slugs[theirs], "another tenant was visible")
}

func TestAUserWithNoGrantsIsRefusedEverything(t *testing.T) {
	h, db := liveServer(t)

	slug := uniqueTenantSlug(t)
	token := asUser(t, db, slug, "nobody@"+slug+".example")

	// Signing in works and authorizes nothing. That is the correct state for an
	// account nobody has granted anything yet, and it must read as a refusal
	// rather than as a broken login.
	for _, path := range []string{"/api/v1/registry", "/api/v1/gateway/status"} {
		rec := do(t, h, http.MethodGet, path, nil, asToken(token))
		require.Equal(t, http.StatusForbidden, rec.Code, path)
	}

	// But they can still ask who they are, or there would be no way to see
	// that the account is real and simply holds nothing.
	rec := do(t, h, http.MethodGet, "/api/v1/auth/session", nil, asToken(token))
	require.Equal(t, http.StatusOK, rec.Code)

	var me api.SessionInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	require.Equal(t, "session", me.Kind)
	require.Empty(t, me.Permissions)
}

func TestSigningInThroughTheAPIWorksEndToEnd(t *testing.T) {
	h, db := liveServer(t)
	ctx := context.Background()

	slug := uniqueTenantSlug(t)
	_, err := db.CreateTenant(ctx, slug, slug)
	require.NoError(t, err)
	user, err := db.CreateUser(ctx, "alice@"+slug+".example", "Alice", "hunter2")
	require.NoError(t, err)
	require.NoError(t, db.SetGrants(ctx, user.ID,
		[]authz.Grant{{Role: authz.RoleViewer, Scope: authz.TenantScope(slug)}}, nil))

	rec := do(t, h, http.MethodPost, "/api/v1/auth/login", apiserver.LoginRequest{
		Email: "alice@" + slug + ".example", Password: "hunter2",
	}, func(r *http.Request) { r.Header.Del("Authorization") })
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var session api.Session
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &session))
	require.NotEmpty(t, session.Token)
	require.Equal(t, "alice@"+slug+".example", session.User.Email)

	// The token works, which is the whole claim: a local password is a
	// principal, and no identity provider was involved.
	rec = do(t, h, http.MethodGet, "/api/v1/auth/session", nil, asToken(session.Token))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestEveryFailedSignInAnswersIdentically(t *testing.T) {
	h, db := liveServer(t)
	ctx := context.Background()

	slug := uniqueTenantSlug(t)
	_, err := db.CreateTenant(ctx, slug, slug)
	require.NoError(t, err)
	_, err = db.CreateUser(ctx, "alice@"+slug+".example", "", "hunter2")
	require.NoError(t, err)

	noAuth := func(r *http.Request) { r.Header.Del("Authorization") }
	for _, req := range []apiserver.LoginRequest{
		{Email: "alice@" + slug + ".example", Password: "wrong"},
		{Email: "nobody@" + slug + ".example", Password: "hunter2"},
	} {
		rec := do(t, h, http.MethodPost, "/api/v1/auth/login", req, noAuth)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		// One answer for every failure. A caller learns whether they signed in
		// and nothing else — not whether the tenant exists, not whether the
		// email does.
		require.Contains(t, rec.Body.String(), "email or password is wrong")
	}
}

// ---------------------------------------------------- user-defined roles ----

// The escalation that editable roles would otherwise make trivial (ADR 0028).
//
// A tenant admin holds role:manage in their own tenant, so the route check and
// the scope check both pass. If nothing looked at *what the role confers*, they
// could hand themselves any permission in the vocabulary.
func TestATenantAdminCannotGrantThemselvesARoleTheyCannotHold(t *testing.T) {
	h, db := liveServer(t)
	ctx := context.Background()

	slug := uniqueTenantSlug(t)
	token := asUser(t, db, slug, "admin@"+slug+".example",
		authz.Grant{Role: authz.RoleTenantAdmin, Scope: authz.TenantScope(slug)})

	// Defined by somebody who is allowed to: it carries a permission a tenant
	// admin deliberately does not have.
	role := "escalator-" + uniqueTenantSlug(t)
	_, err := db.PutRole(ctx, role, "carries what a tenant admin lacks",
		[]authz.Permission{authz.PermKeyGenerate})
	require.NoError(t, err)

	me := userByEmail(t, db, "admin@"+slug+".example")
	self := me.ID.String()

	// In their *own* tenant, where they hold role:manage. The only thing that
	// can refuse this is the conferred-permission check.
	rec := do(t, h, http.MethodPut, "/api/v1/users/"+self+"/grants",
		apiserver.PutGrantsRequest{Grants: []api.Grant{
			{Role: role, Scope: authz.TenantScope(slug)},
		}}, asToken(token))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "signingkey:generate")

	held, err := db.GrantsForUser(ctx, me.ID)
	require.NoError(t, err)
	require.Len(t, held, 1, "nothing was written")
}

// The same hole through a built-in role, which is why this is not only about
// user-defined ones: tenant_admin deliberately lacks tenant:manage, and
// platform_admin has it.
func TestATenantAdminCannotGrantThemselvesPlatformAdminInTheirOwnTenant(t *testing.T) {
	h, db := liveServer(t)
	ctx := context.Background()

	slug := uniqueTenantSlug(t)
	token := asUser(t, db, slug, "admin@"+slug+".example",
		authz.Grant{Role: authz.RoleTenantAdmin, Scope: authz.TenantScope(slug)})

	me := userByEmail(t, db, "admin@"+slug+".example")
	self := me.ID.String()

	rec := do(t, h, http.MethodPut, "/api/v1/users/"+self+"/grants",
		apiserver.PutGrantsRequest{Grants: []api.Grant{
			{Role: authz.RolePlatformAdmin, Scope: authz.TenantScope(slug)},
		}}, asToken(token))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	held, err := db.GrantsForUser(ctx, me.ID)
	require.NoError(t, err)
	require.Len(t, held, 1)
}

// A grant that confers nothing new is still allowed, or the check would make
// delegation impossible rather than safe.
func TestAnAdminMayGrantARoleTheyFullyHold(t *testing.T) {
	h, db := liveServer(t)

	slug := uniqueTenantSlug(t)
	token := asUser(t, db, slug, "admin@"+slug+".example",
		authz.Grant{Role: authz.RoleTenantAdmin, Scope: authz.TenantScope(slug)})

	me := userByEmail(t, db, "admin@"+slug+".example")
	self := me.ID.String()

	// tool_user is tool:list + tool:call, both of which tenant_admin holds.
	rec := do(t, h, http.MethodPut, "/api/v1/users/"+self+"/grants",
		apiserver.PutGrantsRequest{Grants: []api.Grant{
			{Role: authz.RoleTenantAdmin, Scope: authz.TenantScope(slug)},
			{Role: authz.RoleToolUser, Scope: authz.TenantScope(slug)},
		}}, asToken(token))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Defining a role you could not grant is refused where it is typed, rather
// than being stored and refused later at every attempt to use it.
func TestARoleCannotBeDefinedWithAPermissionTheAuthorLacks(t *testing.T) {
	h, db := liveServer(t)

	slug := uniqueTenantSlug(t)
	token := asUser(t, db, slug, "admin@"+slug+".example",
		authz.Grant{Role: authz.RoleTenantAdmin, Scope: authz.TenantScope(slug)})

	rec := do(t, h, http.MethodPut, "/api/v1/roles/sneaky",
		apiserver.PutRoleRequest{Permissions: []string{string(authz.PermKeyGenerate)}},
		asToken(token))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// The vocabulary is closed. A permission nothing enforces would be a role that
// looks like it grants something and does not, and a typo would be
// indistinguishable from a deliberate restriction.
func TestARoleCannotCarryAPermissionThatDoesNotExist(t *testing.T) {
	h, db := liveServer(t)

	slug := uniqueTenantSlug(t)
	token := asUser(t, db, slug, "root@"+slug+".example",
		authz.Grant{Role: authz.RolePlatformAdmin, Scope: authz.GlobalScope})

	rec := do(t, h, http.MethodPut, "/api/v1/roles/typo",
		apiserver.PutRoleRequest{Permissions: []string{"tool:calll"}}, asToken(token))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "no such permission")
}

// userByEmail finds a user by the address the test gave them.
//
// Not `ListUsersInTenant(...)[0]`, which these tests used to do. That lists
// everyone *granted into* a tenant, and a global grant reaches every tenant —
// so any test anywhere that creates a platform admin lands in every other
// test's listing. The old form passed only while no test happened to create
// one, which is not isolation, it is luck.
func userByEmail(t *testing.T, db *store.Store, email string) store.User {
	t.Helper()
	users, err := db.ListAllUsers(context.Background())
	require.NoError(t, err)
	for _, u := range users {
		if u.Email == email {
			return u
		}
	}
	t.Fatalf("no user %q", email)
	return store.User{}
}
