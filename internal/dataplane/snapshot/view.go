// Copyright 2026 The MCPDoll Authors.

package snapshot

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// View is a verified snapshot, indexed for serving.
//
// Where the previous design precomputed one catalog per declared audience, this
// one indexes per tenant and composes a catalog per *principal* on demand
// (ADR 0016). There are as many possible catalogs as there are principals, so
// precomputing them all into the signed artifact would mean signing and
// verifying millions of tool references to save work the first connection does
// in microseconds — see ADR 0018.
type View struct {
	pb *snapshotpb.Snapshot

	Version  int64
	ID       string
	BuiltAt  time.Time
	LoadedAt time.Time

	namespaces map[string]*snapshotpb.Namespace
	servers    map[string]*snapshotpb.Server
	toolsets   map[string]*snapshotpb.Toolset
	policies   map[string]*snapshotpb.Policy
	plugins    map[string]*snapshotpb.PluginManifest

	tenants       map[string]*snapshotpb.Tenant
	tenantsBySlug map[string]*snapshotpb.Tenant

	// toolsByTenant holds each tenant's admitted tools, already in the stable
	// order a client will see them. Filtering a sorted slice preserves order,
	// so composing a principal's catalog never sorts.
	toolsByTenant map[string][]*Tool

	// toolsByDigest indexes by content digest, for audit and drift lookups.
	toolsByDigest map[string]*snapshotpb.ToolDefinition

	// The compiled RBAC this snapshot carries.
	catalog    authz.Catalog
	principals map[string]*snapshotpb.Principal

	// engine compiles a principal's grants into a decider. Held on the view so
	// a deployment using a different engine (ADR 0020) gets it everywhere.
	engine authz.Engine

	// mu guards the principal cache, which is populated lazily as principals
	// connect. The cache belongs to this view and dies with it: a swap drops
	// the whole map rather than invalidating entry by entry, because a stale
	// view surviving a swap would serve revoked access.
	mu    sync.RWMutex
	cache map[string]*PrincipalView
}

// PrincipalView is one principal's catalog, composed from its grants.
type PrincipalView struct {
	Principal *snapshotpb.Principal
	Tenant    *snapshotpb.Tenant

	// Tools in the stable total order the client sees. See [sortTools].
	Tools []*Tool

	// byQualifiedName resolves a tools/call target in one lookup.
	byQualifiedName map[string]*Tool

	// callable is the subset this principal may invoke, which is not always
	// the subset it may see: `tool:list` and `tool:call` are separate
	// permissions (ADR 0015).
	callable map[string]bool

	// Policies that apply, in priority order.
	Policies []*snapshotpb.Policy

	// pluginsByHook is pre-grouped and pre-sorted so the pipeline never sorts
	// on the hot path.
	pluginsByHook map[snapshotpb.Hook][]*snapshotpb.PluginManifest

	// TTLMs is the merged ceiling: the deployment default narrowed by every
	// contributing toolset and every applicable policy.
	TTLMs int

	// TokenEstimate is the summed cost of this catalog.
	TokenEstimate int
}

// Tool couples an admitted definition with the server that serves it, the
// namespace it belongs to, and the tenant it was admitted for — so the serving
// path needs no further joins.
type Tool struct {
	Def       *snapshotpb.ToolDefinition
	Server    *snapshotpb.Server
	Namespace *snapshotpb.Namespace
	Tenant    *snapshotpb.Tenant
	Toolset   *snapshotpb.Toolset

	// ToolsetPriority is the primary sort key, captured here so ordering does
	// not chase a pointer per comparison.
	ToolsetPriority int32
}

// QualifiedName is the name clients see.
func (t *Tool) QualifiedName() string { return t.Def.QualifiedName }

// EffectClass of the tool.
func (t *Tool) EffectClass() snapshotpb.EffectClass { return t.Def.EffectClass }

// Scope is the authorization scope a grant must cover to reach this tool.
func (t *Tool) Scope() string {
	return authz.ToolScope(t.Tenant.Slug, t.Toolset.Name, t.Def.Name)
}

