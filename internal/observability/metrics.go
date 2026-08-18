// Copyright 2026 The MCPDoll Authors.

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

	// ---- caches -----------------------------------------------------------
	CatalogCacheOps metric.Int64Counter
	VerdictCacheOps metric.Int64Counter
	IdempotencyOps  metric.Int64Counter

	// ---- snapshot ---------------------------------------------------------
	SnapshotVersion metric.Int64Gauge
	SnapshotAgeSecs metric.Float64Gauge
	SnapshotSwaps   metric.Int64Counter
	SnapshotRejects metric.Int64Counter

	// ---- tenancy / cost ---------------------------------------------------
	TokensConsumed metric.Int64Counter
	CostMicros     metric.Int64Counter
	RateLimited    metric.Int64Counter

	// ---- control plane ----------------------------------------------------
	AdmissionStageLatency metric.Float64Histogram
	AdmissionOutcomes     metric.Int64Counter
	DriftEvents           metric.Int64Counter
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
		metric.WithDescription("tools/list requests served, by audience and cache result"))
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
		metric.WithDescription("Backend health: 0 healthy, 1 degraded, 2 ejected, 3 quarantined"))
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

	out.CatalogCacheOps, err = m.Int64Counter("mcpdoll.cache.catalog",
		metric.WithDescription("Catalog cache operations, by result (hit/miss/evict)"))
	track(err)
	out.VerdictCacheOps, err = m.Int64Counter("mcpdoll.cache.verdict",
		metric.WithDescription("Guard verdict cache operations, by result"))
	track(err)
	out.IdempotencyOps, err = m.Int64Counter("mcpdoll.cache.idempotency",
		metric.WithDescription("Idempotency key operations, by result (new/replay)"))
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

	out.TokensConsumed, err = m.Int64Counter("mcpdoll.tokens.consumed",
		metric.WithDescription("Estimated tokens of catalog and result content served"),
		metric.WithUnit("{token}"))
	track(err)
	out.CostMicros, err = m.Int64Counter("mcpdoll.cost",
		metric.WithDescription("Attributed cost in micro-units, by project and team"),
		metric.WithUnit("{micro}"))
	track(err)
	out.RateLimited, err = m.Int64Counter("mcpdoll.rate_limited",
		metric.WithDescription("Requests rejected by a rate limit or token budget"))
	track(err)

	out.AdmissionStageLatency, err = m.Float64Histogram("mcpdoll.admission.stage_latency",
		metric.WithDescription("Duration of one admission pipeline stage"), metric.WithUnit("ms"))
	track(err)
	out.AdmissionOutcomes, err = m.Int64Counter("mcpdoll.admission.outcomes",
		metric.WithDescription("Admission runs by stage and outcome"))
	track(err)
	out.DriftEvents, err = m.Int64Counter("mcpdoll.drift.events",
		metric.WithDescription("Drift detections, by class and configured action"))
	track(err)

	if firstErr != nil {
		return nil, fmt.Errorf("observability: registering instruments: %w", firstErr)
	}
	return out, nil
}
