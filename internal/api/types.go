// Copyright 2026 The MCPDoll Authors.

// Package api holds the wire types every MCPDoll surface speaks.
//
// It sits above the server rather than inside it, and that is the whole point:
// the HTTP server, the CLI, and the gateway inspector all marshal these structs.
// `mcpdoll registry show --output json` and `GET /api/v1/registry` return the
// same bytes because they are the same struct, not because two authors kept two
// definitions in agreement.
//
// The console's client is generated from api/openapi.yaml, which
// tools/paritycheck holds against this package. Three surfaces, one shape.
package api

import (
	"encoding/json"
	"sort"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
)

// Health is the liveness response.
type Health struct {
	Status  string `json:"status" yaml:"status"`
	Version string `json:"version" yaml:"version"`
	// RegistryPath is the document this control plane is serving, so an
	// operator staring at unexpected output can tell which file produced it.
	RegistryPath string `json:"registry_path,omitempty" yaml:"registry_path,omitempty"`
	SnapshotPath string `json:"snapshot_path,omitempty" yaml:"snapshot_path,omitempty"`
}

// HookList is the closed set of pipeline hooks, in execution order.
type HookList struct {
	Hooks []string `json:"hooks" yaml:"hooks"`
}

// Registry is a registry document rendered for display.
//
// It is deliberately not the on-disk YAML: defaults are resolved (an unset
// serving_mode reads as "strict" because that is what the engine will do), and
// rule bodies are summarised to counts. Showing an operator the raw document
// would make them apply the defaulting rules in their head.
type Registry struct {
	Org        string      `json:"org" yaml:"org"`
	Version    int64       `json:"version" yaml:"version"`
	Namespaces []Namespace `json:"namespaces" yaml:"namespaces"`
	Servers    []Server    `json:"servers" yaml:"servers"`
	Bundles    []Bundle    `json:"bundles" yaml:"bundles"`
	Audiences  []Audience  `json:"audiences" yaml:"audiences"`
	Policies   []Policy    `json:"policies,omitempty" yaml:"policies,omitempty"`
	Plugins    []Plugin    `json:"plugins,omitempty" yaml:"plugins,omitempty"`
}

// Namespace is one ownership boundary.
type Namespace struct {
	ID            string `json:"id" yaml:"id"`
	Name          string `json:"name" yaml:"name"`
	Prefix        string `json:"prefix" yaml:"prefix"`
	OwnerIdpGroup string `json:"owner_idp_group,omitempty" yaml:"owner_idp_group,omitempty"`
	Team          string `json:"team,omitempty" yaml:"team,omitempty"`
	Project       string `json:"project,omitempty" yaml:"project,omitempty"`
}

// Server is one registered backend.
type Server struct {
	ID                 string   `json:"id" yaml:"id"`
	Name               string   `json:"name" yaml:"name"`
	Namespace          string   `json:"namespace" yaml:"namespace"`
	Endpoint           string   `json:"endpoint" yaml:"endpoint"`
	ServingMode        string   `json:"serving_mode" yaml:"serving_mode"`
	Criticality        string   `json:"criticality,omitempty" yaml:"criticality,omitempty"`
	DataClassification string   `json:"data_classification,omitempty" yaml:"data_classification,omitempty"`
	ComplianceScope    []string `json:"compliance_scope,omitempty" yaml:"compliance_scope,omitempty"`
	DefaultEffectClass string   `json:"default_effect_class" yaml:"default_effect_class"`
	CanaryTool         string   `json:"canary_tool,omitempty" yaml:"canary_tool,omitempty"`

	// ToolOverrides names the tools whose classification the registry states
	// explicitly. Those are the ones a reviewer should look at: everything else
	// inherits default_effect_class.
	ToolOverrides map[string]string `json:"tool_overrides,omitempty" yaml:"tool_overrides,omitempty"`

	// ExcludedTools are withheld from every audience. Kept separate from
	// ToolOverrides because an exclusion is not an override with an empty
	// effect class, and rendering it as one reads as "unclassified".
	ExcludedTools []string `json:"excluded_tools,omitempty" yaml:"excluded_tools,omitempty"`
}