// Build indexes a verified snapshot into a serving view.
//
// Every referential problem is a build error. The data plane would rather keep
// serving its previous snapshot than activate one with a dangling reference: a
// half-valid snapshot produces confusing partial outages that are much harder
// to diagnose than a refused publish.
func Build(snap *snapshotpb.Snapshot) (*View, error) {
	return BuildWithEngine(snap, authz.BuiltinEngine{})
}

// BuildWithEngine is [Build] with an explicit authorization engine.
func BuildWithEngine(snap *snapshotpb.Snapshot, engine authz.Engine) (*View, error) {
	if snap == nil {
		return nil, fmt.Errorf("snapshot: nil snapshot")
	}
	if engine == nil {
		return nil, fmt.Errorf("snapshot: nil authorization engine")
	}

	v := &View{
		pb:            snap,
		Version:       snap.Version,
		ID:            snap.Id,
		LoadedAt:      time.Now(),
		namespaces:    map[string]*snapshotpb.Namespace{},
		servers:       map[string]*snapshotpb.Server{},
		toolsets:      map[string]*snapshotpb.Toolset{},
		policies:      map[string]*snapshotpb.Policy{},
		plugins:       map[string]*snapshotpb.PluginManifest{},
		tenants:       map[string]*snapshotpb.Tenant{},
		tenantsBySlug: map[string]*snapshotpb.Tenant{},
		toolsByTenant: map[string][]*Tool{},
		toolsByDigest: map[string]*snapshotpb.ToolDefinition{},
		principals:    map[string]*snapshotpb.Principal{},
		cache:         map[string]*PrincipalView{},
		engine:        engine,
	}
	if snap.BuiltAt != nil {
		v.BuiltAt = snap.BuiltAt.AsTime()
	}
	if snap.Version <= 0 {
		// The store refuses anything no newer than what it serves, and version
		// 0 would compare as older than everything — so it could never
		// activate, and a build that produced one would fail silently.
		return nil, fmt.Errorf("snapshot: version must be positive, got %d", snap.Version)
	}

	for _, t := range snap.Tenants {
		if t.Id == "" || t.Slug == "" {
			return nil, fmt.Errorf("snapshot: a tenant has no id or slug")
		}
		if _, dup := v.tenants[t.Id]; dup {
			return nil, fmt.Errorf("snapshot: tenant id %q appears twice", t.Id)
		}
		if _, dup := v.tenantsBySlug[t.Slug]; dup {
			// Slugs are how scopes name tenants, so two tenants sharing one
			// would make every grant on it ambiguous.
			return nil, fmt.Errorf("snapshot: tenant slug %q appears twice", t.Slug)
		}
		v.tenants[t.Id] = t
		v.tenantsBySlug[t.Slug] = t
	}

	prefixes := map[string]string{}
	for _, ns := range snap.Namespaces {
		if _, dup := v.namespaces[ns.Id]; dup {
			return nil, fmt.Errorf("snapshot: namespace id %q appears twice", ns.Id)
		}
		if prior, dup := prefixes[ns.Prefix]; dup {
			// Every tool in both namespaces would produce the same qualified
			// name, so the collision is total rather than occasional.
			return nil, fmt.Errorf(
				"snapshot: namespaces %q and %q share the prefix %q; every tool in "+
					"them would collide", prior, ns.Id, ns.Prefix)
		}
		prefixes[ns.Prefix] = ns.Id
		v.namespaces[ns.Id] = ns
	}
	for _, ts := range snap.Toolsets {
		if _, dup := v.toolsets[ts.Id]; dup {
			return nil, fmt.Errorf("snapshot: toolset id %q appears twice", ts.Id)
		}
		v.toolsets[ts.Id] = ts
	}
	for _, p := range snap.Policies {
		v.policies[p.Id] = p
	}
	for _, srv := range snap.Servers {
		if _, dup := v.servers[srv.Id]; dup {
			return nil, fmt.Errorf("snapshot: server id %q appears twice", srv.Id)
		}
		if _, ok := v.namespaces[srv.NamespaceId]; !ok {
			return nil, fmt.Errorf("snapshot: server %q references unknown namespace %q",
				srv.Id, srv.NamespaceId)
		}
		for _, b := range srv.Bindings {
			if _, ok := v.tenants[b.TenantId]; !ok {
				return nil, fmt.Errorf(
					"snapshot: server %q has a binding for unknown tenant %q",
					srv.Id, b.TenantId)
			}
		}
		v.servers[srv.Id] = srv
	}
	for _, p := range snap.Plugins {
		v.plugins[p.Id] = p
	}
	if err := checkMutatingPluginConflicts(snap.Plugins); err != nil {
		return nil, err
	}

	for _, def := range snap.Tools {
		server, ok := v.servers[def.ServerId]
		if !ok {
			return nil, fmt.Errorf("snapshot: tool %q references unknown server %q",
				def.QualifiedName, def.ServerId)
		}
		namespace, ok := v.namespaces[def.NamespaceId]
		if !ok {
			return nil, fmt.Errorf("snapshot: tool %q references unknown namespace %q",
				def.QualifiedName, def.NamespaceId)
		}
		tenant, ok := v.tenants[def.TenantId]
		if !ok {
			return nil, fmt.Errorf("snapshot: tool %q references unknown tenant %q",
				def.QualifiedName, def.TenantId)
		}
		toolset, ok := v.toolsets[def.ToolsetId]
		if !ok {
			return nil, fmt.Errorf("snapshot: tool %q references unknown toolset %q",
				def.QualifiedName, def.ToolsetId)
		}
		// The qualified name is assigned at admission and clients depend on it.
		// If it disagreed with the namespace's prefix, a grant scoped to the
		// namespace would not cover the name the client actually calls.
		if want := namespace.Prefix + "." + def.Name; def.QualifiedName != want {
			return nil, fmt.Errorf(
				"snapshot: tool %q has qualified name %q but namespace %q has prefix %q",
				def.Name, def.QualifiedName, namespace.Id, namespace.Prefix)
		}
		if def.InputSchemaJson == "" {
			// The SDK panics on a tool with no input schema, which would
			// crash-loop the fleet on a bad publish. Refuse it here instead.
			return nil, fmt.Errorf("snapshot: tool %q has no input schema", def.QualifiedName)
		}

		tool := &Tool{
			Def: def, Server: server, Namespace: namespace,
			Tenant: tenant, Toolset: toolset,
			ToolsetPriority: toolset.Priority,
		}
		v.toolsByTenant[def.TenantId] = append(v.toolsByTenant[def.TenantId], tool)
		v.toolsByDigest[def.Digest] = def
	}

	// Sorted once, here. Composing a principal's catalog filters this slice,
	// and filtering preserves order — so the hot path never sorts.
	for _, tools := range v.toolsByTenant {
		sortTools(tools)
	}

	// A qualified name must be unique within a tenant, or a tools/call is
	// ambiguous. Across tenants it is expected: that is the whole point of
	// per-tenant admission.
	for tenantID, tools := range v.toolsByTenant {
		seen := map[string]string{}
		for _, t := range tools {
			if prior, dup := seen[t.Def.QualifiedName]; dup {
				return nil, fmt.Errorf(
					"snapshot: tenant %q has two tools named %q (from toolsets %q and %q)",
					v.tenants[tenantID].Slug, t.Def.QualifiedName, prior, t.Toolset.Name)
			}
			seen[t.Def.QualifiedName] = t.Toolset.Name
		}
	}

	// Token budgets are checked against what was admitted, per tenant. A
	// toolset over budget for one tenant and under for another is possible
	// once tenants run different releases, and both must be reported.
	for tenantID, tools := range v.toolsByTenant {
		perToolset := map[string]int32{}
		for _, t := range tools {
			perToolset[t.Toolset.Id] += t.Def.TokenEstimate
		}
		for id, used := range perToolset {
			ts := v.toolsets[id]
			if ts.TokenBudget > 0 && used > ts.TokenBudget {
				return nil, fmt.Errorf(
					"snapshot: toolset %q costs %d tokens for tenant %q, over its "+
						"%d-token budget",
					ts.Name, used, v.tenants[tenantID].Slug, ts.TokenBudget)
			}
		}
	}

	if err := v.indexRBAC(snap.Rbac); err != nil {
		return nil, err
	}
	return v, nil
}

