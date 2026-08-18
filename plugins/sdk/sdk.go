// Copyright 2026 The MCPDoll Authors.

// Package sdk is what a WASM plugin author imports.
//
// It hides the ABI entirely. An author writes one function —
//
//	func handle(inv *sdk.Invocation) *sdk.Verdict
//
// — registers it with [Handle], and never sees a pointer, a length, or a pinned
// buffer. Getting the ABI wrong is not a compile error and not a loud runtime
// failure; it is silent memory corruption, so it belongs in exactly one place
// that is tested once rather than in every plugin.
//
// Register the handler from `init()`, never from `main()` — see [Handle] for
// why that distinction is load-bearing rather than stylistic.
//
// Build a plugin with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
//
// The `c-shared` build mode produces a *reactor* module — one that initializes
// and then waits to be called — rather than a command module that runs `main`
// and exits. A command module cannot be invoked after it returns, which is why
// the mode matters.
//
// See docs/architecture/plugin-authoring.md for the full guide.
package sdk

import (
	"encoding/json"
)

// Invocation is what the host passes a plugin.
//
// Every field is restricted to the plugin's declared `reads` scopes before it
// gets here — a plugin that did not declare `principal` sees an empty one. The
// manifest is a contract enforced on both sides: what a plugin may read and what
// it may write.
type Invocation struct {
	// Hook is the extension point, e.g. "on_tool_result".
	Hook string `json:"hook"`

	// Shadow reports that this verdict will be recorded but not acted on.
	//
	// Worth honouring. A plugin that calls a model can skip the expensive path in
	// shadow mode; a plugin that is merely computing does not need to care.
	Shadow bool `json:"shadow"`

	// Audience and Principal identify who is asking.
	Audience  string    `json:"audience,omitempty"`
	Principal Principal `json:"principal,omitzero"`

	// Tool is the tool being called, for the call and result hooks.
	Tool *Tool `json:"tool,omitempty"`

	// Arguments are the call's arguments, at ON_TOOL_CALL.
	Arguments map[string]any `json:"arguments,omitempty"`

	// Result is the backend's result, at ON_TOOL_RESULT.
	Result *Result `json:"result,omitempty"`

	// Catalog is the tool list, at ON_CATALOG.
	Catalog []Tool `json:"catalog,omitempty"`

	// Config is the plugin's own configuration from the manifest.
	Config map[string]any `json:"config,omitempty"`

	// InputResponses carries a client's answers on an MRTR retry, so a plugin
	// that deferred can pick up where it left off.
	InputResponses map[string]any `json:"inputResponses,omitempty"`
	// PluginState is whatever this plugin returned with its deferral.
	PluginState string `json:"pluginState,omitempty"`
}

// Principal is who the request is on behalf of.
type Principal struct {
	Subject string            `json:"subject"`
	Groups  []string          `json:"groups,omitempty"`
	Claims  map[string]string `json:"claims,omitempty"`
}

// Tool is one tool as the plugin sees it.
type Tool struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// EffectClass is "read", "write" or "destructive".
	EffectClass string `json:"effect_class,omitempty"`
	Backend     string `json:"backend,omitempty"`
	Digest      string `json:"digest,omitempty"`
}

// Result is a tool result.
type Result struct {
	Content []Content `json:"content,omitempty"`
	IsError bool      `json:"isError,omitempty"`
}

// Content is one content block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Verdict is what a plugin returns. The five decisions are the whole vocabulary.
type Verdict struct {
	Decision      string            `json:"decision"`
	Reason        string            `json:"reason,omitempty"`
	Confidence    float64           `json:"confidence,omitempty"`
	Patch         json.RawMessage   `json:"patch,omitempty"`
	Annotations   map[string]any    `json:"annotations,omitempty"`
	TTLMs         int               `json:"ttlMs,omitempty"`
	InputRequests json.RawMessage   `json:"inputRequests,omitempty"`
	PluginState   string            `json:"pluginState,omitempty"`
	Provenance    map[string]string `json:"provenance,omitempty"`
}

// The five decisions.
const (
	Allow    = "allow"
	Deny     = "deny"
	Mutate   = "mutate"
	Annotate = "annotate"
	Defer    = "defer"
)

// AllowVerdict proceeds unchanged. Also how a plugin abstains: "no opinion" and
// "approved" are the same answer, so a plugin that cannot decide cannot block.
func AllowVerdict() *Verdict { return &Verdict{Decision: Allow} }

