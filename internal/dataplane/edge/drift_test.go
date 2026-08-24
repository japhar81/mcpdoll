// Copyright 2026 The MCPDoll Authors.

package edge_test

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/fixtures"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/health"
)

// Drift, end to end, against a backend that really does redeploy.
//
// The unit tests in internal/dataplane/health prove the classifier and the
// state machine. These prove the thing that actually matters to a user: that a
// backend changing its schema stops its tools being called, and that a backend
// merely rewording a description does not.
//
// Nothing here is mocked. The fixture rewrites its own tool registration, which
// is what a real redeploy looks like from the gateway's side.

func TestDriftARewordedDescriptionKeepsServing(t *testing.T) {
	h := newHarness(t, harnessOptions{WithProber: true, SkipHostile: true})
	ctx := context.Background()

	h.Prober.ProbeAll(ctx)
	backend := requireBackend(t, h, "warehouse-flaky")
	require.Equal(t, health.StateHealthy, backend.State,
		"precondition: the backend matches what was admitted")

	// The docs team rewrites a description and redeploys.
	h.Misbehaving.DriftAs(fixtures.DriftCosmetic)
	h.Prober.ProbeAll(ctx)

	backend = requireBackend(t, h, "warehouse-flaky")
	require.Len(t, backend.Drift, 1)
	require.Equal(t, health.DriftCosmetic, backend.Drift[0].Kind)

	// Still healthy, and still callable. Blocking here would take a tool out of
	// service over a sentence, and dropping it from the catalog would
	// invalidate every connected client's prompt cache.
	require.Equal(t, health.StateHealthy, backend.State)
	require.Empty(t, backend.BlockedTools())

	session := h.Connect(t, nil)
	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"}})
	require.NoError(t, err)
	require.False(t, res.IsError, "a reworded description must not stop a tool working")
}

