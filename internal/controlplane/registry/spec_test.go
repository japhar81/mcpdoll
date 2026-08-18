// Copyright 2026 The MCPDoll Authors.

package registry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// minimalSpec is the smallest document that validates. Tests mutate a copy of
// this text rather than a struct, so they exercise the YAML decoder's strictness
// as well as the validator.
const minimalSpec = `
org: org_test
version: 1
catalog:
  ttl: 5m
  degraded_ttl: 30s
namespaces:
  - id: ns_crm
    name: crm
    prefix: crm
    owner_idp_group: eng-crm
servers:
  - id: srv_crm
    name: crm-prod
    namespace: ns_crm
    endpoint: http://localhost:9101
    default_effect_class: read
bundles:
  - id: bnd_all
    name: everything
    priority: 10
    entries:
      - namespace: ns_crm
audiences:
  - id: aud_agents
    slug: agents
    name: Agents
    bundles: [bnd_all]
`

func TestParseMinimal(t *testing.T) {
	spec, err := Parse([]byte(minimalSpec))
	require.NoError(t, err)
	require.Equal(t, "org_test", spec.Org)
	require.Equal(t, int64(1), spec.Version)
	require.Len(t, spec.Namespaces, 1)
	require.Len(t, spec.Servers, 1)
	require.Equal(t, "crm", spec.Namespaces[0].Prefix)
}

