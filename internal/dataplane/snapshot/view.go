// Copyright 2026 The MCPDoll Authors.

package snapshot

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// View is an immutable, indexed snapshot ready to serve from.
//
// Indexing happens once per snapshot activation rather than per request. The
// serving path does map lookups and reads a pre-sorted slice; it never sorts,
// filters by scanning, or allocates a catalog from scratch. That is what keeps
// per-request cost independent of how many backends the org has registered.
//
// A View is never mutated after Build returns. Concurrent readers therefore need
// no synchronization at all, which is the point of swapping a pointer rather
// than updating a shared structure.
type View struct {
	pb *snapshotpb.Snapshot

	Version  int64
	ID       string
	BuiltAt  time.Time
	OrgID    string
	LoadedAt time.Time

	namespaces map[string]*snapshotpb.Namespace
	servers    map[string]*snapshotpb.Server
	bundles    map[string]*snapshotpb.Bundle
	policies   map[string]*snapshotpb.Policy
	plugins    map[string]*snapshotpb.PluginManifest

	// audiences indexed by the URL slug the endpoint is served at.
	audiences map[string]*AudienceView

	// tools indexed by content digest, for audit and drift lookups.
	toolsByDigest map[string]*snapshotpb.ToolDefinition
}

// AudienceView is one endpoint's precomputed catalog.
type AudienceView struct {
	Audience *snapshotpb.Audience

	// Tools in the stable total order clients will see them in. See
	// [sortTools] for why the order is what it is.
	Tools []*Tool

	// byQualifiedName resolves a tools/call target in one lookup.
	byQualifiedName map[string]*Tool

	// Policies that apply, in priority order.
	Policies []*snapshotpb.Policy

	// Plugins scoped to this audience, grouped by hook and pre-sorted by
	// priority, so the pipeline never sorts on the hot path.
	pluginsByHook map[snapshotpb.Hook][]*snapshotpb.PluginManifest

	// TTLMs is the merged ceiling: the minimum of the org default, every
	// contributing bundle's override, and every applicable policy's cap.
	TTLMs int

	// IdentityFiltered is true when this audience's catalog can differ between
	// principals — because a policy is identity-specific or an
	// identity_dependent plugin runs at ON_CATALOG.
	//
	// When true the list result must be cacheScope: private. A view shaped for
	// one principal served to another is a confidentiality bug, so this is
	// computed at build time and not left to a runtime judgement call.
	IdentityFiltered bool

	// TokenEstimate is the summed cost of the catalog, for the console's budget
	// meter and for per-tenant budget accounting.
	TokenEstimate int
}

// Tool couples an admitted definition with the server that serves it and the
// namespace it belongs to, so the serving path needs no further joins.
type Tool struct {
	Def       *snapshotpb.ToolDefinition
	Server    *snapshotpb.Server
	Namespace *snapshotpb.Namespace

	// BundlePriority is the priority of the bundle that contributed this tool,
	// captured because it is the primary sort key.
	BundlePriority int32
}

// QualifiedName is the name clients see.
func (t *Tool) QualifiedName() string { return t.Def.QualifiedName }

// EffectClass of the tool.
func (t *Tool) EffectClass() snapshotpb.EffectClass { return t.Def.EffectClass }

