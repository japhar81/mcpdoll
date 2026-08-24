// Copyright 2026 Henry Zektser.

package registry

import (
	"fmt"
	"sort"
	"strings"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// The registry document uses short, lowercase names for enums
// (`effect_class: destructive`) while the snapshot uses protobuf's fully
// qualified constants. These parsers are the single crossing point.
//
// Every parser rejects the empty string rather than defaulting. A silent default
// on an enum that gates behaviour — an effect class, a failure mode, a rollout
// state — is how a destructive tool ends up classified as a read, or a plugin
// nobody has observed ends up enforcing on live traffic.

var effectClasses = map[string]snapshotpb.EffectClass{
	"read":        snapshotpb.EffectClass_EFFECT_CLASS_READ,
	"write":       snapshotpb.EffectClass_EFFECT_CLASS_WRITE,
	"destructive": snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE,
}

// ParseEffectClass maps a document name to the snapshot enum.
func ParseEffectClass(name string) (snapshotpb.EffectClass, error) {
	// Also accept the protobuf constant, so a policy rule copied out of a
	// snapshot dump still parses.
	if v, ok := effectClasses[strings.ToLower(strings.TrimSpace(name))]; ok {
		return v, nil
	}
	if v, ok := snapshotpb.EffectClass_value[strings.ToUpper(name)]; ok &&
		v != int32(snapshotpb.EffectClass_EFFECT_CLASS_UNSPECIFIED) {
		return snapshotpb.EffectClass(v), nil
	}
	return snapshotpb.EffectClass_EFFECT_CLASS_UNSPECIFIED,
		fmt.Errorf("effect class %q is not one of %s", name, keys(effectClasses))
}

// EffectClassName renders the enum back as a document name, for CLI output.
func EffectClassName(ec snapshotpb.EffectClass) string {
	for name, v := range effectClasses {
		if v == ec {
			return name
		}
	}
	return "unspecified"
}

// The two enums that default. Named because the default has to be applied
// again wherever a document is *displayed* — showing an operator an empty
// serving_mode when the engine will use strict is how a config review misses
// something.
const (
	// ServingModeStrict is what an unset serving_mode means.
	ServingModeStrict = "strict"
	// RolloutShadow is what an unset rollout means.
	RolloutShadow = "shadow"
)

var servingModes = map[string]snapshotpb.ServingMode{
	ServingModeStrict: snapshotpb.ServingMode_SERVING_MODE_STRICT,
	"advisory":        snapshotpb.ServingMode_SERVING_MODE_ADVISORY,
}

// parseServingMode defaults to strict when unset.
//
// This is the one enum with a default, and the default is the safe direction:
// strict means a divergent definition is never served. An operator who wants the
// looser behaviour has to ask for it.
func ParseServingMode(name string) (snapshotpb.ServingMode, error) {
	if strings.TrimSpace(name) == "" {
		return snapshotpb.ServingMode_SERVING_MODE_STRICT, nil
	}
	if v, ok := servingModes[strings.ToLower(name)]; ok {
		return v, nil
	}
	return snapshotpb.ServingMode_SERVING_MODE_UNSPECIFIED,
		fmt.Errorf("serving_mode %q is not one of %s", name, keys(servingModes))
}

var hooks = map[string]snapshotpb.Hook{
	"on_request":     snapshotpb.Hook_HOOK_ON_REQUEST,
	"on_identity":    snapshotpb.Hook_HOOK_ON_IDENTITY,
	"on_catalog":     snapshotpb.Hook_HOOK_ON_CATALOG,
	"on_tool_call":   snapshotpb.Hook_HOOK_ON_TOOL_CALL,
	"on_tool_result": snapshotpb.Hook_HOOK_ON_TOOL_RESULT,
	"on_response":    snapshotpb.Hook_HOOK_ON_RESPONSE,
	"on_audit":       snapshotpb.Hook_HOOK_ON_AUDIT,
}

// ParseHook maps a document hook name to the snapshot enum.
//
// The set is closed at seven. Adding an eighth requires an ADR
// (docs/adr/0007-seven-hooks.md), so an unrecognised name is a hard error rather
// than something to pass through.
func ParseHook(name string) (snapshotpb.Hook, error) {
	if v, ok := hooks[strings.ToLower(strings.TrimSpace(name))]; ok {
		return v, nil
	}
	return snapshotpb.Hook_HOOK_UNSPECIFIED,
		fmt.Errorf("hook %q is not one of the seven pipeline hooks %s", name, keys(hooks))
}

// HookName renders a hook as its document name.
func HookName(h snapshotpb.Hook) string {
	for name, v := range hooks {
		if v == h {
			return name
		}
	}
	return "unspecified"
}

// HookNames lists the seven hooks in execution order, which is the order a
// reader expects them in help text and in the console.
func HookNames() []string {
	return []string{
		"on_request", "on_identity", "on_catalog", "on_tool_call",
		"on_tool_result", "on_response", "on_audit",
	}
}

var runtimes = map[string]snapshotpb.PluginRuntime{
	"wasm": snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
	"grpc": snapshotpb.PluginRuntime_PLUGIN_RUNTIME_GRPC,
}

func ParseRuntime(name string) (snapshotpb.PluginRuntime, error) {
	if v, ok := runtimes[strings.ToLower(strings.TrimSpace(name))]; ok {
		return v, nil
	}
	return snapshotpb.PluginRuntime_PLUGIN_RUNTIME_UNSPECIFIED,
		fmt.Errorf("runtime %q is not one of %s", name, keys(runtimes))
}

// RuntimeName renders a runtime as its document name.
func RuntimeName(r snapshotpb.PluginRuntime) string {
	for name, v := range runtimes {
		if v == r {
			return name
		}
	}
	return "unspecified"
}

var rollouts = map[string]snapshotpb.RolloutState{
	RolloutShadow: snapshotpb.RolloutState_ROLLOUT_STATE_SHADOW,
	"canary":      snapshotpb.RolloutState_ROLLOUT_STATE_CANARY,
	"enforce":     snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE,
}

// parseRollout defaults to shadow when unset.
//
// Shadow is the only safe default: a plugin whose verdicts nobody has observed
// must not be acting on live traffic. Promoting to enforce is an explicit,
// reviewable edit.
func ParseRollout(name string) (snapshotpb.RolloutState, error) {
	if strings.TrimSpace(name) == "" {
		return snapshotpb.RolloutState_ROLLOUT_STATE_SHADOW, nil
	}
	if v, ok := rollouts[strings.ToLower(name)]; ok {
		return v, nil
	}
	return snapshotpb.RolloutState_ROLLOUT_STATE_UNSPECIFIED,
		fmt.Errorf("rollout %q is not one of %s", name, keys(rollouts))
}

// RolloutName renders a rollout state as its document name.
func RolloutName(r snapshotpb.RolloutState) string {
	for name, v := range rollouts {
		if v == r {
			return name
		}
	}
	return "unspecified"
}

var failureModes = map[string]snapshotpb.FailureMode{
	"open":   snapshotpb.FailureMode_FAILURE_MODE_OPEN,
	"closed": snapshotpb.FailureMode_FAILURE_MODE_CLOSED,
}

func ParseFailureMode(name string) (snapshotpb.FailureMode, error) {
	if v, ok := failureModes[strings.ToLower(strings.TrimSpace(name))]; ok {
		return v, nil
	}
	return snapshotpb.FailureMode_FAILURE_MODE_UNSPECIFIED,
		fmt.Errorf("failure mode %q is not one of %s", name, keys(failureModes))
}

// FailureModeName renders a failure mode as its document name.
func FailureModeName(m snapshotpb.FailureMode) string {
	for name, v := range failureModes {
		if v == m {
			return name
		}
	}
	return "unspecified"
}

var decisions = map[string]snapshotpb.PolicyDecision{
	"allow":   snapshotpb.PolicyDecision_POLICY_DECISION_ALLOW,
	"deny":    snapshotpb.PolicyDecision_POLICY_DECISION_DENY,
	"hide":    snapshotpb.PolicyDecision_POLICY_DECISION_HIDE,
	"confirm": snapshotpb.PolicyDecision_POLICY_DECISION_CONFIRM,
}

func ParseDecision(name string) (snapshotpb.PolicyDecision, error) {
	if v, ok := decisions[strings.ToLower(strings.TrimSpace(name))]; ok {
		return v, nil
	}
	return snapshotpb.PolicyDecision_POLICY_DECISION_UNSPECIFIED,
		fmt.Errorf("decision %q is not one of %s", name, keys(decisions))
}

// DecisionName renders a policy decision as its document name.
func DecisionName(d snapshotpb.PolicyDecision) string {
	for name, v := range decisions {
		if v == d {
			return name
		}
	}
	return "unspecified"
}

// keys renders a map's keys sorted, so an error message lists the valid options
// in a stable order rather than a random one.
func keys[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
