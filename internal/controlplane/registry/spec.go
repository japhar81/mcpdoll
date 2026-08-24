// Copyright 2026 The MCPDoll Authors.

// Package registry defines MCPDoll's declarative registry: the document that
// says which backends exist, how their tools are classified, and which audiences
// see what.
//
// The registry is expressed as YAML rather than only as database rows. That is a
// deliberate choice with three consequences worth stating:
//
//   - It is reviewable. A change to who can call a destructive tool arrives as a
//     diff, in a pull request, with an author and a reviewer.
//   - It is reproducible. The same document plus the same backends produces the
//     same snapshot, so a snapshot can be rebuilt from source rather than only
//     restored from a backup.
//   - It matches how platform teams already work, and it is the shape RAGdoll
//     uses for pipeline specs (ADR 0001).
//
// The control plane's database is the *editable* representation and this document
// is what it resolves to; today only the document exists (see docs/deferred.md).
package registry

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// MaxPrefixLength bounds a namespace prefix.
//
// The qualified name is `<prefix>.<tool>` and the whole thing has a 64-character
// budget (MCP bounds tool names, and a long name is also pure prompt cost paid on
// every request). A prefix beyond this leaves too little room for a descriptive
// tool name, so it is rejected at authoring time rather than discovered when a
// backend publishes a long name.
const MaxPrefixLength = 12

// MaxQualifiedNameLength is the total budget for `<prefix>.<tool>`.
const MaxQualifiedNameLength = 64

// Spec is a complete registry document.
type Spec struct {
	// Org owns everything in the document.
	Org string `yaml:"org"`

	// Version is the snapshot version this document produces. It must increase
	// on every publish: the data plane refuses a snapshot no newer than the one
	// it is serving, which is what stops a replayed older document rolling
	// policy backwards.
	Version int64 `yaml:"version"`

	Catalog    CatalogSpec     `yaml:"catalog"`
	Namespaces []NamespaceSpec `yaml:"namespaces"`
	Servers    []ServerSpec    `yaml:"servers"`
	// Toolsets replace bundles: a toolset is the unit an admin *grants*, which
	// is what its name should say. See ADR 0016.
	Toolsets []ToolsetSpec `yaml:"toolsets"`
	Policies []PolicySpec  `yaml:"policies"`
	Plugins  []PluginSpec  `yaml:"plugins"`
}

// CatalogSpec holds org-wide list-result defaults.
type CatalogSpec struct {
	// TTL is the ceiling on the ttlMs the edge advertises. A bundle or policy
	// may narrow it, never widen it.
	TTL time.Duration `yaml:"ttl"`
	// DegradedTTL is the shortened TTL used when a catalog is served from
	// last-known-good, so clients re-ask sooner.
	DegradedTTL time.Duration `yaml:"degraded_ttl"`
}

// NamespaceSpec binds a short prefix to an owning team.
type NamespaceSpec struct {
	ID     string `yaml:"id"`
	Name   string `yaml:"name"`
	Prefix string `yaml:"prefix"`
	// OwnerIdpGroup is the IdP group that may publish into this namespace.
	OwnerIdpGroup string `yaml:"owner_idp_group"`
	Team          string `yaml:"team"`
	Project       string `yaml:"project"`
}