// Bundle is a named group of namespaces with a serving priority.
type Bundle struct {
	ID          string   `json:"id" yaml:"id"`
	Name        string   `json:"name" yaml:"name"`
	Priority    int32    `json:"priority" yaml:"priority"`
	TokenBudget int32    `json:"token_budget,omitempty" yaml:"token_budget,omitempty"`
	Namespaces  []string `json:"namespaces" yaml:"namespaces"`
}

// Audience is one published MCP endpoint.
type Audience struct {
	ID               string   `json:"id" yaml:"id"`
	Slug             string   `json:"slug" yaml:"slug"`
	Name             string   `json:"name,omitempty" yaml:"name,omitempty"`
	Bundles          []string `json:"bundles" yaml:"bundles"`
	Policies         []string `json:"policies,omitempty" yaml:"policies,omitempty"`
	AllowedIdpGroups []string `json:"allowed_idp_groups,omitempty" yaml:"allowed_idp_groups,omitempty"`
}

// Policy is a named rule set. Rule bodies are summarised: the console renders
// them from the registry document, not from this list.
type Policy struct {
	ID        string `json:"id" yaml:"id"`
	Name      string `json:"name" yaml:"name"`
	Priority  int32  `json:"priority" yaml:"priority"`
	RuleCount int    `json:"rule_count" yaml:"rule_count"`
}

// Plugin is one registered pipeline plugin.
type Plugin struct {
	ID      string   `json:"id" yaml:"id"`
	Name    string   `json:"name" yaml:"name"`
	Version string   `json:"version,omitempty" yaml:"version,omitempty"`
	Runtime string   `json:"runtime" yaml:"runtime"`
	Hooks   []string `json:"hooks" yaml:"hooks"`

	Priority int32 `json:"priority" yaml:"priority"`

	// Rollout is the field to read first. It is resolved, never empty: an
	// unset rollout means shadow, and reporting "" would make an inert plugin
	// indistinguishable from an enforcing one.
	Rollout       string `json:"rollout" yaml:"rollout"`
	CanaryPercent int32  `json:"canary_percent,omitempty" yaml:"canary_percent,omitempty"`

	Reads  []string `json:"reads,omitempty" yaml:"reads,omitempty"`
	Writes []string `json:"writes,omitempty" yaml:"writes,omitempty"`

	// IdentityDependent forces cacheScope: private at on_catalog. Surfacing it
	// explains a cache behaviour that is otherwise mystifying.
	IdentityDependent bool   `json:"identity_dependent,omitempty" yaml:"identity_dependent,omitempty"`
	ArtifactDigest    string `json:"artifact_digest,omitempty" yaml:"artifact_digest,omitempty"`
}

// ServerList is the listServers response.
type ServerList struct {
	Servers []Server `json:"servers" yaml:"servers"`
}

// PluginList is the listPlugins response.
type PluginList struct {
	Plugins []Plugin `json:"plugins" yaml:"plugins"`
}

// RegistrySummary is the validateRegistry response: counts, not contents.
type RegistrySummary struct {
	File       string `json:"file,omitempty" yaml:"file,omitempty"`
	Valid      bool   `json:"valid" yaml:"valid"`
	Org        string `json:"org" yaml:"org"`
	Version    int64  `json:"version" yaml:"version"`
	Namespaces int    `json:"namespaces" yaml:"namespaces"`
	Servers    int    `json:"servers" yaml:"servers"`
	Bundles    int    `json:"bundles" yaml:"bundles"`
	Audiences  int    `json:"audiences" yaml:"audiences"`
	Policies   int    `json:"policies" yaml:"policies"`
	Plugins    int    `json:"plugins" yaml:"plugins"`
}

