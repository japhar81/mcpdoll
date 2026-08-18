// Copyright 2026 The MCPDoll Authors.

package snapshot

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// fixture builds a small but realistic two-namespace snapshot: a CRM backend
// and a billing backend, exposed through one bundle to one audience.
type fixture struct {
	b *Builder
}

func newFixture(version int64) *fixture {
	b := NewBuilder("org_1", version).WithID("snap_test")

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
		Endpoint:    "https://crm.internal/mcp",
		ServingMode: snapshotpb.ServingMode_SERVING_MODE_STRICT,
		Criticality: "high", DataClassification: "confidential",
	})
	b.AddServer(&snapshotpb.Server{
		Id: "srv_bil", Name: "billing-prod", NamespaceId: "ns_bil",
		Endpoint:    "https://billing.internal/mcp",
		ServingMode: snapshotpb.ServingMode_SERVING_MODE_STRICT,
		Criticality: "critical", DataClassification: "restricted",
	})

	return &fixture{b: b}
}

func (f *fixture) tool(serverID, nsID, prefix, name string, effect snapshotpb.EffectClass) *fixture {
	f.b.AddTool(ToolInput{
		ServerID: serverID, NamespaceID: nsID, Prefix: prefix,
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

	f.b.AddBundle(&snapshotpb.Bundle{
		Id: "bnd_support", Name: "support", Priority: 10,
		Entries: []*snapshotpb.BundleEntry{
			{NamespaceId: "ns_crm"},
			{NamespaceId: "ns_bil"},
		},
	})
	f.b.AddAudience(&snapshotpb.Audience{
		Id: "aud_support", Slug: "support-agents", Name: "Support Agents",
		BundleIds: []string{"bnd_support"},
	})
	return f
}

func TestBuildIndexesEverything(t *testing.T) {
	v := defaultFixture(1).build(t)

	require.Equal(t, int64(1), v.Version)
	require.Equal(t, "org_1", v.OrgID)
	require.Equal(t, []string{"support-agents"}, v.AudienceSlugs())
	require.NotNil(t, v.Server("srv_crm"))
	require.Nil(t, v.Server("srv_absent"))
	require.NotNil(t, v.Namespace("ns_crm"))
	require.Len(t, v.Servers(), 2)

	av := v.Audience("support-agents")
	require.NotNil(t, av)
	require.Len(t, av.Tools, 4)
	require.NotNil(t, av.Tool("crm.lookup_customer"))
	require.Nil(t, av.Tool("crm.absent"))
	require.Nil(t, av.Tool("lookup_customer"), "the unqualified name must not resolve")
	require.Positive(t, av.TokenEstimate)
}

func TestBuildJoinsToolToServerAndNamespace(t *testing.T) {
	v := defaultFixture(1).build(t)
	tool := v.Audience("support-agents").Tool("bil.void_invoice")
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
	got := qualifiedNames(v.Audience("support-agents"))
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
	before := qualifiedNames(defaultFixture(1).build(t).Audience("support-agents"))

	// Add a tool to an existing namespace, sorting into the middle of it.
	withExtra := defaultFixture(2)
	withExtra.tool("srv_crm", "ns_crm", "crm", "merge_customer", snapshotpb.EffectClass_EFFECT_CLASS_WRITE)
	after := qualifiedNames(withExtra.build(t).Audience("support-agents"))

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
		Id: "srv_hr", Name: "hr-prod", NamespaceId: "ns_hr", Endpoint: "https://hr.internal/mcp",
	})
	withNamespace.tool("srv_hr", "ns_hr", "hr", "lookup_employee", snapshotpb.EffectClass_EFFECT_CLASS_READ)
	// The audience must include it for it to appear.
	withNamespace.b.AddBundle(&snapshotpb.Bundle{
		Id: "bnd_hr", Name: "hr", Priority: 20,
		Entries: []*snapshotpb.BundleEntry{{NamespaceId: "ns_hr"}},
	})
	snap, err := withNamespace.b.Build()
	require.NoError(t, err)
	for _, aud := range snap.Audiences {
		if aud.Slug == "support-agents" {
			aud.BundleIds = append(aud.BundleIds, "bnd_hr")
		}
	}
	view, err := Build(snap)
	require.NoError(t, err)
	got := qualifiedNames(view.Audience("support-agents"))
	require.Equal(t, append(append([]string{}, before...), "hr.lookup_employee"), got,
		"a lower-priority bundle appends after the existing block, leaving it untouched")
}