// Build indexes a verified snapshot into a serving view.
//
// Every referential problem is a build error. The data plane would rather keep
// serving its previous snapshot than activate one with a dangling reference: a
// half-valid snapshot produces confusing partial outages that are much harder to
// diagnose than a refused activation.
func Build(snap *snapshotpb.Snapshot) (*View, error) {
	if snap == nil {
		return nil, fmt.Errorf("snapshot: cannot build a view from nothing")
	}
	if snap.Version <= 0 {
		return nil, fmt.Errorf("snapshot: version must be positive, got %d", snap.Version)
	}

	v := &View{
		pb:            snap,
		Version:       snap.Version,
		ID:            snap.Id,
		OrgID:         snap.OrgId,
		LoadedAt:      time.Now().UTC(),
		namespaces:    make(map[string]*snapshotpb.Namespace, len(snap.Namespaces)),
		servers:       make(map[string]*snapshotpb.Server, len(snap.Servers)),
		bundles:       make(map[string]*snapshotpb.Bundle, len(snap.Bundles)),
		policies:      make(map[string]*snapshotpb.Policy, len(snap.Policies)),
		plugins:       make(map[string]*snapshotpb.PluginManifest, len(snap.Plugins)),
		audiences:     make(map[string]*AudienceView, len(snap.Audiences)),
		toolsByDigest: make(map[string]*snapshotpb.ToolDefinition, len(snap.Tools)),
	}
	if snap.BuiltAt != nil {
		v.BuiltAt = snap.BuiltAt.AsTime()
	}

	for _, ns := range snap.Namespaces {
		if _, dup := v.namespaces[ns.Id]; dup {
			return nil, fmt.Errorf("snapshot: namespace %q appears twice", ns.Id)
		}
		v.namespaces[ns.Id] = ns
	}
	// A duplicate prefix would make two namespaces' tools collide under one
	// name. Admission rejects it, but a snapshot is also checked here because
	// the data plane does not assume the control plane is correct.
	byPrefix := map[string]string{}
	for _, ns := range snap.Namespaces {
		if prior, dup := byPrefix[ns.Prefix]; dup {
			return nil, fmt.Errorf("snapshot: namespaces %q and %q share the prefix %q",
				prior, ns.Id, ns.Prefix)
		}
		byPrefix[ns.Prefix] = ns.Id
	}

	for _, s := range snap.Servers {
		if _, dup := v.servers[s.Id]; dup {
			return nil, fmt.Errorf("snapshot: server %q appears twice", s.Id)
		}
		if _, ok := v.namespaces[s.NamespaceId]; !ok {
			return nil, fmt.Errorf("snapshot: server %q references unknown namespace %q", s.Id, s.NamespaceId)
		}
		v.servers[s.Id] = s
	}

	for _, p := range snap.Policies {
		if _, dup := v.policies[p.Id]; dup {
			return nil, fmt.Errorf("snapshot: policy %q appears twice", p.Id)
		}
		v.policies[p.Id] = p
	}

	for _, p := range snap.Plugins {
		if _, dup := v.plugins[p.Id]; dup {
			return nil, fmt.Errorf("snapshot: plugin %q appears twice", p.Id)
		}
		v.plugins[p.Id] = p
	}
	if err := checkMutatingPluginConflicts(snap.Plugins); err != nil {
		return nil, err
	}

	// Tools, joined to their server and namespace.
	toolsByNamespace := map[string][]*Tool{}
	byQualified := map[string]*snapshotpb.ToolDefinition{}
	for _, def := range snap.Tools {
		if _, dup := v.toolsByDigest[def.Digest]; dup {
			// Two entries with the same digest are the same definition; the
			// snapshotter should have deduplicated. Refusing is safer than
			// guessing which server it belongs to.
			return nil, fmt.Errorf("snapshot: tool digest %s appears twice", def.Digest)
		}
		srv, ok := v.servers[def.ServerId]
		if !ok {
			return nil, fmt.Errorf("snapshot: tool %q references unknown server %q",
				def.QualifiedName, def.ServerId)
		}
		ns, ok := v.namespaces[def.NamespaceId]
		if !ok {
			return nil, fmt.Errorf("snapshot: tool %q references unknown namespace %q",
				def.QualifiedName, def.NamespaceId)
		}
		if prior, dup := byQualified[def.QualifiedName]; dup {
			return nil, fmt.Errorf("snapshot: qualified name %q is claimed by two definitions (%s and %s)",
				def.QualifiedName, prior.Digest, def.Digest)
		}
		expected := ns.Prefix + "." + def.Name
		if def.QualifiedName != expected {
			return nil, fmt.Errorf(
				"snapshot: tool %q has qualified name %q but namespace prefix %q implies %q",
				def.Name, def.QualifiedName, ns.Prefix, expected)
		}
		byQualified[def.QualifiedName] = def
		v.toolsByDigest[def.Digest] = def
		toolsByNamespace[def.NamespaceId] = append(toolsByNamespace[def.NamespaceId], &Tool{
			Def: def, Server: srv, Namespace: ns,
		})
	}

	for _, b := range snap.Bundles {
		if _, dup := v.bundles[b.Id]; dup {
			return nil, fmt.Errorf("snapshot: bundle %q appears twice", b.Id)
		}
		for _, entry := range b.Entries {
			if _, ok := v.namespaces[entry.NamespaceId]; !ok {
				return nil, fmt.Errorf("snapshot: bundle %q references unknown namespace %q",
					b.Id, entry.NamespaceId)
			}
			for _, qn := range entry.QualifiedNames {
				if _, ok := byQualified[qn]; !ok {
					return nil, fmt.Errorf("snapshot: bundle %q names tool %q, which is not admitted",
						b.Id, qn)
				}
			}
		}
		v.bundles[b.Id] = b
	}

	defaults := snap.Catalog
	if defaults == nil {
		defaults = &snapshotpb.CatalogDefaults{}
	}

	for _, aud := range snap.Audiences {
		av, err := v.buildAudience(aud, toolsByNamespace, defaults)
		if err != nil {
			return nil, err
		}
		if _, dup := v.audiences[aud.Slug]; dup {
			return nil, fmt.Errorf("snapshot: audience slug %q appears twice", aud.Slug)
		}
		v.audiences[aud.Slug] = av
	}

	return v, nil
}