// indexRBAC reads the compiled authorization state the snapshot carries.
func (v *View) indexRBAC(rbac *snapshotpb.RBAC) error {
	v.catalog = authz.Catalog{}
	if rbac == nil {
		// A snapshot with no RBAC serves nobody. Legal — a freshly built
		// snapshot with no users yet is exactly that — and not an error.
		return nil
	}

	for _, rp := range rbac.RolePermissions {
		if v.catalog[rp.Role] == nil {
			v.catalog[rp.Role] = map[authz.Permission]struct{}{}
		}
		v.catalog[rp.Role][authz.Permission(rp.Permission)] = struct{}{}
	}

	for _, p := range rbac.Principals {
		if p.Id == "" {
			return fmt.Errorf("snapshot: a principal has no id")
		}
		if _, dup := v.principals[p.Id]; dup {
			return fmt.Errorf("snapshot: principal id %q appears twice", p.Id)
		}
		if _, ok := v.tenants[p.TenantId]; !ok {
			return fmt.Errorf("snapshot: principal %q references unknown tenant %q",
				p.Subject, p.TenantId)
		}
		v.principals[p.Id] = p
	}
	return nil
}

// Principal composes and caches one principal's catalog.
//
// Lazily, on first connect, and cached for the life of this view. The cost is a
// scope-prefix test per admitted tool in the principal's tenant against a
// compiled decider — nanoseconds each (ADR 0015) — so a principal with a
// thousand tools costs tens of microseconds once.
func (v *View) Principal(ctx context.Context, id string) (*PrincipalView, error) {
	v.mu.RLock()
	cached, ok := v.cache[id]
	v.mu.RUnlock()
	if ok {
		return cached, nil
	}

	principal, ok := v.principals[id]
	if !ok {
		return nil, fmt.Errorf("snapshot: no principal %q in snapshot %d", id, v.Version)
	}
	tenant, ok := v.tenants[principal.TenantId]
	if !ok {
		return nil, fmt.Errorf("snapshot: principal %q references unknown tenant", id)
	}

	grants := make([]authz.Grant, 0, len(principal.Grants))
	for _, g := range principal.Grants {
		grants = append(grants, authz.Grant{Role: g.Role, Scope: g.Scope})
	}

	decide, err := v.engine.Prepare(ctx, grants, v.catalog)
	if err != nil {
		return nil, fmt.Errorf("snapshot: compiling grants for %q: %w", principal.Subject, err)
	}

	view := v.composePrincipal(principal, tenant, decide)

	v.mu.Lock()
	// Re-check: two connections for one principal can race here, and the
	// second must not replace a view the first already handed out.
	if existing, ok := v.cache[id]; ok {
		v.mu.Unlock()
		return existing, nil
	}
	v.cache[id] = view
	v.mu.Unlock()
	return view, nil
}

