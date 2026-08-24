// Copyright 2026 The MCPDoll Authors.

package authz_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// ------------------------------------------------------------------ scopes ---

func TestParseScopeRejectsRatherThanDefaulting(t *testing.T) {
	t.Parallel()

	// The dangerous failure mode: a malformed scope parsing as something
	// permissive. Every one of these must be refused, not coerced.
	for _, malformed := range []string{
		"",
		"acme",              // no t/ prefix
		"t/",                // no tenant
		"t//ts/crm",         // empty tenant
		"t/acme/ts/",        // empty toolset
		"t/acme/ts/crm/",    // empty tool
		"t/acme/crm",        // wrong separator for a toolset
		"t/acme/ts/crm/a/b", // nothing exists below a tool
		"**",
		"/",
	} {
		_, ok := authz.ParseScope(malformed)
		require.Falsef(t, ok, "%q parsed as a valid scope", malformed)
	}
}

func TestParseScopeReadsEachDepth(t *testing.T) {
	t.Parallel()

	global, ok := authz.ParseScope(authz.GlobalScope)
	require.True(t, ok)
	require.True(t, global.Global)
	require.Empty(t, global.Tenant)

	tenant, ok := authz.ParseScope(authz.TenantScope("acme"))
	require.True(t, ok)
	require.Equal(t, authz.ParsedScope{Tenant: "acme"}, tenant)

	toolset, ok := authz.ParseScope(authz.ToolsetScope("acme", "crm"))
	require.True(t, ok)
	require.Equal(t, authz.ParsedScope{Tenant: "acme", Toolset: "crm"}, toolset)

	tool, ok := authz.ParseScope(authz.ToolScope("acme", "crm", "lookup"))
	require.True(t, ok)
	require.Equal(t, authz.ParsedScope{Tenant: "acme", Toolset: "crm", Tool: "lookup"}, tool)
}

func TestScopeCoversIsNotSymmetric(t *testing.T) {
	t.Parallel()

	wide := authz.TenantScope("acme")
	narrow := authz.ToolScope("acme", "crm", "lookup")

	require.True(t, authz.ScopeCovers(wide, narrow), "wide should cover narrow")
	require.False(t, authz.ScopeCovers(narrow, wide),
		"narrow must NOT cover wide — this direction is the privilege escalation")
}

func TestScopeCoversRespectsNameBoundaries(t *testing.T) {
	t.Parallel()

	// The bug a bare strings.HasPrefix would have. Each of these pairs shares
	// a textual prefix and must not cover.
	for _, pair := range [][2]string{
		{authz.TenantScope("acme"), authz.TenantScope("acme-corp")},
		{authz.ToolsetScope("acme", "crm"), authz.ToolsetScope("acme", "crm-legacy")},
		{authz.ToolScope("acme", "crm", "lookup"), authz.ToolScope("acme", "crm", "lookup_all")},
	} {
		require.Falsef(t, authz.ScopeCovers(pair[0], pair[1]),
			"%q must not cover %q — they only share a prefix", pair[0], pair[1])
	}
}

func TestTenantOfIsEmptyForGlobal(t *testing.T) {
	t.Parallel()

	// Global is not "tenant zero". A caller filtering by tenant must not
	// mistake a platform-wide grant for a grant in a tenant named "".
	require.Empty(t, authz.TenantOf(authz.GlobalScope))
	require.Empty(t, authz.TenantOf("nonsense"))
	require.Equal(t, "acme", authz.TenantOf(authz.ToolScope("acme", "crm", "x")))
}

// ------------------------------------------------------------------- roles ---

func TestDefaultCatalogIsValid(t *testing.T) {
	t.Parallel()
	require.NoError(t, authz.ValidateCatalog(authz.DefaultCatalog()))
}