func TestDriftAChangedSchemaIsRefusedUnderStrict(t *testing.T) {
	h := newHarness(t, harnessOptions{WithProber: true, SkipHostile: true})
	ctx := context.Background()

	session := h.Connect(t, nil)
	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"}})
	require.NoError(t, err)
	require.False(t, res.IsError, "precondition: the tool works before the backend changes")

	// The backend redeploys with a new required parameter. Every caller's
	// arguments are now invalid — including arguments a model built from the
	// admitted schema, which is the whole hazard.
	h.Misbehaving.DriftAs(fixtures.DriftSemantic)
	h.Prober.ProbeAll(ctx)

	backend := requireBackend(t, h, "warehouse-flaky")
	require.Equal(t, health.StateDrifted, backend.State)
	require.Equal(t, []string{"whs.check_stock"}, backend.BlockedTools())

	fresh := h.Connect(t, nil)
	res, err = fresh.CallTool(ctx, &sdk.CallToolParams{Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"}})
	require.NoError(t, err, "a refusal is a tool error, not a protocol error")
	require.True(t, res.IsError)

	text := contentText(res)
	require.Contains(t, text, "whs.check_stock")
	require.Contains(t, text, "input schema changed")
	// The model must not retry: nothing it can do will help until somebody
	// publishes, and a refusal that reads as transient produces a retry loop.
	require.Contains(t, text, "retrying will not help")

	raw, err := json.Marshal(res.Meta["mcpdoll"])
	require.NoError(t, err)
	var detail map[string]any
	require.NoError(t, json.Unmarshal(raw, &detail))
	require.Equal(t, "drift_blocked", detail["outcome"])
	require.Equal(t, false, detail["retryable"])
}

func TestDriftTheCatalogIsUnchangedByDriftEitherWay(t *testing.T) {
	h := newHarness(t, harnessOptions{WithProber: true, SkipHostile: true})
	ctx := context.Background()

	before := h.Connect(t, nil)
	listed, err := before.ListTools(ctx, nil)
	require.NoError(t, err)
	admitted := findTool(t, listed.Tools, "whs.check_stock")

	h.Misbehaving.DriftAs(fixtures.DriftSemantic)
	h.Prober.ProbeAll(ctx)

	after := h.Connect(t, nil)
	listedAfter, err := after.ListTools(ctx, nil)
	require.NoError(t, err)

	// The tool is still listed, with its admitted schema, even though calling
	// it is refused. Removing it would invalidate every client's prompt cache
	// over a backend deploy that may be rolled back in five minutes — and only
	// a publish changes the catalog. See ADR 0006.
	still := findTool(t, listedAfter.Tools, "whs.check_stock")
	require.Equal(t, admitted.Description, still.Description)
	require.Equal(t, admitted.InputSchema, still.InputSchema,
		"the gateway serves the admitted schema, never the backend's current one")
}

func TestDriftAdvisoryModeServesThroughIt(t *testing.T) {
	h := newHarness(t, harnessOptions{
		WithProber: true, SkipHostile: true, AdvisoryWarehouse: true,
	})
	ctx := context.Background()

	h.Misbehaving.DriftAs(fixtures.DriftSemantic)
	h.Prober.ProbeAll(ctx)

	backend := requireBackend(t, h, "warehouse-flaky")
	require.Equal(t, health.StateDrifted, backend.State,
		"advisory still notices; it just does not act")
	require.Len(t, backend.Drift, 1)
	require.Equal(t, health.DriftSemantic, backend.Drift[0].Kind)
	require.Empty(t, backend.BlockedTools())

	// The call goes through to a backend that now demands a parameter the
	// caller does not know about. Whether it succeeds is the backend's
	// business — the gateway's job here was to let it try, which is exactly
	// what advisory means and why choosing it is a decision.
	session := h.Connect(t, nil)
	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"}})
	require.NoError(t, err)
	require.NotContains(t, contentText(res), "retrying will not help",
		"an advisory backend must never produce a drift refusal")
}

func TestDriftAnUnadmittedToolIsReportedButStaysUnservable(t *testing.T) {
	h := newHarness(t, harnessOptions{WithProber: true, SkipHostile: true})
	ctx := context.Background()

	h.Misbehaving.AddSurpriseTool("exfiltrate_all")
	h.Prober.ProbeAll(ctx)

	backend := requireBackend(t, h, "warehouse-flaky")
	var added *health.ToolDrift
	for i := range backend.Drift {
		if backend.Drift[i].Kind == health.DriftAdded {
			added = &backend.Drift[i]
		}
	}
	require.NotNil(t, added, "a tool that appeared should be reported")
	require.Equal(t, "exfiltrate_all", added.Name)
	require.Empty(t, added.QualifiedName,
		"the prober must not invent a qualified name; the prefix is admission's to grant")

	// It is reported, and it is still not served. Reporting is so somebody
	// publishes it deliberately, not so it leaks in through the side door.
	require.Equal(t, health.StateHealthy, backend.State)

	session := h.Connect(t, nil)
	listed, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.NotContains(t, toolNames(listed.Tools), "whs.exfiltrate_all")
}

func TestDriftRecoveryClearsTheBlock(t *testing.T) {
	h := newHarness(t, harnessOptions{WithProber: true, SkipHostile: true})
	ctx := context.Background()

	h.Misbehaving.DriftAs(fixtures.DriftSemantic)
	h.Prober.ProbeAll(ctx)
	require.NotEmpty(t, requireBackend(t, h, "warehouse-flaky").BlockedTools())

	// The bad deploy is rolled back.
	h.Misbehaving.Undrift()
	h.Prober.ProbeAll(ctx)

	backend := requireBackend(t, h, "warehouse-flaky")
	require.Equal(t, health.StateHealthy, backend.State)
	require.Empty(t, backend.Drift)
	require.Empty(t, backend.BlockedTools())

	// A block that outlived the drift would need a gateway restart to clear,
	// which is precisely the kind of operational trap this must not set.
	session := h.Connect(t, nil)
	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"}})
	require.NoError(t, err)
	require.False(t, res.IsError)
}

func requireBackend(t *testing.T, h *harness, name string) health.Backend {
	t.Helper()
	for _, b := range h.Prober.Registry().All() {
		if b.ServerName == name {
			return b
		}
	}
	t.Fatalf("no backend named %q in the health registry", name)
	return health.Backend{}
}
