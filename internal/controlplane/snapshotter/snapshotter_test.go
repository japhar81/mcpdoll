// Copyright 2026 The MCPDoll Authors.

package snapshotter_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/fixtures"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/snapshotter"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
)

// These tests build snapshots from *live* fixture backends. That is the point: the
// snapshotter's job is to reconcile a declared registry against what backends
// actually publish, and a fake backend would publish whatever made the test pass.

type env struct {
	modern   *fixtures.Backend
	legacy   *fixtures.Backend
	signer   *snapshot.Signer
	verifier *snapshot.Verifier
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		modern: fixtures.NewModern(),
		legacy: fixtures.NewLegacy(),
	}
	e.modern.Start()
	e.legacy.Start()
	t.Cleanup(e.modern.Close)
	t.Cleanup(e.legacy.Close)

	pub, priv, err := snapshot.GenerateKey()
	require.NoError(t, err)
	e.signer, err = snapshot.NewSigner("test", priv)
	require.NoError(t, err)
	e.verifier, err = snapshot.NewVerifierFromKeys(map[string]ed25519.PublicKey{"test": pub})
	require.NoError(t, err)
	return e
}

// spec returns a registry document pointing at the live fixtures.
func (e *env) spec(t *testing.T, version int64, extra string) *registry.Spec {
	t.Helper()
	doc := fmt.Sprintf(`
org: org_test
version: %d
catalog:
  ttl: 5m
  degraded_ttl: 30s
namespaces:
  - id: ns_crm
    name: crm
    prefix: crm
    owner_idp_group: eng-crm
  - id: ns_hr
    name: hr
    prefix: hr
    owner_idp_group: eng-hr
servers:
  - id: srv_crm
    name: crm-prod
    namespace: ns_crm
    endpoint: %s
    default_effect_class: read
    tools:
      update_customer:
        effect_class: write
  - id: srv_hr
    name: hr-legacy
    namespace: ns_hr
    endpoint: %s
    default_effect_class: read
bundles:
  - id: bnd_all
    name: everything
    priority: 10
    entries:
      - namespace: ns_crm
      - namespace: ns_hr
audiences:
  - id: aud_agents
    slug: agents
    name: Agents
    bundles: [bnd_all]
%s`, version, e.modern.URL(), e.legacy.URL(), extra)

	spec, err := registry.Parse([]byte(doc))
	require.NoError(t, err)
	return spec
}

