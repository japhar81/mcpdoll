// Copyright 2026 Henry Zektser.

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
	Toolsets   []Toolset   `json:"toolsets" yaml:"toolsets"`
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
	ID                 string    `json:"id" yaml:"id"`
	Name               string    `json:"name" yaml:"name"`
	Namespace          string    `json:"namespace" yaml:"namespace"`
	Bindings           []Binding `json:"bindings" yaml:"bindings"`
	ServingMode        string    `json:"serving_mode" yaml:"serving_mode"`
	Criticality        string    `json:"criticality,omitempty" yaml:"criticality,omitempty"`
	DataClassification string    `json:"data_classification,omitempty" yaml:"data_classification,omitempty"`
	ComplianceScope    []string  `json:"compliance_scope,omitempty" yaml:"compliance_scope,omitempty"`
	DefaultEffectClass string    `json:"default_effect_class" yaml:"default_effect_class"`
	CanaryTool         string    `json:"canary_tool,omitempty" yaml:"canary_tool,omitempty"`

	// ToolOverrides names the tools whose classification the registry states
	// explicitly. Those are the ones a reviewer should look at: everything else
	// inherits default_effect_class.
	ToolOverrides map[string]string `json:"tool_overrides,omitempty" yaml:"tool_overrides,omitempty"`

	// ExcludedTools are withheld from every audience. Kept separate from
	// ToolOverrides because an exclusion is not an override with an empty
	// effect class, and rendering it as one reads as "unclassified".
	ExcludedTools []string `json:"excluded_tools,omitempty" yaml:"excluded_tools,omitempty"`
}

// Toolset is a named, grantable group of tools.
//
// Replaces Bundle: a bundle grouped namespaces for publication, whereas a
// toolset is the unit an administrator grants — and its name appears inside
// every grant scope. See ADR 0016.
type Toolset struct {
	ID          string   `json:"id" yaml:"id"`
	Name        string   `json:"name" yaml:"name"`
	Priority    int32    `json:"priority" yaml:"priority"`
	TokenBudget int32    `json:"token_budget,omitempty" yaml:"token_budget,omitempty"`
	Namespaces  []string `json:"namespaces" yaml:"namespaces"`
	Tools       []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	Exclude     []string `json:"exclude,omitempty" yaml:"exclude,omitempty"`
}

