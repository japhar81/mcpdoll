// Copyright 2026 Henry Zektser.

package health

import (
	"context"
	"log/slog"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	"github.com/mcpdoll/mcpdoll/internal/mcp"
	"github.com/mcpdoll/mcpdoll/internal/observability"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// Lister is the part of the backend pool a probe needs.
//
// An interface rather than the concrete pool so the prober's tests do not need
// five live HTTP servers to exercise a state transition.
type Lister interface {
	ListTools(ctx context.Context, target backends.Target, principal backends.Principal) ([]*sdk.Tool, error)
	NegotiatedVersion(target backends.Target) string
}

// CircuitReporter is the optional half of [Lister] that exposes breaker state.
//
// Optional because a breaker belongs to the call path, not to probing: a
// prober that required one could not be tested without a pool.
type CircuitReporter interface {
	CircuitState(target backends.Target) backends.State
}

// Options configures a Prober.
type Options struct {
	Pool     Lister
	Snapshot *snapshot.Store
	Registry *Registry

	Interval time.Duration
	Timeout  time.Duration
	// GraceWindow is how long a failing backend stays degraded before it is
	// reported unavailable. It matches the edge's grace window by
	// configuration, not by coincidence — two different answers to "is this
	// backend still worth waiting for" would be visible to users as a catalog
	// and an error message disagreeing.
	GraceWindow time.Duration
	// EWMAAlpha weights the newest latency sample. Higher reacts faster and is
	// noisier.
	EWMAAlpha float64

	// Concurrency bounds simultaneous probes. Twenty backends probed at once
	// every thirty seconds is a small thundering herd against whatever they
	// share.
	Concurrency int

	// Metrics is optional. Without it the prober still classifies and blocks;
	// it just does so unobservably, which is the right behaviour for a test and
	// the wrong one for a deployment.
	Metrics *observability.Metrics

	Logger *slog.Logger
	// Now is injectable so tests can advance a clock rather than sleep.
	Now func() time.Time
}

// Prober periodically asks every backend what it publishes.
//
// It does not change what is served — only a snapshot does that. What it
// changes is what the gateway *knows*, which is what lets a strict backend
// refuse a drifted tool and an operator see why.
type Prober struct {
	opts Options
	log  *slog.Logger
	now  func() time.Time
}

// New builds a Prober, filling in defaults.
func New(opts Options) *Prober {
	if opts.Interval <= 0 {
		opts.Interval = 30 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.GraceWindow <= 0 {
		opts.GraceWindow = 10 * time.Minute
	}
	if opts.EWMAAlpha <= 0 || opts.EWMAAlpha > 1 {
		opts.EWMAAlpha = 0.2
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Registry == nil {
		opts.Registry = NewRegistry()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Prober{opts: opts, log: opts.Logger, now: opts.Now}
}

// Registry exposes the conditions this prober maintains.
func (p *Prober) Registry() *Registry { return p.opts.Registry }

// Run probes until the context is cancelled.
//
// The first sweep happens immediately rather than after one interval: a gateway
// that has just started should learn what it is fronting now, not in thirty
// seconds.
func (p *Prober) Run(ctx context.Context) {
	p.ProbeAll(ctx)

	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.ProbeAll(ctx)
		}
	}
}

// ProbeAll probes every backend in the current snapshot once.
func (p *Prober) ProbeAll(ctx context.Context) {
	view := p.opts.Snapshot.Current()
	if view == nil {
		// No snapshot means nothing is being served, so there is nothing whose
		// fitness to serve could be in question.
		return
	}

	// The serving snapshot's age, and the revocation list's. A gauge nobody
	// samples never appears, and this is the only loop that runs on a timer.
	//
	// The revocation age matters more than it looks: ADR 0023 bounds the
	// leaked-credential exposure rather than eliminating it, and this gauge is
	// the bound. A revoked credential keeps working for exactly as long as this
	// number says the list is out of date.
	if m := p.opts.Metrics; m != nil {
		m.SnapshotAgeSecs.Record(ctx, view.Age().Seconds())

		revocations := p.opts.Snapshot.Revocations()
		m.RevocationsAgeSecs.Record(ctx, revocations.Age().Seconds())
		m.RevocationsVersion.Record(ctx, revocations.Version)
		m.RevocationsCount.Record(ctx, int64(revocations.Count()))
	}

	servers := view.Servers()
	keep := map[backends.Target]bool{}
	for _, s := range servers {
		for _, b := range s.Bindings {
			keep[backends.Target{ServerID: s.Id, TenantID: b.TenantId}] = true
		}
	}
	// Before probing, not after: a binding dropped from the snapshot must stop
	// blocking calls immediately, not at the end of a sweep that might fail.
	p.opts.Registry.Forget(keep)

	admitted := admittedByBinding(view.Proto())

	sem := make(chan struct{}, p.opts.Concurrency)
	var wg sync.WaitGroup
	for _, server := range servers {
		for _, binding := range server.Bindings {
			wg.Add(1)
			go func(server *snapshotpb.Server, binding *snapshotpb.Binding) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
				target := backends.Target{ServerID: server.Id, TenantID: binding.TenantId}
				p.probe(ctx, view, server, binding, admitted[target])
			}(server, binding)
		}
	}
	wg.Wait()
}

func admittedByBinding(snap *snapshotpb.Snapshot) map[backends.Target]map[string]*snapshotpb.ToolDefinition {
	out := map[backends.Target]map[string]*snapshotpb.ToolDefinition{}
	for _, tool := range snap.Tools {
		// Keyed by (server, tenant): the same tool admitted for two tenants is
		// two definitions with two digests, and comparing one tenant's backend
		// against another's admitted set would report drift that is not there.
		target := backends.Target{ServerID: tool.ServerId, TenantID: tool.TenantId}
		byName, ok := out[target]
		if !ok {
			byName = map[string]*snapshotpb.ToolDefinition{}
			out[target] = byName
		}
		// Keyed by the backend's own name: the qualified name carries a prefix
		// the backend knows nothing about.
		byName[tool.Name] = tool
	}
	return out
}

func (p *Prober) probe(
	ctx context.Context,
	view *snapshot.View,
	server *snapshotpb.Server,
	binding *snapshotpb.Binding,
	admitted map[string]*snapshotpb.ToolDefinition,
) {
	target := backends.Target{ServerID: server.Id, TenantID: binding.TenantId}
	previous, _ := p.opts.Registry.Backend(target)

	tenantSlug := binding.TenantId
	if t := view.TenantByID(binding.TenantId); t != nil {
		tenantSlug = t.Slug
	}

	current := Backend{
		Target:        target,
		ServerName:    server.Name,
		TenantSlug:    tenantSlug,
		Endpoint:      binding.Primary,
		ServingMode:   servingModeName(server.ServingMode),
		LastProbe:     p.now(),
		LastSuccess:   previous.LastSuccess,
		LatencyEWMAMs: previous.LatencyEWMAMs,
		ToolsAdmitted: len(admitted),
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	started := p.now()
	// Probing with an empty principal is deliberate. A probe must measure the
	// backend, not one user's entitlements, and borrowing a real principal's
	// identity to make a background request is exactly the kind of ambient
	// authority this system exists to avoid.
	tools, err := p.opts.Pool.ListTools(probeCtx, target, backends.Principal{})
	elapsed := p.now().Sub(started)

	if err != nil {
		current.ConsecutiveFailures = previous.ConsecutiveFailures + 1
		current.Error = err.Error()
		// The previous sweep's drift is kept. A backend that is down has not
		// stopped being drifted, and clearing the list would silently unblock
		// tools that strict mode was refusing.
		current.Drift = previous.Drift
		current.ToolsObserved = previous.ToolsObserved
		current.State = classify(err, current.Drift, current.LastSuccess, p.now(), p.opts.GraceWindow)

		p.opts.Registry.Set(current)
		p.record(ctx, current, elapsed, "error")
		p.logTransition(previous, current)
		return
	}

	current.LastSuccess = p.now()
	current.LatencyEWMAMs = ewma(
		time.Duration(previous.LatencyEWMAMs)*time.Millisecond,
		elapsed, p.opts.EWMAAlpha,
	).Milliseconds()
	current.NegotiatedVersion = p.opts.Pool.NegotiatedVersion(target)
	current.ToolsObserved = len(tools)

	observed, digestErr := mcp.DigestTools(tools)
	if digestErr != nil {
		// The backend answered but published something that will not
		// canonicalize. Reported as an error rather than as drift: nothing can
		// be said about *which* tool changed when the catalog cannot be read.
		current.Error = "the backend's catalog could not be canonicalized: " + digestErr.Error()
		current.State = StateDrifted
		current.Drift = previous.Drift
		p.opts.Registry.Set(current)
		p.record(ctx, current, elapsed, "uncanonicalizable")
		p.logTransition(previous, current)
		return
	}

	current.Drift = Diff(admitted, observed)

	// Replicas are compared against the *primary's* observed catalog, not
	// against the admitted set. During a rolling deploy the primary is already
	// ahead of what was admitted, and measuring replicas against the snapshot
	// would flag every one of them for agreeing with each other. See ADR 0017.
	current.Replicas = p.probeReplicas(ctx, server, binding, observed)

	current.State = classify(nil, current.Drift, current.LastSuccess, p.now(), p.opts.GraceWindow)

	p.opts.Registry.Set(current)
	p.record(ctx, current, elapsed, "ok")
	p.recordDrift(ctx, previous, current)
	p.logTransition(previous, current)
}

// stateCode is the numeric health gauge.
//
// A gauge rather than a label-per-state counter, because the question is "what
// is this backend now", and a counter answers "how often has it been". The
// encoding is documented on the instrument so a dashboard need not guess.
func stateCode(s State) int64 {
	switch s {
	case StateHealthy:
		return 1
	case StateDegraded:
		return 2
	case StateUnavailable:
		return 3
	case StateDrifted:
		return 4
	default:
		return 0
	}
}

// record publishes one probe's outcome.
func (p *Prober) record(ctx context.Context, b Backend, elapsed time.Duration, outcome string) {
	m := p.opts.Metrics
	if m == nil {
		return
	}
	backend := observability.MetricAttrs(observability.AttrBackend.String(b.ServerName))

	m.ProbeRuns.Add(ctx, 1, observability.MetricAttrs(
		observability.AttrBackend.String(b.ServerName),
		observability.AttrOutcome.String(outcome),
	))
	m.ProbeLatency.Record(ctx, float64(elapsed.Microseconds())/1000.0, backend)
	m.BackendHealthState.Record(ctx, stateCode(b.State), backend)

	// The breaker's state is the pool's, not the prober's — but the prober is
	// the only thing that runs on a timer, and a gauge nobody samples is a
	// gauge that never appears.
	if state, ok := p.circuitState(b.Target); ok {
		m.BackendCircuitState.Record(ctx, state, backend)
	}
}

// recordDrift counts drift as it appears, not on every sweep.
//
// Counting per sweep would turn one unfixed drift into a rate proportional to
// the probe interval, which reads as an escalating problem rather than a
// standing one.
func (p *Prober) recordDrift(ctx context.Context, previous, current Backend) {
	m := p.opts.Metrics
	if m == nil {
		return
	}
	seen := map[string]bool{}
	for _, d := range previous.Drift {
		seen[d.Name+"|"+string(d.Kind)] = true
	}
	for _, d := range current.Drift {
		if seen[d.Name+"|"+string(d.Kind)] {
			continue
		}
		action := "recorded"
		if d.Kind.Blocking() && current.ServingMode == "strict" {
			action = "blocked"
		}
		m.DriftEvents.Add(ctx, 1, observability.MetricAttrs(
			observability.AttrBackend.String(current.ServerName),
			observability.AttrDriftClass.String(string(d.Kind)),
			observability.AttrOutcome.String(action),
		))
	}
}

// circuitState reads the pool's breaker, when the pool exposes one.
func (p *Prober) circuitState(target backends.Target) (int64, bool) {
	reporter, ok := p.opts.Pool.(CircuitReporter)
	if !ok {
		return 0, false
	}
	switch reporter.CircuitState(target) {
	case backends.StateOpen:
		return 2, true
	case backends.StateHalfOpen:
		return 1, true
	default:
		return 0, true
	}
}

// logTransition logs a state change, and only a state change.
//
// A prober that logs every successful probe produces a line per backend per
// interval forever, which is how a log stops being read.
func (p *Prober) logTransition(previous, current Backend) {
	if previous.State == current.State {
		return
	}

	// A backend seen for the first time has no previous state. Reporting that
	// as `from=""` and calling a first healthy observation a "recovery" reads
	// as though something had been wrong at startup, when in fact nothing had
	// been looked at yet.
	first := previous.State == ""
	from := string(previous.State)
	if first {
		from = string(StateUnknown)
	}

	attrs := []any{
		slog.String("server", current.ServerName),
		slog.String("from", from),
		slog.String("to", string(current.State)),
		slog.String("serving_mode", current.ServingMode),
	}
	if current.Error != "" {
		attrs = append(attrs, slog.String("error", current.Error))
	}
	if blocked := current.BlockedTools(); len(blocked) > 0 {
		attrs = append(attrs,
			slog.Int("blocked_tools", len(blocked)),
			slog.Any("blocked", blocked))
	}
	if n := len(current.Drift); n > 0 {
		attrs = append(attrs, slog.Int("drift", n))
	}

	if first {
		// One line per backend at startup is a useful inventory; the same line
		// every interval afterwards is what stops a log being read, which is
		// why only transitions are logged from here on.
		p.log.Info("backend observed for the first time", attrs...)
		return
	}

	switch current.State {
	case StateHealthy:
		p.log.Info("backend recovered", attrs...)
	case StateDrifted:
		// Warn rather than error even in strict mode: the gateway is behaving
		// exactly as configured, and the thing that needs attention is a
		// publish, not an incident.
		p.log.Warn("backend catalog drifted from what was admitted", attrs...)
	case StateDegraded:
		p.log.Warn("backend failing; tools stay listed during the grace window", attrs...)
	case StateUnavailable:
		p.log.Error("backend unavailable", attrs...)
	default:
		p.log.Info("backend state changed", attrs...)
	}
}

func servingModeName(mode snapshotpb.ServingMode) string {
	switch mode {
	case snapshotpb.ServingMode_SERVING_MODE_ADVISORY:
		return "advisory"
	default:
		// Unspecified resolves to strict, matching what admission does. Naming
		// it "unspecified" here would report a mode that has no behaviour.
		return "strict"
	}
}

// probeReplicas checks a binding's non-primary hosts against the primary.
//
// A replica whose *semantic* digest diverges leaves the routable pool. A
// replica whose description differs is left alone: the gateway serves the
// admitted description regardless (ADR 0006), so the difference is unobservable
// to a client, and ejecting a healthy host over prose trades real capacity for
// nothing.
func (p *Prober) probeReplicas(
	ctx context.Context,
	server *snapshotpb.Server,
	binding *snapshotpb.Binding,
	primary map[string]mcp.ToolDigests,
) []Replica {
	if len(binding.Replicas) == 0 {
		return nil
	}

	out := make([]Replica, 0, len(binding.Replicas))
	for _, endpoint := range binding.Replicas {
		replica := Replica{Endpoint: endpoint}

		probeCtx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
		discovered, err := mcp.Discover(probeCtx, mcp.DiscoverOptions{
			Endpoint: endpoint,
			Timeout:  p.opts.Timeout,
		})
		cancel()

		if err != nil {
			replica.Error = err.Error()
			out = append(out, replica)
			continue
		}
		replica.NegotiatedVersion = discovered.NegotiatedVersion

		observed, err := mcp.DigestTools(discovered.Tools)
		if err != nil {
			replica.Error = "catalog could not be canonicalized: " + err.Error()
			out = append(out, replica)
			continue
		}

		// Only blocking divergence removes a replica. Prose differences are
		// counted so an operator can see a partial deploy, and ignored for
		// routing.
		for _, d := range diffObserved(primary, observed) {
			if d.Kind.Blocking() {
				replica.Diverged = append(replica.Diverged, d)
			}
		}
		replica.Routable = len(replica.Diverged) == 0
		out = append(out, replica)
	}
	return out
}

// diffObserved compares two live catalogs.
//
// Distinct from [Diff], which compares an observed catalog against *admitted*
// definitions. Here both sides are observations, so there is no admitted
// qualified name to report and no notion of a tool being "unadmitted".
func diffObserved(primary, replica map[string]mcp.ToolDigests) []ToolDrift {
	var out []ToolDrift

	for name, want := range primary {
		got, present := replica[name]
		if !present {
			out = append(out, ToolDrift{
				Name: name, Kind: DriftRemoved,
				Detail: "the primary publishes this tool and the replica does not",
			})
			continue
		}
		if got.Full == want.Full {
			continue
		}
		if got.Semantic == want.Semantic {
			out = append(out, ToolDrift{
				Name: name, Kind: DriftCosmetic,
				Detail: "prose differs from the primary; the schema matches",
			})
			continue
		}
		out = append(out, ToolDrift{
			Name: name, Kind: DriftSemantic,
			AdmittedDigest: string(want.Semantic),
			ObservedDigest: string(got.Semantic),
			Detail:         "the schema differs from the primary",
		})
	}

	for name := range replica {
		if _, ok := primary[name]; !ok {
			out = append(out, ToolDrift{
				Name: name, Kind: DriftAdded,
				Detail: "the replica publishes a tool the primary does not",
			})
		}
	}
	return out
}