func (v *View) composePrincipal(
	principal *snapshotpb.Principal,
	tenant *snapshotpb.Tenant,
	decide authz.Decider,
) *PrincipalView {
	defaults := v.pb.Catalog
	ttl := 0
	if defaults != nil {
		ttl = int(defaults.TtlMs)
	}

	out := &PrincipalView{
		Principal:       principal,
		Tenant:          tenant,
		byQualifiedName: map[string]*Tool{},
		callable:        map[string]bool{},
		pluginsByHook:   map[snapshotpb.Hook][]*snapshotpb.PluginManifest{},
		TTLMs:           ttl,
	}

	contributing := map[string]*snapshotpb.Toolset{}

	// The tenant's tools are already sorted, and filtering preserves order.
	for _, tool := range v.toolsByTenant[tenant.Id] {
		scope := tool.Scope()
		if !decide(authz.PermToolList, scope) {
			continue
		}
		out.Tools = append(out.Tools, tool)
		out.byQualifiedName[tool.Def.QualifiedName] = tool
		out.TokenEstimate += int(tool.Def.TokenEstimate)
		contributing[tool.Toolset.Id] = tool.Toolset

		// Listing and calling are separate permissions. A principal that may
		// see a tool without invoking it is a legitimate role; the reverse is
		// refused at admission (ADR 0015).
		if decide(authz.PermToolCall, scope) {
			out.callable[tool.Def.QualifiedName] = true
		}
	}

	// A toolset may only narrow the TTL, never widen it.
	for _, ts := range contributing {
		if ts.TtlMs > 0 && (out.TTLMs == 0 || int(ts.TtlMs) < out.TTLMs) {
			out.TTLMs = int(ts.TtlMs)
		}
	}

	for _, policy := range v.pb.Policies {
		out.Policies = append(out.Policies, policy)
		// A policy may only narrow. The merged TTL is the tightest of the
		// deployment default, every contributing toolset, and every applicable
		// policy — widening anywhere would let a narrow rule be undone by a
		// broad one somewhere else.
		for _, rule := range policy.Rules {
			if rule.MaxTtlMs > 0 && (out.TTLMs == 0 || int(rule.MaxTtlMs) < out.TTLMs) {
				out.TTLMs = int(rule.MaxTtlMs)
			}
		}
	}
	slices.SortStableFunc(out.Policies, func(a, b *snapshotpb.Policy) int {
		return cmp.Compare(a.Priority, b.Priority)
	})

	for _, plugin := range v.pb.Plugins {
		if !pluginAppliesTo(plugin, contributing) {
			continue
		}
		for _, hook := range plugin.Hooks {
			out.pluginsByHook[hook] = append(out.pluginsByHook[hook], plugin)
		}
	}
	for hook := range out.pluginsByHook {
		slices.SortStableFunc(out.pluginsByHook[hook],
			func(a, b *snapshotpb.PluginManifest) int {
				return cmp.Compare(a.Priority, b.Priority)
			})
	}
	return out
}