// Binding is one tenant's hosts for a backend.
type Binding struct {
	Tenant string `json:"tenant" yaml:"tenant"`
	// Primary is the definition source; replicas are compared against it.
	Primary  string   `json:"primary" yaml:"primary"`
	Replicas []string `json:"replicas,omitempty" yaml:"replicas,omitempty"`
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
	Toolsets   int    `json:"toolsets" yaml:"toolsets"`
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
		Toolsets:   []Toolset{},
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

	for _, ts := range spec.Toolsets {
		toolset := Toolset{
			ID: ts.ID, Name: ts.Name, Priority: ts.Priority,
			TokenBudget: ts.TokenBudget,
			Namespaces:  ts.Namespaces,
			Tools:       ts.Tools,
			Exclude:     ts.Exclude,
		}
		if toolset.Namespaces == nil {
			toolset.Namespaces = []string{}
		}
		out.Toolsets = append(out.Toolsets, toolset)
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
	bindings := make([]Binding, 0, len(srv.Bindings))
	for _, b := range srv.Bindings {
		bindings = append(bindings, Binding{
			Tenant: b.Tenant, Primary: b.Primary, Replicas: b.Replicas,
		})
	}

	out := Server{
		ID: srv.ID, Name: srv.Name, Namespace: srv.Namespace,
		Bindings: bindings, ServingMode: mode,
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
		Toolsets: len(spec.Toolsets),
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
	// Tenants the gateway is serving. Replaces the audience count: with one
	// endpoint and per-principal catalogs, "how many audiences" no longer
	// describes anything (ADR 0019).
	Tenants int `json:"tenants" yaml:"tenants"`
	// Tools admitted across every tenant.
	Tools int `json:"tools" yaml:"tools"`

	// RevocationsVersion and RevocationsAgeSeconds are the second signed
	// artifact's state. The age is the exposure window for a revoked
	// credential (ADR 0023), which is why it belongs on the status every
	// surface already reads rather than only in a metric.
	RevocationsVersion    int64   `json:"revocations_version" yaml:"revocations_version"`
	RevocationsAgeSeconds float64 `json:"revocations_age_seconds" yaml:"revocations_age_seconds"`
	RevokedPrincipals     int     `json:"revoked_principals" yaml:"revoked_principals"`
}

// TenantList is the listTenants response for the gateway.
type TenantList struct {
	GatewayStatus
	// Registered are the tenants the control plane's registry declares. They
	// are what the next snapshot will publish, which is not necessarily what
	// the gateway is publishing now.
	Registered []TenantSummary `json:"registered" yaml:"registered"`
}

// TenantSummary is one tenant, joined across the three places a tenant exists.
//
// A tenant is a record in the database, a set of bindings in the registry, and
// a slice of the serving snapshot, and those three drift apart in ways that are
// individually invisible. A tenant with users and no bindings has nobody to
// serve; a binding for a tenant nobody created has nothing to authenticate
// against. Reporting them together is what makes either mistake findable.
type TenantSummary struct {
	// ID is empty for a tenant that appears only in the registry.
	ID     string `json:"id,omitempty" yaml:"id,omitempty"`
	Slug   string `json:"slug" yaml:"slug"`
	Name   string `json:"name" yaml:"name"`
	Status string `json:"status" yaml:"status"`
	// Users in this tenant. Zero for a tenant that exists only as a binding.
	Users int `json:"users" yaml:"users"`
	// Backends bound to this tenant in the registry. Zero means no tool can
	// reach it, whatever grants its users hold.
	Backends int `json:"backends" yaml:"backends"`
	// Tools admitted for this tenant by the serving snapshot.
	Tools     int    `json:"tools" yaml:"tools"`
	CreatedAt string `json:"created_at,omitempty" yaml:"created_at,omitempty"`
}

// Tenant is one tenant record.
type Tenant struct {
	ID        string `json:"id" yaml:"id"`
	Slug      string `json:"slug" yaml:"slug"`
	Name      string `json:"name" yaml:"name"`
	Status    string `json:"status" yaml:"status"`
	CreatedAt string `json:"created_at" yaml:"created_at"`
}

// User is one identity inside a tenant.
type User struct {
	ID string `json:"id" yaml:"id"`
	// No tenant. A user is a person; which tenants they reach is what their
	// grants say, and which tenant an agent session resolves to is what the key
	// says.
	Email       string `json:"email" yaml:"email"`
	DisplayName string `json:"display_name,omitempty" yaml:"display_name,omitempty"`
	Status      string `json:"status" yaml:"status"`
	// HasPassword, never the hash. Whether local sign-in is possible is the
	// only thing any caller needs, and it is the only thing safe to say.
	HasPassword bool   `json:"has_password" yaml:"has_password"`
	CreatedAt   string `json:"created_at" yaml:"created_at"`
}

// UserList is a set of users.
//
// Tenant is set when the list was scoped to one — those are the users *granted
// into* it — and empty for the global listing.
type UserList struct {
	Tenant string `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	Users  []User `json:"users" yaml:"users"`
}

// Grant is one role held at one scope.
type Grant struct {
	Role  string `json:"role" yaml:"role"`
	Scope string `json:"scope" yaml:"scope"`
}

// GrantList is what a user holds directly.
//
// Directly: a key's effective grants are the intersection of what the key
// declares with this set, recomputed at every resolution, which is what makes
// suspending a user suspend every key they hold (ADR 0014).
type GrantList struct {
	UserID string  `json:"user_id" yaml:"user_id"`
	Grants []Grant `json:"grants" yaml:"grants"`
}

// APIKey is an agent credential's metadata. Never the secret.
type APIKey struct {
	ID     string `json:"id" yaml:"id"`
	UserID string `json:"user_id" yaml:"user_id"`
	// Tenant this key acts in. An MCP session resolves to exactly one, and this
	// is where that comes from.
	Tenant string `json:"tenant" yaml:"tenant"`
	Name   string `json:"name" yaml:"name"`
	// Prefix is the lookup half of the key. Public by construction: it is what
	// identifies the row before anything is verified.
	Prefix string `json:"prefix" yaml:"prefix"`
	// Declared is what the key asks for. Its effective grants are this
	// intersected with the owner's, so a key can narrow but never widen.
	Declared []Grant `json:"declared_grants" yaml:"declared_grants"`
	// Active is the resolved answer to "would this key authenticate right now",
	// which is not derivable from any single field below.
	Active     bool   `json:"active" yaml:"active"`
	CreatedAt  string `json:"created_at" yaml:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty" yaml:"last_used_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty" yaml:"expires_at,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty" yaml:"revoked_at,omitempty"`
}

// APIKeyList is every key one user holds, revoked ones included.
type APIKeyList struct {
	UserID string   `json:"user_id" yaml:"user_id"`
	Keys   []APIKey `json:"keys" yaml:"keys"`
}

// MintedAPIKey is a new key, the one time its secret is knowable.
type MintedAPIKey struct {
	Key APIKey `json:"key" yaml:"key"`
	// Secret is stored only as an Argon2id hash, so this response is the only
	// place it will ever appear. A caller that does not capture it has to mint
	// another key.
	Secret string `json:"secret" yaml:"secret"`
}

// Role is one role and everything it permits.
type Role struct {
	Name        string   `json:"name" yaml:"name"`
	Permissions []string `json:"permissions" yaml:"permissions"`
}

// RoleCatalog is the whole role model, plus every permission that exists.
//
// Permissions is the closed set, not just the ones some role happens to use:
// a UI that offered only the permissions already in play could never grant a
// new one, and the set being closed is the property that keeps it reviewable
// (ADR 0015).
type RoleCatalog struct {
	Roles       []Role   `json:"roles" yaml:"roles"`
	Permissions []string `json:"permissions" yaml:"permissions"`
}

// Session is a successful sign-in. Carries the token exactly once.
type Session struct {
	// Token is the credential. Stored only as a SHA-256 digest of CSPRNG
	// output, so this response is the only place it will ever appear.
	Token     string `json:"token" yaml:"token"`
	ExpiresAt string `json:"expires_at" yaml:"expires_at"`
	User      User   `json:"user" yaml:"user"`
	// Grants the user holds. Returned at sign-in so a console can render from
	// them immediately rather than making a second call to find out what to
	// show.
	Grants []Grant `json:"grants" yaml:"grants"`
}

// SessionInfo is who the caller is and what they may do.
type SessionInfo struct {
	// Kind is how they authenticated: session, api_key, or static.
	Kind    string  `json:"kind" yaml:"kind"`
	Subject string  `json:"subject" yaml:"subject"`
	Tenant  string  `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	UserID  string  `json:"user_id,omitempty" yaml:"user_id,omitempty"`
	Grants  []Grant `json:"grants" yaml:"grants"`
	// Permissions the caller holds at global scope. What a console renders
	// from: a button that 403s is worse than a button that is not there.
	//
	// Global scope only, deliberately. A tenant admin holds their permissions
	// at `t/acme` and this list is empty for them — the console must ask about
	// a specific scope for those, and a flattened union would claim more than
	// they have.
	Permissions []string `json:"permissions" yaml:"permissions"`
}

// RevocationReport is what the control plane published and what the gateway is
// applying.
//
// Both, because the gap between them is the exposure. ADR 0023 does not
// eliminate the leaked-credential window — failing closed on an unreachable
// list would let a control-plane outage stop tool calls — it bounds it, and
// this is where an operator sees the bound.
type RevocationReport struct {
	// Version the control plane last published.
	Version int64 `json:"version" yaml:"version"`
	// ServingVersion the gateway is applying. Behind Version means a revoked
	// credential is still working.
	ServingVersion    int64   `json:"serving_version" yaml:"serving_version"`
	ServingAgeSeconds float64 `json:"serving_age_seconds" yaml:"serving_age_seconds"`
	// InEffect is the one-line answer: has what was published reached the
	// gateway?
	InEffect bool `json:"in_effect" yaml:"in_effect"`
	// PrunedThrough is the snapshot version that already reflects everything
	// dropped from the list.
	PrunedThrough int64 `json:"pruned_through" yaml:"pruned_through"`
	// Path the list is written to. Empty means revocation waits for a snapshot.
	Path        string       `json:"path,omitempty" yaml:"path,omitempty"`
	Warning     string       `json:"warning,omitempty" yaml:"warning,omitempty"`
	Revocations []Revocation `json:"revocations" yaml:"revocations"`
}

// Revocation is one refused principal.
type Revocation struct {
	PrincipalID string `json:"principal_id" yaml:"principal_id"`
	Kind        string `json:"kind" yaml:"kind"`
	UserID      string `json:"user_id,omitempty" yaml:"user_id,omitempty"`
	Reason      string `json:"reason,omitempty" yaml:"reason,omitempty"`
	RevokedAt   string `json:"revoked_at" yaml:"revoked_at"`
}

// Catalog is the tool list one identity receives from one audience.
//
// This is the answer to "which tools can this agent call?" that cannot be
// wrong, because it is produced by making the same request the agent makes
// rather than by re-deriving policy.
type Catalog struct {
	// Tenant and Subject identify whose catalog this is. There is no audience:
	// the principal is the audience (ADR 0016).
	Tenant          string `json:"tenant" yaml:"tenant"`
	Subject         string `json:"subject,omitempty" yaml:"subject,omitempty"`
	ProtocolVersion string `json:"protocol_version" yaml:"protocol_version"`
	ServerName      string `json:"server_name" yaml:"server_name"`

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
	Tool    string `json:"tool" yaml:"tool"`
	IsError bool   `json:"is_error" yaml:"is_error"`
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

	Tenants []TenantSnapshotSummary `json:"tenants" yaml:"tenants"`
	Tools   []ToolSummary           `json:"tools,omitempty" yaml:"tools,omitempty"`
}

// TenantSnapshotSummary is one tenant's slice of a snapshot.
type TenantSnapshotSummary struct {
	Slug string `json:"slug" yaml:"slug"`
	Name string `json:"name" yaml:"name"`
	// Tools admitted for this tenant. What any given principal sees is a
	// subset, decided by their grants.
	Tools         int `json:"tools" yaml:"tools"`
	TokenEstimate int `json:"token_estimate" yaml:"token_estimate"`
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
	Toolsets   int    `json:"toolsets" yaml:"toolsets"`
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
	Source  string   `json:"source,omitempty" yaml:"source,omitempty"`
	Valid   bool     `json:"valid" yaml:"valid"`
	Version int64    `json:"version" yaml:"version"`
	KeyID   string   `json:"key_id" yaml:"key_id"`
	Tenants []string `json:"tenants" yaml:"tenants"`
	Tools   int      `json:"tools" yaml:"tools"`
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
