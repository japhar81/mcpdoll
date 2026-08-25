// Copyright 2026 Henry Zektser.

package snapshot

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// A credential that spans tenants (ADR 0027). The case that motivates it is
// asking one model to compare the same tool's answers in test and in live.

func twoTenantFixture(t *testing.T) *fixture {
	t.Helper()
	b := NewBuilder(1).WithID("snap_span")

	for _, tn := range []struct{ id, slug string }{
		{"tn_live", "crm-live"},
		{"tn_test", "crm-test"},
	} {
		b.AddTenant(&snapshotpb.Tenant{
			Id: tn.id, Slug: tn.slug, Name: tn.slug, Status: "active",
		})
	}
	b.AddNamespace(&snapshotpb.Namespace{
		Id: "ns_crm", Name: "crm", Prefix: "crm",
		OwningTeamId: "team_rev", ProjectId: "prj", OwnerIdpGroup: "eng-crm",
	})
	// The same backend deployed per tenant — which is the whole point: the two
	// serve identical tool names against different data.
	b.AddServer(&snapshotpb.Server{
		Id: "srv_crm", Name: "crm", NamespaceId: "ns_crm",
		Bindings: []*snapshotpb.Binding{
			{TenantId: "tn_live", Primary: "https://live.internal/mcp"},
			{TenantId: "tn_test", Primary: "https://test.internal/mcp"},
		},
		ServingMode: snapshotpb.ServingMode_SERVING_MODE_STRICT,
		Criticality: "high", DataClassification: "confidential",
	})
	b.AddToolset(&snapshotpb.Toolset{Id: "ts_support", Name: "support"})

	for _, tenantID := range []string{"tn_live", "tn_test"} {
		b.AddTool(ToolInput{
			ServerID: "srv_crm", NamespaceID: "ns_crm", Prefix: "crm",
			TenantID: tenantID, ToolsetID: "ts_support",
			Name: "lookup_customer", Description: "Looks a customer up.",
			InputSchema: json.RawMessage(
				`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
			EffectClass: snapshotpb.EffectClass_EFFECT_CLASS_READ,
		})
	}
	return &fixture{b: b}
}

func spanningPrincipal(id string, spans bool, tenantID string, scopes ...string) *snapshotpb.Principal {
	p := &snapshotpb.Principal{
		Id: id, Subject: id, TenantId: tenantID, SpansTenants: spans,
	}
	for _, scope := range scopes {
		p.Grants = append(p.Grants, &snapshotpb.Grant{Role: "tool_user", Scope: scope})
	}
	return p
}

func toolCatalog(t *testing.T, f *fixture, id string) []string {
	t.Helper()
	pv, err := f.store.PrincipalView(context.Background(), id)
	require.NoError(t, err)
	names := make([]string, 0, len(pv.Tools))
	for i := range pv.Tools {
		names = append(names, pv.Name(i))
	}
	return names
}

func TestASpanningCredentialSeesBothTenantsQualified(t *testing.T) {
	t.Parallel()
	f := twoTenantFixture(t)
	f.setRBAC(
		authz.Catalog{"tool_user": {
			authz.PermToolList: {}, authz.PermToolCall: {},
		}},
		[]*snapshotpb.Principal{
			spanningPrincipal("span", true, "", "t/crm-live", "t/crm-test"),
		},
	)
	f.build(t)

	require.Equal(t,
		[]string{"crm-live.crm.lookup_customer", "crm-test.crm.lookup_customer"},
		toolCatalog(t, f, "span"),
		"both tenants' tools, each qualified, in slug order")
}

// The default is unchanged, and that matters: an ordinary credential must keep
// seeing the short name, or every existing agent prompt breaks.
func TestAnOrdinaryCredentialStillSeesShortNames(t *testing.T) {
	t.Parallel()
	f := twoTenantFixture(t)
	f.setRBAC(
		authz.Catalog{"tool_user": {
			authz.PermToolList: {}, authz.PermToolCall: {},
		}},
		[]*snapshotpb.Principal{
			spanningPrincipal("single", false, "tn_live", "t/crm-live"),
		},
	)
	f.build(t)

	require.Equal(t,
		[]string{"crm.lookup_customer"},
		toolCatalog(t, f, "single"))
}

// Qualification does not depend on how many tenants are actually in the
// catalog. Qualifying only on collision would mean a tool's name changed when
// somebody was granted a second tenant — silently breaking every prompt that
// named it, for a reason invisible from the tool itself.
func TestASpanningCredentialWithOneTenantIsStillQualified(t *testing.T) {
	t.Parallel()
	f := twoTenantFixture(t)
	f.setRBAC(
		authz.Catalog{"tool_user": {
			authz.PermToolList: {}, authz.PermToolCall: {},
		}},
		[]*snapshotpb.Principal{
			spanningPrincipal("narrow", true, "", "t/crm-live"),
		},
	)
	f.build(t)

	require.Equal(t,
		[]string{"crm-live.crm.lookup_customer"},
		toolCatalog(t, f, "narrow"),
		"a name must not depend on what else is in the catalog")
}

// Spanning widens naming, never authorization. The grants still decide, so a
// spanning key granted one tenant cannot see the other.
func TestSpanningDoesNotWidenAccess(t *testing.T) {
	t.Parallel()
	f := twoTenantFixture(t)
	f.setRBAC(
		authz.Catalog{"tool_user": {
			authz.PermToolList: {}, authz.PermToolCall: {},
		}},
		[]*snapshotpb.Principal{
			spanningPrincipal("narrow", true, "", "t/crm-live"),
		},
	)
	f.build(t)

	names := toolCatalog(t, f, "narrow")
	require.NotContains(t, names, "crm-test.crm.lookup_customer",
		"a tenant the grants do not reach must not appear")
}

// A global grant reaches everything the snapshot carries, and a spanning
// credential holding one should see it all rather than nothing.
func TestAGlobalGrantSpansEveryTenant(t *testing.T) {
	t.Parallel()
	f := twoTenantFixture(t)
	f.setRBAC(
		authz.Catalog{"admin": {
			authz.PermToolList: {}, authz.PermToolCall: {},
		}},
		[]*snapshotpb.Principal{
			{
				Id: "root", Subject: "root", SpansTenants: true,
				Grants: []*snapshotpb.Grant{{Role: "admin", Scope: "*"}},
			},
		},
	)
	f.build(t)

	require.Len(t, toolCatalog(t, f, "root"), 2)
}

// Each qualified name must resolve back to the tool in its own tenant, or a
// call would reach the wrong backend — the failure this whole scheme exists to
// make impossible.
func TestEachQualifiedNameResolvesToItsOwnTenant(t *testing.T) {
	t.Parallel()
	f := twoTenantFixture(t)
	f.setRBAC(
		authz.Catalog{"tool_user": {
			authz.PermToolList: {}, authz.PermToolCall: {},
		}},
		[]*snapshotpb.Principal{
			spanningPrincipal("span", true, "", "t/crm-live", "t/crm-test"),
		},
	)
	f.build(t)

	pv, err := f.store.PrincipalView(context.Background(), "span")
	require.NoError(t, err)

	live := pv.Tool("crm-live.crm.lookup_customer")
	require.NotNil(t, live)
	require.Equal(t, "crm-live", live.Tenant.Slug)

	test := pv.Tool("crm-test.crm.lookup_customer")
	require.NotNil(t, test)
	require.Equal(t, "crm-test", test.Tenant.Slug)

	// And the unqualified name resolves to neither, so a client cannot address
	// a tool ambiguously and be silently routed to one of them.
	require.Nil(t, pv.Tool("crm.lookup_customer"))
}

func TestASpanningViewHasNoSingleTenant(t *testing.T) {
	t.Parallel()
	f := twoTenantFixture(t)
	f.setRBAC(
		authz.Catalog{"tool_user": {
			authz.PermToolList: {}, authz.PermToolCall: {},
		}},
		[]*snapshotpb.Principal{
			spanningPrincipal("span", true, "", "t/crm-live", "t/crm-test"),
		},
	)
	f.build(t)

	pv, err := f.store.PrincipalView(context.Background(), "span")
	require.NoError(t, err)
	require.True(t, pv.Spanning())
	require.Nil(t, pv.Tenant, "nothing may read a single tenant off a spanning view")
	require.Equal(t, "crm-live, crm-test", pv.TenantLabel())
}

// tool:list and tool:call are separate permissions, and the separation has to
// survive into what the view reports — the edge refuses a call on this.
//
// This went unenforced for a long time: the map existed, the composition was
// right, and nothing on the dispatch path read it. Every built-in role with
// tool:list also had tool:call except `viewer`, so nothing surfaced it.
func TestListingAToolIsNotPermissionToCallIt(t *testing.T) {
	t.Parallel()
	f := twoTenantFixture(t)
	f.setRBAC(
		authz.Catalog{"looker": {authz.PermToolList: {}}},
		[]*snapshotpb.Principal{
			{
				Id: "looker", Subject: "looker", TenantId: "tn_live",
				Grants: []*snapshotpb.Grant{{Role: "looker", Scope: "t/crm-live"}},
			},
		},
	)
	f.build(t)

	pv, err := f.store.PrincipalView(context.Background(), "looker")
	require.NoError(t, err)
	require.Len(t, pv.Tools, 1, "the tool is listed")
	require.False(t, pv.Callable("crm.lookup_customer"), "and must not be callable")
}