// TestBundlePriorityLeadsTheOrdering: bundle priority is the primary key, so a
// high-priority bundle's tools come first regardless of prefix.
func TestBundlePriorityLeadsTheOrdering(t *testing.T) {
	f := newFixture(1)
	f.tool("srv_crm", "ns_crm", "crm", "lookup_customer", snapshotpb.EffectClass_EFFECT_CLASS_READ)
	f.tool("srv_bil", "ns_bil", "bil", "get_invoice", snapshotpb.EffectClass_EFFECT_CLASS_READ)

	// "crm" sorts after "bil" alphabetically, but its bundle has the lower
	// (higher-priority) number, so it must come first.
	f.b.AddBundle(&snapshotpb.Bundle{
		Id: "bnd_crm", Name: "crm-first", Priority: 1,
		Entries: []*snapshotpb.BundleEntry{{NamespaceId: "ns_crm"}},
	})
	f.b.AddBundle(&snapshotpb.Bundle{
		Id: "bnd_bil", Name: "billing-second", Priority: 2,
		Entries: []*snapshotpb.BundleEntry{{NamespaceId: "ns_bil"}},
	})
	f.b.AddAudience(&snapshotpb.Audience{
		Id: "aud_a", Slug: "a", BundleIds: []string{"bnd_crm", "bnd_bil"},
	})

	got := qualifiedNames(f.build(t).Audience("a"))
	require.Equal(t, []string{"crm.lookup_customer", "bil.get_invoice"}, got)
}

// TestDuplicateToolAcrossBundlesAppearsOnce: the first bundle to contribute a
// tool owns it, so a tool included by two bundles must not appear twice nor
// move in the ordering.
func TestDuplicateToolAcrossBundlesAppearsOnce(t *testing.T) {
	f := newFixture(1)
	f.tool("srv_crm", "ns_crm", "crm", "lookup_customer", snapshotpb.EffectClass_EFFECT_CLASS_READ)

	f.b.AddBundle(&snapshotpb.Bundle{
		Id: "bnd_1", Name: "one", Priority: 1,
		Entries: []*snapshotpb.BundleEntry{{NamespaceId: "ns_crm"}},
	})
	f.b.AddBundle(&snapshotpb.Bundle{
		Id: "bnd_2", Name: "two", Priority: 2,
		Entries: []*snapshotpb.BundleEntry{
			{NamespaceId: "ns_crm", QualifiedNames: []string{"crm.lookup_customer"}},
		},
	})
	f.b.AddAudience(&snapshotpb.Audience{
		Id: "aud_a", Slug: "a", BundleIds: []string{"bnd_1", "bnd_2"},
	})

	av := f.build(t).Audience("a")
	require.Equal(t, []string{"crm.lookup_customer"}, qualifiedNames(av))
	require.EqualValues(t, 1, av.Tools[0].BundlePriority,
		"the first contributing bundle's priority is what sorts the tool")
}

// ------------------------------------------------- bundle include / exclude ----