// NewRegistry resolves a validated spec into its display form.
func NewRegistry(spec *registry.Spec) Registry {
	out := Registry{
		Org:     spec.Org,
		Version: spec.Version,
		// Non-nil slices: `[]` and `null` are different values to a TypeScript
		// client, and one of them makes `.map` throw.
		Namespaces: []Namespace{},
		Servers:    []Server{},
		Bundles:    []Bundle{},
		Audiences:  []Audience{},
	}

	for _, ns := range spec.Namespaces {
		out.Namespaces = append(out.Namespaces, Namespace{
			ID: ns.ID, Name: ns.Name, Prefix: ns.Prefix,
			OwnerIdpGroup: ns.OwnerIdpGroup, Team: ns.Team, Project: ns.Project,
		})
	}

	for _, srv := range spec.Servers {
		out.Servers = append(out.Servers, newServer(srv))
	}

	for _, b := range spec.Bundles {
		bundle := Bundle{
			ID: b.ID, Name: b.Name, Priority: b.Priority,
			TokenBudget: b.TokenBudget, Namespaces: []string{},
		}
		for _, entry := range b.Entries {
			bundle.Namespaces = append(bundle.Namespaces, entry.Namespace)
		}
		out.Bundles = append(out.Bundles, bundle)
	}

	for _, a := range spec.Audiences {
		bundles := a.Bundles
		if bundles == nil {
			bundles = []string{}
		}
		out.Audiences = append(out.Audiences, Audience{
			ID: a.ID, Slug: a.Slug, Name: a.Name,
			Bundles: bundles, Policies: a.Policies, AllowedIdpGroups: a.AllowedIdpGroups,
		})
	}

	for _, p := range spec.Policies {
		out.Policies = append(out.Policies, Policy{
			ID: p.ID, Name: p.Name, Priority: p.Priority, RuleCount: len(p.Rules),
		})
	}

	for _, p := range spec.Plugins {
		out.Plugins = append(out.Plugins, newPlugin(p))
	}

	return out
}

func newServer(srv registry.ServerSpec) Server {
	mode := srv.ServingMode
	if mode == "" {
		mode = registry.ServingModeStrict
	}
	out := Server{
		ID: srv.ID, Name: srv.Name, Namespace: srv.Namespace,
		Endpoint: srv.Endpoint, ServingMode: mode,
		Criticality: srv.Criticality, DataClassification: srv.DataClassification,
		ComplianceScope:    srv.ComplianceScope,
		DefaultEffectClass: srv.DefaultEffectClass,
		CanaryTool:         srv.CanaryTool,
	}
	for name, tool := range srv.Tools {
		if tool.Exclude {
			out.ExcludedTools = append(out.ExcludedTools, name)
			continue
		}
		if out.ToolOverrides == nil {
			out.ToolOverrides = map[string]string{}
		}
		out.ToolOverrides[name] = tool.EffectClass
	}
	// Map iteration order is randomised, so an unsorted list would make two
	// identical responses differ and defeat any caching or diffing downstream.
	sort.Strings(out.ExcludedTools)
	return out
}

func newPlugin(p registry.PluginSpec) Plugin {
	rollout := p.Rollout
	if rollout == "" {
		rollout = registry.RolloutShadow
	}
	return Plugin{
		ID: p.ID, Name: p.Name, Version: p.Version, Runtime: p.Runtime,
		Hooks: p.Hooks, Priority: p.Priority, Rollout: rollout,
		CanaryPercent: p.CanaryPercent, Reads: p.Reads, Writes: p.Writes,
		IdentityDependent: p.IdentityDependent, ArtifactDigest: p.ArtifactDigest,
	}
}

// NewRegistrySummary counts a validated spec.
func NewRegistrySummary(spec *registry.Spec) RegistrySummary {
	return RegistrySummary{
		Valid: true, Org: spec.Org, Version: spec.Version,
		Namespaces: len(spec.Namespaces), Servers: len(spec.Servers),
		Bundles: len(spec.Bundles), Audiences: len(spec.Audiences),
		Policies: len(spec.Policies), Plugins: len(spec.Plugins),
	}
}

// ---------------------------------------------------------------- gateway ----

// GatewayStatus is what a data plane reports about itself.
type GatewayStatus struct {
	GatewayURL string `json:"gateway_url" yaml:"gateway_url"`
	Status     string `json:"status" yaml:"status"`
	Ready      bool   `json:"ready" yaml:"ready"`
	// SnapshotVersion identifies what is being served. Two instances reporting
	// different versions is the signal that a rollout is mid-flight.
	SnapshotVersion int64 `json:"snapshot_version" yaml:"snapshot_version"`
	Audiences       int   `json:"audiences" yaml:"audiences"`
}

