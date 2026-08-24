// Copyright 2026 Henry Zektser.

package authz_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// The conformance suite.
//
// Two engines implement one model, and that is only defensible while they
// agree on everything. A disagreement would surface as an authorization
// difference between a test run and a production deployment — the builtin
// engine decides in CI, Casbin decides in the cluster — which is the worst
// possible place to discover a policy bug.
//
// If this test is ever weakened to make a change land, delete one of the two
// engines instead of keeping both.

func engines(t *testing.T) map[string]authz.Engine {
	t.Helper()
	casbinEngine, err := authz.NewCasbinEngine(context.Background())
	require.NoError(t, err, "the casbin engine failed its boot probe")
	return map[string]authz.Engine{
		"builtin": authz.BuiltinEngine{},
		"casbin":  casbinEngine,
	}
}

// conformanceCase is one (grants, catalog) compilation and the questions asked
// of it. Every question is asked of both engines.
type conformanceCase struct {
	name    string
	catalog authz.Catalog
	grants  []authz.Grant
	// asks maps a question to the expected answer, so the suite pins the
	// *behaviour* as well as the agreement. Two engines that agree on a wrong
	// answer would otherwise pass.
	asks []ask
}

type ask struct {
	permission authz.Permission
	scope      string
	want       bool
	why        string
}

func conformanceCases() []conformanceCase {
	std := authz.DefaultCatalog()

	return []conformanceCase{
		{
			name:    "a tenant grant covers every depth beneath it",
			catalog: std,
			grants:  []authz.Grant{{Role: authz.RoleToolUser, Scope: authz.TenantScope("acme")}},
			asks: []ask{
				{authz.PermToolList, authz.TenantScope("acme"), true,
					"the grant's own scope"},
				{authz.PermToolList, authz.ToolsetScope("acme", "crm"), true,
					"a toolset inside the granted tenant"},
				{authz.PermToolCall, authz.ToolScope("acme", "crm", "lookup_customer"), true,
					"a tool inside the granted tenant"},
			},
		},
		{
			name:    "a toolset grant does not reach the tenant above it",
			catalog: std,
			grants: []authz.Grant{
				{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "crm")},
			},
			asks: []ask{
				{authz.PermToolCall, authz.ToolScope("acme", "crm", "lookup_customer"), true,
					"a tool inside the granted toolset"},
				{authz.PermToolCall, authz.ToolsetScope("acme", "crm"), true,
					"the granted toolset itself"},
				{authz.PermToolCall, authz.TenantScope("acme"), false,
					"the tenant above the grant — this is the direction that must not work"},
				{authz.PermToolCall, authz.ToolsetScope("acme", "hr"), false,
					"a sibling toolset"},
			},
		},
		{
			name:    "a single-tool grant reaches exactly one tool",
			catalog: std,
			grants: []authz.Grant{
				{Role: authz.RoleToolUser, Scope: authz.ToolScope("acme", "crm", "lookup_customer")},
			},
			asks: []ask{
				{authz.PermToolCall, authz.ToolScope("acme", "crm", "lookup_customer"), true,
					"the granted tool"},
				{authz.PermToolCall, authz.ToolScope("acme", "crm", "delete_customer"), false,
					"a sibling tool in the same toolset"},
				{authz.PermToolList, authz.ToolsetScope("acme", "crm"), false,
					"the toolset above the grant"},
			},
		},
		{
			name:    "tenants are isolated even when the names look alike",
			catalog: std,
			grants:  []authz.Grant{{Role: authz.RoleTenantAdmin, Scope: authz.TenantScope("acme")}},
			asks: []ask{
				{authz.PermUserManage, authz.TenantScope("acme"), true,
					"the granted tenant"},
				{authz.PermUserManage, authz.TenantScope("globex"), false,
					"an unrelated tenant"},
				{authz.PermUserManage, authz.TenantScope("acme-corp"), false,
					"a tenant whose name has the granted one as a prefix — the boundary case"},
				{authz.PermUserManage, authz.ToolsetScope("acme-corp", "crm"), false,
					"a toolset inside the prefix-colliding tenant"},
			},
		},
		{
			name:    "the global scope covers every tenant",
			catalog: std,
			grants:  []authz.Grant{{Role: authz.RolePlatformAdmin, Scope: authz.GlobalScope}},
			asks: []ask{
				{authz.PermTenantManage, authz.GlobalScope, true, "global itself"},
				{authz.PermTenantManage, authz.TenantScope("anything"), true, "any tenant"},
				{authz.PermToolCall, authz.ToolScope("anything", "any", "tool"), true,
					"any tool anywhere"},
			},
		},
		{
			name:    "a role the catalog does not define authorizes nothing",
			catalog: std,
			grants: []authz.Grant{
				{Role: "deleted_role", Scope: authz.GlobalScope},
			},
			asks: []ask{
				// A deleted role must fail closed rather than failing the
				// compilation — grants outlive the roles they name.
				{authz.PermToolList, authz.TenantScope("acme"), false,
					"a grant naming a role that no longer exists"},
			},
		},
		{
			name:    "no grants is default-deny, not default-allow",
			catalog: std,
			grants:  nil,
			asks: []ask{
				{authz.PermToolList, authz.TenantScope("acme"), false, "any question at all"},
				{authz.PermToolList, authz.GlobalScope, false, "even at global scope"},
			},
		},
		{
			name:    "an empty catalog authorizes nothing however broad the grant",
			catalog: authz.Catalog{},
			grants:  []authz.Grant{{Role: authz.RolePlatformAdmin, Scope: authz.GlobalScope}},
			asks: []ask{
				{authz.PermTenantManage, authz.GlobalScope, false,
					"a role with no permissions grants none"},
			},
		},
		{
			name:    "several grants compose without shadowing each other",
			catalog: std,
			grants: []authz.Grant{
				{Role: authz.RoleViewer, Scope: authz.TenantScope("acme")},
				{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "crm")},
			},
			asks: []ask{
				{authz.PermRegistryRead, authz.TenantScope("acme"), true,
					"from the tenant-wide viewer grant"},
				{authz.PermToolCall, authz.ToolScope("acme", "crm", "x"), true,
					"from the narrower tool_user grant"},
				{authz.PermToolCall, authz.ToolScope("acme", "hr", "x"), false,
					"tool_user does not extend past its toolset"},
				{authz.PermRegistryWrite, authz.TenantScope("acme"), false,
					"neither role grants it"},
			},
		},
		{
			name: "a role name containing a comma cannot inject a policy",
			// Casbin policies are CSV. An unescaped comma in a role name would
			// shift the following fields left, turning a scope into a
			// permission. The builtin engine is immune by construction, so
			// this case is really about the Casbin adapter — and about the two
			// staying in agreement on hostile input.
			catalog: authz.Catalog{
				"weird,role": {authz.PermToolList: {}},
			},
			grants: []authz.Grant{
				{Role: "weird,role", Scope: authz.TenantScope("acme")},
			},
			asks: []ask{
				{authz.PermToolList, authz.TenantScope("acme"), true,
					"the comma is data, not a field separator"},
				{authz.PermTenantManage, authz.TenantScope("acme"), false,
					"nothing was injected by the comma"},
			},
		},
	}
}