// TestParseRejectsUnknownKey: a typo in a registry document must fail, not be
// silently ignored. `serving_mod: strict` would otherwise leave the server in
// strict mode by luck rather than by intent.
func TestParseRejectsUnknownKey(t *testing.T) {
	bad := strings.Replace(minimalSpec, "    default_effect_class: read",
		"    default_effect_class: read\n    serving_mod: strict", 1)
	_, err := Parse([]byte(bad))
	require.ErrorContains(t, err, "serving_mod")
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name:    "no org",
			mutate:  func(s string) string { return strings.Replace(s, "org: org_test", "org: \"\"", 1) },
			wantErr: "org is required",
		},
		{
			name:    "version must increase from zero",
			mutate:  func(s string) string { return strings.Replace(s, "version: 1", "version: 0", 1) },
			wantErr: "version must be a positive integer",
		},
		{
			name: "degraded TTL cannot exceed the catalog TTL",
			mutate: func(s string) string {
				return strings.Replace(s, "degraded_ttl: 30s", "degraded_ttl: 10m", 1)
			},
			wantErr: "exceeds catalog.ttl",
		},
		{
			name:    "namespace without a prefix",
			mutate:  func(s string) string { return strings.Replace(s, "    prefix: crm", "    prefix: \"\"", 1) },
			wantErr: "has no prefix",
		},
		{
			name: "prefix containing a dot would make names ambiguous",
			mutate: func(s string) string {
				return strings.Replace(s, "    prefix: crm", "    prefix: a.b", 1)
			},
			wantErr: "ambiguous",
		},
		{
			name: "prefix over the length budget",
			mutate: func(s string) string {
				return strings.Replace(s, "    prefix: crm", "    prefix: averylongprefixindeed", 1)
			},
			wantErr: "over the",
		},
		{
			name: "prefix with an illegal character",
			mutate: func(s string) string {
				return strings.Replace(s, "    prefix: crm", "    prefix: CRM", 1)
			},
			wantErr: "lowercase",
		},
		{
			name: "server referencing an unknown namespace",
			mutate: func(s string) string {
				return strings.Replace(s, "    namespace: ns_crm", "    namespace: ns_gone", 1)
			},
			wantErr: "unknown namespace",
		},
		{
			name: "server without a default effect class",
			mutate: func(s string) string {
				return strings.Replace(s, "    default_effect_class: read", "", 1)
			},
			wantErr: "no default_effect_class",
		},
		{
			name: "unknown effect class",
			mutate: func(s string) string {
				return strings.Replace(s, "default_effect_class: read", "default_effect_class: readonly", 1)
			},
			wantErr: "effect class",
		},
		{
			name: "endpoint with an unsupported scheme",
			mutate: func(s string) string {
				return strings.Replace(s, "http://localhost:9101", "stdio:///usr/bin/server", 1)
			},
			wantErr: "only http and https",
		},
		{
			name: "endpoint that is not a URL",
			mutate: func(s string) string {
				return strings.Replace(s, "http://localhost:9101", "not a url at all", 1)
			},
			wantErr: "endpoint",
		},
		{
			name: "bundle referencing an unknown namespace",
			mutate: func(s string) string {
				return strings.Replace(s, "      - namespace: ns_crm", "      - namespace: ns_gone", 1)
			},
			wantErr: "unknown namespace",
		},
		{
			name: "bundle with no entries contributes nothing",
			mutate: func(s string) string {
				return strings.Replace(s, "    entries:\n      - namespace: ns_crm", "    entries: []", 1)
			},
			wantErr: "no entries",
		},
		{
			name: "bundle TTL trying to widen the catalog TTL",
			mutate: func(s string) string {
				return strings.Replace(s, "    priority: 10", "    priority: 10\n    ttl: 1h", 1)
			},
			wantErr: "may only narrow",
		},
		{
			name: "bundle entry naming a tool with a prefix",
			mutate: func(s string) string {
				return strings.Replace(s, "      - namespace: ns_crm",
					"      - namespace: ns_crm\n        tools: [crm.lookup]", 1)
			},
			wantErr: "unqualified name",
		},
		{
			name: "audience referencing an unknown bundle",
			mutate: func(s string) string {
				return strings.Replace(s, "    bundles: [bnd_all]", "    bundles: [bnd_gone]", 1)
			},
			wantErr: "unknown bundle",
		},
		{
			name: "audience with no bundles has an empty catalog",
			mutate: func(s string) string {
				return strings.Replace(s, "    bundles: [bnd_all]", "    bundles: []", 1)
			},
			wantErr: "no bundles",
		},
		{
			name: "audience without a slug has no endpoint",
			mutate: func(s string) string {
				return strings.Replace(s, "    slug: agents", "    slug: \"\"", 1)
			},
			wantErr: "no slug",
		},
		{
			name: "slug with a character that is illegal in a URL path",
			mutate: func(s string) string {
				return strings.Replace(s, "    slug: agents", "    slug: My Agents", 1)
			},
			wantErr: "appears in a URL path",
		},
		{
			name: "two namespaces sharing a prefix",
			mutate: func(s string) string {
				// Insert into the namespaces list rather than appending to the
				// document, which would land inside `audiences:`.
				return strings.Replace(s, "    owner_idp_group: eng-crm",
					"    owner_idp_group: eng-crm\n  - id: ns_other\n    name: other\n    prefix: crm", 1)
			},
			wantErr: "share the prefix",
		},
		{
			name: "duplicate namespace id",
			mutate: func(s string) string {
				return strings.Replace(s, "    owner_idp_group: eng-crm",
					"    owner_idp_group: eng-crm\n  - id: ns_crm\n    name: dup\n    prefix: dup", 1)
			},
			wantErr: "appears twice",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mutate(minimalSpec)))
			require.Error(t, err)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestValidateReportsEveryProblem: fixing a registry one error per run is a bad
// afternoon, and the errors are usually related.
func TestValidateReportsEveryProblem(t *testing.T) {
	bad := strings.Replace(minimalSpec, "org: org_test", "org: \"\"", 1)
	bad = strings.Replace(bad, "version: 1", "version: 0", 1)
	bad = strings.Replace(bad, "    prefix: crm", "    prefix: \"\"", 1)

	_, err := Parse([]byte(bad))
	require.Error(t, err)
	require.ErrorContains(t, err, "problem(s)")
	for _, want := range []string{"org is required", "version must be", "no prefix"} {
		require.ErrorContains(t, err, want)
	}
}

// ------------------------------------------------------------------ plugins ---