func TestBundleEntrySelectsSubset(t *testing.T) {
	f := defaultFixture(1)
	// Replace the whole-namespace entry with a two-tool subset.
	snap, err := f.b.Build()
	require.NoError(t, err)
	snap.Bundles[0].Entries = []*snapshotpb.BundleEntry{
		{NamespaceId: "ns_crm", QualifiedNames: []string{"crm.lookup_customer"}},
		{NamespaceId: "ns_bil", ExcludeQualifiedNames: []string{"bil.void_invoice"}},
	}
	v, err := Build(snap)
	require.NoError(t, err)

	require.Equal(t, []string{"bil.get_invoice", "crm.lookup_customer"},
		qualifiedNames(v.Audience("support-agents")),
		"the destructive billing tool was excluded and the second CRM tool was never included")
}

func TestBundleEntryExcludeBeatsInclude(t *testing.T) {
	f := defaultFixture(1)
	snap, err := f.b.Build()
	require.NoError(t, err)
	snap.Bundles[0].Entries = []*snapshotpb.BundleEntry{{
		NamespaceId:           "ns_crm",
		QualifiedNames:        []string{"crm.lookup_customer", "crm.update_customer"},
		ExcludeQualifiedNames: []string{"crm.update_customer"},
	}}
	v, err := Build(snap)
	require.NoError(t, err)
	require.Equal(t, []string{"crm.lookup_customer"}, qualifiedNames(v.Audience("support-agents")))
}

// ----------------------------------------------------------- cacheScope ------

// TestFilteredCatalogIsNeverPublic is the explicit assertion the build brief
// requires. Serving one principal's filtered catalog from a shared cache to
// another principal is a confidentiality bug, so an identity-filtered view must
// be marked private.
func TestFilteredCatalogIsNeverPublic(t *testing.T) {
	t.Run("unfiltered is public", func(t *testing.T) {
		av := defaultFixture(1).build(t).Audience("support-agents")
		require.False(t, av.IdentityFiltered)
		require.Equal(t, "public", av.CacheScope())
	})

	t.Run("identity-specific policy forces private", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.AddPolicy(&snapshotpb.Policy{
			Id: "pol_ent", Name: "entitlements", Priority: 1,
			Rules: []*snapshotpb.PolicyRule{{
				Decision:         snapshotpb.PolicyDecision_POLICY_DECISION_ALLOW,
				IdentitySpecific: true,
				Reason:           "per-principal entitlement filtering",
			}},
		})
		snap, err := f.b.Build()
		require.NoError(t, err)
		snap.Audiences[0].PolicyIds = []string{"pol_ent"}
		v, err := Build(snap)
		require.NoError(t, err)

		av := v.Audience("support-agents")
		require.True(t, av.IdentityFiltered)
		require.Equal(t, "private", av.CacheScope(),
			"an identity-filtered catalog must never be cacheScope: public")
	})

	t.Run("group-conditioned hide forces private", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.AddPolicy(&snapshotpb.Policy{
			Id: "pol_hide", Name: "hide-destructive", Priority: 1,
			Rules: []*snapshotpb.PolicyRule{{
				Decision:          snapshotpb.PolicyDecision_POLICY_DECISION_HIDE,
				EffectClasses:     []string{"EFFECT_CLASS_DESTRUCTIVE"},
				RequiredIdpGroups: []string{"billing-admins"},
				Reason:            "destructive billing tools are admin-only",
			}},
		})
		snap, err := f.b.Build()
		require.NoError(t, err)
		snap.Audiences[0].PolicyIds = []string{"pol_hide"}
		v, err := Build(snap)
		require.NoError(t, err)

		av := v.Audience("support-agents")
		require.True(t, av.IdentityFiltered,
			"a rule that hides tools from some groups but not others makes the catalog identity-specific")
		require.Equal(t, "private", av.CacheScope())
	})

	t.Run("identity-dependent ON_CATALOG plugin forces private", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.AddPlugin(&snapshotpb.PluginManifest{
			Id: "plg_ent", Name: "entitlements", Version: "1.0.0",
			Runtime:           snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
			Hooks:             []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_CATALOG},
			Priority:          10,
			IdentityDependent: true,
			Rollout:           snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE,
			Writes:            []string{"catalog.tools"},
		})
		av := f.build(t).Audience("support-agents")
		require.True(t, av.IdentityFiltered)
		require.Equal(t, "private", av.CacheScope())
	})

	t.Run("identity-dependent plugin on another hook does not force private", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.AddPlugin(&snapshotpb.PluginManifest{
			Id: "plg_hdr", Name: "header-map", Version: "1.0.0",
			Runtime:           snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
			Hooks:             []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_CALL},
			Priority:          10,
			IdentityDependent: true,
			Rollout:           snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE,
			Writes:            []string{"request.headers"},
		})
		av := f.build(t).Audience("support-agents")
		require.False(t, av.IdentityFiltered,
			"a plugin that shapes the call but not the catalog leaves the catalog shareable")
		require.Equal(t, "public", av.CacheScope())
	})
}