func TestEnginesAgreeAndAreCorrect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range conformanceCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			deciders := map[string]authz.Decider{}
			for name, engine := range engines(t) {
				decide, err := engine.Prepare(ctx, tc.grants, tc.catalog)
				require.NoErrorf(t, err, "%s engine failed to compile", name)
				deciders[name] = decide
			}

			for _, a := range tc.asks {
				for name, decide := range deciders {
					got := decide(a.permission, a.scope)
					require.Equalf(t, a.want, got,
						"%s engine: %s @ %s\n  expected %v because %s",
						name, a.permission, a.scope, a.want, a.why)
				}
			}
		})
	}
}

// TestEnginesAgreeOnAGeneratedMatrix sweeps every scope pair rather than the
// hand-picked ones above.
//
// The curated cases encode intent; this one catches the combination nobody
// thought to write down. It is exhaustive over a small universe rather than
// random, so a failure is reproducible without a seed.
func TestEnginesAgreeOnAGeneratedMatrix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tenants := []string{"acme", "acme-corp", "globex"}
	toolsets := []string{"crm", "crm-legacy"}
	tools := []string{"lookup", "lookup_all"}

	// Deliberately includes names that are prefixes of one another at every
	// level: acme/acme-corp, crm/crm-legacy, lookup/lookup_all. Prefix
	// collision is the failure mode a boundary-less prefix test has, and it
	// must be wrong at each depth independently.
	var scopes []string
	scopes = append(scopes, authz.GlobalScope)
	for _, tenant := range tenants {
		scopes = append(scopes, authz.TenantScope(tenant))
		for _, toolset := range toolsets {
			scopes = append(scopes, authz.ToolsetScope(tenant, toolset))
			for _, tool := range tools {
				scopes = append(scopes, authz.ToolScope(tenant, toolset, tool))
			}
		}
	}

	catalog := authz.DefaultCatalog()
	engineSet := engines(t)
	comparisons := 0

	for _, grantScope := range scopes {
		grants := []authz.Grant{{Role: authz.RoleToolUser, Scope: grantScope}}

		deciders := map[string]authz.Decider{}
		for name, engine := range engineSet {
			decide, err := engine.Prepare(ctx, grants, catalog)
			require.NoError(t, err)
			deciders[name] = decide
		}

		for _, requestScope := range scopes {
			builtin := deciders["builtin"](authz.PermToolCall, requestScope)
			casbin := deciders["casbin"](authz.PermToolCall, requestScope)
			require.Equalf(t, builtin, casbin,
				"engines disagree: grant %q, request %q (builtin=%v casbin=%v)",
				grantScope, requestScope, builtin, casbin)

			// And both must match the scope predicate they are supposed to be
			// implementing. Agreeing with each other while both disagreeing
			// with ScopeCovers would mean the model drifted from its own
			// definition.
			require.Equalf(t, authz.ScopeCovers(grantScope, requestScope), builtin,
				"engine disagrees with ScopeCovers: grant %q, request %q",
				grantScope, requestScope)
			comparisons++
		}
	}

	// Exact rather than a floor: every scope is asked of every scope, so the
	// count is knowable. A magic threshold would silently tolerate the matrix
	// shrinking, which is precisely how a sweep stops sweeping.
	require.Equal(t, len(scopes)*len(scopes), comparisons,
		"the matrix did not cover every scope pair")
	t.Logf("%d scope pairs compared across %d engines", comparisons, len(engineSet))
}