// AudienceList is the listAudiences response.
//
// It carries a count rather than names on purpose: the data plane does not
// enumerate its endpoints to an unauthenticated caller, and inventing names
// here from the registry would report what *should* be served rather than what
// is.
type AudienceList struct {
	GatewayStatus
	// Registered are the audiences the control plane's registry declares. They
	// are what the next snapshot will publish, which is not necessarily what
	// the gateway is publishing now.
	Registered []Audience `json:"registered" yaml:"registered"`
}

// Catalog is the tool list one identity receives from one audience.
//
// This is the answer to "which tools can this agent call?" that cannot be
// wrong, because it is produced by making the same request the agent makes
// rather than by re-deriving policy.
type Catalog struct {
	Audience        string   `json:"audience" yaml:"audience"`
	Subject         string   `json:"subject,omitempty" yaml:"subject,omitempty"`
	Groups          []string `json:"groups,omitempty" yaml:"groups,omitempty"`
	ProtocolVersion string   `json:"protocol_version" yaml:"protocol_version"`
	ServerName      string   `json:"server_name" yaml:"server_name"`

	// TTLMs and CacheScope are the catalog's cacheability, and belong next to
	// the tools rather than in a debug view: a filtered catalog that came back
	// `public` is a cross-tenant leak, and it is visible here.
	TTLMs      int    `json:"ttl_ms" yaml:"ttl_ms"`
	CacheScope string `json:"cache_scope" yaml:"cache_scope"`

	Tools []CatalogTool `json:"tools" yaml:"tools"`
}