// ------------------------------------------------------------------ TTL ------

// TestTTLIsTheMinimumOfContributors: TTL may only be narrowed. A bundle or
// policy that could widen it would defeat the org-wide ceiling.
func TestTTLIsTheMinimumOfContributors(t *testing.T) {
	orgTTL := 5 * time.Minute

	t.Run("org default applies when nothing narrows it", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.WithCatalogDefaults(orgTTL, 30*time.Second)
		av := f.build(t).Audience("support-agents")
		require.Equal(t, int(orgTTL.Milliseconds()), av.TTLMs)
	})

	t.Run("bundle narrows", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.WithCatalogDefaults(orgTTL, 30*time.Second)
		snap, err := f.b.Build()
		require.NoError(t, err)
		snap.Bundles[0].TtlMs = 60_000
		v, err := Build(snap)
		require.NoError(t, err)
		require.Equal(t, 60_000, v.Audience("support-agents").TTLMs)
	})

	t.Run("bundle cannot widen", func(t *testing.T) {
		f := defaultFixture(1)
		f.b.WithCatalogDefaults(time.Minute, 10*time.Second)
		snap, err := f.b.Build()
		require.NoError(t, err)
		snap.Bundles[0].TtlMs = 3_600_000 // an hour, far above the org ceiling
		v, err := Build(snap)
		require.NoError(t, err)
		require.Equal(t, 60_000, v.Audience("support-agents").TTLMs,
			"a bundle must not be able to raise the TTL above the org default")
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
		snap.Audiences[0].PolicyIds = []string{"pol_short"}
		snap.Bundles[0].TtlMs = 60_000
		v, err := Build(snap)
		require.NoError(t, err)
		require.Equal(t, 15_000, v.Audience("support-agents").TTLMs,
			"the tightest of org, bundle and policy wins")
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
			name:    "bundle references an unknown namespace",
			mutate:  func(s *snapshotpb.Snapshot) { s.Bundles[0].Entries[0].NamespaceId = "ns_gone" },
			wantErr: "unknown namespace",
		},
		{
			name: "bundle names an unadmitted tool",
			mutate: func(s *snapshotpb.Snapshot) {
				s.Bundles[0].Entries[0].QualifiedNames = []string{"crm.not_admitted"}
			},
			wantErr: "not admitted",
		},
		{
			name:    "audience references an unknown bundle",
			mutate:  func(s *snapshotpb.Snapshot) { s.Audiences[0].BundleIds = []string{"bnd_gone"} },
			wantErr: "unknown bundle",
		},
		{
			name:    "audience references an unknown policy",
			mutate:  func(s *snapshotpb.Snapshot) { s.Audiences[0].PolicyIds = []string{"pol_gone"} },
			wantErr: "unknown policy",
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
			name: "duplicate tool digest",
			mutate: func(s *snapshotpb.Snapshot) {
				s.Tools = append(s.Tools, s.Tools[0])
			},
			wantErr: "appears twice",
		},
		{
			name: "qualified name disagrees with the namespace prefix",
			mutate: func(s *snapshotpb.Snapshot) {
				s.Tools[0].QualifiedName = "wrong.lookup_customer"
			},
			wantErr: "implies",
		},
		{
			name:    "audience without a slug has no endpoint",
			mutate:  func(s *snapshotpb.Snapshot) { s.Audiences[0].Slug = "" },
			wantErr: "no slug",
		},
		{
			name: "duplicate audience slug",
			mutate: func(s *snapshotpb.Snapshot) {
				dup := &snapshotpb.Audience{
					Id: "aud_other", Slug: s.Audiences[0].Slug,
					BundleIds: s.Audiences[0].BundleIds,
				}
				s.Audiences = append(s.Audiences, dup)
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
	require.ErrorContains(t, err, "cannot build a view from nothing")
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
	plugins := v.Audience("support-agents").PluginsFor(snapshotpb.Hook_HOOK_ON_AUDIT)
	require.Len(t, plugins, 2)
	// Tie-broken by name so the order is reproducible.
	require.Equal(t, "audit-a", plugins[0].Name)
	require.Equal(t, "audit-b", plugins[1].Name)
}

func TestPluginsAreScopedToAudiences(t *testing.T) {
	f := defaultFixture(1)
	f.b.AddAudience(&snapshotpb.Audience{
		Id: "aud_other", Slug: "other", BundleIds: []string{"bnd_support"},
	})
	f.b.AddPlugin(&snapshotpb.PluginManifest{
		Id: "plg_scoped", Name: "scoped", Version: "1.0.0",
		Runtime:     snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:       []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_REQUEST},
		Priority:    10,
		AudienceIds: []string{"aud_support"},
	})
	f.b.AddPlugin(&snapshotpb.PluginManifest{
		Id: "plg_global", Name: "global", Version: "1.0.0",
		Runtime:  snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:    []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_REQUEST},
		Priority: 20,
	})

	v := f.build(t)
	support := v.Audience("support-agents").PluginsFor(snapshotpb.Hook_HOOK_ON_REQUEST)
	require.Equal(t, []string{"scoped", "global"}, pluginNames(support))

	other := v.Audience("other").PluginsFor(snapshotpb.Hook_HOOK_ON_REQUEST)
	require.Equal(t, []string{"global"}, pluginNames(other),
		"an audience-scoped plugin must not run for another audience")
}