const pluginSpec = `
org: org_test
version: 1
catalog:
  ttl: 5m
  degraded_ttl: 30s
namespaces:
  - id: ns_crm
    name: crm
    prefix: crm
servers:
  - id: srv_crm
    name: crm-prod
    namespace: ns_crm
    endpoint: http://localhost:9101
    default_effect_class: read
bundles:
  - id: bnd_all
    name: everything
    priority: 10
    entries:
      - namespace: ns_crm
audiences:
  - id: aud_agents
    slug: agents
    bundles: [bnd_all]
plugins:
  - id: plg_redact
    name: redact
    version: 1.0.0
    runtime: wasm
    hooks: [on_tool_result]
    priority: 50
    budget: 20ms
    reads: [result.content]
    writes: [result.content]
    artifact_ref: file://plugins/redact.wasm
    artifact_digest: "sha256:abc"
    fuel_limit: 5000000
    failure_policy:
      read: open
      destructive: closed
`

func TestParsePlugin(t *testing.T) {
	spec, err := Parse([]byte(pluginSpec))
	require.NoError(t, err)
	require.Len(t, spec.Plugins, 1)

	p := spec.Plugins[0]
	require.Equal(t, "wasm", p.Runtime)
	require.Equal(t, []string{"on_tool_result"}, p.Hooks)
	// Rollout defaults to shadow: a plugin whose verdicts nobody has observed
	// must not be acting on live traffic.
	require.Empty(t, p.Rollout)
	rollout, err := ParseRollout(p.Rollout)
	require.NoError(t, err)
	require.Equal(t, snapshotpb.RolloutState_ROLLOUT_STATE_SHADOW, rollout)
}

func TestValidatePluginRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name:    "unknown runtime",
			mutate:  func(s string) string { return strings.Replace(s, "runtime: wasm", "runtime: python", 1) },
			wantErr: "runtime",
		},
		{
			name:    "unknown hook",
			mutate:  func(s string) string { return strings.Replace(s, "[on_tool_result]", "[on_everything]", 1) },
			wantErr: "seven pipeline hooks",
		},
		{
			name:    "no hooks means it would never run",
			mutate:  func(s string) string { return strings.Replace(s, "hooks: [on_tool_result]", "hooks: []", 1) },
			wantErr: "declares no hooks",
		},
		{
			name: "canary without a percentage",
			mutate: func(s string) string {
				return strings.Replace(s, "    priority: 50", "    priority: 50\n    rollout: canary", 1)
			},
			wantErr: "canary_percent",
		},
		{
			name: "canary percentage outside canary rollout",
			mutate: func(s string) string {
				return strings.Replace(s, "    priority: 50", "    priority: 50\n    canary_percent: 10", 1)
			},
			wantErr: "not in canary rollout",
		},
		{
			name: "wasm plugin without an artifact",
			mutate: func(s string) string {
				return strings.Replace(s, "    artifact_ref: file://plugins/redact.wasm\n", "", 1)
			},
			wantErr: "no artifact_ref",
		},
		{
			name: "artifact without a digest cannot fail closed on a swap",
			mutate: func(s string) string {
				return strings.Replace(s, "    artifact_digest: \"sha256:abc\"\n", "", 1)
			},
			wantErr: "no artifact_digest",
		},
		{
			name: "grpc plugin without an endpoint",
			mutate: func(s string) string {
				s = strings.Replace(s, "runtime: wasm", "runtime: grpc", 1)
				s = strings.Replace(s, "    artifact_ref: file://plugins/redact.wasm\n", "", 1)
				return strings.Replace(s, "    artifact_digest: \"sha256:abc\"\n", "", 1)
			},
			wantErr: "no endpoint",
		},
		{
			name: "unknown failure mode",
			mutate: func(s string) string {
				return strings.Replace(s, "      read: open", "      read: maybe", 1)
			},
			wantErr: "failure mode",
		},
		{
			name: "failure policy keyed on an unknown effect class",
			mutate: func(s string) string {
				return strings.Replace(s, "      read: open", "      readonly: open", 1)
			},
			wantErr: "effect class",
		},
		{
			name: "plugin scoped to an unknown audience",
			mutate: func(s string) string {
				return strings.Replace(s, "    priority: 50", "    priority: 50\n    audiences: [aud_gone]", 1)
			},
			wantErr: "unknown audience",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mutate(pluginSpec)))
			require.Error(t, err)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// ------------------------------------------------------------------- enums ----

