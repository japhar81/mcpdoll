// Copyright 2026 Henry Zektser.

package snapshot

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// fixture builds a small but realistic two-namespace snapshot: a CRM backend
// and a billing backend, exposed through one bundle to one audience.
type fixture struct {
	b *Builder
}

func newFixture(version int64) *fixture {
	b := NewBuilder(version).WithID("snap_test")

	b.AddTenant(&snapshotpb.Tenant{
		Id: "tn_acme", Slug: "acme", Name: "Acme", Status: "active",
	})

	b.AddNamespace(&snapshotpb.Namespace{
		Id: "ns_crm", Name: "crm", Prefix: "crm",
		OwningTeamId: "team_rev", ProjectId: "prj_go_to_market",
		OwnerIdpGroup: "eng-crm",
	})
	b.AddNamespace(&snapshotpb.Namespace{
		Id: "ns_bil", Name: "billing", Prefix: "bil",
		OwningTeamId: "team_fin", ProjectId: "prj_finance",
		OwnerIdpGroup: "eng-billing",
	})

	b.AddServer(&snapshotpb.Server{
		Id: "srv_crm", Name: "crm-prod", NamespaceId: "ns_crm",
		Bindings: []*snapshotpb.Binding{
			{TenantId: "tn_acme", Primary: "https://crm.internal/mcp"},
		},
		ServingMode: snapshotpb.ServingMode_SERVING_MODE_STRICT,
		Criticality: "high", DataClassification: "confidential",
	})
	b.AddServer(&snapshotpb.Server{
		Id: "srv_bil", Name: "billing-prod", NamespaceId: "ns_bil",
		Bindings: []*snapshotpb.Binding{
			{TenantId: "tn_acme", Primary: "https://billing.internal/mcp"},
		},
		ServingMode: snapshotpb.ServingMode_SERVING_MODE_STRICT,
		Criticality: "critical", DataClassification: "restricted",
	})

	return &fixture{b: b}
}

func (f *fixture) tool(serverID, nsID, prefix, name string, effect snapshotpb.EffectClass) *fixture {
	f.b.AddTool(ToolInput{
		ServerID: serverID, NamespaceID: nsID, Prefix: prefix,
		TenantID: "tn_acme", ToolsetID: "ts_support",
		Name:        name,
		Description: "Does " + name + ".",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		EffectClass: effect,
	})
	return f
}

// toolIn adds a tool to a named toolset, for tests that need more than the
// default fixture's single one.
func (f *fixture) toolIn(toolsetID, serverID, nsID, prefix, name string, effect snapshotpb.EffectClass) *fixture {
	f.b.AddTool(ToolInput{
		ServerID: serverID, NamespaceID: nsID, Prefix: prefix,
		TenantID: "tn_acme", ToolsetID: toolsetID,
		Name:        name,
		Description: "Does " + name + ".",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		EffectClass: effect,
	})
	return f
}

func (f *fixture) build(t *testing.T) *View {
	t.Helper()
	snap, err := f.b.Build()
	require.NoError(t, err)
	v, err := Build(snap)
	require.NoError(t, err)
	return v
}

// defaultFixture is the shape most tests want: two namespaces, four tools, one
// bundle, one audience.
func defaultFixture(version int64) *fixture {
	f := newFixture(version)
	f.tool("srv_crm", "ns_crm", "crm", "lookup_customer", snapshotpb.EffectClass_EFFECT_CLASS_READ)
	f.tool("srv_crm", "ns_crm", "crm", "update_customer", snapshotpb.EffectClass_EFFECT_CLASS_WRITE)
	f.tool("srv_bil", "ns_bil", "bil", "get_invoice", snapshotpb.EffectClass_EFFECT_CLASS_READ)
	f.tool("srv_bil", "ns_bil", "bil", "void_invoice", snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE)

	f.b.AddToolset(&snapshotpb.Toolset{
		Id: "ts_support", Name: "support", Priority: 10,
	})

	// A principal holding the whole toolset, which is the shape most tests
	// want: everything admitted for the tenant is visible to it.
	f.b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{{
		Id: "usr_alice", TenantId: "tn_acme", Subject: "alice@example.com",
		Grants: []*snapshotpb.Grant{
			{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "support")},
		},
	}})
	return f
}

