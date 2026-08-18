// Copyright 2026 The MCPDoll Authors.

// Package pipeline is MCPDoll's hook engine: the thing that runs plugins at each
// of the seven extension points, under a budget, and combines their verdicts.
//
// The engine's contract with the rest of the system is deliberately narrow. It
// knows nothing about MCP — the edge translates — and nothing about how a plugin
// is executed; a [Host] is an interface. What it owns is the part that is easy to
// get subtly wrong: ordering, budgets, failure policy, rollout state, and the
// audit record of what actually happened.
package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// Decision is a plugin's verdict. Exactly these five.
//
// The set is closed for the same reason the hook set is: every additional
// decision is a new case every hook site must handle, a new thing a plugin
// author must understand, and a new way for two plugins to interact
// surprisingly. Adding a sixth requires an ADR.
type Decision string

const (
	// DecisionAllow proceeds unchanged. Also the value an abstaining plugin
	// returns — "I have no opinion" and "I approve" are deliberately the same
	// answer, because a plugin that cannot decide must not be able to block.
	DecisionAllow Decision = "allow"

	// DecisionDeny refuses the request.
	DecisionDeny Decision = "deny"

	// DecisionMutate proceeds with an RFC 6902 patch applied, scoped to the
	// plugin's declared writes.
	DecisionMutate Decision = "mutate"

	// DecisionAnnotate proceeds unchanged but attaches metadata for the audit
	// trail and the console.
	DecisionAnnotate Decision = "annotate"

	// DecisionDefer cannot decide without client input. Becomes an MRTR
	// `input_required` result; the retry re-enters the plugin with the response.
	DecisionDefer Decision = "defer"
)

// Valid reports whether d is one of the five.
func (d Decision) Valid() bool {
	switch d {
	case DecisionAllow, DecisionDeny, DecisionMutate, DecisionAnnotate, DecisionDefer:
		return true
	default:
		return false
	}
}

// Verdict is what a plugin returns.
type Verdict struct {
	Decision Decision `json:"decision"`

	// Reason is surfaced in the audit trail and, for a denial, in the
	// model-legible error the client receives. A denial without a reason is
	// nearly useless to whoever has to explain it later.
	Reason string `json:"reason,omitempty"`

	// Confidence in [0,1]. Below a plugin's own threshold the host treats the
	// verdict as abstaining, which stops a low-confidence guess silently
	// denying. Zero means "not applicable" rather than "no confidence" — a
	// deterministic plugin has no meaningful confidence to report.
	Confidence float64 `json:"confidence,omitempty"`

	// Patch is an RFC 6902 patch document, valid only for DecisionMutate.
	Patch json.RawMessage `json:"patch,omitempty"`

	// Annotations for the audit trail and the console.
	Annotations map[string]any `json:"annotations,omitempty"`

	// TTLMs is how long this verdict may be cached. Zero means do not cache.
	TTLMs int `json:"ttlMs,omitempty"`

	// InputRequests and PluginState carry a deferral. PluginState is the
	// plugin's own opaque value; the edge wraps it in a signed envelope before
	// it reaches the client, so a plugin author never has to think about
	// forgery.
	InputRequests json.RawMessage `json:"inputRequests,omitempty"`
	PluginState   string          `json:"pluginState,omitempty"`

	// Provenance the audit record must carry: for the guard, the pinned model,
	// prompt and policy versions. Without it an audit entry cannot answer "what
	// decided this, and would it decide the same today".
	Provenance map[string]string `json:"provenance,omitempty"`
}

// Validate checks a verdict's internal consistency.
//
// A plugin is a third-party artifact and its output is untrusted input. A
// verdict that says "mutate" with no patch, or carries a patch it is not
// entitled to apply, is a bug or an attack; either way the engine must not act
// on it.
func (v *Verdict) Validate(manifest *snapshotpb.PluginManifest) error {
	if !v.Decision.Valid() {
		return fmt.Errorf("pipeline: %q is not a valid decision", v.Decision)
	}
	if v.Confidence < 0 || v.Confidence > 1 {
		return fmt.Errorf("pipeline: confidence %v is outside [0,1]", v.Confidence)
	}

	switch v.Decision {
	case DecisionMutate:
		if len(v.Patch) == 0 {
			return fmt.Errorf("pipeline: a mutate verdict carries no patch")
		}
		if len(manifest.Writes) == 0 {
			return fmt.Errorf(
				"pipeline: plugin %q returned a mutation but declares no writes; "+
					"the manifest's write scopes are an enforced contract, not documentation",
				manifest.Name)
		}
	case DecisionDefer:
		if len(v.InputRequests) == 0 {
			return fmt.Errorf("pipeline: a defer verdict carries no input requests")
		}
	default:
		if len(v.Patch) > 0 {
			return fmt.Errorf(
				"pipeline: a %s verdict carries a patch, which would be silently ignored",
				v.Decision)
		}
	}
	return nil
}