// ServerSpec is one registered MCP backend.
type ServerSpec struct {
	ID        string `yaml:"id"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`

	// Bindings are this backend's hosts, one entry per tenant. The same
	// logical backend is a different deployment for each tenant — Acme's CRM
	// at acme.realapp.com, Globex's at globex.realapp.com — and they will
	// eventually run different versions. Each binding is discovered and
	// admitted separately. See ADR 0017.
	Bindings []BindingSpec `yaml:"bindings"`

	// ServingMode is "strict" (default) or "advisory". It controls what happens
	// to the *backend* on divergence, never what clients see — the gateway
	// always serves admitted definitions (ADR 0006).
	ServingMode string `yaml:"serving_mode"`

	// PinnedProtocolVersion forces a protocol version for this backend. Pinning
	// exists because a backend that silently upgrades can change result shapes
	// under a catalog that was already admitted.
	PinnedProtocolVersion string `yaml:"pinned_protocol_version"`

	Criticality        string   `yaml:"criticality"`
	DataClassification string   `yaml:"data_classification"`
	ComplianceScope    []string `yaml:"compliance_scope"`
	Team               string   `yaml:"team"`
	Project            string   `yaml:"project"`

	// CanaryTool lets the prober distinguish "the catalog is reachable" from
	// "invocations actually work".
	CanaryTool string `yaml:"canary_tool"`

	// Tools carries per-tool classification. A tool the backend publishes but
	// this map does not mention gets the DefaultEffectClass, which is why that
	// field has to be stated explicitly.
	Tools map[string]ToolSpec `yaml:"tools"`

	// DefaultEffectClass applies to tools not named in Tools. Required: silently
	// defaulting an unclassified tool to "read" would let a destructive tool be
	// treated as safe to retry and to run speculatively alongside a guard check.
	DefaultEffectClass string `yaml:"default_effect_class"`

	TokenExchange *TokenExchangeSpec `yaml:"token_exchange"`
	Health        *HealthSpec        `yaml:"health"`
}

// BindingSpec is one tenant's hosts for a backend.
type BindingSpec struct {
	// Tenant is the tenant slug this binding serves. It appears in every scope
	// derived from the tools admitted here.
	Tenant string `yaml:"tenant"`

	// Primary is the definition source. Discovery reads it; replicas are
	// compared against it. Naming one makes "which host is correct" a
	// configuration decision rather than a vote between two disagreeing hosts.
	Primary string `yaml:"primary"`

	// Replicas are interchangeable hosts serving the same tenant. A replica
	// whose semantic digest diverges from the primary leaves the routable pool
	// and is reported — the catalog does not change, so a rolling deploy costs
	// capacity rather than churning every client's prompt cache.
	Replicas []string `yaml:"replicas"`
}

// Endpoints returns every host in a binding, primary first.
func (b BindingSpec) Endpoints() []string {
	out := make([]string, 0, 1+len(b.Replicas))
	if b.Primary != "" {
		out = append(out, b.Primary)
	}
	return append(out, b.Replicas...)
}

// ToolSpec classifies one tool.
type ToolSpec struct {
	// EffectClass is "read", "write" or "destructive".
	EffectClass string `yaml:"effect_class"`
	// Exclude omits the tool from the registry entirely, so it can never be
	// bundled or called. Use this for a backend tool the organization has
	// decided not to expose.
	Exclude bool `yaml:"exclude"`
}

// TokenExchangeSpec configures RFC 8693 exchange for a backend. There is no
// passthrough option, by design.
type TokenExchangeSpec struct {
	TokenEndpoint       string            `yaml:"token_endpoint"`
	Audience            string            `yaml:"audience"`
	Scopes              []string          `yaml:"scopes"`
	ClientCredentialRef string            `yaml:"client_credential_ref"`
	Header              string            `yaml:"header"`
	ClaimHeaders        map[string]string `yaml:"claim_headers"`
}

// HealthSpec overrides the org health policy for one backend.
type HealthSpec struct {
	ProbeInterval      time.Duration `yaml:"probe_interval"`
	ProbeTimeout       time.Duration `yaml:"probe_timeout"`
	GraceWindow        time.Duration `yaml:"grace_window"`
	EjectAfterFailures int           `yaml:"eject_after_failures"`
}