// CatalogTool is one admitted tool as an identity sees it.
type CatalogTool struct {
	Name        string `json:"name" yaml:"name"`
	Namespace   string `json:"namespace" yaml:"namespace"`
	Title       string `json:"title,omitempty" yaml:"title,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// CallResult is the outcome of exercising one tool through the gateway.
type CallResult struct {
	Tool     string `json:"tool" yaml:"tool"`
	Audience string `json:"audience" yaml:"audience"`
	IsError  bool   `json:"is_error" yaml:"is_error"`
	// NeedsInput reports an MRTR deferral: the call did not fail, it is waiting
	// for a human.
	NeedsInput bool   `json:"needs_input" yaml:"needs_input"`
	DurationMS int64  `json:"duration_ms" yaml:"duration_ms"`
	Text       string `json:"text" yaml:"text"`

	// GatewayDetail is the gateway's own `_meta` annotation — which plugin
	// mutated the result, which hook denied it. Raw, because it is the
	// pipeline's shape rather than this API's.
	GatewayDetail json.RawMessage `json:"gateway_detail,omitempty" yaml:"gateway_detail,omitempty"`

	InputRequests []string `json:"input_requests,omitempty" yaml:"input_requests,omitempty"`
	// RequestState is the signed envelope to send back with the answers. It is
	// unforgeable, not secret: a client can decode it and see what it binds.
	RequestState string `json:"request_state,omitempty" yaml:"request_state,omitempty"`
}

// --------------------------------------------------------------- snapshots ---

// Snapshot is a signed snapshot rendered for display.
type Snapshot struct {
	// Source names where these bytes came from — a file path, or "uploaded"
	// when the caller supplied them. Two snapshots that differ only in origin
	// are otherwise indistinguishable, and that is exactly when it matters.
	Source         string `json:"source,omitempty" yaml:"source,omitempty"`
	Version        int64  `json:"version" yaml:"version"`
	SnapshotID     string `json:"snapshot_id" yaml:"snapshot_id"`
	Org            string `json:"org" yaml:"org"`
	BuiltAt        string `json:"built_at" yaml:"built_at"`
	Age            string `json:"age" yaml:"age"`
	KeyID          string `json:"key_id" yaml:"key_id"`
	Algorithm      string `json:"algorithm" yaml:"algorithm"`
	RegistryDigest string `json:"registry_digest" yaml:"registry_digest"`

	// Servable reports whether this snapshot would actually activate. A
	// correctly signed snapshot with a dangling reference is refused by every
	// data-plane instance, so signature validity alone is not the question
	// anybody wants answered.
	Servable bool `json:"servable" yaml:"servable"`
	// UnservableReason is why not, when Servable is false.
	UnservableReason string `json:"unservable_reason,omitempty" yaml:"unservable_reason,omitempty"`

	Audiences []AudienceSummary `json:"audiences" yaml:"audiences"`
	Tools     []ToolSummary     `json:"tools,omitempty" yaml:"tools,omitempty"`
}

// AudienceSummary is one audience's slice of a snapshot.
type AudienceSummary struct {
	Slug          string `json:"slug" yaml:"slug"`
	Name          string `json:"name" yaml:"name"`
	Tools         int    `json:"tools" yaml:"tools"`
	TTLMs         int    `json:"ttl_ms" yaml:"ttl_ms"`
	CacheScope    string `json:"cache_scope" yaml:"cache_scope"`
	TokenEstimate int    `json:"token_estimate" yaml:"token_estimate"`
}

// ToolSummary is one admitted tool.
type ToolSummary struct {
	QualifiedName string `json:"qualified_name" yaml:"qualified_name"`
	Backend       string `json:"backend" yaml:"backend"`
	EffectClass   string `json:"effect_class" yaml:"effect_class"`
	Tokens        int    `json:"token_estimate" yaml:"token_estimate"`
	Digest        string `json:"digest" yaml:"digest"`
}

// BuildReport is the outcome of resolving a registry into a snapshot.
type BuildReport struct {
	Version        int64  `json:"version" yaml:"version"`
	SnapshotID     string `json:"snapshot_id" yaml:"snapshot_id"`
	Org            string `json:"org" yaml:"org"`
	RegistryDigest string `json:"registry_digest" yaml:"registry_digest"`
	KeyID          string `json:"key_id" yaml:"key_id"`
	// PublicKey is the trust entry a data plane needs in order to accept what
	// was just built. Emitting it here saves the operator a second command at
	// the moment they need it.
	PublicKey  string `json:"public_key" yaml:"public_key"`
	Namespaces int    `json:"namespaces" yaml:"namespaces"`
	Servers    int    `json:"servers" yaml:"servers"`
	Tools      int    `json:"tools" yaml:"tools"`
	Bundles    int    `json:"bundles" yaml:"bundles"`
	Audiences  int    `json:"audiences" yaml:"audiences"`
	Plugins    int    `json:"plugins" yaml:"plugins"`

	Backends []BackendReport `json:"backends" yaml:"backends"`
	Warnings []string        `json:"warnings,omitempty" yaml:"warnings,omitempty"`
	Output   string          `json:"output,omitempty" yaml:"output,omitempty"`
	DryRun   bool            `json:"dry_run" yaml:"dry_run"`
}

// BackendReport is what discovery found at one backend.
//
// There is no "reachable" field, and no error field: a backend that could not
// be reached fails the build unless allow_unreachable was set, in which case it
// appears in Warnings. Carrying a per-backend error here would suggest a
// snapshot can be partly successful, and it cannot.
type BackendReport struct {
	ServerID   string `json:"server_id" yaml:"server_id"`
	ServerName string `json:"server_name" yaml:"server_name"`
	Endpoint   string `json:"endpoint" yaml:"endpoint"`
	// NegotiatedVersion is the MCP version this backend actually agreed to,
	// which is how a legacy backend becomes visible without anyone declaring it
	// legacy.
	NegotiatedVersion string `json:"negotiated_version,omitempty" yaml:"negotiated_version,omitempty"`
	ToolCount         int    `json:"tool_count" yaml:"tool_count"`
	// Admitted and Excluded name the tools rather than counting them: which
	// tool was dropped is the question, and a count cannot answer it.
	Admitted   []string `json:"admitted" yaml:"admitted"`
	Excluded   []string `json:"excluded,omitempty" yaml:"excluded,omitempty"`
	ObservedAt string   `json:"observed_at" yaml:"observed_at"`
}

// VerifyReport is a signature check's outcome.
type VerifyReport struct {
	Source    string   `json:"source,omitempty" yaml:"source,omitempty"`
	Valid     bool     `json:"valid" yaml:"valid"`
	Version   int64    `json:"version" yaml:"version"`
	KeyID     string   `json:"key_id" yaml:"key_id"`
	Audiences []string `json:"audiences" yaml:"audiences"`
	Tools     int      `json:"tools" yaml:"tools"`
}

// SigningKey is a freshly minted keypair's *public* half.
//
// The private key is never in this struct. `mcpdoll keys generate` writes it to
// a file with 0600; the API writes it to the server's key directory and returns
// this. Neither hands a signing key back over a network connection, because
// whoever holds it can publish configuration to every data-plane instance.
type SigningKey struct {
	KeyID     string `json:"key_id" yaml:"key_id"`
	Directory string `json:"directory" yaml:"directory"`
	PublicKey string `json:"public_key" yaml:"public_key"`
	// TrustEntry is the exact line to paste into a data plane's
	// trusted_signing_keys list.
	TrustEntry string `json:"trust_entry" yaml:"trust_entry"`
}

// ------------------------------------------------------- backend health -----

// BackendHealthReport is what the gateway's prober knows.
//
// These types mirror internal/dataplane/health rather than reusing it, because
// the wire shape is a contract and the prober's internals are not. The prober
// is free to add a field without that becoming a published API — and this
// package must not drag the data plane's dependencies into the CLI.
type BackendHealthReport struct {
	Summary  BackendHealthSummary `json:"summary" yaml:"summary"`
	Backends []BackendHealth      `json:"backends" yaml:"backends"`
}

// BackendHealthSummary counts backends by state.
type BackendHealthSummary struct {
	Total       int `json:"total" yaml:"total"`
	Healthy     int `json:"healthy" yaml:"healthy"`
	Degraded    int `json:"degraded" yaml:"degraded"`
	Unavailable int `json:"unavailable" yaml:"unavailable"`
	Drifted     int `json:"drifted" yaml:"drifted"`
	Unknown     int `json:"unknown" yaml:"unknown"`
	// BlockedTools is what a strict backend's drift actually costs. Always zero
	// in an all-advisory deployment, however far its backends have moved.
	BlockedTools int `json:"blocked_tools" yaml:"blocked_tools"`
}

// BackendHealth is one backend's observed condition.
type BackendHealth struct {
	ServerID   string `json:"server_id" yaml:"server_id"`
	ServerName string `json:"server_name" yaml:"server_name"`
	Endpoint   string `json:"endpoint" yaml:"endpoint"`
	// State is unknown, healthy, degraded, unavailable, or drifted.
	State string `json:"state" yaml:"state"`
	// ServingMode decides what the drift costs: strict refuses, advisory
	// serves and records.
	ServingMode string `json:"serving_mode" yaml:"serving_mode"`

	LastProbe   string `json:"last_probe" yaml:"last_probe"`
	LastSuccess string `json:"last_success,omitempty" yaml:"last_success,omitempty"`

	ConsecutiveFailures int   `json:"consecutive_failures" yaml:"consecutive_failures"`
	LatencyEWMAMs       int64 `json:"latency_ewma_ms" yaml:"latency_ewma_ms"`

	NegotiatedVersion string `json:"negotiated_version,omitempty" yaml:"negotiated_version,omitempty"`
	Error             string `json:"error,omitempty" yaml:"error,omitempty"`

	ToolsAdmitted int         `json:"tools_admitted" yaml:"tools_admitted"`
	ToolsObserved int         `json:"tools_observed" yaml:"tools_observed"`
	Drift         []ToolDrift `json:"drift,omitempty" yaml:"drift,omitempty"`
}

// ToolDrift is one tool's difference from what was admitted.
type ToolDrift struct {
	Name string `json:"name" yaml:"name"`
	// QualifiedName is absent for an added tool, which has none — assigning one
	// is admission's job.
	QualifiedName string `json:"qualified_name,omitempty" yaml:"qualified_name,omitempty"`
	// Kind is cosmetic, semantic, removed, or added.
	Kind string `json:"kind" yaml:"kind"`

	AdmittedDigest string `json:"admitted_digest,omitempty" yaml:"admitted_digest,omitempty"`
	ObservedDigest string `json:"observed_digest,omitempty" yaml:"observed_digest,omitempty"`
	Detail         string `json:"detail" yaml:"detail"`
}