// checkMutatingPluginConflicts rejects two mutating plugins that share a hook
// and a priority.
//
// If two plugins both patch the same hook at the same priority, the order in
// which their patches apply is unspecified — and two patches applied in
// different orders can produce different requests. Catching it at build time
// makes it a snapshot-build failure with a name in it, rather than a
// nondeterministic production behaviour nobody can reproduce.
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

func (v *View) buildAudience(
	aud *snapshotpb.Audience,
	toolsByNamespace map[string][]*Tool,
	defaults *snapshotpb.CatalogDefaults,
) (*AudienceView, error) {
	if aud.Slug == "" {
		return nil, fmt.Errorf("snapshot: audience %q has no slug, so it has no endpoint", aud.Id)
	}

	av := &AudienceView{
		Audience:        aud,
		byQualifiedName: map[string]*Tool{},
		pluginsByHook:   map[snapshotpb.Hook][]*snapshotpb.PluginManifest{},
		TTLMs:           int(defaults.TtlMs),
	}

	for _, pid := range aud.PolicyIds {
		p, ok := v.policies[pid]
		if !ok {
			return nil, fmt.Errorf("snapshot: audience %q references unknown policy %q", aud.Slug, pid)
		}
		av.Policies = append(av.Policies, p)
	}
	slices.SortStableFunc(av.Policies, func(a, b *snapshotpb.Policy) int {
		return cmp.Compare(a.Priority, b.Priority)
	})

	// A policy may only narrow the TTL, never widen it — otherwise a
	// permissive policy could override the org-wide ceiling.
	for _, p := range av.Policies {
		for _, rule := range p.Rules {
			if rule.MaxTtlMs > 0 && rule.MaxTtlMs < int32(av.TTLMs) {
				av.TTLMs = int(rule.MaxTtlMs)
			}
			if rule.IdentitySpecific {
				av.IdentityFiltered = true
			}
			// A rule that can hide or deny per-principal makes the catalog
			// identity-specific whether or not it says so, because two
			// principals can legitimately receive different lists.
			switch rule.Decision {
			case snapshotpb.PolicyDecision_POLICY_DECISION_HIDE,
				snapshotpb.PolicyDecision_POLICY_DECISION_DENY:
				if len(rule.RequiredIdpGroups) > 0 {
					av.IdentityFiltered = true
				}
			}
		}
	}

	// Collect tools bundle by bundle, in the audience's declared bundle order.
	seen := map[string]bool{}
	for _, bid := range aud.BundleIds {
		b, ok := v.bundles[bid]
		if !ok {
			return nil, fmt.Errorf("snapshot: audience %q references unknown bundle %q", aud.Slug, bid)
		}
		if b.TtlMs > 0 && b.TtlMs < int32(av.TTLMs) {
			av.TTLMs = int(b.TtlMs)
		}
		for _, entry := range b.Entries {
			include := selectTools(toolsByNamespace[entry.NamespaceId], entry)
			for _, tool := range include {
				// First bundle to contribute a tool owns it. A later bundle
				// re-including the same tool must not duplicate it in the list
				// or move it in the ordering.
				if seen[tool.Def.QualifiedName] {
					continue
				}
				seen[tool.Def.QualifiedName] = true
				withPriority := &Tool{
					Def:            tool.Def,
					Server:         tool.Server,
					Namespace:      tool.Namespace,
					BundlePriority: b.Priority,
				}
				av.Tools = append(av.Tools, withPriority)
				av.byQualifiedName[tool.Def.QualifiedName] = withPriority
				av.TokenEstimate += int(tool.Def.TokenEstimate)
			}
		}
		if b.TokenBudget > 0 && av.TokenEstimate > int(b.TokenBudget) {
			return nil, fmt.Errorf(
				"snapshot: audience %q exceeds bundle %q token budget: %d > %d",
				aud.Slug, b.Name, av.TokenEstimate, b.TokenBudget)
		}
	}

	sortTools(av.Tools)

	for _, p := range v.pb.Plugins {
		if !pluginAppliesTo(p, aud.Id) {
			continue
		}
		for _, h := range p.Hooks {
			av.pluginsByHook[h] = append(av.pluginsByHook[h], p)
		}
		// An identity-dependent plugin at ON_CATALOG can shape the list per
		// principal, which makes the result unshareable.
		if p.IdentityDependent && slices.Contains(p.Hooks, snapshotpb.Hook_HOOK_ON_CATALOG) {
			av.IdentityFiltered = true
		}
	}
	for hook := range av.pluginsByHook {
		slices.SortStableFunc(av.pluginsByHook[hook], func(a, b *snapshotpb.PluginManifest) int {
			if c := cmp.Compare(a.Priority, b.Priority); c != 0 {
				return c
			}
			// Tie-break by name so the order is reproducible even for two
			// non-mutating plugins that share a priority.
			return strings.Compare(a.Name, b.Name)
		})
	}

	if av.TTLMs < 0 {
		av.TTLMs = 0
	}
	return av, nil
}