// DenyVerdict refuses, with a reason.
//
// The reason is not optional in practice: it reaches the model as the
// explanation for the refusal and reaches the audit trail as the record of why.
// A denial nobody can explain is a support ticket.
func DenyVerdict(reason string) *Verdict {
	return &Verdict{Decision: Deny, Reason: reason}
}

// MutateVerdict proceeds with an RFC 6902 patch. The patch must stay within the
// plugin's declared `writes`; the host rejects it otherwise.
func MutateVerdict(reason string, patch []PatchOp) *Verdict {
	raw, err := json.Marshal(patch)
	if err != nil {
		// A patch that will not marshal cannot be applied, and failing open here
		// is safer than emitting a malformed one the host would reject anyway.
		return &Verdict{Decision: Allow, Reason: "internal: patch could not be encoded"}
	}
	return &Verdict{Decision: Mutate, Reason: reason, Patch: raw}
}

// AnnotateVerdict proceeds unchanged, attaching metadata.
func AnnotateVerdict(annotations map[string]any) *Verdict {
	return &Verdict{Decision: Annotate, Annotations: annotations}
}

// PatchOp is one RFC 6902 operation.
type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Replace builds a replace operation.
func Replace(path string, value any) PatchOp {
	return PatchOp{Op: "replace", Path: path, Value: value}
}

// Add builds an add operation.
func Add(path string, value any) PatchOp {
	return PatchOp{Op: "add", Path: path, Value: value}
}

// Remove builds a remove operation.
func Remove(path string) PatchOp {
	return PatchOp{Op: "remove", Path: path}
}

// Handler is a plugin's one function.
type Handler func(*Invocation) *Verdict

// registered is the handler [Handle] installed.
var registered Handler

// Handle registers the plugin's handler.
//
// **Call it from `init()`, not from `main()`.**
//
// A reactor module — which is what `-buildmode=c-shared` produces, and what the
// host instantiates with `_initialize` — runs package initialization and then
// waits to be called. It never runs `main`. A plugin that registers in `main`
// therefore compiles, loads, passes its ABI check, and then allows every single
// request, because the handler was never installed.
//
// That failure is silent by construction: allowing is the correct behaviour for
// an abstaining plugin, so nothing looks wrong. The only symptom is a security
// control that quietly does nothing. Hence the emphasis, and hence the
// distinctive reason string [dispatch] returns when it finds no handler.
//
//	func init() { sdk.Handle(handle) }
//	func main() {}
func Handle(h Handler) { registered = h }

// dispatch is called by the generated ABI shim. It is exported for the shim's
// use and is not part of a plugin author's surface.
func dispatch(input []byte) []byte {
	if registered == nil {
		return mustMarshal(&Verdict{
			Decision: Allow,
			Reason:   "plugin registered no handler",
		})
	}

	var inv Invocation
	if err := json.Unmarshal(input, &inv); err != nil {
		// Malformed input is the host's bug, not the request's. Allowing is the
		// right failure: the engine's per-effect-class failure policy is what
		// decides whether a broken check should block traffic, and that decision
		// does not belong to the plugin.
		return mustMarshal(&Verdict{
			Decision: Allow,
			Reason:   "plugin could not decode its invocation: " + err.Error(),
		})
	}

	verdict := registered(&inv)
	if verdict == nil {
		verdict = AllowVerdict()
	}
	return mustMarshal(verdict)
}

func mustMarshal(v *Verdict) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		// Hand-built so this path cannot itself fail to marshal.
		return []byte(`{"decision":"allow","reason":"plugin could not encode its verdict"}`)
	}
	return raw
}

// ConfigString reads a string from the plugin's configuration.
func (inv *Invocation) ConfigString(key, fallback string) string {
	if v, ok := inv.Config[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// ConfigStrings reads a string list from the plugin's configuration.
func (inv *Invocation) ConfigStrings(key string) []string {
	raw, ok := inv.Config[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ConfigBool reads a boolean from the plugin's configuration.
func (inv *Invocation) ConfigBool(key string, fallback bool) bool {
	if v, ok := inv.Config[key].(bool); ok {
		return v
	}
	return fallback
}

// HasGroup reports whether the principal belongs to a group.
func (inv *Invocation) HasGroup(group string) bool {
	for _, g := range inv.Principal.Groups {
		if g == group {
			return true
		}
	}
	return false
}

// HasAnyGroup reports whether the principal belongs to any of the groups.
func (inv *Invocation) HasAnyGroup(groups ...string) bool {
	for _, g := range groups {
		if inv.HasGroup(g) {
			return true
		}
	}
	return false
}
