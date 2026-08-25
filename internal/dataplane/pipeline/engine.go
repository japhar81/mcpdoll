// Copyright 2026 Henry Zektser.

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	"github.com/mcpdoll/mcpdoll/internal/observability"
	"github.com/mcpdoll/mcpdoll/internal/platform/logging"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// Host executes a plugin.
//
// One interface for both runtimes: the engine's job is ordering, budgets, and
// verdict combination, none of which depend on whether a plugin is WASM in-process
// or gRPC out-of-process. Keeping that boundary honest is what makes the WASM
// host testable without a WASM module and the engine testable without either.
type Host interface {
	// Invoke runs the plugin. Returning an error means the plugin failed; the
	// engine applies the manifest's failure policy for the request's effect
	// class. A plugin that wants to abstain returns an allow verdict instead.
	Invoke(ctx context.Context, inv *Invocation) (*Verdict, error)

	// Close releases the plugin's resources.
	Close() error
}

// Invocation is one call into a plugin.
type Invocation struct {
	Manifest *snapshotpb.PluginManifest
	Hook     snapshotpb.Hook

	// Context is the request state the plugin is allowed to see, already
	// restricted to its declared `reads` scopes. Canonical JSON.
	Context []byte

	// Shadow tells the plugin its verdict will not be acted on, so it can
	// cheapen its work — the guard skips escalation to the larger model in
	// shadow mode rather than paying for a decision nobody will use.
	Shadow bool

	// Deadline the engine will enforce.
	Deadline time.Time
}

// Options configures an Engine.
type Options struct {
	Logger    *slog.Logger
	Telemetry *observability.Provider
	Metrics   *observability.Metrics

	// TotalBudget caps the wall-clock time all plugins together may consume for
	// one request.
	TotalBudget time.Duration
	// HookBudget is the default per-hook deadline when a manifest omits one.
	HookBudget time.Duration
	// PluginBudget is the default per-plugin deadline when a manifest omits one.
	PluginBudget time.Duration

	// CircuitFailureThreshold is consecutive plugin failures before its breaker
	// opens.
	CircuitFailureThreshold int
	CircuitCooldown         time.Duration

	// Hosts resolves a manifest to its runtime. The engine never constructs a
	// host itself, so a deployment can run with WASM only, gRPC only, or
	// neither.
	Hosts HostResolver

	// TraceSink receives every request's trace. Nil discards them, which is a
	// legitimate configuration for a deployment that has not wired an audit
	// store yet — but it means the waterfall has nothing to show.
	TraceSink func(*Trace)

	// SampleFn decides canary membership. Injectable so tests are deterministic;
	// production uses a hash of the request id.
	SampleFn func(requestID string, percent int32) bool
}

// HostResolver maps a plugin manifest to a running host.
type HostResolver interface {
	// Host returns the host for a manifest, or an error if the plugin cannot be
	// run. An error is not fatal: the engine records a skip and applies the
	// failure policy.
	Host(manifest *snapshotpb.PluginManifest) (Host, error)
}

// Engine runs the hook pipeline.
type Engine struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	breakers map[string]*breaker
}

