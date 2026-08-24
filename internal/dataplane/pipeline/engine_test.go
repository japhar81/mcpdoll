// Copyright 2026 The MCPDoll Authors.

package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/pipeline"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// fakeHost is a scripted plugin. The engine's job is ordering, budgets, failure
// policy, and rollout — none of which need a real runtime to exercise, and all of
// which are much easier to test precisely with a host that does exactly what the
// test says.
//
// The WASM host is tested separately against a real compiled plugin; the two
// suites cover different things on purpose.
type fakeHost struct {
	verdict *pipeline.Verdict
	err     error
	delay   time.Duration
	calls   atomic.Int64
	// lastPayload records what the plugin was given, for the scope tests.
	lastPayload atomic.Value
}

func (h *fakeHost) Invoke(ctx context.Context, inv *pipeline.Invocation) (*pipeline.Verdict, error) {
	h.calls.Add(1)
	h.lastPayload.Store(append([]byte(nil), inv.Context...))
	if h.delay > 0 {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if h.err != nil {
		return nil, h.err
	}
	return h.verdict, nil
}

func (h *fakeHost) Close() error { return nil }

// hostMap resolves manifests to fake hosts by plugin id.
type hostMap map[string]pipeline.Host

func (m hostMap) Host(manifest *snapshotpb.PluginManifest) (pipeline.Host, error) {
	h, ok := m[manifest.Id]
	if !ok {
		return nil, fmt.Errorf("no host for %s", manifest.Id)
	}
	return h, nil
}

// harness builds an engine over a snapshot with the given plugins.
type harness struct {
	engine   *pipeline.Engine
	audience *snapshot.PrincipalView
	hosts    hostMap
	traces   []*pipeline.Trace
}

func newHarness(t *testing.T, opts pipeline.Options, manifests ...*snapshotpb.PluginManifest) *harness {
	t.Helper()

	b := snapshot.NewBuilder(1).
		WithCatalogDefaults(5*time.Minute, 30*time.Second)
	b.AddTenant(&snapshotpb.Tenant{Id: "tn_test", Slug: "test", Name: "Test", Status: "active"})
	b.AddToolset(&snapshotpb.Toolset{Id: "ts_test", Name: "test", Priority: 10})
	b.SetRBAC(authz.DefaultCatalog(), []*snapshotpb.Principal{{
		Id: "usr_test", TenantId: "tn_test", Subject: "test@example.com",
		Grants: []*snapshotpb.Grant{
			{Role: authz.RoleToolUser, Scope: authz.TenantScope("test")},
		},
	}})
	b.AddNamespace(&snapshotpb.Namespace{Id: "ns_crm", Name: "crm", Prefix: "crm"})
	b.AddServer(&snapshotpb.Server{
		Id: "srv_crm", Name: "crm-prod", NamespaceId: "ns_crm",
		Bindings: []*snapshotpb.Binding{{TenantId: "tn_test", Primary: "http://localhost:1"}},
	})
	b.AddTool(snapshot.ToolInput{
		ServerID: "srv_crm", NamespaceID: "ns_crm",
		TenantID: "tn_test", ToolsetID: "ts_test", Prefix: "crm",
		Name: "lookup", Description: "Look something up.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		EffectClass: snapshotpb.EffectClass_EFFECT_CLASS_READ,
	})
	for _, m := range manifests {
		b.AddPlugin(m)
	}

	snap, err := b.Build()
	require.NoError(t, err)
	view, err := snapshot.Build(snap)
	require.NoError(t, err)

	pv, err := view.Principal(context.Background(), "usr_test")
	require.NoError(t, err)

	h := &harness{hosts: hostMap{}, audience: pv}
	opts.Hosts = h.hosts
	opts.TraceSink = func(tr *pipeline.Trace) { h.traces = append(h.traces, tr) }

	h.engine, err = pipeline.New(opts)
	require.NoError(t, err)
	return h
}

func (h *harness) install(id string, host pipeline.Host) { h.hosts[id] = host }

func (h *harness) run(t *testing.T, hook snapshotpb.Hook, payload string) (*pipeline.HookResult, *pipeline.Trace) {
	t.Helper()
	trace := &pipeline.Trace{RequestID: "req_test", Audience: "agents", StartedAt: time.Now()}
	req := &pipeline.HookRequest{
		RequestID:   "req_test",
		Audience:    h.audience,
		Hook:        hook,
		EffectClass: snapshotpb.EffectClass_EFFECT_CLASS_READ,
		Payload:     []byte(payload),
		Trace:       trace,
	}
	result, err := h.engine.Run(context.Background(), req)
	require.NoError(t, err)
	return result, trace
}

func manifest(id, name string, opts ...func(*snapshotpb.PluginManifest)) *snapshotpb.PluginManifest {
	m := &snapshotpb.PluginManifest{
		Id: id, Name: name, Version: "1.0.0",
		Runtime:  snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:    []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_CALL},
		Priority: 10,
		Rollout:  snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func withPriority(p int32) func(*snapshotpb.PluginManifest) {
	return func(m *snapshotpb.PluginManifest) { m.Priority = p }
}

func withWrites(scopes ...string) func(*snapshotpb.PluginManifest) {
	return func(m *snapshotpb.PluginManifest) { m.Writes = scopes }
}

func withRollout(r snapshotpb.RolloutState) func(*snapshotpb.PluginManifest) {
	return func(m *snapshotpb.PluginManifest) { m.Rollout = r }
}

func withBudget(d time.Duration) func(*snapshotpb.PluginManifest) {
	return func(m *snapshotpb.PluginManifest) { m.BudgetMs = int32(d.Milliseconds()) }
}

func withFailurePolicy(policy map[string]snapshotpb.FailureMode) func(*snapshotpb.PluginManifest) {
	return func(m *snapshotpb.PluginManifest) { m.FailurePolicy = policy }
}

// ------------------------------------------------------------------ basics ---

func TestNoPluginsIsAnAllow(t *testing.T) {
	h := newHarness(t, pipeline.Options{})
	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{"tool":"crm.lookup"}`)

	require.Equal(t, pipeline.DecisionAllow, result.Decision)
	require.Len(t, trace.Hooks, 1)
	require.Empty(t, trace.Hooks[0].Outcomes)
}

func TestAllowVerdictProceeds(t *testing.T) {
	m := manifest("plg_a", "allower")
	h := newHarness(t, pipeline.Options{}, m)
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow}})

	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{"tool":"crm.lookup"}`)
	require.Equal(t, pipeline.DecisionAllow, result.Decision)
	require.Len(t, trace.Hooks[0].Outcomes, 1)
	require.True(t, trace.Hooks[0].Outcomes[0].Ran)
	require.True(t, trace.Hooks[0].Outcomes[0].Enforced)
}

func TestDenyVerdictStopsTheHook(t *testing.T) {
	first := manifest("plg_a", "denier", withPriority(10))
	second := manifest("plg_b", "later", withPriority(20))
	h := newHarness(t, pipeline.Options{}, first, second)

	denier := &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionDeny, Reason: "not allowed here",
	}}
	later := &fakeHost{verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow}}
	h.install("plg_a", denier)
	h.install("plg_b", later)

	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{"tool":"crm.lookup"}`)

	require.Equal(t, pipeline.DecisionDeny, result.Decision)
	require.Equal(t, "not allowed here", result.Reason)
	require.Equal(t, "denier", result.DecidedBy)
	require.Zero(t, later.calls.Load(),
		"a later plugin must not run after a denial")

	// The pre-empted plugin is *recorded* as short-circuited, not simply absent.
	// "It was skipped because an earlier plugin decided" and "it was not
	// configured" are different answers to the same question.
	require.Len(t, trace.Hooks[0].Outcomes, 2)
	require.Equal(t, pipeline.SkipShortCircuited, trace.Hooks[0].Outcomes[1].SkipReason)
	require.True(t, trace.Hooks[0].Outcomes[1].Skipped)
}

func TestPluginsRunInPriorityOrder(t *testing.T) {
	var order []string
	makeHost := func(name string) pipeline.Host {
		return &recordingHost{name: name, order: &order}
	}

	h := newHarness(t, pipeline.Options{},
		manifest("plg_c", "third", withPriority(30)),
		manifest("plg_a", "first", withPriority(10)),
		manifest("plg_b", "second", withPriority(20)),
	)
	h.install("plg_a", makeHost("first"))
	h.install("plg_b", makeHost("second"))
	h.install("plg_c", makeHost("third"))

	h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, []string{"first", "second", "third"}, order)
}

type recordingHost struct {
	name  string
	order *[]string
}

func (h *recordingHost) Invoke(context.Context, *pipeline.Invocation) (*pipeline.Verdict, error) {
	*h.order = append(*h.order, h.name)
	return &pipeline.Verdict{Decision: pipeline.DecisionAllow}, nil
}
func (h *recordingHost) Close() error { return nil }

// ------------------------------------------------------------------ shadow ---

// TestShadowRecordsWithoutActing is the whole value of shadow mode: the plugin
// runs, its verdict is recorded, and nothing changes.
func TestShadowRecordsWithoutActing(t *testing.T) {
	m := manifest("plg_a", "shadowed", withRollout(snapshotpb.RolloutState_ROLLOUT_STATE_SHADOW))
	h := newHarness(t, pipeline.Options{}, m)
	host := &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionDeny, Reason: "would have denied",
	}}
	h.install("plg_a", host)

	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)

	require.Equal(t, pipeline.DecisionAllow, result.Decision,
		"a shadow verdict must not be acted on")
	require.Equal(t, int64(1), host.calls.Load(), "but the plugin must still run")

	outcome := trace.Hooks[0].Outcomes[0]
	require.True(t, outcome.Ran)
	require.False(t, outcome.Enforced)
	require.True(t, outcome.Diverged,
		"a shadow deny is a divergence: it is what an operator reads before promoting")
	require.Equal(t, pipeline.DecisionDeny, outcome.Verdict.Decision)
	require.Len(t, trace.Divergences(), 1)
}

// TestShadowPluginIsToldItIsShadowed lets an expensive plugin cheapen its work.
func TestShadowPluginIsToldItIsShadowed(t *testing.T) {
	h := newHarness(t, pipeline.Options{},
		manifest("plg_a", "shadowed", withRollout(snapshotpb.RolloutState_ROLLOUT_STATE_SHADOW)))
	host := &shadowProbe{}
	h.install("plg_a", host)

	h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.True(t, host.sawShadow, "a shadowed plugin should be told so")
}

type shadowProbe struct{ sawShadow bool }

func (h *shadowProbe) Invoke(_ context.Context, inv *pipeline.Invocation) (*pipeline.Verdict, error) {
	h.sawShadow = inv.Shadow
	return &pipeline.Verdict{Decision: pipeline.DecisionAllow}, nil
}
func (h *shadowProbe) Close() error { return nil }

// TestUnsetRolloutIsShadow: the zero value must be the safe one. A plugin whose
// rollout nobody configured must not be enforcing.
func TestUnsetRolloutIsShadow(t *testing.T) {
	m := manifest("plg_a", "unset")
	m.Rollout = snapshotpb.RolloutState_ROLLOUT_STATE_UNSPECIFIED
	h := newHarness(t, pipeline.Options{}, m)
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionDeny, Reason: "would deny",
	}})

	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, pipeline.DecisionAllow, result.Decision,
		"an unconfigured rollout must not enforce")
	require.False(t, trace.Hooks[0].Outcomes[0].Enforced)
}

// TestCanaryEnforcesForTheSample.
func TestCanaryEnforces(t *testing.T) {
	m := manifest("plg_a", "canary", withRollout(snapshotpb.RolloutState_ROLLOUT_STATE_CANARY))
	m.CanaryPercent = 50

	t.Run("inside the sample", func(t *testing.T) {
		h := newHarness(t, pipeline.Options{
			SampleFn: func(string, int32) bool { return true },
		}, m)
		h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
			Decision: pipeline.DecisionDeny, Reason: "denied",
		}})
		result, _ := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
		require.Equal(t, pipeline.DecisionDeny, result.Decision)
	})

	t.Run("outside the sample", func(t *testing.T) {
		h := newHarness(t, pipeline.Options{
			SampleFn: func(string, int32) bool { return false },
		}, m)
		host := &fakeHost{verdict: &pipeline.Verdict{
			Decision: pipeline.DecisionDeny, Reason: "denied",
		}}
		h.install("plg_a", host)
		result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
		require.Equal(t, pipeline.DecisionAllow, result.Decision)
		require.Equal(t, int64(1), host.calls.Load(),
			"outside the sample the plugin still runs and is recorded")
		require.True(t, trace.Hooks[0].Outcomes[0].Diverged)
	})
}

// TestCanarySamplingIsStableForARequest: a request that a canary plugin denied at
// one hook must not find itself outside the sample at the next, which would
// produce a half-enforced request nobody could reason about.
func TestCanarySamplingIsStableForARequest(t *testing.T) {
	// The production sampler hashes the request id, so the same id always lands
	// on the same side.
	h := newHarness(t, pipeline.Options{})
	_ = h

	engine, err := pipeline.New(pipeline.Options{Hosts: hostMap{}})
	require.NoError(t, err)
	_ = engine

	// Exercise the default sampler directly via two engines over the same ids.
	seen := map[string]bool{}
	for i := range 50 {
		id := fmt.Sprintf("req_%d", i)
		first := pipeline.SampleForTest(id, 50)
		for range 5 {
			require.Equal(t, first, pipeline.SampleForTest(id, 50),
				"sampling for %s must be stable across calls", id)
		}
		seen[id] = first
	}

	// And the split should be roughly the requested proportion, or the canary
	// percentage means nothing.
	var in int
	for _, v := range seen {
		if v {
			in++
		}
	}
	require.Greater(t, in, 10, "a 50%% canary should include a substantial share")
	require.Less(t, in, 40, "a 50%% canary should exclude a substantial share")
}

// ------------------------------------------------------------------ budget ---

func TestHookBudgetSkipsRemainingPlugins(t *testing.T) {
	h := newHarness(t, pipeline.Options{
		TotalBudget: time.Second,
		HookBudget:  40 * time.Millisecond,
	},
		manifest("plg_slow", "slow", withPriority(10), withBudget(200*time.Millisecond)),
		manifest("plg_next", "next", withPriority(20)),
	)
	h.install("plg_slow", &fakeHost{
		delay:   60 * time.Millisecond,
		verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow},
	})
	next := &fakeHost{verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow}}
	h.install("plg_next", next)

	_, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)

	require.Zero(t, next.calls.Load(), "the hook budget should have stopped the second plugin")
	require.True(t, trace.Hooks[0].Exhausted)
	require.Equal(t, pipeline.SkipHookBudget, trace.Hooks[0].Outcomes[1].SkipReason)
}

// TestPluginDeadlineIsCappedByTheHook: a plugin with a generous manifest budget
// must not be able to overrun the hook that contains it.
func TestPluginDeadlineIsCappedByTheHook(t *testing.T) {
	h := newHarness(t, pipeline.Options{
		TotalBudget: time.Second,
		HookBudget:  30 * time.Millisecond,
	}, manifest("plg_a", "greedy", withBudget(5*time.Second)))

	h.install("plg_a", &fakeHost{
		delay:   2 * time.Second,
		verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow},
	})

	start := time.Now()
	_, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 500*time.Millisecond,
		"a plugin's own budget must not exceed the hook's")
	require.NotEmpty(t, trace.Hooks[0].Outcomes[0].Error)
}

func TestRequestBudgetIsSharedAcrossHooks(t *testing.T) {
	m1 := manifest("plg_a", "first")
	m1.Hooks = []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_CALL}
	m2 := manifest("plg_b", "second")
	m2.Hooks = []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_RESULT}

	h := newHarness(t, pipeline.Options{
		TotalBudget: 50 * time.Millisecond,
		HookBudget:  50 * time.Millisecond,
		// Above the hook budget, so the hook deadline is what caps the plugin and
		// the first hook genuinely spends the request's whole allowance.
		PluginBudget: 100 * time.Millisecond,
	}, m1, m2)

	h.install("plg_a", &fakeHost{
		delay:   60 * time.Millisecond,
		verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow},
	})
	second := &fakeHost{verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow}}
	h.install("plg_b", second)

	// One request, two hooks, one budget.
	trace := &pipeline.Trace{RequestID: "req_shared", Audience: "agents"}
	req := &pipeline.HookRequest{
		RequestID: "req_shared", Audience: h.audience,
		Hook:    snapshotpb.Hook_HOOK_ON_TOOL_CALL,
		Payload: []byte(`{}`), Trace: trace,
	}
	_, err := h.engine.Run(context.Background(), req)
	require.NoError(t, err)

	req.Hook = snapshotpb.Hook_HOOK_ON_TOOL_RESULT
	_, err = h.engine.Run(context.Background(), req)
	require.NoError(t, err)

	require.Zero(t, second.calls.Load(),
		"the first hook consumed the request's whole plugin budget")
	require.Equal(t, pipeline.SkipBudgetExalted, trace.Hooks[1].Outcomes[0].SkipReason)
}

// ------------------------------------------------------------ failure policy ---

func TestFailureFailsOpenByDefault(t *testing.T) {
	h := newHarness(t, pipeline.Options{}, manifest("plg_a", "broken"))
	h.install("plg_a", &fakeHost{err: errors.New("boom")})

	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, pipeline.DecisionAllow, result.Decision,
		"a plugin outage should not take down every read in the organization")
	require.Equal(t, "open", trace.Hooks[0].Outcomes[0].FailureMode)
	require.Contains(t, trace.Hooks[0].Outcomes[0].Error, "boom")
}

// TestFailureFailsClosedWhenConfigured: on a destructive call a missing check is
// not a safe default.
func TestFailureFailsClosedWhenConfigured(t *testing.T) {
	m := manifest("plg_a", "guard", withFailurePolicy(map[string]snapshotpb.FailureMode{
		"EFFECT_CLASS_READ":        snapshotpb.FailureMode_FAILURE_MODE_OPEN,
		"EFFECT_CLASS_DESTRUCTIVE": snapshotpb.FailureMode_FAILURE_MODE_CLOSED,
	}))
	h := newHarness(t, pipeline.Options{}, m)
	h.install("plg_a", &fakeHost{err: errors.New("model unreachable")})

	// A read fails open.
	result, _ := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, pipeline.DecisionAllow, result.Decision)

	// The same failure on a destructive call denies.
	trace := &pipeline.Trace{RequestID: "req_d"}
	result, err := h.engine.Run(context.Background(), &pipeline.HookRequest{
		RequestID: "req_d", Audience: h.audience,
		Hook:        snapshotpb.Hook_HOOK_ON_TOOL_CALL,
		EffectClass: snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE,
		Payload:     []byte(`{}`), Trace: trace,
	})
	require.NoError(t, err)
	require.Equal(t, pipeline.DecisionDeny, result.Decision,
		"a fail-closed plugin's outage must deny a destructive call")
	require.Contains(t, result.Reason, "fail closed")
	require.Equal(t, "closed", trace.Hooks[0].Outcomes[0].FailureMode)
}

// TestShadowFailureNeverDenies: a plugin in shadow cannot deny, even fail-closed.
// Shadow means "not acting", and a shadow plugin that could take down traffic by
// failing would make shadow mode useless as a safe way to try something.
func TestShadowFailureNeverDenies(t *testing.T) {
	m := manifest("plg_a", "guard",
		withRollout(snapshotpb.RolloutState_ROLLOUT_STATE_SHADOW),
		withFailurePolicy(map[string]snapshotpb.FailureMode{
			"EFFECT_CLASS_READ": snapshotpb.FailureMode_FAILURE_MODE_CLOSED,
		}))
	h := newHarness(t, pipeline.Options{}, m)
	h.install("plg_a", &fakeHost{err: errors.New("boom")})

	result, _ := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, pipeline.DecisionAllow, result.Decision)
}

// --------------------------------------------------------------- breaker -----

func TestCircuitOpensAfterConsecutiveFailures(t *testing.T) {
	h := newHarness(t, pipeline.Options{
		CircuitFailureThreshold: 3,
		CircuitCooldown:         time.Hour,
	}, manifest("plg_a", "flaky"))
	host := &fakeHost{err: errors.New("boom")}
	h.install("plg_a", host)

	for range 3 {
		h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	}
	require.Equal(t, int64(3), host.calls.Load())
	require.Equal(t, "open", h.engine.CircuitState("plg_a"))

	// Further requests skip the plugin entirely.
	_, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, int64(3), host.calls.Load(), "an open circuit must skip the plugin")
	require.Equal(t, pipeline.SkipCircuitOpen, trace.Hooks[0].Outcomes[0].SkipReason)
}

func TestCircuitClosesOnSuccess(t *testing.T) {
	h := newHarness(t, pipeline.Options{CircuitFailureThreshold: 3}, manifest("plg_a", "flaky"))
	failing := &fakeHost{err: errors.New("boom")}
	h.install("plg_a", failing)

	h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, "closed", h.engine.CircuitState("plg_a"), "two failures is below the threshold")

	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow}})
	h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, "closed", h.engine.CircuitState("plg_a"))

	// And the counter reset: three more failures are needed, not one.
	h.install("plg_a", failing)
	h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, "closed", h.engine.CircuitState("plg_a"))
}

// TestHostUnavailableIsSkippedNotFatal: a plugin whose host cannot be resolved is
// recorded and skipped, so a misconfigured plugin does not break every request.
func TestHostUnavailableIsSkipped(t *testing.T) {
	h := newHarness(t, pipeline.Options{}, manifest("plg_missing", "missing"))
	// Deliberately no host installed.

	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, pipeline.DecisionAllow, result.Decision)
	require.Equal(t, pipeline.SkipHostUnavailable, trace.Hooks[0].Outcomes[0].SkipReason)
	require.NotEmpty(t, trace.Hooks[0].Outcomes[0].Error)
}

// -------------------------------------------------------------- mutation -----

func TestMutateAppliesPatch(t *testing.T) {
	h := newHarness(t, pipeline.Options{},
		manifest("plg_a", "mutator", withWrites("arguments")))
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionMutate,
		Reason:   "normalized the id",
		Patch:    json.RawMessage(`[{"op":"replace","path":"/arguments/id","value":"CUS_1"}]`),
	}})

	result, _ := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{"arguments":{"id":"cus_1"}}`)
	require.Equal(t, pipeline.DecisionMutate, result.Decision)
	require.JSONEq(t, `{"arguments":{"id":"CUS_1"}}`, string(result.Payload))
}