// principalView composes alice's catalog, which is what most tests assert
// against: she holds the whole toolset, so her view is everything admitted for
// the tenant.
func (v *View) principalView(t *testing.T) *PrincipalView {
	t.Helper()
	pv, err := v.Principal(context.Background(), "usr_alice")
	require.NoError(t, err)
	return pv
}

func TestBuildIndexesEverything(t *testing.T) {
	v := defaultFixture(1).build(t)

	require.Equal(t, int64(1), v.Version)
	require.Equal(t, "snap_test", v.ID)
	require.Equal(t, []string{"acme"}, v.TenantSlugs())
	require.NotNil(t, v.Server("srv_crm"))
	require.Nil(t, v.Server("srv_absent"))
	require.NotNil(t, v.Namespace("ns_crm"))
	require.Len(t, v.Servers(), 2)

	av := v.principalView(t)
	require.NotNil(t, av)
	require.Len(t, av.Tools, 4)
	require.NotNil(t, av.Tool("crm.lookup_customer"))
	require.Nil(t, av.Tool("crm.absent"))
	require.Nil(t, av.Tool("lookup_customer"), "the unqualified name must not resolve")
	require.Positive(t, av.TokenEstimate)
}

func TestBuildJoinsToolToServerAndNamespace(t *testing.T) {
	v := defaultFixture(1).build(t)
	tool := v.principalView(t).Tool("bil.void_invoice")
	require.NotNil(t, tool)
	require.Equal(t, "billing-prod", tool.Server.Name)
	require.Equal(t, "bil", tool.Namespace.Prefix)
	require.Equal(t, snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE, tool.EffectClass())
	require.True(t, tool.Def.RequiresIdempotencyKey,
		"a destructive tool must be marked as requiring an idempotency key")
}

// -------------------------------------------------------- stable ordering ----

// TestCatalogOrderIsStable pins the ordering contract:
// (bundle priority, namespace prefix, tool name).
func TestCatalogOrderIsStable(t *testing.T) {
	v := defaultFixture(1).build(t)
	got := qualifiedNames(v.principalView(t))
	require.Equal(t, []string{
		"bil.get_invoice",
		"bil.void_invoice",
		"crm.lookup_customer",
		"crm.update_customer",
	}, got)
}

// TestAddingAToolDoesNotPerturbExistingOrder is the property the build brief
// calls out by name, and it is a real cost control rather than tidiness: model
// providers cache prompt prefixes, and the catalog sits near the front of the
// prompt. If adding one tool reorders the list, every client's cache is
// invalidated and every request pays full price again.
func TestAddingAToolDoesNotPerturbExistingOrder(t *testing.T) {
	before := qualifiedNames(defaultFixture(1).build(t).principalView(t))

	// Add a tool to an existing namespace, sorting into the middle of it.
	withExtra := defaultFixture(2)
	withExtra.tool("srv_crm", "ns_crm", "crm", "merge_customer", snapshotpb.EffectClass_EFFECT_CLASS_WRITE)
	after := qualifiedNames(withExtra.build(t).principalView(t))

	require.Len(t, after, len(before)+1)
	require.Contains(t, after, "crm.merge_customer")

	// Every previously-present name keeps its relative order.
	require.Equal(t, before, without(after, "crm.merge_customer"),
		"adding a tool must not move any existing entry")

	// And a whole new namespace must not disturb the existing partitions
	// either — it appends its own block in prefix order.
	withNamespace := defaultFixture(3)
	withNamespace.b.AddNamespace(&snapshotpb.Namespace{
		Id: "ns_hr", Name: "hr", Prefix: "hr", OwnerIdpGroup: "eng-hr",
	})
	withNamespace.b.AddServer(&snapshotpb.Server{
		Id: "srv_hr", Name: "hr-prod", NamespaceId: "ns_hr", Bindings: []*snapshotpb.Binding{{TenantId: "tn_acme", Primary: "https://x/mcp"}},
	})
	withNamespace.tool("srv_hr", "ns_hr", "hr", "lookup_employee", snapshotpb.EffectClass_EFFECT_CLASS_READ)
	// The audience must include it for it to appear.
	withNamespace.b.AddToolset(&snapshotpb.Toolset{Id: "bnd_hr", Name: "hr", Priority: 20})
	snap, err := withNamespace.b.Build()
	require.NoError(t, err)
	view, err := Build(snap)
	require.NoError(t, err)
	got := qualifiedNames(view.principalView(t))
	require.Equal(t, append(append([]string{}, before...), "hr.lookup_employee"), got,
		"a lower-priority bundle appends after the existing block, leaving it untouched")
}