func checkMutatingPluginConflicts(plugins []*snapshotpb.PluginManifest) error {
	type slot struct {
		hook     snapshotpb.Hook
		priority int32
	}
	seen := map[slot]string{}
	for _, p := range plugins {
		// A plugin that declares no writes cannot mutate, so a shared priority
		// is harmless for it.
		if len(p.Writes) == 0 {
			continue
		}
		for _, h := range p.Hooks {
			key := slot{hook: h, priority: p.Priority}
			if prior, dup := seen[key]; dup {
				return fmt.Errorf(
					"snapshot: mutating plugins %q and %q both run at %s with priority %d; "+
						"their patch order would be unspecified",
					prior, p.Name, h, p.Priority)
			}
			seen[key] = p.Name
		}
	}
	return nil
}

// sortTools puts a catalog in its stable total order.
//
// The order is a cost control, not a presentation detail: a client's prompt
// cache is keyed on the serialized tool list, so a reordering makes every
// connected client pay full price again. Sorting on keys fixed at admission
// time — toolset priority, namespace prefix, tool name — means adding a tool
// appends within its partition and leaves every earlier entry in place.
//
// The key is the *toolset* priority where it used to be the bundle's; see
// ADR 0010 as amended.
func sortTools(tools []*Tool) {
	slices.SortStableFunc(tools, func(a, b *Tool) int {
		if c := cmp.Compare(a.ToolsetPriority, b.ToolsetPriority); c != 0 {
			return c
		}
		if c := strings.Compare(a.Namespace.Prefix, b.Namespace.Prefix); c != 0 {
			return c
		}
		return strings.Compare(a.Def.Name, b.Def.Name)
	})
}