// TestMutateOutsideScopeIsRejected is what makes the manifest a contract. The
// request proceeds — one misbehaving plugin should not take down a hook — but the
// patch does not apply, and the outcome says why.
func TestMutateOutsideScopeIsRejected(t *testing.T) {
	h := newHarness(t, pipeline.Options{},
		manifest("plg_a", "overreacher", withWrites("result.content")))
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionMutate,
		Patch:    json.RawMessage(`[{"op":"replace","path":"/principal/groups","value":["admins"]}]`),
	}})

	payload := `{"principal":{"groups":["users"]},"result":{"content":[]}}`
	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, payload)

	require.Equal(t, pipeline.DecisionAllow, result.Decision)
	require.JSONEq(t, payload, string(result.Payload),
		"an out-of-scope patch must not be applied")

	outcome := trace.Hooks[0].Outcomes[0]
	require.False(t, outcome.Enforced)
	require.Contains(t, outcome.Error, "outside the plugin's declared writes")
}

// TestMutateWithoutDeclaredWritesIsRejected: a plugin that declares no writes
// cannot mutate at all.
func TestMutateWithoutDeclaredWritesIsRejected(t *testing.T) {
	h := newHarness(t, pipeline.Options{}, manifest("plg_a", "sneaky"))
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionMutate,
		Patch:    json.RawMessage(`[{"op":"replace","path":"/arguments/id","value":"x"}]`),
	}})

	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{"arguments":{"id":"a"}}`)
	require.Equal(t, pipeline.DecisionAllow, result.Decision)
	require.Contains(t, trace.Hooks[0].Outcomes[0].Error, "declares no writes")
}

func TestMutationsChainInPriorityOrder(t *testing.T) {
	h := newHarness(t, pipeline.Options{},
		manifest("plg_a", "first", withPriority(10), withWrites("arguments")),
		manifest("plg_b", "second", withPriority(20), withWrites("arguments")),
	)
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionMutate,
		Patch:    json.RawMessage(`[{"op":"replace","path":"/arguments/step","value":"one"}]`),
	}})
	h.install("plg_b", &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionMutate,
		Patch:    json.RawMessage(`[{"op":"add","path":"/arguments/also","value":"two"}]`),
	}})

	result, _ := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{"arguments":{"step":"zero"}}`)
	require.JSONEq(t, `{"arguments":{"step":"one","also":"two"}}`, string(result.Payload))
}