// New builds an engine.
func New(opts Options) (*Engine, error) {
	if opts.Hosts == nil {
		return nil, errors.New("pipeline: a host resolver is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Telemetry == nil {
		opts.Telemetry = observability.NoopProvider()
	}
	if opts.Metrics == nil {
		m, err := observability.NewMetrics(opts.Telemetry.Meter)
		if err != nil {
			return nil, fmt.Errorf("pipeline: %w", err)
		}
		opts.Metrics = m
	}
	if opts.TotalBudget <= 0 {
		opts.TotalBudget = 250 * time.Millisecond
	}
	if opts.HookBudget <= 0 {
		opts.HookBudget = 50 * time.Millisecond
	}
	if opts.PluginBudget <= 0 {
		opts.PluginBudget = 25 * time.Millisecond
	}
	if opts.CircuitFailureThreshold < 1 {
		opts.CircuitFailureThreshold = 5
	}
	if opts.CircuitCooldown <= 0 {
		opts.CircuitCooldown = 30 * time.Second
	}
	if opts.SampleFn == nil {
		opts.SampleFn = hashSample
	}
	return &Engine{
		opts:     opts,
		log:      opts.Logger,
		breakers: map[string]*breaker{},
	}, nil
}

// Run executes one hook and returns the combined verdict.
//
// This is the whole engine. Everything else in the package is either building
// its input or recording its output.
func (e *Engine) Run(ctx context.Context, req *HookRequest) (*HookResult, error) {
	hookStart := time.Now()

	plugins := req.PrincipalView.PluginsFor(req.Hook)
	hookTrace := HookTrace{
		Hook:     hookName(req.Hook),
		BudgetMS: float64(e.hookBudget(req).Milliseconds()),
	}
	result := &HookResult{Decision: DecisionAllow}

	if len(plugins) == 0 {
		req.Trace.Hooks = append(req.Trace.Hooks, hookTrace)
		return result, nil
	}

	ctx, span := e.opts.Telemetry.Tracer.Start(ctx, "pipeline."+hookName(req.Hook),
		trace.WithAttributes(
			observability.AttrHook.String(hookName(req.Hook)),
			// The label, not `.Tenant.Slug`: a spanning credential's view has
			// no single tenant and that field is nil (ADR 0027).
			observability.AttrTenant.String(req.PrincipalView.TenantLabel()),
		))
	defer span.End()

	hookDeadline := hookStart.Add(e.hookBudget(req))

	for _, manifest := range plugins {
		// Two independent budgets, checked in this order because the request
		// budget is the one that protects the *client's* latency: a request that
		// has already spent its whole plugin allowance must stop, even if this
		// particular hook has time left.
		if remaining := e.requestRemaining(req); remaining <= 0 {
			hookTrace.Outcomes = append(hookTrace.Outcomes,
				skipOutcome(manifest, req.Hook, SkipBudgetExalted))
			e.recordSkip(ctx, manifest, SkipBudgetExalted)
			continue
		}
		if time.Now().After(hookDeadline) {
			hookTrace.Exhausted = true
			hookTrace.Outcomes = append(hookTrace.Outcomes,
				skipOutcome(manifest, req.Hook, SkipHookBudget))
			e.recordSkip(ctx, manifest, SkipHookBudget)
			continue
		}

		outcome := e.runPlugin(ctx, req, manifest, hookDeadline)
		hookTrace.Outcomes = append(hookTrace.Outcomes, outcome)

		if !outcome.Enforced || outcome.Verdict == nil {
			continue
		}

		// A deny or a defer ends the hook: there is nothing for a later plugin
		// to usefully add to a refusal, and continuing would let a lower-priority
		// plugin's mutation be applied to a request that is not going to happen.
		switch outcome.Verdict.Decision {
		case DecisionDeny:
			result.Decision = DecisionDeny
			result.Reason = outcome.Verdict.Reason
			result.DecidedBy = manifest.Name
			e.markShortCircuited(&hookTrace, plugins, manifest, req.Hook)
			e.finishHook(req, &hookTrace, hookStart)
			return result, nil
		case DecisionDefer:
			result.Decision = DecisionDefer
			result.Reason = outcome.Verdict.Reason
			result.DecidedBy = manifest.Name
			result.InputRequests = outcome.Verdict.InputRequests
			result.PluginState = outcome.Verdict.PluginState
			e.markShortCircuited(&hookTrace, plugins, manifest, req.Hook)
			e.finishHook(req, &hookTrace, hookStart)
			return result, nil
		case DecisionMutate:
			patched, err := ApplyPatch(req.Payload, outcome.Verdict.Patch, manifest.Writes)
			if err != nil {
				// A patch outside the declared scopes is a manifest violation.
				// Refusing the patch — rather than the request — keeps one
				// misbehaving plugin from taking down a hook, and the outcome
				// records exactly what was rejected.
				idx := len(hookTrace.Outcomes) - 1
				hookTrace.Outcomes[idx].Error = err.Error()
				hookTrace.Outcomes[idx].Enforced = false
				e.log.WarnContext(ctx, "rejected a plugin patch",
					logging.FieldPlugin, manifest.Name,
					logging.FieldHook, hookName(req.Hook),
					"err", err)
				continue
			}
			req.Payload = patched
			result.Payload = patched
			result.Decision = DecisionMutate
			result.DecidedBy = manifest.Name
		case DecisionAnnotate:
			if result.Annotations == nil {
				result.Annotations = map[string]any{}
			}
			for k, v := range outcome.Verdict.Annotations {
				result.Annotations[manifest.Name+"."+k] = v
			}
		}
	}

	e.finishHook(req, &hookTrace, hookStart)
	if result.Payload == nil {
		result.Payload = req.Payload
	}
	return result, nil
}

// runPlugin invokes one plugin under its own deadline and breaker.
func (e *Engine) runPlugin(
	ctx context.Context,
	req *HookRequest,
	manifest *snapshotpb.PluginManifest,
	hookDeadline time.Time,
) Outcome {
	outcome := Outcome{
		PluginID:   manifest.Id,
		PluginName: manifest.Name,
		Hook:       hookName(req.Hook),
		Rollout:    rolloutName(manifest.Rollout),
	}

	br := e.breakerFor(manifest.Id)
	if !br.Allow() {
		outcome.Skipped = true
		outcome.SkipReason = SkipCircuitOpen
		e.recordSkip(ctx, manifest, SkipCircuitOpen)
		return outcome
	}

	host, err := e.opts.Hosts.Host(manifest)
	if err != nil {
		outcome.Skipped = true
		outcome.SkipReason = SkipHostUnavailable
		outcome.Error = err.Error()
		outcome.FailureMode = failureModeName(e.failureMode(manifest, req.EffectClass))
		e.recordSkip(ctx, manifest, SkipHostUnavailable)
		return outcome
	}

	// Whether this invocation's verdict will be acted on. A shadow plugin still
	// runs — that is the point — but its verdict is recorded, not applied.
	enforce := e.shouldEnforce(req, manifest)

	budget := e.pluginBudget(manifest)
	// Never run past the hook's deadline, whatever the manifest says: a plugin
	// with a generous budget must not be able to overrun the hook that contains
	// it.
	deadline := time.Now().Add(budget)
	if deadline.After(hookDeadline) {
		deadline = hookDeadline
	}
	outcome.BudgetMS = float64(time.Until(deadline).Microseconds()) / 1000.0

	invokeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	start := time.Now()
	verdict, err := host.Invoke(invokeCtx, &Invocation{
		Manifest: manifest,
		Hook:     req.Hook,
		Context:  req.Payload,
		Shadow:   !enforce,
		Deadline: deadline,
	})
	elapsed := time.Since(start)

	outcome.Ran = true
	outcome.DurationMS = float64(elapsed.Microseconds()) / 1000.0
	e.addConsumed(req, elapsed)
	e.opts.Metrics.PluginLatency.Record(ctx, outcome.DurationMS,
		observability.MetricAttrs(
			observability.AttrPlugin.String(manifest.Name),
			observability.AttrHook.String(hookName(req.Hook)),
		))

	if err != nil {
		br.Failure()
		if br.JustOpened() {
			e.opts.Metrics.CircuitTrips.Add(ctx, 1,
				observability.MetricAttrs(observability.AttrPlugin.String(manifest.Name)))
			e.log.WarnContext(ctx, "plugin circuit opened",
				logging.FieldPlugin, manifest.Name,
				"consecutive_failures", br.Consecutive())
		}
		mode := e.failureMode(manifest, req.EffectClass)
		outcome.Error = err.Error()
		outcome.FailureMode = failureModeName(mode)

		if mode == snapshotpb.FailureMode_FAILURE_MODE_CLOSED && enforce {
			// Fail closed: a missing check is not a safe default here, so the
			// failure itself becomes a denial.
			outcome.Enforced = true
			outcome.Verdict = &Verdict{
				Decision: DecisionDeny,
				Reason: fmt.Sprintf("%s could not evaluate this request and is configured to fail closed for %s tools",
					manifest.Name, effectClassName(req.EffectClass)),
			}
		}
		return outcome
	}

	if verdict == nil {
		verdict = &Verdict{Decision: DecisionAllow}
	}
	// Validate *before* recording success. Returning garbage is a plugin failure,
	// and a Success() here would reset the consecutive counter that the following
	// Failure() then increments — so a plugin that always returns garbage would
	// oscillate at one failure and never trip its breaker.
	if err := verdict.Validate(manifest); err != nil {
		br.Failure()
		outcome.Error = err.Error()
		outcome.FailureMode = failureModeName(e.failureMode(manifest, req.EffectClass))
		return outcome
	}
	br.Success()

	outcome.Verdict = verdict
	outcome.Enforced = enforce

	e.opts.Metrics.PluginVerdicts.Add(ctx, 1, observability.MetricAttrs(
		observability.AttrPlugin.String(manifest.Name),
		observability.AttrVerdict.String(string(verdict.Decision)),
		observability.AttrRollout.String(rolloutName(manifest.Rollout)),
	))

	// A shadow verdict that would have changed the outcome is the signal an
	// operator needs before promoting the plugin.
	if !enforce && verdict.Decision != DecisionAllow && verdict.Decision != DecisionAnnotate {
		outcome.Diverged = true
		e.opts.Metrics.ShadowDivergence.Add(ctx, 1, observability.MetricAttrs(
			observability.AttrPlugin.String(manifest.Name),
			observability.AttrVerdict.String(string(verdict.Decision)),
		))
		e.log.InfoContext(ctx, "shadow verdict diverged",
			logging.FieldPlugin, manifest.Name,
			logging.FieldHook, hookName(req.Hook),
			logging.FieldVerdict, string(verdict.Decision),
			"reason", verdict.Reason)
	}

	return outcome
}

// shouldEnforce decides whether a plugin's verdict is acted on.
func (e *Engine) shouldEnforce(req *HookRequest, manifest *snapshotpb.PluginManifest) bool {
	switch manifest.Rollout {
	case snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE:
		return true
	case snapshotpb.RolloutState_ROLLOUT_STATE_CANARY:
		return e.opts.SampleFn(req.RequestID, manifest.CanaryPercent)
	default:
		// Shadow, and the zero value. Defaulting an unset rollout to shadow is
		// the safe direction: a plugin nobody has configured must not enforce.
		return false
	}
}

// failureMode resolves the manifest's per-effect-class failure policy.
//
// The default is fail-open, and the asymmetry is deliberate: a plugin outage
// should not take down every read in the organization. A deployment that wants a
// check to be mandatory says so per effect class, which is where the trade-off
// actually differs — availability matters more on a read, correctness matters
// more on a destructive call.
func (e *Engine) failureMode(
	manifest *snapshotpb.PluginManifest,
	effect snapshotpb.EffectClass,
) snapshotpb.FailureMode {
	if mode, ok := manifest.FailurePolicy[effect.String()]; ok &&
		mode != snapshotpb.FailureMode_FAILURE_MODE_UNSPECIFIED {
		return mode
	}
	return snapshotpb.FailureMode_FAILURE_MODE_OPEN
}

func (e *Engine) hookBudget(req *HookRequest) time.Duration {
	// Never promise a hook more than the request has left.
	remaining := e.requestRemaining(req)
	if remaining < e.opts.HookBudget {
		return remaining
	}
	return e.opts.HookBudget
}

func (e *Engine) pluginBudget(manifest *snapshotpb.PluginManifest) time.Duration {
	if manifest.BudgetMs > 0 {
		return time.Duration(manifest.BudgetMs) * time.Millisecond
	}
	return e.opts.PluginBudget
}

func (e *Engine) requestRemaining(req *HookRequest) time.Duration {
	req.mu.Lock()
	defer req.mu.Unlock()
	return e.opts.TotalBudget - req.consumed
}

func (e *Engine) addConsumed(req *HookRequest, d time.Duration) {
	req.mu.Lock()
	req.consumed += d
	req.mu.Unlock()
}

func (e *Engine) finishHook(req *HookRequest, hookTrace *HookTrace, start time.Time) {
	hookTrace.DurationMS = float64(time.Since(start).Microseconds()) / 1000.0
	req.Trace.Hooks = append(req.Trace.Hooks, *hookTrace)
	req.mu.Lock()
	req.Trace.ConsumedMS = float64(req.consumed.Microseconds()) / 1000.0
	req.mu.Unlock()
	req.Trace.TotalBudgetMS = float64(e.opts.TotalBudget.Milliseconds())

	e.opts.Metrics.HookLatency.Record(context.Background(), hookTrace.DurationMS,
		observability.MetricAttrs(observability.AttrHook.String(hookTrace.Hook)))
	if hookTrace.Exhausted {
		e.opts.Metrics.BudgetExhausted.Add(context.Background(), 1,
			observability.MetricAttrs(observability.AttrHook.String(hookTrace.Hook)))
	}
}

// markShortCircuited records the plugins that never ran because an earlier one
// decided.
//
// Without this the trace would simply lack them, which reads as "they were not
// configured" rather than "they were pre-empted" — a materially different thing
// when someone is asking why a plugin did not fire.
func (e *Engine) markShortCircuited(
	hookTrace *HookTrace,
	plugins []*snapshotpb.PluginManifest,
	decidedAt *snapshotpb.PluginManifest,
	hook snapshotpb.Hook,
) {
	seen := false
	for _, manifest := range plugins {
		if manifest.Id == decidedAt.Id {
			seen = true
			continue
		}
		if !seen {
			continue
		}
		hookTrace.Outcomes = append(hookTrace.Outcomes,
			skipOutcome(manifest, hook, SkipShortCircuited))
	}
}

func (e *Engine) recordSkip(ctx context.Context, manifest *snapshotpb.PluginManifest, reason string) {
	e.opts.Metrics.PluginSkips.Add(ctx, 1, observability.MetricAttrs(
		observability.AttrPlugin.String(manifest.Name),
		observability.AttrSkipReason.String(reason),
	))
}

func (e *Engine) breakerFor(pluginID string) *breaker {
	e.mu.Lock()
	defer e.mu.Unlock()
	br, ok := e.breakers[pluginID]
	if !ok {
		br = newBreaker(e.opts.CircuitFailureThreshold, e.opts.CircuitCooldown)
		e.breakers[pluginID] = br
	}
	return br
}

// CircuitState reports a plugin's breaker state, for the console's health view.
func (e *Engine) CircuitState(pluginID string) string {
	e.mu.Lock()
	br, ok := e.breakers[pluginID]
	e.mu.Unlock()
	if !ok {
		return "closed"
	}
	return br.State()
}

// EmitTrace hands a completed trace to the sink.
func (e *Engine) EmitTrace(t *Trace) {
	if e.opts.TraceSink != nil {
		e.opts.TraceSink(t)
	}
}

func skipOutcome(manifest *snapshotpb.PluginManifest, hook snapshotpb.Hook, reason string) Outcome {
	return Outcome{
		PluginID:   manifest.Id,
		PluginName: manifest.Name,
		Hook:       hookName(hook),
		Rollout:    rolloutName(manifest.Rollout),
		Skipped:    true,
		SkipReason: reason,
	}
}

// hashSample decides canary membership from the request id.
//
// Hashing rather than a random draw so a given request is consistently in or out
// across every hook: a request that a canary plugin denied at ON_TOOL_CALL must
// not find itself outside the sample at ON_TOOL_RESULT, which would produce a
// half-enforced request nobody can reason about.
func hashSample(requestID string, percent int32) bool {
	if percent <= 0 {
		return false
	}
	if percent >= 100 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(requestID))
	return int32(h.Sum32()%100) < percent
}