// TestToolsetPriorityLeadsTheOrdering: catalog order is a cost control, and the
// leading key is the toolset's priority — not the namespace prefix.
func TestToolsetPriorityLeadsTheOrdering(t *testing.T) {
	f := newFixture(1)
	f.b.AddToolset(&snapshotpb.Toolset{Id: "ts_crm", Name: "crm-first", Priority: 1})
	f.b.AddToolset(&snapshotpb.Toolset{Id: "ts_bil", Name: "billing-second", Priority: 2})

	f.toolIn("ts_crm", "srv_crm", "ns_crm", "crm", "lookup_customer",
		snapshotpb.EffectClass_EFFECT_CLASS_READ)
	f.toolIn("ts_bil", "srv_bil", "ns_bil", "bil", "get_invoice",
		snapshotpb.EffectClass_EFFECT_CLASS_READ)

	f.b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{{
		Id: "usr_alice", TenantId: "tn_acme", Subject: "alice@example.com",
		Grants: []*snapshotpb.Grant{
			{Role: authz.RoleToolUser, Scope: authz.TenantScope("acme")},
		},
	}})

	// "crm" sorts after "bil" alphabetically, but its toolset has the lower
	// (higher-priority) number, so it must come first.
	got := qualifiedNames(f.build(t).principalView(t))
	require.Equal(t, []string{"crm.lookup_customer", "bil.get_invoice"}, got)
}

// ------------------------------------------------------- grants shape it -----

// A principal's catalog *is* its grants. These are the tests that replaced
// bundle include/exclude, which no longer exists: selection moved from the
// publisher's document to the administrator's grant.

func TestAToolsetGrantSeesExactlyThatToolset(t *testing.T) {
	f := newFixture(1)
	f.b.AddToolset(&snapshotpb.Toolset{Id: "ts_crm", Name: "crm", Priority: 1})
	f.b.AddToolset(&snapshotpb.Toolset{Id: "ts_bil", Name: "billing", Priority: 2})
	f.toolIn("ts_crm", "srv_crm", "ns_crm", "crm", "lookup_customer",
		snapshotpb.EffectClass_EFFECT_CLASS_READ)
	f.toolIn("ts_bil", "srv_bil", "ns_bil", "bil", "void_invoice",
		snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE)

	f.b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{{
		Id: "usr_alice", TenantId: "tn_acme", Subject: "alice@example.com",
		Grants: []*snapshotpb.Grant{
			{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "crm")},
		},
	}})

	av := f.build(t).principalView(t)
	require.Equal(t, []string{"crm.lookup_customer"}, qualifiedNames(av),
		"a toolset grant must not reach a sibling toolset")
}

func TestASingleToolGrantSeesOneTool(t *testing.T) {
	f := defaultFixture(1)
	f.b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{{
		Id: "usr_bob", TenantId: "tn_acme", Subject: "bob@example.com",
		Grants: []*snapshotpb.Grant{
			{Role: authz.RoleToolUser,
				Scope: authz.ToolScope("acme", "support", "lookup_customer")},
		},
	}})

	v := f.build(t)
	pv, err := v.Principal(context.Background(), "usr_bob")
	require.NoError(t, err)

	// "Give Bob one tool" is a grant at a deeper scope, not a bespoke bundle.
	require.Equal(t, []string{"crm.lookup_customer"}, qualifiedNames(pv))
}

func TestAPrincipalWithNoGrantsGetsAnEmptyCatalogNotAnError(t *testing.T) {
	f := defaultFixture(1)
	f.b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{{
		Id: "usr_new", TenantId: "tn_acme", Subject: "new@example.com",
	}})

	v := f.build(t)
	pv, err := v.Principal(context.Background(), "usr_new")

	// The correct state for a just-provisioned user under open_no_access
	// signup. An error here would make "exists but holds nothing" look like a
	// bug rather than the intended state.
	require.NoError(t, err)
	require.Empty(t, pv.Tools)
}