func pluginAppliesTo(p *snapshotpb.PluginManifest, contributing map[string]*snapshotpb.Toolset) bool {
	// Empty means every tool — the common case for a platform-wide plugin.
	if len(p.ToolsetIds) == 0 {
		return true
	}
	for _, id := range p.ToolsetIds {
		if _, ok := contributing[id]; ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- accessors --

// Tenant returns a tenant by slug, or nil.
func (v *View) Tenant(slug string) *snapshotpb.Tenant { return v.tenantsBySlug[slug] }

// TenantByID returns a tenant by its id, or nil.
func (v *View) TenantByID(id string) *snapshotpb.Tenant { return v.tenants[id] }

// TenantSlugs lists the tenants this snapshot serves, sorted.
func (v *View) TenantSlugs() []string {
	out := make([]string, 0, len(v.tenantsBySlug))
	for slug := range v.tenantsBySlug {
		out = append(out, slug)
	}
	slices.Sort(out)
	return out
}

// PrincipalIDs lists every principal the snapshot carries, sorted.
func (v *View) PrincipalIDs() []string {
	out := make([]string, 0, len(v.principals))
	for id := range v.principals {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// ToolsForTenant returns a tenant's admitted tools in catalog order.
func (v *View) ToolsForTenant(tenantID string) []*Tool { return v.toolsByTenant[tenantID] }

// Server returns a backend by id.
func (v *View) Server(id string) *snapshotpb.Server { return v.servers[id] }

// Servers returns every backend, ordered by id.
func (v *View) Servers() []*snapshotpb.Server {
	out := make([]*snapshotpb.Server, 0, len(v.servers))
	for _, s := range v.servers {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *snapshotpb.Server) int {
		return strings.Compare(a.Id, b.Id)
	})
	return out
}

// Namespace returns a namespace by id.
func (v *View) Namespace(id string) *snapshotpb.Namespace { return v.namespaces[id] }

// Toolset returns a toolset by id.
func (v *View) Toolset(id string) *snapshotpb.Toolset { return v.toolsets[id] }

// Plugin returns a plugin manifest by id.
func (v *View) Plugin(id string) *snapshotpb.PluginManifest { return v.plugins[id] }

// ToolByDigest resolves a content address to its admitted definition.
func (v *View) ToolByDigest(digest string) *snapshotpb.ToolDefinition {
	return v.toolsByDigest[digest]
}

// Proto returns the underlying snapshot.
func (v *View) Proto() *snapshotpb.Snapshot { return v.pb }

// Age is how long ago the snapshot was built.
func (v *View) Age() time.Duration {
	if v.BuiltAt.IsZero() {
		return 0
	}
	return time.Since(v.BuiltAt)
}

// Tool resolves a tools/call target within a principal's catalog.
func (p *PrincipalView) Tool(qualifiedName string) *Tool {
	return p.byQualifiedName[qualifiedName]
}

// Callable reports whether the principal may invoke a tool it can see.
func (p *PrincipalView) Callable(qualifiedName string) bool {
	return p.callable[qualifiedName]
}

// PluginsFor returns the plugins that run at a hook, in priority order.
func (p *PrincipalView) PluginsFor(hook snapshotpb.Hook) []*snapshotpb.PluginManifest {
	return p.pluginsByHook[hook]
}

// CacheScope is the value the edge must put on a list result.
//
// Unconditionally "private" now. Every catalog is derived from a principal's
// grants (ADR 0016), so the condition that once permitted "public" — nothing
// identity-specific applied — is never true.
//
// The function is kept rather than inlined so the invariant has a name, a test,
// and one place where a future public case would have to be argued for. This is
// the only expression in the codebase that decides a catalog's cache scope.
func (p *PrincipalView) CacheScope() string { return "private" }