// BundleSpec is a curated set of tools presented as one catalog.
// ToolsetSpec is a named, grantable group of tools.
//
// It replaces the old `bundle`, and the rename carries meaning: a bundle
// grouped namespaces for *publication*, whereas a toolset is what an
// administrator hands to a user. Its name appears inside every grant scope —
// `t/<tenant>/ts/<toolset>` — so it is part of the authorization surface.
type ToolsetSpec struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`

	// Priority orders a principal's catalog, as bundle priority used to
	// (ADR 0010). Ordering must stay deterministic per principal or every
	// client's prompt cache churns on republish.
	Priority int32 `yaml:"priority"`

	TokenBudget int32         `yaml:"token_budget"`
	TTL         time.Duration `yaml:"ttl"`

	// Namespaces contributes every admitted tool in each named namespace.
	Namespaces []string `yaml:"namespaces"`

	// Tools names individual tools by their backend name, qualified with the
	// namespace prefix. This is what makes a toolset a curated set rather than
	// a mirror of the namespace layout.
	Tools []string `yaml:"tools"`

	// Exclude removes specific tools that Namespaces would otherwise have
	// contributed.
	Exclude []string `yaml:"exclude"`
}

type PolicySpec struct {
	ID       string           `yaml:"id"`
	Name     string           `yaml:"name"`
	Priority int32            `yaml:"priority"`
	Rules    []PolicyRuleSpec `yaml:"rules"`
}

// PolicyRuleSpec is one rule. An empty match field matches anything.
type PolicyRuleSpec struct {
	EffectClasses       []string `yaml:"effect_classes"`
	QualifiedNameGlobs  []string `yaml:"qualified_name_globs"`
	Namespaces          []string `yaml:"namespaces"`
	RequiredIdpGroups   []string `yaml:"required_idp_groups"`
	DataClassifications []string `yaml:"data_classifications"`

	// Decision is "allow", "deny", "hide" or "confirm".
	Decision string        `yaml:"decision"`
	Reason   string        `yaml:"reason"`
	MaxTTL   time.Duration `yaml:"max_ttl"`
	// IdentitySpecific marks the rule as making the catalog principal-specific,
	// forcing cacheScope: private. The snapshot builder also infers this for a
	// group-conditioned hide or deny, so forgetting the flag is not fatal —
	// but stating it is clearer than relying on the inference.
	IdentitySpecific bool `yaml:"identity_specific"`
}

// PluginSpec is one pipeline plugin.
type PluginSpec struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	// Runtime is "wasm" or "grpc".
	Runtime string `yaml:"runtime"`
	// Hooks are the seven pipeline extension points, e.g. ["on_tool_result"].
	Hooks    []string      `yaml:"hooks"`
	Priority int32         `yaml:"priority"`
	Budget   time.Duration `yaml:"budget"`
	Reads    []string      `yaml:"reads"`
	Writes   []string      `yaml:"writes"`
	// FailurePolicy maps an effect class to "open" or "closed".
	FailurePolicy map[string]string `yaml:"failure_policy"`
	// IdentityDependent means the plugin's output depends on who is asking. At
	// ON_CATALOG this forces cacheScope: private.
	IdentityDependent bool `yaml:"identity_dependent"`
	// Rollout is "shadow" (default), "canary" or "enforce". Every plugin starts
	// in shadow: a plugin whose verdicts have never been observed should not be
	// acting on traffic.
	Rollout        string         `yaml:"rollout"`
	CanaryPercent  int32          `yaml:"canary_percent"`
	ArtifactRef    string         `yaml:"artifact_ref"`
	ArtifactDigest string         `yaml:"artifact_digest"`
	FuelLimit      uint64         `yaml:"fuel_limit"`
	Endpoint       string         `yaml:"endpoint"`
	Config         map[string]any `yaml:"config"`
	// Toolsets narrows a plugin to specific toolsets. Empty means every tool.
	// Replaces the old audience scoping (ADR 0016): a toolset is now the only
	// grouping a plugin could reasonably be limited to.
	Toolsets []string `yaml:"toolsets"`
}

// Load reads and validates a registry document.
func Load(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("registry: reading %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse validates a registry document from bytes.
func Parse(raw []byte) (*Spec, error) {
	var spec Spec
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	// A typo in a registry document must be an error, not a silently ignored
	// setting. `serving_mod: strict` would otherwise leave the backend in strict
	// mode by luck rather than by intent.
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("registry: parsing document: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// Validate checks the document's internal consistency.
//
// Every problem is reported at once. An operator fixing a registry one error per
// run is a bad afternoon, and the errors are usually related.
func (s *Spec) Validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if s.Org == "" {
		add("org is required")
	}
	if s.Version <= 0 {
		add("version must be a positive integer that increases on every publish (got %d)", s.Version)
	}
	if s.Catalog.TTL <= 0 {
		add("catalog.ttl must be positive")
	}
	if s.Catalog.DegradedTTL > s.Catalog.TTL {
		add("catalog.degraded_ttl (%s) exceeds catalog.ttl (%s): the degraded value must be shorter so clients re-ask sooner",
			s.Catalog.DegradedTTL, s.Catalog.TTL)
	}

	namespaces := map[string]NamespaceSpec{}
	prefixes := map[string]string{}
	for _, ns := range s.Namespaces {
		switch {
		case ns.ID == "":
			add("a namespace has no id")
			continue
		case ns.Prefix == "":
			add("namespace %q has no prefix", ns.ID)
		}
		if _, dup := namespaces[ns.ID]; dup {
			add("namespace id %q appears twice", ns.ID)
		}
		if prior, dup := prefixes[ns.Prefix]; dup {
			add("namespaces %q and %q share the prefix %q; every tool in them would collide",
				prior, ns.ID, ns.Prefix)
		}
		if err := validatePrefix(ns.Prefix); err != nil {
			add("namespace %q: %v", ns.ID, err)
		}
		namespaces[ns.ID] = ns
		prefixes[ns.Prefix] = ns.ID
	}

	servers := map[string]ServerSpec{}
	for _, srv := range s.Servers {
		if srv.ID == "" {
			add("a server has no id")
			continue
		}
		if _, dup := servers[srv.ID]; dup {
			add("server id %q appears twice", srv.ID)
		}
		servers[srv.ID] = srv

		if _, ok := namespaces[srv.Namespace]; !ok {
			add("server %q references unknown namespace %q", srv.ID, srv.Namespace)
		}
		if len(srv.Bindings) == 0 {
			add("server %q has no bindings; a backend with no tenant pointing at "+
				"it is registered and unreachable by anyone", srv.ID)
		}
		seenTenants := map[string]bool{}
		for _, binding := range srv.Bindings {
			switch {
			case binding.Tenant == "":
				add("server %q has a binding with no tenant", srv.ID)
			case seenTenants[binding.Tenant]:
				// Two bindings for one tenant means two answers to "which host
				// serves this tenant", and nothing here could choose.
				add("server %q has two bindings for tenant %q", srv.ID, binding.Tenant)
			default:
				seenTenants[binding.Tenant] = true
			}

			if binding.Primary == "" {
				add("server %q binding for tenant %q has no primary; the primary "+
					"is what definitions are admitted from", srv.ID, binding.Tenant)
			}
			for _, endpoint := range binding.Endpoints() {
				if err := validateEndpoint(endpoint); err != nil {
					add("server %q binding for tenant %q: %v", srv.ID, binding.Tenant, err)
				}
			}
			// A host listed twice would be probed twice and counted twice in
			// the pool, making capacity look larger than it is.
			seenHosts := map[string]bool{}
			for _, endpoint := range binding.Endpoints() {
				if seenHosts[endpoint] {
					add("server %q binding for tenant %q lists %s twice",
						srv.ID, binding.Tenant, endpoint)
				}
				seenHosts[endpoint] = true
			}
		}
		if _, err := ParseServingMode(srv.ServingMode); err != nil {
			add("server %q: %v", srv.ID, err)
		}
		if srv.DefaultEffectClass == "" {
			add("server %q has no default_effect_class; an unclassified tool must not silently become \"read\", "+
				"because that would make a destructive tool look safe to retry", srv.ID)
		} else if _, err := ParseEffectClass(srv.DefaultEffectClass); err != nil {
			add("server %q default_effect_class: %v", srv.ID, err)
		}
		for name, tool := range srv.Tools {
			if tool.Exclude {
				continue
			}
			if tool.EffectClass == "" {
				add("server %q tool %q has no effect_class", srv.ID, name)
				continue
			}
			if _, err := ParseEffectClass(tool.EffectClass); err != nil {
				add("server %q tool %q: %v", srv.ID, name, err)
			}
		}
		if srv.TokenExchange != nil {
			if srv.TokenExchange.TokenEndpoint == "" {
				add("server %q token_exchange needs a token_endpoint", srv.ID)
			}
			if srv.TokenExchange.ClientCredentialRef == "" {
				add("server %q token_exchange needs a client_credential_ref (a reference, never a secret)", srv.ID)
			}
		}
	}

	toolsets := map[string]ToolsetSpec{}
	for _, ts := range s.Toolsets {
		if ts.ID == "" {
			add("a toolset has no id")
			continue
		}
		if _, dup := toolsets[ts.ID]; dup {
			add("toolset id %q appears twice", ts.ID)
		}
		toolsets[ts.ID] = ts

		// The name goes inside every grant scope for this toolset —
		// `t/<tenant>/ts/<name>` — so a name containing a separator would
		// change what those scopes mean. That is a privilege boundary rather
		// than a naming preference.
		if err := validateToolsetName(ts.Name); err != nil {
			add("toolset %q: %v", ts.ID, err)
		}

		if len(ts.Namespaces) == 0 && len(ts.Tools) == 0 {
			add("toolset %q names no namespaces and no tools, so it would grant nothing", ts.ID)
		}
		if ts.TTL > s.Catalog.TTL {
			add("toolset %q ttl (%s) exceeds catalog.ttl (%s); a toolset may only narrow the TTL",
				ts.ID, ts.TTL, s.Catalog.TTL)
		}
		for _, ns := range ts.Namespaces {
			if _, ok := namespaces[ns]; !ok {
				add("toolset %q references unknown namespace %q", ts.ID, ns)
			}
		}
		// Individually named tools are qualified, because a toolset draws from
		// several namespaces and a bare name would be ambiguous between them.
		for _, t := range append(append([]string{}, ts.Tools...), ts.Exclude...) {
			if !strings.Contains(t, ".") {
				add("toolset %q names tool %q without a namespace prefix; a toolset "+
					"draws from several namespaces, so a bare name is ambiguous", ts.ID, t)
			}
		}
	}

	policies := map[string]bool{}
	for _, p := range s.Policies {
		if p.ID == "" {
			add("a policy has no id")
			continue
		}
		if policies[p.ID] {
			add("policy id %q appears twice", p.ID)
		}
		policies[p.ID] = true
		if len(p.Rules) == 0 {
			add("policy %q has no rules", p.ID)
		}
		for i, rule := range p.Rules {
			if _, err := ParseDecision(rule.Decision); err != nil {
				add("policy %q rule %d: %v", p.ID, i, err)
			}
			for _, ec := range rule.EffectClasses {
				if _, err := ParseEffectClass(ec); err != nil {
					add("policy %q rule %d: %v", p.ID, i, err)
				}
			}
			for _, nsID := range rule.Namespaces {
				if _, ok := namespaces[nsID]; !ok {
					add("policy %q rule %d references unknown namespace %q", p.ID, i, nsID)
				}
			}
			if rule.MaxTTL > s.Catalog.TTL {
				add("policy %q rule %d max_ttl (%s) exceeds catalog.ttl (%s); a policy may only narrow the TTL",
					p.ID, i, rule.MaxTTL, s.Catalog.TTL)
			}
		}
	}

	plugins := map[string]bool{}
	for _, p := range s.Plugins {
		if p.ID == "" {
			add("a plugin has no id")
			continue
		}
		if plugins[p.ID] {
			add("plugin id %q appears twice", p.ID)
		}
		plugins[p.ID] = true

		if _, err := ParseRuntime(p.Runtime); err != nil {
			add("plugin %q: %v", p.ID, err)
		}
		if len(p.Hooks) == 0 {
			add("plugin %q declares no hooks, so it would never run", p.ID)
		}
		for _, h := range p.Hooks {
			if _, err := ParseHook(h); err != nil {
				add("plugin %q: %v", p.ID, err)
			}
		}
		rollout, err := ParseRollout(p.Rollout)
		if err != nil {
			add("plugin %q: %v", p.ID, err)
		}
		if rollout == snapshotpb.RolloutState_ROLLOUT_STATE_CANARY &&
			(p.CanaryPercent <= 0 || p.CanaryPercent > 100) {
			add("plugin %q is in canary but canary_percent is %d; want 1..100",
				p.ID, p.CanaryPercent)
		}
		if rollout != snapshotpb.RolloutState_ROLLOUT_STATE_CANARY && p.CanaryPercent != 0 {
			add("plugin %q sets canary_percent but is not in canary rollout; the value would be ignored", p.ID)
		}
		for ec, mode := range p.FailurePolicy {
			if _, err := ParseEffectClass(ec); err != nil {
				add("plugin %q failure_policy: %v", p.ID, err)
			}
			if _, err := ParseFailureMode(mode); err != nil {
				add("plugin %q failure_policy[%s]: %v", p.ID, ec, err)
			}
		}
		if p.Runtime == "grpc" && p.Endpoint == "" {
			add("plugin %q is a grpc plugin but has no endpoint", p.ID)
		}
		if p.Runtime == "wasm" && p.ArtifactRef == "" {
			add("plugin %q is a wasm plugin but has no artifact_ref", p.ID)
		}
		if p.ArtifactRef != "" && p.ArtifactDigest == "" {
			add("plugin %q has an artifact_ref but no artifact_digest; the digest is what makes a swapped artifact fail closed", p.ID)
		}
		for _, tsID := range p.Toolsets {
			if _, ok := toolsets[tsID]; !ok {
				add("plugin %q references unknown toolset %q", p.ID, tsID)
			}
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Problems: errs}
	}
	return nil
}

// ValidationError carries every problem as a separate string.
//
// The formatted message is what a terminal shows; the slice is what an API
// returns and a console renders as a list. Keeping both means neither caller
// has to parse the other's format — and a validation message can be reworded
// without breaking a client that was splitting on "\n  - ".
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("registry: %d problem(s):\n  - %s",
		len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

func validatePrefix(prefix string) error {
	if len(prefix) > MaxPrefixLength {
		return fmt.Errorf("prefix %q is %d characters, over the %d-character limit; "+
			"the qualified name budget is %d and a long prefix leaves no room for a descriptive tool name",
			prefix, len(prefix), MaxPrefixLength, MaxQualifiedNameLength)
	}
	if strings.Contains(prefix, ".") {
		return fmt.Errorf("prefix %q contains a dot, which would make qualified names ambiguous", prefix)
	}
	for _, r := range prefix {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return fmt.Errorf("prefix %q contains %q; use lowercase letters, digits, underscore or hyphen",
				prefix, r)
		}
	}
	return nil
}

func validateSlug(slug string) error {
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return fmt.Errorf("slug %q contains %q; it appears in a URL path, so use lowercase letters, digits, hyphen or underscore",
				slug, r)
		}
	}
	return nil
}

func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a URL: %w", endpoint, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("endpoint %q has scheme %q; only http and https are supported "+
			"(wrap a stdio backend in a sidecar)", endpoint, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q has no host", endpoint)
	}
	return nil
}

// validateToolsetName enforces what a grant scope can safely contain.
//
// A toolset name appears inside `t/<tenant>/ts/<name>`, so a name containing a
// slash would change the meaning of every scope built from it — a toolset named
// `crm/lookup` makes `t/acme/ts/crm/lookup` indistinguishable from a grant on a
// single tool. That is a privilege boundary, not a formatting preference, and it
// is the same reason a tenant slug is constrained.
func validateToolsetName(name string) error {
	if name == "" {
		return fmt.Errorf("a toolset needs a name")
	}
	if len(name) > 63 {
		return fmt.Errorf("name %q is longer than 63 characters", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf(
				"name %q may contain only lowercase letters, digits, hyphen and "+
					"underscore — it becomes part of every grant scope", name)
		}
	}
	return nil
}
