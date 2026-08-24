// Copyright 2026 The MCPDoll Authors.

package edge_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
)

// These tests break things on purpose. A gateway's value is mostly in how it
// behaves when a backend misbehaves, and that behaviour is easy to get subtly
// wrong in ways no happy-path test notices.

// TestBackendDownKeepsCatalogStable is the grace-window requirement.
//
// When a backend goes away the natural instinct is to drop its tools. That is
// the wrong move: the catalog sits near the front of every client's prompt, so
// removing entries invalidates every client's prompt cache — turning one
// backend's outage into a cost and latency event across the entire fleet. The
// tools stay listed and their calls fail fast with something the model can act
// on.
func TestBackendDownKeepsCatalogStable(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, nil)
	ctx := context.Background()

	before, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Contains(t, toolNames(before.Tools), "whs.check_stock")

	// The warehouse backend becomes genuinely unreachable — 503 at the HTTP
	// layer, not a polite MCP error.
	h.Misbehaving.SetDown(true)

	// The catalog is unchanged, on a fresh session so this is about the gateway
	// and not the client's cache.
	fresh := h.Connect(t, nil)
	after, err := fresh.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, toolNames(before.Tools), toolNames(after.Tools),
		"a backend going down must not change the catalog")

	// The call fails, and fails *legibly*.
	res, err := fresh.CallTool(ctx, &sdk.CallToolParams{
		Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"},
	})
	require.NoError(t, err, "a backend outage is a tool error, not a protocol error")
	require.True(t, res.IsError)

	text := contentText(res)
	require.Contains(t, text, "whs.check_stock")
	require.Contains(t, text, "unavailable")
	require.Contains(t, text, "Do not retry immediately",
		"the error must tell the model what to do, or it will retry in a loop")

	// And structured, so the console and an agent framework can both read it.
	detail := mcpdollMeta(t, res)
	require.Equal(t, "backend_unavailable", detail["outcome"])
	require.Equal(t, "warehouse-flaky", detail["backend"])
	require.Equal(t, "whs.check_stock", detail["tool"])

	// Other backends are unaffected: one backend's outage is not the gateway's.
	crm, err := fresh.CallTool(ctx, &sdk.CallToolParams{
		Name: "crm.lookup_customer", Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.False(t, crm.IsError, contentText(crm))
}

// TestBackendRecoveryIsAutomatic: once the backend returns, calls succeed again
// without operator action.
func TestBackendRecoveryIsAutomatic(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, nil)
	ctx := context.Background()

	h.Misbehaving.SetDown(true)
	res, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"},
	})
	require.NoError(t, err)
	require.True(t, res.IsError)

	h.Misbehaving.SetDown(false)

	// The breaker's cooldown in the harness is 200ms; poll a little past it so
	// the test is not sensitive to scheduling.
	require.Eventually(t, func() bool {
		res, err := session.CallTool(ctx, &sdk.CallToolParams{
			Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"},
		})
		return err == nil && !res.IsError
	}, 3*time.Second, 50*time.Millisecond,
		"the backend should recover without operator intervention")
}

// TestCircuitOpensAndTellsTheModelWhenToRetry: repeated failures open the
// breaker, and the resulting error carries a retry-after so the model is told
// when trying again is worthwhile rather than left to guess.
func TestCircuitOpensAndTellsTheModelWhenToRetry(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, nil)
	ctx := context.Background()

	h.Misbehaving.SetDown(true)

	// The harness threshold is 3 consecutive failures.
	var last *sdk.CallToolResult
	for range 6 {
		res, err := session.CallTool(ctx, &sdk.CallToolParams{
			Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"},
		})
		require.NoError(t, err)
		require.True(t, res.IsError)
		last = res
	}

	require.Equal(t, backends.StateOpen, h.Pool.CircuitState(backends.Target{ServerID: "srv_whs", TenantID: "tn_acme"}),
		"repeated failures must open the breaker")
	require.Equal(t, "open", backends.StateOpen.String())

	detail := mcpdollMeta(t, last)
	require.Equal(t, "circuit_open", detail["kind"],
		"once the breaker is open the error should say so, not merely 'unreachable'")
	require.Contains(t, detail, "retry_after",
		"an open breaker knows when it will next admit a probe; tell the model")
	require.Contains(t, contentText(last), "will not be retried before")

	// A healthy backend's breaker is untouched.
	require.Equal(t, backends.StateClosed, h.Pool.CircuitState(backends.Target{ServerID: "srv_crm", TenantID: "tn_acme"}))
}