// TestSecondPluginSeesTheFirstsMutation: the payload chains, so a later plugin
// inspects what will actually be dispatched rather than what arrived.
func TestSecondPluginSeesTheFirstsMutation(t *testing.T) {
	h := newHarness(t, pipeline.Options{},
		manifest("plg_a", "first", withPriority(10), withWrites("arguments")),
		manifest("plg_b", "second", withPriority(20)),
	)
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionMutate,
		Patch:    json.RawMessage(`[{"op":"replace","path":"/arguments/id","value":"changed"}]`),
	}})
	second := &fakeHost{verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow}}
	h.install("plg_b", second)

	h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{"arguments":{"id":"original"}}`)

	seen, _ := second.lastPayload.Load().([]byte)
	require.Contains(t, string(seen), "changed")
	require.NotContains(t, string(seen), "original")
}

// -------------------------------------------------------------- deferral -----

func TestDeferStopsTheHook(t *testing.T) {
	h := newHarness(t, pipeline.Options{},
		manifest("plg_a", "confirmer", withPriority(10)),
		manifest("plg_b", "later", withPriority(20)),
	)
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
		Decision:      pipeline.DecisionDefer,
		Reason:        "needs confirmation",
		InputRequests: json.RawMessage(`{"approve":{"message":"proceed?"}}`),
		PluginState:   "pending-approval",
	}})
	later := &fakeHost{verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow}}
	h.install("plg_b", later)

	result, _ := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, pipeline.DecisionDefer, result.Decision)
	require.Equal(t, "pending-approval", result.PluginState)
	require.Contains(t, string(result.InputRequests), "approve")
	require.Zero(t, later.calls.Load(),
		"a later plugin must not run on a request that is not going to happen yet")
}

// ------------------------------------------------------------- annotations ---

func TestAnnotationsAreNamespacedByPlugin(t *testing.T) {
	h := newHarness(t, pipeline.Options{},
		manifest("plg_a", "alpha", withPriority(10)),
		manifest("plg_b", "beta", withPriority(20)),
	)
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
		Decision:    pipeline.DecisionAnnotate,
		Annotations: map[string]any{"score": 0.4},
	}})
	h.install("plg_b", &fakeHost{verdict: &pipeline.Verdict{
		Decision:    pipeline.DecisionAnnotate,
		Annotations: map[string]any{"score": 0.9},
	}})

	result, _ := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	// Namespacing means two plugins using the same key do not silently clobber
	// each other, which would make the audit trail quietly wrong.
	require.Equal(t, 0.4, result.Annotations["alpha.score"])
	require.Equal(t, 0.9, result.Annotations["beta.score"])
}

// ---------------------------------------------------------- invalid verdicts --

func TestInvalidVerdictIsTreatedAsAFailure(t *testing.T) {
	tests := []struct {
		name    string
		verdict *pipeline.Verdict
		wantErr string
	}{
		{
			name:    "unknown decision",
			verdict: &pipeline.Verdict{Decision: "maybe"},
			wantErr: "not a valid decision",
		},
		{
			name:    "mutate without a patch",
			verdict: &pipeline.Verdict{Decision: pipeline.DecisionMutate},
			wantErr: "carries no patch",
		},
		{
			name:    "defer without input requests",
			verdict: &pipeline.Verdict{Decision: pipeline.DecisionDefer},
			wantErr: "carries no input requests",
		},
		{
			name: "allow carrying a patch",
			verdict: &pipeline.Verdict{
				Decision: pipeline.DecisionAllow,
				Patch:    json.RawMessage(`[{"op":"remove","path":"/x"}]`),
			},
			wantErr: "silently ignored",
		},
		{
			name:    "confidence out of range",
			verdict: &pipeline.Verdict{Decision: pipeline.DecisionAllow, Confidence: 1.5},
			wantErr: "outside [0,1]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, pipeline.Options{}, manifest("plg_a", "buggy", withWrites("x")))
			h.install("plg_a", &fakeHost{verdict: tc.verdict})

			result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{"x":1}`)
			require.Equal(t, pipeline.DecisionAllow, result.Decision)
			require.Contains(t, trace.Hooks[0].Outcomes[0].Error, tc.wantErr)
		})
	}
}