func TestCatalogRefusesCallWithoutList(t *testing.T) {
	t.Parallel()

	// ADR 0015's invariant: a tool that can be invoked but never appears in a
	// catalog is a hidden capability.
	err := authz.ValidateCatalog(authz.Catalog{
		"sneaky": {authz.PermToolCall: {}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "hidden capability")
}

func TestCatalogReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	err := authz.ValidateCatalog(authz.Catalog{
		"a": {authz.PermToolCall: {}},
		"b": {"not:a:permission": {}},
	})
	require.Error(t, err)
	// One pass, both problems. Fixing a role model one error per attempt is
	// the experience this avoids.
	require.Contains(t, err.Error(), "2 problem(s)")
	require.Contains(t, err.Error(), "hidden capability")
	require.Contains(t, err.Error(), "unknown permission")
}

func TestListWithoutCallIsAllowed(t *testing.T) {
	t.Parallel()

	// The reverse of the forbidden combination. A reviewer who may see what
	// exists without being able to fire it is a legitimate role — `viewer` is
	// exactly that.
	require.NoError(t, authz.ValidateCatalog(authz.Catalog{
		"reviewer": {authz.PermToolList: {}},
	}))
}

func TestTenantAdminCannotMintASigningKey(t *testing.T) {
	t.Parallel()

	catalog := authz.DefaultCatalog()
	perms := catalog[authz.RoleTenantAdmin]

	// A signing key is trusted by every data plane in the deployment, so it is
	// deliberately above a tenant administrator's authority. If this ever
	// becomes convenient to change, it needs an ADR rather than a commit.
	require.NotContains(t, perms, authz.PermKeyGenerate)
	require.Contains(t, catalog[authz.RolePlatformAdmin], authz.PermKeyGenerate)
}

func TestPublisherCannotEditWhatItPublishes(t *testing.T) {
	t.Parallel()

	perms := authz.DefaultCatalog()[authz.RolePublisher]

	// publisher ≠ approver, expressed in the default catalog: the role that
	// puts a snapshot into production cannot change what went into it.
	require.Contains(t, perms, authz.PermSnapshotPublish)
	require.NotContains(t, perms, authz.PermRegistryWrite)
	require.NotContains(t, perms, authz.PermSnapshotBuild)
}

func TestGrantValidation(t *testing.T) {
	t.Parallel()

	require.NoError(t, authz.Grant{Role: "viewer", Scope: authz.GlobalScope}.Validate())
	require.Error(t, authz.Grant{Scope: authz.GlobalScope}.Validate())
	require.Error(t, authz.Grant{Role: "viewer"}.Validate())

	err := authz.Grant{Role: "viewer", Scope: "t/"}.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "malformed scope")
}

// -------------------------------------------------------------- intersect ---

func TestIntersectNarrowsAKeyToItsOwner(t *testing.T) {
	t.Parallel()

	owner := []authz.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "crm")},
		{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "hr")},
	}
	key := []authz.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolScope("acme", "crm", "lookup")},
	}

	// A key may name a narrower scope than its owner holds. That is the point.
	require.Equal(t, key, authz.Intersect(key, owner))
}

func TestIntersectRefusesToWiden(t *testing.T) {
	t.Parallel()

	owner := []authz.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "crm")},
	}
	key := []authz.Grant{
		// Broader than the owner's, and in a toolset the owner cannot reach.
		{Role: authz.RoleToolUser, Scope: authz.TenantScope("acme")},
		{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "billing")},
	}

	// Silently narrowed to nothing rather than erroring: the owner's grants
	// can shrink after a key is minted, and this is that case.
	require.Empty(t, authz.Intersect(key, owner))
}

func TestIntersectRequiresTheSameRole(t *testing.T) {
	t.Parallel()

	owner := []authz.Grant{{Role: authz.RoleViewer, Scope: authz.GlobalScope}}
	key := []authz.Grant{{Role: authz.RoleToolUser, Scope: authz.GlobalScope}}

	// The owner holds global scope, but as viewer. A key cannot acquire
	// tool_user's permissions by naming the role its owner does not hold.
	require.Empty(t, authz.Intersect(key, owner))
}

func TestRevokingTheOwnerRevokesTheKey(t *testing.T) {
	t.Parallel()

	key := []authz.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolScope("acme", "crm", "lookup")},
	}
	owner := []authz.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "crm")},
	}
	require.NotEmpty(t, authz.Intersect(key, owner))

	// The admin revokes the user's grant. The key must die with it, without
	// anybody touching the key — this is the property ADR 0014 rests on, and
	// the reason the intersection is recomputed rather than stored.
	require.Empty(t, authz.Intersect(key, nil))
}

func TestIntersectHandlesEmptyInputs(t *testing.T) {
	t.Parallel()

	grants := []authz.Grant{{Role: authz.RoleViewer, Scope: authz.GlobalScope}}

	// A key that declares nothing holds nothing. It does not inherit its
	// owner's grants by omission — which would be the widening this design
	// refuses.
	require.Empty(t, authz.Intersect(nil, grants))
	require.Empty(t, authz.Intersect(grants, nil))
}

// --------------------------------------------------------------- deciders ---

func TestDenyAllDenies(t *testing.T) {
	t.Parallel()
	decide := authz.DenyAll()
	require.False(t, decide(authz.PermToolList, authz.GlobalScope))
	require.False(t, decide(authz.PermToolCall, authz.TenantScope("acme")))
}