// TestCircuitDoesNotOpenOnToolErrors: a tool refusing an argument is the tool
// working correctly. Counting it against backend health would eject a perfectly
// healthy backend because a client kept passing a bad id.
func TestCircuitDoesNotOpenOnToolErrors(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, nil)
	ctx := context.Background()

	for range 10 {
		res, err := session.CallTool(ctx, &sdk.CallToolParams{
			Name: "crm.lookup_customer", Arguments: map[string]any{"customer_id": "cus_missing"},
		})
		require.NoError(t, err)
		require.True(t, res.IsError, "precondition: the tool returns a tool-level error")
	}

	require.Equal(t, backends.StateClosed, h.Pool.CircuitState(backends.Target{ServerID: "srv_crm", TenantID: "tn_acme"}),
		"tool-level errors are not backend failures")

	// And the backend still serves a good request.
	ok, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "crm.lookup_customer", Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.False(t, ok.IsError, contentText(ok))
}

// TestSlowBackendRespectsCallerCancellation: a client that gives up must not
// leave the gateway blocked on a backend that will answer eventually.
func TestSlowBackendRespectsCallerCancellation(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, nil)

	h.Misbehaving.SetLatency(600 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"},
	})
	elapsed := time.Since(start)

	require.Error(t, err, "a cancelled call should surface as an error to the caller")
	require.Less(t, elapsed, 500*time.Millisecond,
		"the gateway must abandon the backend call when the caller gives up, not wait it out")
}

// TestFlappingBackendDoesNotCorruptTheCatalog: intermittent failures must affect
// only the failing calls.
func TestFlappingBackendDoesNotCorruptTheCatalog(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, nil)
	ctx := context.Background()

	before, err := session.ListTools(ctx, nil)
	require.NoError(t, err)

	// Every second call returns a tool-level error.
	h.Misbehaving.FailEvery(2)

	var okCount, errCount int
	for range 10 {
		res, err := session.CallTool(ctx, &sdk.CallToolParams{
			Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"},
		})
		require.NoError(t, err)
		if res.IsError {
			errCount++
		} else {
			okCount++
		}
	}
	require.Positive(t, okCount, "some calls should have succeeded")
	require.Positive(t, errCount, "some calls should have failed")

	fresh := h.Connect(t, nil)
	after, err := fresh.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, toolNames(before.Tools), toolNames(after.Tools),
		"a flapping backend must not change the catalog")
}

// TestControlPlaneAbsenceDoesNotStopServing is the availability property the
// whole snapshot design exists for.
//
// The harness has no control plane at all: the snapshot was activated once and
// nothing is streaming updates. Serving must continue indefinitely from that
// snapshot, because a data plane that needs its control plane to answer a request
// has made the control plane a single point of failure for every tool call in the
// organization.
func TestControlPlaneAbsenceDoesNotStopServing(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, nil)
	ctx := context.Background()

	// Nothing is publishing snapshots; the store has exactly one and no source.
	require.Equal(t, []int64{1}, h.Store.History())

	for range 5 {
		res, err := session.CallTool(ctx, &sdk.CallToolParams{
			Name: "crm.lookup_customer", Arguments: map[string]any{"customer_id": "cus_1"},
		})
		require.NoError(t, err)
		require.False(t, res.IsError, contentText(res))
	}

	list, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.NotEmpty(t, list.Tools)
	require.Equal(t, int64(1), h.Edge.SnapshotVersion())
}

// TestDisappearedToolStillFailsClosed: a backend that withdraws a tool must not
// cause the gateway to route the call somewhere else or succeed vacuously. The
// admitted tool stays listed (grace window) and the call fails.
func TestDisappearedToolStillFailsClosed(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	h.Misbehaving.RemoveTool("check_stock")

	fresh := h.Connect(t, nil)
	list, err := fresh.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Contains(t, toolNames(list.Tools), "whs.check_stock",
		"the admitted tool stays listed during the grace window")

	res, err := fresh.CallTool(ctx, &sdk.CallToolParams{
		Name: "whs.check_stock", Arguments: map[string]any{"sku": "SKU-1"},
	})
	require.NoError(t, err)
	require.True(t, res.IsError, "a call to a withdrawn tool must fail, not silently succeed")
}

// mcpdollMeta extracts the gateway's structured detail from a result.
func mcpdollMeta(t *testing.T, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	require.NotNil(t, res.Meta, "the gateway should attach structured detail")
	raw, err := json.Marshal(res.Meta["mcpdoll"])
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}