// selectTools applies a bundle entry's include/exclude lists.
func selectTools(available []*Tool, entry *snapshotpb.BundleEntry) []*Tool {
	excluded := make(map[string]bool, len(entry.ExcludeQualifiedNames))
	for _, qn := range entry.ExcludeQualifiedNames {
		excluded[qn] = true
	}

	// An empty include list means the whole namespace.
	if len(entry.QualifiedNames) == 0 {
		out := make([]*Tool, 0, len(available))
		for _, t := range available {
			if !excluded[t.Def.QualifiedName] {
				out = append(out, t)
			}
		}
		return out
	}

	wanted := make(map[string]bool, len(entry.QualifiedNames))
	for _, qn := range entry.QualifiedNames {
		wanted[qn] = true
	}
	out := make([]*Tool, 0, len(entry.QualifiedNames))
	for _, t := range available {
		qn := t.Def.QualifiedName
		if wanted[qn] && !excluded[qn] {
			out = append(out, t)
		}
	}
	return out
}

// sortTools imposes the stable total ordering clients see:
// (bundle priority, namespace prefix, tool name).
//
// The order is a *contract*, not a presentation detail. Model providers cache
// prompt prefixes, and a catalog is usually near the front of the prompt; if the
// order moves, every client's cache is invalidated and every request pays full
// price again. Sorting on a key that is fixed at admission time — rather than,
// say, insertion order or a timestamp — means adding a tool appends within its
// namespace partition and leaves every earlier entry in place.
func sortTools(tools []*Tool) {
	slices.SortStableFunc(tools, func(a, b *Tool) int {
		if c := cmp.Compare(a.BundlePriority, b.BundlePriority); c != 0 {
			return c
		}
		if c := strings.Compare(a.Namespace.Prefix, b.Namespace.Prefix); c != 0 {
			return c
		}
		return strings.Compare(a.Def.Name, b.Def.Name)
	})
}