func TestListAndCallAreSeparatePrivileges(t *testing.T) {
	f := defaultFixture(1)
	f.b.SetRBAC(authz.Catalog{
		// A role that may see tools without invoking them. The reverse
		// combination is refused at admission (ADR 0015).
		"reader": {authz.PermToolList: {}},
	}, []*snapshotpb.Principal{{
		Id: "usr_carol", TenantId: "tn_acme", Subject: "carol@example.com",
		Grants: []*snapshotpb.Grant{
			{Role: "reader", Scope: authz.TenantScope("acme")},
		},
	}})

	v := f.build(t)
	pv, err := v.Principal(context.Background(), "usr_carol")
	require.NoError(t, err)

	require.NotEmpty(t, pv.Tools, "the reader can see the catalog")
	require.False(t, pv.Callable("crm.lookup_customer"),
		"but must not be able to invoke anything in it")
}

func TestTwoPrincipalsInOneTenantGetDifferentCatalogs(t *testing.T) {
	f := newFixture(1)
	f.b.AddToolset(&snapshotpb.Toolset{Id: "ts_crm", Name: "crm", Priority: 1})
	f.b.AddToolset(&snapshotpb.Toolset{Id: "ts_bil", Name: "billing", Priority: 2})
	f.toolIn("ts_crm", "srv_crm", "ns_crm", "crm", "lookup_customer",
		snapshotpb.EffectClass_EFFECT_CLASS_READ)
	f.toolIn("ts_bil", "srv_bil", "ns_bil", "bil", "get_invoice",
		snapshotpb.EffectClass_EFFECT_CLASS_READ)

	f.b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{
		{
			Id: "usr_alice", TenantId: "tn_acme", Subject: "alice@example.com",
			Grants: []*snapshotpb.Grant{{
				Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "crm"),
			}},
		},
		{
			Id: "usr_bob", TenantId: "tn_acme", Subject: "bob@example.com",
			Grants: []*snapshotpb.Grant{{
				Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "billing"),
			}},
		},
	})

	v := f.build(t)
	alice, err := v.Principal(context.Background(), "usr_alice")
	require.NoError(t, err)
	bob, err := v.Principal(context.Background(), "usr_bob")
	require.NoError(t, err)

	// Same URL, same tenant, different catalogs. This is the property that
	// lets ADR 0019 collapse the per-audience endpoints into one.
	require.Equal(t, []string{"crm.lookup_customer"}, qualifiedNames(alice))
	require.Equal(t, []string{"bil.get_invoice"}, qualifiedNames(bob))
}

func TestPrincipalViewsAreCachedPerView(t *testing.T) {
	v := defaultFixture(1).build(t)

	first, err := v.Principal(context.Background(), "usr_alice")
	require.NoError(t, err)
	second, err := v.Principal(context.Background(), "usr_alice")
	require.NoError(t, err)

	// The same pointer: composing is cheap but not free, and a connection per
	// request would recompose on every one.
	require.Same(t, first, second)
}

// ----------------------------------------------------------- cacheScope ------

// TestEveryCatalogIsPrivate is the explicit assertion the build brief requires,
// as amended by ADR 0016.
//
// It used to be conditional: a catalog nothing had filtered could be advertised
// public. That condition is now never true — every catalog is derived from a
// principal's grants — so the answer is unconditional, and this test says so in
// one place rather than leaving a `public` branch that can never be reached but
// could be reintroduced by accident.
func TestEveryCatalogIsPrivate(t *testing.T) {
	v := defaultFixture(1).build(t)

	pv, err := v.Principal(context.Background(), "usr_alice")
	require.NoError(t, err)

	// Serving one principal's catalog from a shared cache to another principal
	// is a confidentiality bug, and with per-principal catalogs there is no
	// case left where sharing is safe.
	require.Equal(t, "private", pv.CacheScope())
}

// ------------------------------------------------------------------ TTL ------