func (e *env) build(t *testing.T, spec *registry.Spec) *snapshotter.Result {
	t.Helper()
	result, err := snapshotter.Build(context.Background(), snapshotter.Options{
		Spec: spec, Signer: e.signer, DiscoverTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	return result
}

// TestBuildDiscoversLiveBackends is the core loop: declare, discover, resolve,
// sign.
func TestBuildDiscoversLiveBackends(t *testing.T) {
	e := newEnv(t)
	result := e.build(t, e.spec(t, 1, ""))

	require.Equal(t, int64(1), result.Snapshot.Version)
	require.NotEmpty(t, result.Snapshot.Id)
	require.NotEmpty(t, result.Snapshot.RegistryDigest,
		"the registry digest lets a snapshot be traced back to the document it came from")

	// Three CRM tools and two HR tools, all discovered live.
	require.Len(t, result.Snapshot.Tools, 5)
	names := qualifiedNames(result)
	require.Equal(t, []string{
		"crm.list_open_tickets", "crm.lookup_customer", "crm.update_customer",
		"hr.get_org_chart", "hr.lookup_employee",
	}, names)

	// The signature verifies, and the result is servable.
	snap, err := e.verifier.Verify(result.Signed)
	require.NoError(t, err)
	view, err := snapshot.Build(snap)
	require.NoError(t, err)
	require.Equal(t, []string{"agents"}, view.AudienceSlugs())
}

// TestBuildRecordsNegotiatedVersionPerBackend: a legacy backend negotiating down
// is normal, but it must be visible — some capabilities are unavailable below
// 2026-07-28 and an operator should not have to guess which backends are affected.
func TestBuildRecordsNegotiatedVersionPerBackend(t *testing.T) {
	e := newEnv(t)
	result := e.build(t, e.spec(t, 1, ""))

	byName := map[string]snapshotter.BackendReport{}
	for _, r := range result.Discovered {
		byName[r.ServerName] = r
	}
	require.Equal(t, "2026-07-28", byName["crm-prod"].NegotiatedVersion)
	require.Equal(t, "2025-11-25", byName["hr-legacy"].NegotiatedVersion)

	require.Contains(t, strings.Join(result.Warnings, "\n"), "hr-legacy",
		"a downgraded backend should be reported as a warning")
	require.Contains(t, strings.Join(result.Warnings, "\n"), "2025-11-25")
}

// TestBuildAppliesEffectClassOverrides: classification is declared in the
// registry, never inferred from a tool's name or description.
func TestBuildAppliesEffectClassOverrides(t *testing.T) {
	e := newEnv(t)
	result := e.build(t, e.spec(t, 1, ""))

	byName := map[string]string{}
	requiresKey := map[string]bool{}
	for _, tool := range result.Snapshot.Tools {
		byName[tool.QualifiedName] = registry.EffectClassName(tool.EffectClass)
		requiresKey[tool.QualifiedName] = tool.RequiresIdempotencyKey
	}

	require.Equal(t, "read", byName["crm.lookup_customer"], "the server default applies")
	require.Equal(t, "write", byName["crm.update_customer"], "the per-tool override applies")

	require.False(t, requiresKey["crm.lookup_customer"],
		"a read needs no idempotency key")
	require.True(t, requiresKey["crm.update_customer"],
		"a write requires an idempotency key, derived from the effect class at build time")
}

// TestBuildExcludesTools: a backend tool the organization has decided not to
// expose must not merely be unbundled — it must not be in the registry at all,
// so no bundle can accidentally include it later.
func TestBuildExcludesTools(t *testing.T) {
	e := newEnv(t)
	spec := e.spec(t, 1, "")
	spec.Servers[0].Tools["update_customer"] = registry.ToolSpec{Exclude: true}

	result := e.build(t, spec)
	require.NotContains(t, qualifiedNames(result), "crm.update_customer")
	require.Len(t, result.Snapshot.Tools, 4)

	var crm snapshotter.BackendReport
	for _, r := range result.Discovered {
		if r.ServerName == "crm-prod" {
			crm = r
		}
	}
	require.Equal(t, []string{"update_customer"}, crm.Excluded,
		"the exclusion should be reported, not silent")
}

// TestBuildCanonicalizesSchemas: the CRM backend's update_customer schema uses a
// `$ref` into `$defs`. The snapshot must carry the resolved form, because the
// digest is over meaning rather than layout.
func TestBuildCanonicalizesSchemas(t *testing.T) {
	e := newEnv(t)
	result := e.build(t, e.spec(t, 1, ""))

	var update string
	for _, tool := range result.Snapshot.Tools {
		if tool.QualifiedName == "crm.update_customer" {
			update = tool.InputSchemaJson
		}
	}
	require.NotEmpty(t, update, "the schema must be present, not dropped")
	require.NotContains(t, update, "$ref", "the reference should have been inlined")
	require.NotContains(t, update, "$defs", "the container should have been dropped")
	require.Contains(t, update, `"enum":["bronze","silver","gold"]`,
		"the referenced subschema should appear at the reference site")
	// Canonical form: keys sorted, no insignificant whitespace.
	require.True(t, strings.HasPrefix(update, `{"properties"`),
		"canonical output sorts keys, so `properties` precedes `required` and `type`: %s", update)
}

// TestBuildRejectsUnreachableBackendByDefault. Silently shipping a catalog with a
// backend's tools missing is the prompt-cache-invalidating change the grace
// window exists to prevent, so it has to be asked for explicitly.
func TestBuildRejectsUnreachableBackendByDefault(t *testing.T) {
	e := newEnv(t)
	spec := e.spec(t, 1, "")
	spec.Servers[1].Endpoint = "http://127.0.0.1:1" // nothing listens here

	_, err := snapshotter.Build(context.Background(), snapshotter.Options{
		Spec: spec, Signer: e.signer, DiscoverTimeout: 2 * time.Second,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "could not be discovered")
	require.ErrorContains(t, err, "--allow-unreachable",
		"the error should name the flag that overrides it")
	require.ErrorContains(t, err, "prompt caches",
		"and should say why the default is what it is")
}

func TestBuildAllowUnreachable(t *testing.T) {
	e := newEnv(t)
	spec := e.spec(t, 1, "")
	spec.Servers[1].Endpoint = "http://127.0.0.1:1"

	result, err := snapshotter.Build(context.Background(), snapshotter.Options{
		Spec: spec, Signer: e.signer, DiscoverTimeout: 2 * time.Second,
		AllowUnreachable: true,
	})
	require.NoError(t, err)

	// The CRM tools are present; the HR tools are not.
	names := qualifiedNames(result)
	require.Contains(t, names, "crm.lookup_customer")
	require.NotContains(t, names, "hr.lookup_employee")

	// But the server itself is still registered, so its identity and
	// configuration survive the outage.
	var found bool
	for _, s := range result.Snapshot.Servers {
		if s.Name == "hr-legacy" {
			found = true
		}
	}
	require.True(t, found, "an unreachable server stays registered")
	require.Contains(t, strings.Join(result.Warnings, "\n"), "unreachable")
}

// TestBuildRejectsQualifiedNameCollision: two backends in the same namespace can
// both publish `lookup_customer`. MCPDoll never auto-renames, because clients
// depend on the name — so the collision is a build failure that names both
// culprits.
func TestBuildRejectsQualifiedNameCollision(t *testing.T) {
	e := newEnv(t)
	second := fixtures.NewModern()
	second.Start()
	t.Cleanup(second.Close)

	spec := e.spec(t, 1, "")
	// A second server in the *same* namespace, publishing the same tool names.
	spec.Servers = append(spec.Servers, registry.ServerSpec{
		ID:                 "srv_crm_two",
		Name:               "crm-staging",
		Namespace:          "ns_crm",
		Endpoint:           second.URL(),
		DefaultEffectClass: "read",
	})

	_, err := snapshotter.Build(context.Background(), snapshotter.Options{
		Spec: spec, Signer: e.signer, DiscoverTimeout: 10 * time.Second,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "is published by both")
	require.ErrorContains(t, err, "crm-prod")
	require.ErrorContains(t, err, "crm-staging")
	require.ErrorContains(t, err, "never auto-renames",
		"the error should explain why it is not resolved automatically")
}

// TestBuildIsDeterministic: the same document plus the same backends must produce
// the same snapshot content. Without this, a snapshot cannot be rebuilt from
// source and compared, which is the audit step the registry digest exists for.
func TestBuildIsDeterministic(t *testing.T) {
	e := newEnv(t)
	first := e.build(t, e.spec(t, 1, ""))
	second := e.build(t, e.spec(t, 1, ""))

	require.Equal(t, first.Snapshot.RegistryDigest, second.Snapshot.RegistryDigest)
	require.Equal(t, qualifiedNames(first), qualifiedNames(second))

	firstDigests := map[string]string{}
	for _, tool := range first.Snapshot.Tools {
		firstDigests[tool.QualifiedName] = tool.Digest
	}
	for _, tool := range second.Snapshot.Tools {
		require.Equal(t, firstDigests[tool.QualifiedName], tool.Digest,
			"tool %q hashed differently across two builds", tool.QualifiedName)
	}
}

// TestBuildRejectsOverBudgetBundle: an over-budget bundle fails the build rather
// than shipping a catalog that will not fit a context window.
func TestBuildRejectsOverBudgetBundle(t *testing.T) {
	e := newEnv(t)
	spec := e.spec(t, 1, "")
	spec.Bundles[0].TokenBudget = 1

	_, err := snapshotter.Build(context.Background(), snapshotter.Options{
		Spec: spec, Signer: e.signer, DiscoverTimeout: 10 * time.Second,
	})
	require.ErrorContains(t, err, "token budget")
}

func TestBuildRequiresItsDependencies(t *testing.T) {
	e := newEnv(t)
	_, err := snapshotter.Build(context.Background(), snapshotter.Options{Signer: e.signer})
	require.ErrorContains(t, err, "registry spec is required")

	_, err = snapshotter.Build(context.Background(), snapshotter.Options{
		Spec: e.spec(t, 1, ""),
	})
	require.ErrorContains(t, err, "signer is required")
}

// TestBuildResolvesPolicies checks the registry's short enum names become the
// snapshot's protobuf constants, and that a group-conditioned hide reaches the
// view as identity-specific.
func TestBuildResolvesPolicies(t *testing.T) {
	e := newEnv(t)
	spec := e.spec(t, 1, `
policies:
  - id: pol_hide
    name: hide-writes
    priority: 5
    rules:
      - effect_classes: [write]
        required_idp_groups: [crm-admins]
        decision: hide
        reason: write tools are admin-only
        max_ttl: 1m
`)
	spec.Audiences[0].Policies = []string{"pol_hide"}

	result := e.build(t, spec)
	require.Len(t, result.Snapshot.Policies, 1)

	rule := result.Snapshot.Policies[0].Rules[0]
	require.Equal(t, []string{"EFFECT_CLASS_WRITE"}, rule.EffectClasses)
	require.Equal(t, "hide", registry.DecisionName(rule.Decision))
	require.EqualValues(t, 60_000, rule.MaxTtlMs)

	view, err := snapshot.Build(result.Snapshot)
	require.NoError(t, err)
	av := view.Audience("agents")
	require.Equal(t, 60_000, av.TTLMs, "the policy's max_ttl narrows the catalog TTL")
	require.True(t, av.IdentityFiltered,
		"a group-conditioned hide makes the catalog identity-specific even without the explicit flag")
	require.Equal(t, "private", av.CacheScope())
}

// TestBuildQualifiesBundleToolNames: the document names tools unqualified inside
// a namespace; the snapshot must carry qualified names.
func TestBuildQualifiesBundleToolNames(t *testing.T) {
	e := newEnv(t)
	spec := e.spec(t, 1, "")
	spec.Bundles[0].Entries[0].Tools = []string{"lookup_customer"}
	spec.Bundles[0].Entries[1].Exclude = []string{"get_org_chart"}

	result := e.build(t, spec)
	entry := result.Snapshot.Bundles[0].Entries[0]
	require.Equal(t, []string{"crm.lookup_customer"}, entry.QualifiedNames)
	require.Equal(t, []string{"hr.get_org_chart"},
		result.Snapshot.Bundles[0].Entries[1].ExcludeQualifiedNames)

	view, err := snapshot.Build(result.Snapshot)
	require.NoError(t, err)
	var served []string
	for _, tool := range view.Audience("agents").Tools {
		served = append(served, tool.Def.QualifiedName)
	}
	require.Equal(t, []string{"crm.lookup_customer", "hr.lookup_employee"}, served)
}

func qualifiedNames(r *snapshotter.Result) []string {
	out := make([]string, 0, len(r.Snapshot.Tools))
	for _, tool := range r.Snapshot.Tools {
		out = append(out, tool.QualifiedName)
	}
	// The snapshot's tool order follows the servers in document order, so sort
	// for a stable assertion.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
