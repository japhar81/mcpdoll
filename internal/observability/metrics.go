// Copyright 2026 Henry Zektser.

package observability

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// Metrics is the complete instrument set. Every instrument is created once at
// startup and held here, rather than looked up by name at each call site: a
// typo in an instrument name is then a compile error, and there is one place to
// read to know what MCPDoll actually measures.
//
// Buckets are not configured here. Histogram bucket boundaries are a
// deployment concern and belong in the collector's view configuration, which is
// where an operator can change them without a rebuild.
type Metrics struct {
	// ---- per-tool serving -------------------------------------------------
	ToolCalls    metric.Int64Counter
	ToolLatency  metric.Float64Histogram
	ToolErrors   metric.Int64Counter
	CatalogLists metric.Int64Counter
	CatalogSize  metric.Int64Histogram

	// ---- backends ---------------------------------------------------------
	BackendDispatches   metric.Int64Counter
	BackendLatency      metric.Float64Histogram
	BackendCircuitState metric.Int64Gauge
	ProbeRuns           metric.Int64Counter
	ProbeLatency        metric.Float64Histogram
	BackendHealthState  metric.Int64Gauge

	// ---- pipeline ---------------------------------------------------------
	HookLatency      metric.Float64Histogram
	PluginLatency    metric.Float64Histogram
	PluginVerdicts   metric.Int64Counter
	BudgetExhausted  metric.Int64Counter
	PluginSkips      metric.Int64Counter
	CircuitTrips     metric.Int64Counter
	ShadowDivergence metric.Int64Counter

	// ---- snapshot ---------------------------------------------------------
	SnapshotVersion metric.Int64Gauge
	SnapshotAgeSecs metric.Float64Gauge
	SnapshotSwaps   metric.Int64Counter
	SnapshotRejects metric.Int64Counter

	// ---- revocation -------------------------------------------------------
	//
	// RevocationsAgeSecs is the one to alert on. ADR 0023 does not eliminate
	// the leaked-credential exposure — failing closed on an unreachable list
	// would let a control-plane outage stop tool calls — it bounds it, and this
	// is the bound: a revoked credential keeps working for exactly as long as
	// this list is out of date.
	RevocationsAgeSecs metric.Float64Gauge
	RevocationsVersion metric.Int64Gauge
	RevocationsCount   metric.Int64Gauge
	RevocationRefusals metric.Int64Counter

	// ---- drift ------------------------------------------------------------
	DriftEvents metric.Int64Counter
}