// TestTTLIsTheMinimumOfContributors: TTL may only be narrowed. A bundle or
// policy that could widen it would defeat the org-wide ceiling.
func TestTTLIsTheMinimumOfContributors(t *testing.T) {
	orgTTL := 5 * time.Minute

	t.Run("org default applies when nothing narrows it", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.WithCatalogDefaults(orgTTL, 30*time.Second)
		av := f.build(t).principalView(t)
		require.Equal(t, int(orgTTL.Milliseconds()), av.TTLMs)
	})

	t.Run("toolset narrows", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.WithCatalogDefaults(orgTTL, 30*time.Second)
		snap, err := f.b.Build()
		require.NoError(t, err)
		snap.Toolsets[0].TtlMs = 60_000
		v, err := Build(snap)
		require.NoError(t, err)
		require.Equal(t, 60_000, v.principalView(t).TTLMs)
	})

	t.Run("toolset cannot widen", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.WithCatalogDefaults(time.Minute, 10*time.Second)
		snap, err := f.b.Build()
		require.NoError(t, err)
		snap.Toolsets[0].TtlMs = 3_600_000 // an hour, far above the ceiling
		v, err := Build(snap)
		require.NoError(t, err)
		require.Equal(t, 60_000, v.principalView(t).TTLMs,
			"a toolset must not be able to raise the TTL above the default")
	})

	t.Run("policy narrows further", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.WithCatalogDefaults(orgTTL, 30*time.Second)
		f.b.AddPolicy(&snapshotpb.Policy{
			Id: "pol_short", Name: "short-ttl", Priority: 1,
			Rules: []*snapshotpb.PolicyRule{{
				Decision: snapshotpb.PolicyDecision_POLICY_DECISION_ALLOW,
				MaxTtlMs: 15_000,
			}},
		})
		snap, err := f.b.Build()
		require.NoError(t, err)
		snap.Toolsets[0].TtlMs = 60_000
		v, err := Build(snap)
		require.NoError(t, err)
		require.Equal(t, 15_000, v.principalView(t).TTLMs,
			"the tightest of default, toolset and policy wins")
	})
}

// -------------------------------------------------- referential integrity ----