func TestBundleTokenBudgetIsEnforcedAtBuild(t *testing.T) {
	f := defaultFixture(1)
	snap, err := f.b.Build()
	require.NoError(t, err)
	snap.Bundles[0].TokenBudget = 1 // absurdly low
	_, err = Build(snap)
	require.ErrorContains(t, err, "token budget")
}

func TestToolByDigest(t *testing.T) {
	v := defaultFixture(1).build(t)
	tool := v.Audience("support-agents").Tool("crm.lookup_customer")
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
	b := NewBuilder("org_1", 1)
	b.AddNamespace(&snapshotpb.Namespace{Id: "ns_a", Name: "a", Prefix: ""})
	_, err := b.Build()
	require.ErrorContains(t, err, "no prefix")

	b2 := NewBuilder("org_1", 1)
	b2.AddNamespace(&snapshotpb.Namespace{Id: "ns_a", Name: "a", Prefix: "a.b"})
	_, err = b2.Build()
	require.ErrorContains(t, err, "ambiguous")
}

func TestBuilderRejectsExternalRefInSchema(t *testing.T) {
	b := NewBuilder("org_1", 1)
	b.AddNamespace(&snapshotpb.Namespace{Id: "ns_a", Name: "a", Prefix: "a"})
	b.AddServer(&snapshotpb.Server{Id: "srv_a", NamespaceId: "ns_a", Endpoint: "https://a/mcp"})
	b.AddTool(ToolInput{
		ServerID: "srv_a", NamespaceID: "ns_a", Prefix: "a", Name: "t",
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

func qualifiedNames(av *AudienceView) []string {
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
