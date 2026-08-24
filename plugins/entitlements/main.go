// Copyright 2026 Henry Zektser.

// Command entitlements filters a catalog to the tools a principal is entitled
// to, and refuses calls to the ones it hid.
//
// It runs at two hooks, and running at both is the point. Filtering the catalog
// alone is cosmetic: a model that has seen a tool name once — in an earlier
// session, in a system prompt, in a colleague's transcript — can name it in a
// call, and a gateway that only hid it will happily dispatch. So ON_CATALOG
// shapes what is *offered* and ON_TOOL_CALL enforces what is *permitted*, from
// the same rules.
//
// Configuration (from the plugin manifest's `config`):
//
//	rules:            ordered list of {match, allow_groups, deny_groups, effect_classes}
//	default:          "allow" or "deny" when no rule matches (default "allow")
//	hide_denied:      true to omit denied tools from the catalog (default true)
//
// A rule's `match` is a glob over the qualified tool name: `bil.*`, `*_invoice`,
// or `*`.
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o entitlements.wasm .
package main

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/mcpdoll/mcpdoll/plugins/sdk"
)

// Registration happens in init, not main: a reactor module never runs main.
// See sdk.Handle.
func init() { sdk.Handle(handle) }

func main() {}

func handle(inv *sdk.Invocation) *sdk.Verdict {
	rules, defaultAllow, hideDenied, err := parseConfig(inv)
	if err != nil {
		// A misconfigured entitlement filter must not deny everything: that
		// would be an outage caused by a typo. The engine's failure policy is
		// what decides whether a broken check should block, per effect class.
		return &sdk.Verdict{
			Decision:    sdk.Allow,
			Reason:      "entitlements: " + err.Error(),
			Annotations: map[string]any{"config_error": err.Error()},
		}
	}

	switch inv.Hook {
	case "on_catalog":
		return filterCatalog(inv, rules, defaultAllow, hideDenied)
	case "on_tool_call":
		return enforceCall(inv, rules, defaultAllow)
	default:
		return sdk.AllowVerdict()
	}
}

// filterCatalog removes the tools this principal may not use.
func filterCatalog(
	inv *sdk.Invocation,
	rules []rule,
	defaultAllow, hideDenied bool,
) *sdk.Verdict {
	if len(inv.Catalog) == 0 {
		return sdk.AllowVerdict()
	}
	if !hideDenied {
		// The operator has chosen to show tools the principal cannot call. That
		// is a legitimate choice — a discoverable catalog with honest refusals —
		// so the call hook still enforces.
		return sdk.AllowVerdict()
	}

	// Walk backwards so each removal's index is still valid: an RFC 6902 patch
	// applies in order, and removing index 2 shifts everything after it.
	var ops []sdk.PatchOp
	var removed []string
	for i := len(inv.Catalog) - 1; i >= 0; i-- {
		tool := inv.Catalog[i]
		if allowed(tool.Name, tool.EffectClass, inv.Principal.Groups, rules, defaultAllow) {
			continue
		}
		ops = append(ops, sdk.Remove(fmt.Sprintf("/catalog/%d", i)))
		removed = append(removed, tool.Name)
	}

	if len(ops) == 0 {
		return sdk.AllowVerdict()
	}
	sort.Strings(removed)

	verdict := sdk.MutateVerdict(
		fmt.Sprintf("hid %d tool(s) this principal is not entitled to", len(removed)), ops)
	verdict.Annotations = map[string]any{
		"hidden":           removed,
		"principal_groups": inv.Principal.Groups,
	}
	return verdict
}

// enforceCall refuses a call the principal is not entitled to make.
func enforceCall(inv *sdk.Invocation, rules []rule, defaultAllow bool) *sdk.Verdict {
	if inv.Tool == nil {
		return sdk.AllowVerdict()
	}
	if allowed(inv.Tool.Name, inv.Tool.EffectClass, inv.Principal.Groups, rules, defaultAllow) {
		return sdk.AllowVerdict()
	}
	// The reason reaches the model, so it should say what would fix it without
	// enumerating the groups that would — which would be an information leak
	// about the organization's structure.
	return sdk.DenyVerdict(fmt.Sprintf(
		"%s requires an entitlement this principal does not hold", inv.Tool.Name))
}

// rule is one entitlement rule.
type rule struct {
	Match         string   `json:"match"`
	AllowGroups   []string `json:"allow_groups,omitempty"`
	DenyGroups    []string `json:"deny_groups,omitempty"`
	EffectClasses []string `json:"effect_classes,omitempty"`
}

// allowed evaluates the rules in order; the first match decides.
//
// First-match-wins rather than most-specific-wins: an ordered list is something
// an operator can read top to bottom and predict, whereas specificity scoring
// produces outcomes nobody expects from rules that each look correct.
func allowed(toolName, effectClass string, groups []string, rules []rule, defaultAllow bool) bool {
	for _, r := range rules {
		if !matches(r.Match, toolName) {
			continue
		}
		if len(r.EffectClasses) > 0 && !contains(r.EffectClasses, effectClass) {
			continue
		}
		// Deny wins within a matched rule: a rule that both allows and denies a
		// principal's groups is a misconfiguration, and denying is the safe
		// reading of an ambiguous rule.
		if len(r.DenyGroups) > 0 && intersects(r.DenyGroups, groups) {
			return false
		}
		if len(r.AllowGroups) > 0 {
			return intersects(r.AllowGroups, groups)
		}
		return true
	}
	return defaultAllow
}

// matches applies a glob to a qualified tool name.
func matches(pattern, name string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	// path.Match treats "/" specially and tool names use ".", so neither is a
	// separator here — which is what makes `bil.*` match `bil.void_invoice`.
	ok, err := path.Match(pattern, name)
	if err != nil {
		// A malformed pattern matches nothing rather than everything: a typo
		// should narrow a rule's reach, not widen it.
		return false
	}
	return ok
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if strings.EqualFold(v, needle) {
			return true
		}
	}
	return false
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func parseConfig(inv *sdk.Invocation) (rules []rule, defaultAllow, hideDenied bool, err error) {
	defaultAllow = true
	hideDenied = inv.ConfigBool("hide_denied", true)

	switch strings.ToLower(inv.ConfigString("default", "allow")) {
	case "allow":
		defaultAllow = true
	case "deny":
		defaultAllow = false
	default:
		return nil, false, false, fmt.Errorf(
			"default %q is not \"allow\" or \"deny\"", inv.ConfigString("default", ""))
	}

	raw, ok := inv.Config["rules"]
	if !ok {
		return nil, defaultAllow, hideDenied, nil
	}
	// The config arrives as generic JSON, so round-trip it into the typed form
	// rather than hand-walking maps.
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false, false, fmt.Errorf("rules could not be re-encoded: %w", err)
	}
	if err := json.Unmarshal(encoded, &rules); err != nil {
		return nil, false, false, fmt.Errorf("rules are not a list of rule objects: %w", err)
	}
	for i, r := range rules {
		if r.Match == "" {
			return nil, false, false, fmt.Errorf("rule %d has no match pattern", i)
		}
	}
	return rules, defaultAllow, hideDenied, nil
}
