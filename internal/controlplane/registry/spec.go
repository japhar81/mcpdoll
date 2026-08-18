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
	Bundles    []BundleSpec    `yaml:"bundles"`
	Audiences  []AudienceSpec  `yaml:"audiences"`
	Policies   []PolicySpec    `yaml:"policies"`
	Plugins    []PluginSpec    `yaml:"plugins"`
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
	Endpoint  string `yaml:"endpoint"`

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
type BundleSpec struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Project  string `yaml:"project"`
	Priority int32  `yaml:"priority"`
	// TokenBudget caps the summed cost of the bundle's tools. Exceeding it fails
	// the snapshot build rather than shipping a catalog that will not fit a
	// context window.
	TokenBudget int32             `yaml:"token_budget"`
	TTL         time.Duration     `yaml:"ttl"`
	Entries     []BundleEntrySpec `yaml:"entries"`
}

// BundleEntrySpec includes a namespace, optionally narrowed to specific tools.
type BundleEntrySpec struct {
	Namespace string `yaml:"namespace"`
	// Tools, when set, restricts the entry to these unqualified tool names.
	// Unqualified because the author is already inside a namespace and repeating
	// the prefix is noise that can disagree with it.
	Tools []string `yaml:"tools"`
	// Exclude drops tools from the entry, applied after Tools.
	Exclude []string `yaml:"exclude"`
}

// AudienceSpec is one MCP endpoint.
type AudienceSpec struct {
	ID       string   `yaml:"id"`
	Slug     string   `yaml:"slug"`
	Name     string   `yaml:"name"`
	Project  string   `yaml:"project"`
	Bundles  []string `yaml:"bundles"`
	Policies []string `yaml:"policies"`
	// AllowedIdpGroups restricts who may use the endpoint. Empty means any
	// authenticated principal, which is only appropriate when the bundles are
	// themselves unrestricted.
	AllowedIdpGroups []string       `yaml:"allowed_idp_groups"`
	RateLimits       *RateLimitSpec `yaml:"rate_limits"`
}

// RateLimitSpec bounds one audience.
type RateLimitSpec struct {
	RequestsPerMinute  int32 `yaml:"requests_per_minute"`
	ConcurrentRequests int32 `yaml:"concurrent_requests"`
	TokensPerMinute    int64 `yaml:"tokens_per_minute"`
}

// PolicySpec is an authorization and shaping rule set.
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
	Audiences      []string       `yaml:"audiences"`
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
		if err := validateEndpoint(srv.Endpoint); err != nil {
			add("server %q: %v", srv.ID, err)
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

	bundles := map[string]BundleSpec{}
	for _, b := range s.Bundles {
		if b.ID == "" {
			add("a bundle has no id")
			continue
		}
		if _, dup := bundles[b.ID]; dup {
			add("bundle id %q appears twice", b.ID)
		}
		bundles[b.ID] = b
		if len(b.Entries) == 0 {
			add("bundle %q has no entries, so it would contribute nothing", b.ID)
		}
		if b.TTL > s.Catalog.TTL {
			add("bundle %q ttl (%s) exceeds catalog.ttl (%s); a bundle may only narrow the TTL",
				b.ID, b.TTL, s.Catalog.TTL)
		}
		for _, entry := range b.Entries {
			if _, ok := namespaces[entry.Namespace]; !ok {
				add("bundle %q references unknown namespace %q", b.ID, entry.Namespace)
			}
			for _, t := range entry.Tools {
				if strings.Contains(t, ".") {
					add("bundle %q entry names tool %q with a prefix; use the unqualified name, "+
						"since the entry already names the namespace", b.ID, t)
				}
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

	audiences := map[string]bool{}
	slugs := map[string]string{}
	for _, a := range s.Audiences {
		if a.ID == "" {
			add("an audience has no id")
			continue
		}
		if audiences[a.ID] {
			add("audience id %q appears twice", a.ID)
		}
		audiences[a.ID] = true
		if a.Slug == "" {
			add("audience %q has no slug, so it has no endpoint", a.ID)
		} else {
			if prior, dup := slugs[a.Slug]; dup {
				add("audiences %q and %q share the slug %q", prior, a.ID, a.Slug)
			}
			if err := validateSlug(a.Slug); err != nil {
				add("audience %q: %v", a.ID, err)
			}
			slugs[a.Slug] = a.ID
		}
		if len(a.Bundles) == 0 {
			add("audience %q has no bundles, so its catalog would always be empty", a.ID)
		}
		for _, bid := range a.Bundles {
			if _, ok := bundles[bid]; !ok {
				add("audience %q references unknown bundle %q", a.ID, bid)
			}
		}
		for _, pid := range a.Policies {
			if !policies[pid] {
				add("audience %q references unknown policy %q", a.ID, pid)
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
		for _, aid := range p.Audiences {
			if !audiences[aid] {
				add("plugin %q references unknown audience %q", p.ID, aid)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("registry: %d problem(s):\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}
	return nil
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