// TestPreparedDecidersDoNotShareState guards the compilation boundary.
//
// Each principal gets its own decider, and one principal's decisions must not
// be reachable from another's. A shared enforcer or a captured slice would make
// this fail, and the symptom in production would be one user seeing another's
// tools.
func TestPreparedDecidersDoNotShareState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	catalog := authz.DefaultCatalog()

	for name, engine := range engines(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			alice, err := engine.Prepare(ctx,
				[]authz.Grant{{Role: authz.RoleToolUser, Scope: authz.TenantScope("acme")}}, catalog)
			require.NoError(t, err)

			bob, err := engine.Prepare(ctx,
				[]authz.Grant{{Role: authz.RoleToolUser, Scope: authz.TenantScope("globex")}}, catalog)
			require.NoError(t, err)

			require.True(t, alice(authz.PermToolCall, authz.TenantScope("acme")))
			require.False(t, alice(authz.PermToolCall, authz.TenantScope("globex")))

			require.True(t, bob(authz.PermToolCall, authz.TenantScope("globex")))
			require.False(t, bob(authz.PermToolCall, authz.TenantScope("acme")))

			// And alice again, after bob was compiled: a shared enforcer would
			// now answer with bob's policy.
			require.True(t, alice(authz.PermToolCall, authz.TenantScope("acme")))
			require.False(t, alice(authz.PermToolCall, authz.TenantScope("globex")))
		})
	}
}

func BenchmarkScopeCovers(b *testing.B) {
	// The hot path: one call per admitted tool per grant, per catalog listing.
	// It must not allocate.
	grant := authz.ToolsetScope("acme", "crm")
	request := authz.ToolScope("acme", "crm", "lookup_customer")

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !authz.ScopeCovers(grant, request) {
			b.Fatal("unexpected deny")
		}
	}
}

func BenchmarkBuiltinDecide(b *testing.B) {
	decide, err := authz.BuiltinEngine{}.Prepare(context.Background(),
		[]authz.Grant{
			{Role: authz.RoleViewer, Scope: authz.TenantScope("acme")},
			{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "crm")},
		}, authz.DefaultCatalog())
	if err != nil {
		b.Fatal(err)
	}
	scopes := make([]string, 100)
	for i := range scopes {
		scopes[i] = authz.ToolScope("acme", "crm", fmt.Sprintf("tool_%d", i))
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !decide(authz.PermToolCall, scopes[i%len(scopes)]) {
			b.Fatal("unexpected deny")
		}
	}
}