// HookRequest is one hook invocation's input.
type HookRequest struct {
	RequestID     string
	PrincipalView *snapshot.PrincipalView
	Hook          snapshotpb.Hook
	EffectClass   snapshotpb.EffectClass

	// Payload is the request state plugins see and may patch. Canonical JSON.
	Payload []byte

	// Trace accumulates across hooks for one request.
	Trace *Trace

	mu       sync.Mutex
	consumed time.Duration
}

// HookResult is one hook's combined verdict.
type HookResult struct {
	Decision  Decision
	Reason    string
	DecidedBy string

	// Payload is the (possibly patched) state to carry forward.
	Payload []byte

	Annotations map[string]any

	// InputRequests and PluginState carry a deferral to the edge.
	InputRequests []byte
	PluginState   string
}

func hookName(h snapshotpb.Hook) string {
	switch h {
	case snapshotpb.Hook_HOOK_ON_REQUEST:
		return "on_request"
	case snapshotpb.Hook_HOOK_ON_IDENTITY:
		return "on_identity"
	case snapshotpb.Hook_HOOK_ON_CATALOG:
		return "on_catalog"
	case snapshotpb.Hook_HOOK_ON_TOOL_CALL:
		return "on_tool_call"
	case snapshotpb.Hook_HOOK_ON_TOOL_RESULT:
		return "on_tool_result"
	case snapshotpb.Hook_HOOK_ON_RESPONSE:
		return "on_response"
	case snapshotpb.Hook_HOOK_ON_AUDIT:
		return "on_audit"
	default:
		return "unspecified"
	}
}

func rolloutName(r snapshotpb.RolloutState) string {
	switch r {
	case snapshotpb.RolloutState_ROLLOUT_STATE_CANARY:
		return "canary"
	case snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE:
		return "enforce"
	default:
		return "shadow"
	}
}

func failureModeName(m snapshotpb.FailureMode) string {
	if m == snapshotpb.FailureMode_FAILURE_MODE_CLOSED {
		return "closed"
	}
	return "open"
}

func effectClassName(ec snapshotpb.EffectClass) string {
	switch ec {
	case snapshotpb.EffectClass_EFFECT_CLASS_WRITE:
		return "write"
	case snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE:
		return "destructive"
	default:
		return "read"
	}
}