// NewMetrics registers every instrument on the provided meter.
func NewMetrics(m metric.Meter) (*Metrics, error) {
	var firstErr error
	// track keeps the constructor readable: the OTel API returns an error from
	// every instrument constructor, and 30 explicit checks would bury the list
	// of what is actually measured.
	track := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	out := &Metrics{}
	var err error

	out.ToolCalls, err = m.Int64Counter("mcpdoll.tool.calls",
		metric.WithDescription("Tool invocations served, by tool and outcome"))
	track(err)
	out.ToolLatency, err = m.Float64Histogram("mcpdoll.tool.latency",
		metric.WithDescription("End-to-end tool call latency as observed by the client"),
		metric.WithUnit("ms"))
	track(err)
	out.ToolErrors, err = m.Int64Counter("mcpdoll.tool.errors",
		metric.WithDescription("Tool calls that returned an error, by error kind"))
	track(err)
	out.CatalogLists, err = m.Int64Counter("mcpdoll.catalog.lists",
		metric.WithDescription("tools/list requests served, by tenant and cache result"))
	track(err)
	out.CatalogSize, err = m.Int64Histogram("mcpdoll.catalog.size",
		metric.WithDescription("Number of tools in a served catalog, after filtering"),
		metric.WithUnit("{tool}"))
	track(err)

	out.BackendDispatches, err = m.Int64Counter("mcpdoll.backend.dispatches",
		metric.WithDescription("Requests dispatched to a backend, by backend and outcome"))
	track(err)
	out.BackendLatency, err = m.Float64Histogram("mcpdoll.backend.latency",
		metric.WithDescription("Backend dispatch latency"), metric.WithUnit("ms"))
	track(err)
	out.BackendCircuitState, err = m.Int64Gauge("mcpdoll.backend.circuit_state",
		metric.WithDescription("Circuit breaker state per backend: 0 closed, 1 half-open, 2 open"))
	track(err)
	out.ProbeRuns, err = m.Int64Counter("mcpdoll.backend.probes",
		metric.WithDescription("Health probes run, by backend and outcome"))
	track(err)
	out.ProbeLatency, err = m.Float64Histogram("mcpdoll.backend.probe_latency",
		metric.WithDescription("Health probe latency"), metric.WithUnit("ms"))
	track(err)
	out.BackendHealthState, err = m.Int64Gauge("mcpdoll.backend.health_state",
		metric.WithDescription(
			"Backend health: 0 unknown, 1 healthy, 2 degraded, 3 unavailable, 4 drifted"))
	track(err)

	out.HookLatency, err = m.Float64Histogram("mcpdoll.hook.latency",
		metric.WithDescription("Time spent in one pipeline hook, all plugins included"),
		metric.WithUnit("ms"))
	track(err)
	out.PluginLatency, err = m.Float64Histogram("mcpdoll.plugin.latency",
		metric.WithDescription("Time spent in one plugin invocation"), metric.WithUnit("ms"))
	track(err)
	out.PluginVerdicts, err = m.Int64Counter("mcpdoll.plugin.verdicts",
		metric.WithDescription("Plugin verdicts, by plugin, verdict and rollout state"))
	track(err)
	out.BudgetExhausted, err = m.Int64Counter("mcpdoll.pipeline.budget_exhausted",
		metric.WithDescription("Requests where a hook or the total plugin budget ran out"))
	track(err)
	out.PluginSkips, err = m.Int64Counter("mcpdoll.plugin.skips",
		metric.WithDescription("Plugin invocations skipped, by plugin and skip reason"))
	track(err)
	out.CircuitTrips, err = m.Int64Counter("mcpdoll.plugin.circuit_trips",
		metric.WithDescription("Plugin circuit breaker openings"))
	track(err)
	out.ShadowDivergence, err = m.Int64Counter("mcpdoll.plugin.shadow_divergence",
		metric.WithDescription("Shadow-mode verdicts that differ from the enforced outcome"))
	track(err)

	out.SnapshotVersion, err = m.Int64Gauge("mcpdoll.snapshot.version",
		metric.WithDescription("Monotonic version of the snapshot this instance is serving"))
	track(err)
	out.SnapshotAgeSecs, err = m.Float64Gauge("mcpdoll.snapshot.age",
		metric.WithDescription("Age of the serving snapshot, for detecting a stalled distribution"),
		metric.WithUnit("s"))
	track(err)
	out.SnapshotSwaps, err = m.Int64Counter("mcpdoll.snapshot.swaps",
		metric.WithDescription("Successful snapshot activations"))
	track(err)
	out.SnapshotRejects, err = m.Int64Counter("mcpdoll.snapshot.rejects",
		metric.WithDescription("Snapshots refused, by reason (signature/version/validation)"))
	track(err)

	out.RevocationsAgeSecs, err = m.Float64Gauge("mcpdoll.revocations.age",
		metric.WithDescription(
			"Age of the revocation list in effect. This is the exposure window for a "+
				"revoked credential, and the number to alert on"),
		metric.WithUnit("s"))
	track(err)
	out.RevocationsVersion, err = m.Int64Gauge("mcpdoll.revocations.version",
		metric.WithDescription("Version of the revocation list this instance is applying"))
	track(err)
	out.RevocationsCount, err = m.Int64Gauge("mcpdoll.revocations.principals",
		metric.WithDescription("Principals the revocation list currently refuses"))
	track(err)
	out.RevocationRefusals, err = m.Int64Counter("mcpdoll.revocations.refusals",
		metric.WithDescription("Requests refused because the credential was revoked"))
	track(err)

	out.DriftEvents, err = m.Int64Counter("mcpdoll.drift.events",
		metric.WithDescription("Drift detections, by class and configured action"))
	track(err)

	if firstErr != nil {
		return nil, fmt.Errorf("observability: registering instruments: %w", firstErr)
	}
	return out, nil
}