// Outcome is what the engine did with a plugin at a hook, for the audit trail and
// the console's request-trace waterfall.
//
// Recorded for *every* plugin at every hook, including the ones that did not
// run. "This plugin was skipped because its circuit was open" is exactly the
// information someone needs at 3am, and it is unavailable after the fact if it
// was not written down at the time.
type Outcome struct {
	PluginID   string `json:"plugin_id"`
	PluginName string `json:"plugin_name"`
	Hook       string `json:"hook"`
	Rollout    string `json:"rollout"`

	// Ran reports whether the plugin was invoked at all.
	Ran bool `json:"ran"`
	// Skipped and SkipReason explain a plugin that did not run.
	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skip_reason,omitempty"`

	// Verdict is what the plugin said, if it ran.
	Verdict *Verdict `json:"verdict,omitempty"`

	// Enforced reports whether the verdict was acted on. A shadow-mode plugin
	// produces a verdict that is recorded and not enforced; the difference
	// between the two is the whole value of shadow mode.
	Enforced bool `json:"enforced"`

	// Diverged reports that a shadow verdict differs from what was actually
	// done. This is the signal an operator watches before promoting a plugin.
	Diverged bool `json:"diverged,omitempty"`

	DurationMS float64 `json:"duration_ms"`
	// BudgetMS is what the plugin was allowed, for spotting a plugin that is
	// consistently sailing close to its limit.
	BudgetMS float64 `json:"budget_ms"`

	// Error is set when the plugin failed rather than returning a verdict.
	Error string `json:"error,omitempty"`
	// FailureMode records how the engine treated a failure, which is per effect
	// class and therefore not obvious from the error alone.
	FailureMode string `json:"failure_mode,omitempty"`
}

// Skip reasons. Constants rather than free text so a dashboard can group them.
const (
	SkipCircuitOpen     = "circuit_open"
	SkipBudgetExalted   = "request_budget_exhausted"
	SkipHookBudget      = "hook_budget_exhausted"
	SkipNotInCanary     = "not_in_canary_sample"
	SkipHostUnavailable = "host_unavailable"
	SkipShortCircuited  = "earlier_plugin_decided"
)

// Trace is the complete record of one request's journey through the pipeline.
//
// This is what the console's waterfall renders and what the audit trail stores.
// It is assembled even when nothing interesting happened, because "no plugin
// touched this request" is itself the answer to a common question.
type Trace struct {
	RequestID string    `json:"request_id"`
	Audience  string    `json:"audience"`
	Principal string    `json:"principal"`
	Tool      string    `json:"tool,omitempty"`
	StartedAt time.Time `json:"started_at"`

	// Hooks in execution order.
	Hooks []HookTrace `json:"hooks"`

	// TotalBudgetMS and ConsumedMS show how much of the request's plugin budget
	// was used, which is the number that predicts a future timeout.
	TotalBudgetMS float64 `json:"total_budget_ms"`
	ConsumedMS    float64 `json:"consumed_ms"`

	// Decision is the pipeline's final answer.
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
	// DecidedBy names the plugin whose verdict won, or "" when nothing decided.
	DecidedBy string `json:"decided_by,omitempty"`
}

// HookTrace is one hook's slice of a request.
type HookTrace struct {
	Hook       string    `json:"hook"`
	Outcomes   []Outcome `json:"outcomes"`
	DurationMS float64   `json:"duration_ms"`
	BudgetMS   float64   `json:"budget_ms"`
	// Exhausted reports that the hook ran out of budget, so some plugins were
	// skipped.
	Exhausted bool `json:"exhausted,omitempty"`
}

// Summary renders a trace as a compact line for logs.
func (t *Trace) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", t.Decision, t.Tool)
	if t.DecidedBy != "" {
		fmt.Fprintf(&b, " (by %s)", t.DecidedBy)
	}
	var ran, skipped int
	for _, h := range t.Hooks {
		for _, o := range h.Outcomes {
			if o.Ran {
				ran++
			}
			if o.Skipped {
				skipped++
			}
		}
	}
	fmt.Fprintf(&b, " [%d plugin(s) ran, %d skipped, %.1f/%.0fms]",
		ran, skipped, t.ConsumedMS, t.TotalBudgetMS)
	return b.String()
}

// Divergences returns the shadow verdicts that differ from what was done. This is
// the console's shadow-diff view, and the thing to read before promoting a
// plugin out of shadow.
func (t *Trace) Divergences() []Outcome {
	var out []Outcome
	for _, h := range t.Hooks {
		for _, o := range h.Outcomes {
			if o.Diverged {
				out = append(out, o)
			}
		}
	}
	return out
}