// TestBuildRejectsBrokenReferences: the data plane keeps serving its previous
// snapshot rather than activating a half-valid one, because a partial outage is
// much harder to diagnose than a refused activation.
func TestBuildRejectsBrokenReferences(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*snapshotpb.Snapshot)
		wantErr string
	}{
		{
			name:    "server references an unknown namespace",
			mutate:  func(s *snapshotpb.Snapshot) { s.Servers[0].NamespaceId = "ns_gone" },
			wantErr: "unknown namespace",
		},
		{
			name:    "tool references an unknown server",
			mutate:  func(s *snapshotpb.Snapshot) { s.Tools[0].ServerId = "srv_gone" },
			wantErr: "unknown server",
		},
		{
			name:    "tool references an unknown toolset",
			mutate:  func(s *snapshotpb.Snapshot) { s.Tools[0].ToolsetId = "ts_gone" },
			wantErr: "unknown toolset",
		},
		{
			name:    "tool references an unknown tenant",
			mutate:  func(s *snapshotpb.Snapshot) { s.Tools[0].TenantId = "tn_gone" },
			wantErr: "unknown tenant",
		},
		{
			name: "two namespaces share a prefix",
			mutate: func(s *snapshotpb.Snapshot) {
				s.Namespaces[1].Prefix = s.Namespaces[0].Prefix
			},
			wantErr: "share the prefix",
		},
		{
			name: "duplicate namespace id",
			mutate: func(s *snapshotpb.Snapshot) {
				s.Namespaces = append(s.Namespaces, s.Namespaces[0])
			},
			wantErr: "appears twice",
		},
		{
			name: "duplicate server id",
			mutate: func(s *snapshotpb.Snapshot) {
				s.Servers = append(s.Servers, s.Servers[0])
			},
			wantErr: "appears twice",
		},
		{
			name: "the same qualified name twice in one tenant",
			mutate: func(s *snapshotpb.Snapshot) {
				s.Tools = append(s.Tools, s.Tools[0])
			},
			// Across tenants this is the normal case and the whole point of
			// per-tenant admission; within one tenant a tools/call would be
			// ambiguous.
			wantErr: "two tools named",
		},
		{
			name: "qualified name disagrees with the namespace prefix",
			mutate: func(s *snapshotpb.Snapshot) {
				s.Tools[0].QualifiedName = "wrong.lookup_customer"
			},
			wantErr: "has prefix",
		},
		{
			name: "duplicate tenant slug",
			mutate: func(s *snapshotpb.Snapshot) {
				// Slugs are how scopes name tenants, so two sharing one would
				// make every grant on it ambiguous.
				s.Tenants = append(s.Tenants, &snapshotpb.Tenant{
					Id: "tn_other", Slug: s.Tenants[0].Slug, Name: "Other",
				})
			},
			wantErr: "appears twice",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := defaultFixture(1).b.Build()
			require.NoError(t, err)
			tc.mutate(snap)
			_, err = Build(snap)
			require.Error(t, err)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestBuildRejectsNonPositiveVersion(t *testing.T) {
	snap, err := defaultFixture(1).b.Build()
	require.NoError(t, err)
	snap.Version = 0
	_, err = Build(snap)
	require.ErrorContains(t, err, "version must be positive")

	_, err = Build(nil)
	require.ErrorContains(t, err, "nil snapshot")
}

// TestBuildRejectsConflictingMutatingPlugins: two plugins that both patch the
// same hook at the same priority would apply in an unspecified order, and two
// patch orders can produce two different requests. That has to be a build
// failure, not a production coin flip.
func TestBuildRejectsConflictingMutatingPlugins(t *testing.T) {
	f := defaultFixture(1)
	for _, name := range []string{"redact", "rewrite"} {
		f.b.AddPlugin(&snapshotpb.PluginManifest{
			Id: "plg_" + name, Name: name, Version: "1.0.0",
			Runtime:  snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
			Hooks:    []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_RESULT},
			Priority: 50,
			Writes:   []string{"result.content"},
		})
	}
	_, err := f.b.Build()
	require.ErrorContains(t, err, "patch order would be unspecified")
	require.ErrorContains(t, err, "redact")
	require.ErrorContains(t, err, "rewrite")
}

// TestNonMutatingPluginsMaySharePriority: only a declared writer can conflict.
// Two observers at the same priority are harmless.
func TestNonMutatingPluginsMaySharePriority(t *testing.T) {
	f := defaultFixture(1)
	for _, name := range []string{"audit-a", "audit-b"} {
		f.b.AddPlugin(&snapshotpb.PluginManifest{
			Id: "plg_" + name, Name: name, Version: "1.0.0",
			Runtime:  snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
			Hooks:    []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_AUDIT},
			Priority: 50,
			Reads:    []string{"request", "verdicts"},
		})
	}
	v := f.build(t)
	plugins := v.principalView(t).PluginsFor(snapshotpb.Hook_HOOK_ON_AUDIT)
	require.Len(t, plugins, 2)
	// Tie-broken by name so the order is reproducible.
	require.Equal(t, "audit-a", plugins[0].Name)
	require.Equal(t, "audit-b", plugins[1].Name)
}

func TestPluginsAreScopedToToolsets(t *testing.T) {
	f := newFixture(1)
	f.b.AddToolset(&snapshotpb.Toolset{Id: "ts_crm", Name: "crm", Priority: 1})
	f.b.AddToolset(&snapshotpb.Toolset{Id: "ts_bil", Name: "billing", Priority: 2})
	f.toolIn("ts_crm", "srv_crm", "ns_crm", "crm", "lookup_customer",
		snapshotpb.EffectClass_EFFECT_CLASS_READ)
	f.toolIn("ts_bil", "srv_bil", "ns_bil", "bil", "get_invoice",
		snapshotpb.EffectClass_EFFECT_CLASS_READ)

	f.b.AddPlugin(&snapshotpb.PluginManifest{
		Id: "plg_scoped", Name: "scoped", Version: "1.0.0",
		Runtime:    snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:      []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_REQUEST},
		Priority:   10,
		ToolsetIds: []string{"ts_crm"},
	})
	f.b.AddPlugin(&snapshotpb.PluginManifest{
		Id: "plg_global", Name: "global", Version: "1.0.0",
		Runtime:  snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:    []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_REQUEST},
		Priority: 20,
	})

	f.b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{
		{
			Id: "usr_crm", TenantId: "tn_acme", Subject: "crm@example.com",
			Grants: []*snapshotpb.Grant{{
				Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "crm"),
			}},
		},
		{
			Id: "usr_bil", TenantId: "tn_acme", Subject: "bil@example.com",
			Grants: []*snapshotpb.Grant{{
				Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "billing"),
			}},
		},
	})

	v := f.build(t)

	crm, err := v.Principal(context.Background(), "usr_crm")
	require.NoError(t, err)
	require.Equal(t, []string{"scoped", "global"},
		pluginNames(crm.PluginsFor(snapshotpb.Hook_HOOK_ON_REQUEST)))

	bil, err := v.Principal(context.Background(), "usr_bil")
	require.NoError(t, err)

	// Scoping now follows the principal's grants: a plugin limited to a
	// toolset does not run for a principal whose catalog does not include it.
	require.Equal(t, []string{"global"},
		pluginNames(bil.PluginsFor(snapshotpb.Hook_HOOK_ON_REQUEST)),
		"a toolset-scoped plugin must not run for a principal outside it")
}