func TestEnumParsersRejectEmpty(t *testing.T) {
	// Every enum that gates behaviour must reject "" rather than defaulting.
	// A silent default is how a destructive tool ends up classified as a read.
	_, err := ParseEffectClass("")
	require.Error(t, err, "an empty effect class must not default")
	_, err = ParseHook("")
	require.Error(t, err, "an empty hook must not default")
	_, err = ParseRuntime("")
	require.Error(t, err, "an empty runtime must not default")
	_, err = ParseFailureMode("")
	require.Error(t, err, "an empty failure mode must not default")
	_, err = ParseDecision("")
	require.Error(t, err, "an empty decision must not default")
}

// TestEnumParsersWithSafeDefaults documents the two exceptions, and that both
// default in the safe direction.
func TestEnumParsersWithSafeDefaults(t *testing.T) {
	mode, err := ParseServingMode("")
	require.NoError(t, err)
	require.Equal(t, snapshotpb.ServingMode_SERVING_MODE_STRICT, mode,
		"an unset serving mode must be strict: a divergent definition is never served")

	rollout, err := ParseRollout("")
	require.NoError(t, err)
	require.Equal(t, snapshotpb.RolloutState_ROLLOUT_STATE_SHADOW, rollout,
		"an unset rollout must be shadow: an unobserved plugin must not enforce")
}

func TestEnumRoundTrips(t *testing.T) {
	for _, name := range []string{"read", "write", "destructive"} {
		v, err := ParseEffectClass(name)
		require.NoError(t, err)
		require.Equal(t, name, EffectClassName(v))
	}
	for _, name := range HookNames() {
		v, err := ParseHook(name)
		require.NoError(t, err)
		require.Equal(t, name, HookName(v))
	}
	for _, name := range []string{"wasm", "grpc"} {
		v, err := ParseRuntime(name)
		require.NoError(t, err)
		require.Equal(t, name, RuntimeName(v))
	}
	for _, name := range []string{"shadow", "canary", "enforce"} {
		v, err := ParseRollout(name)
		require.NoError(t, err)
		require.Equal(t, name, RolloutName(v))
	}
	for _, name := range []string{"open", "closed"} {
		v, err := ParseFailureMode(name)
		require.NoError(t, err)
		require.Equal(t, name, FailureModeName(v))
	}
	for _, name := range []string{"allow", "deny", "hide", "confirm"} {
		v, err := ParseDecision(name)
		require.NoError(t, err)
		require.Equal(t, name, DecisionName(v))
	}
}

// TestHookNamesIsExactlySeven: the set is closed, and the ADR says so. A test
// that counts them makes an accidental eighth a failing build rather than a
// quiet expansion of the contract plugin authors reason about.
func TestHookNamesIsExactlySeven(t *testing.T) {
	names := HookNames()
	require.Len(t, names, 7)
	require.Equal(t, []string{
		"on_request", "on_identity", "on_catalog", "on_tool_call",
		"on_tool_result", "on_response", "on_audit",
	}, names, "hooks must be listed in execution order")

	// Every name must parse, and every parsed hook must be distinct.
	seen := map[snapshotpb.Hook]bool{}
	for _, name := range names {
		h, err := ParseHook(name)
		require.NoError(t, err)
		require.False(t, seen[h], "hook %q maps to an already-used enum value", name)
		seen[h] = true
	}
}

func TestParseEffectClassAcceptsProtobufConstant(t *testing.T) {
	// A rule copied out of a snapshot dump uses the protobuf spelling.
	v, err := ParseEffectClass("EFFECT_CLASS_DESTRUCTIVE")
	require.NoError(t, err)
	require.Equal(t, snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE, v)

	// But the unspecified value is never accepted.
	_, err = ParseEffectClass("EFFECT_CLASS_UNSPECIFIED")
	require.Error(t, err)
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(t.TempDir() + "/absent.yaml")
	require.ErrorContains(t, err, "reading")
}