// TestInvalidVerdictsTripTheBreaker: a plugin that always returns garbage should
// eventually be taken out of the path rather than invoked forever.
func TestInvalidVerdictsTripTheBreaker(t *testing.T) {
	h := newHarness(t, pipeline.Options{CircuitFailureThreshold: 2},
		manifest("plg_a", "buggy"))
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{Decision: "nonsense"}})

	h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, "open", h.engine.CircuitState("plg_a"))
}

func TestNilVerdictIsAnAllow(t *testing.T) {
	h := newHarness(t, pipeline.Options{}, manifest("plg_a", "quiet"))
	h.install("plg_a", &fakeHost{verdict: nil})

	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	require.Equal(t, pipeline.DecisionAllow, result.Decision)
	require.True(t, trace.Hooks[0].Outcomes[0].Ran)
}

// ------------------------------------------------------------------ engine ---

func TestNewRequiresAHostResolver(t *testing.T) {
	_, err := pipeline.New(pipeline.Options{})
	require.ErrorContains(t, err, "host resolver is required")
}

func TestTraceSummary(t *testing.T) {
	h := newHarness(t, pipeline.Options{}, manifest("plg_a", "denier"))
	h.install("plg_a", &fakeHost{verdict: &pipeline.Verdict{
		Decision: pipeline.DecisionDeny, Reason: "no",
	}})

	result, trace := h.run(t, snapshotpb.Hook_HOOK_ON_TOOL_CALL, `{}`)
	trace.Decision = result.Decision
	trace.DecidedBy = result.DecidedBy
	trace.Tool = "crm.lookup"

	summary := trace.Summary()
	require.Contains(t, summary, "deny")
	require.Contains(t, summary, "crm.lookup")
	require.Contains(t, summary, "denier")
	require.Contains(t, summary, "1 plugin(s) ran")
}