func TestToolsetTokenBudgetIsEnforcedAtBuild(t *testing.T) {
	f := defaultFixture(1)
	snap, err := f.b.Build()
	require.NoError(t, err)
	snap.Toolsets[0].TokenBudget = 1 // absurdly low
	_, err = Build(snap)
	require.ErrorContains(t, err, "token budget")
}

func TestToolByDigest(t *testing.T) {
	v := defaultFixture(1).build(t)
	tool := v.principalView(t).Tool("crm.lookup_customer")
	require.NotNil(t, tool)
	require.Same(t, tool.Def, v.ToolByDigest(tool.Def.Digest))
	require.Nil(t, v.ToolByDigest("sha256:absent"))
}

func TestViewAge(t *testing.T) {
	v := defaultFixture(1).build(t)
	require.GreaterOrEqual(t, v.Age(), time.Duration(0))
	require.Less(t, v.Age(), time.Minute)

	// A snapshot with no build timestamp reports zero rather than an absurd age.
	v.BuiltAt = time.Time{}
	require.Equal(t, time.Duration(0), v.Age())
}

func TestBuilderRejectsBadNamespacePrefix(t *testing.T) {
	b := NewBuilder(1)
	b.AddNamespace(&snapshotpb.Namespace{Id: "ns_a", Name: "a", Prefix: ""})
	_, err := b.Build()
	require.ErrorContains(t, err, "no prefix")

	b2 := NewBuilder(1)
	b2.AddNamespace(&snapshotpb.Namespace{Id: "ns_a", Name: "a", Prefix: "a.b"})
	_, err = b2.Build()
	require.ErrorContains(t, err, "ambiguous")
}

func TestBuilderRejectsExternalRefInSchema(t *testing.T) {
	b := NewBuilder(1)
	b.AddTenant(&snapshotpb.Tenant{Id: "tn_a", Slug: "a", Name: "A", Status: "active"})
	b.AddToolset(&snapshotpb.Toolset{Id: "ts_a", Name: "a", Priority: 1})
	b.AddNamespace(&snapshotpb.Namespace{Id: "ns_a", Name: "a", Prefix: "a"})
	b.AddServer(&snapshotpb.Server{Id: "srv_a", NamespaceId: "ns_a", Bindings: []*snapshotpb.Binding{{TenantId: "tn_a", Primary: "https://a/mcp"}}})
	b.AddTool(ToolInput{
		ServerID: "srv_a", NamespaceID: "ns_a", Prefix: "a", Name: "t",
		TenantID: "tn_a", ToolsetID: "ts_a",
		InputSchema: json.RawMessage(`{"$ref":"https://evil.example/s.json"}`),
	})
	_, err := b.Build()
	require.ErrorContains(t, err, "external $ref")
}

func TestEstimateTokens(t *testing.T) {
	require.Equal(t, 0, EstimateTokens(nil))
	require.Positive(t, EstimateTokens([]byte(`{"name":"t"}`)))
	// A longer definition must estimate higher than a shorter one.
	short := EstimateTokens([]byte(`{"name":"t"}`))
	long := EstimateTokens([]byte(`{"description":"a much longer description here","name":"t"}`))
	require.Greater(t, long, short)
}

// ------------------------------------------------------------- helpers -------

func qualifiedNames(av *PrincipalView) []string {
	out := make([]string, 0, len(av.Tools))
	for _, t := range av.Tools {
		out = append(out, t.Def.QualifiedName)
	}
	return out
}

func pluginNames(ps []*snapshotpb.PluginManifest) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func without(names []string, drop string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}