func pluginAppliesTo(p *snapshotpb.PluginManifest, audienceID string) bool {
	// Empty means every audience — the common case for a platform-wide plugin.
	if len(p.AudienceIds) == 0 {
		return true
	}
	return slices.Contains(p.AudienceIds, audienceID)
}

// ---------------------------------------------------------------- accessors --

// Audience returns the view for a slug, or nil.
func (v *View) Audience(slug string) *AudienceView { return v.audiences[slug] }

// AudienceSlugs lists every served endpoint, sorted for stable output.
func (v *View) AudienceSlugs() []string {
	out := make([]string, 0, len(v.audiences))
	for slug := range v.audiences {
		out = append(out, slug)
	}
	slices.Sort(out)
	return out
}

// Server returns a registered backend by id, or nil.
func (v *View) Server(id string) *snapshotpb.Server { return v.servers[id] }

// Servers lists every backend, ordered by id for stable output.
func (v *View) Servers() []*snapshotpb.Server {
	out := make([]*snapshotpb.Server, 0, len(v.servers))
	for _, s := range v.servers {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *snapshotpb.Server) int { return strings.Compare(a.Id, b.Id) })
	return out
}

// Namespace returns a namespace by id, or nil.
func (v *View) Namespace(id string) *snapshotpb.Namespace { return v.namespaces[id] }

// Plugin returns a manifest by id, or nil.
func (v *View) Plugin(id string) *snapshotpb.PluginManifest { return v.plugins[id] }

// ToolByDigest resolves a content address to its definition, or nil.
func (v *View) ToolByDigest(digest string) *snapshotpb.ToolDefinition {
	return v.toolsByDigest[digest]
}

// Proto exposes the underlying message for audit and diagnostics. The returned
// value must not be mutated; it is shared by every concurrent reader.
func (v *View) Proto() *snapshotpb.Snapshot { return v.pb }

// Age is how long ago the snapshot was built, which is the signal an operator
// watches to spot a stalled distribution.
func (v *View) Age() time.Duration {
	if v.BuiltAt.IsZero() {
		return 0
	}
	return time.Since(v.BuiltAt)
}

// Tool resolves a tools/call target within this audience, or nil.
func (a *AudienceView) Tool(qualifiedName string) *Tool {
	return a.byQualifiedName[qualifiedName]
}

// PluginsFor returns the plugins registered at a hook, in execution order. The
// slice is shared and must not be mutated.
func (a *AudienceView) PluginsFor(hook snapshotpb.Hook) []*snapshotpb.PluginManifest {
	return a.pluginsByHook[hook]
}

// CacheScope is the value the edge must put on a list result.
//
// "private" whenever the view is identity-filtered. This is the correctness
// property that stops one principal's filtered catalog being served from a
// shared cache to another principal.
func (a *AudienceView) CacheScope() string {
	if a.IdentityFiltered {
		return "private"
	}
	return "public"
}
