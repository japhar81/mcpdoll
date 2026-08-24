// Copyright 2026 The MCPDoll Authors.

// Package snapshotter resolves a registry document plus live backend discovery
// into a signed snapshot.
//
// This is where the control plane's declared intent meets what backends actually
// publish. Two rules govern the meeting:
//
//   - **Every problem is a build failure.** A dangling reference, a name
//     collision, an over-budget bundle, two mutating plugins racing at the same
//     hook: the build stops. The alternative is a snapshot that some data-plane
//     instances refuse and others accept, or one that half-works in production.
//     A build failure is a message with a name in it; a bad snapshot is an
//     incident.
//
//   - **Names are assigned here, never at runtime.** A qualified-name collision
//     is rejected at build time rather than resolved by hashing at serve time.
//     Auto-hashing would give clients a name nobody chose, that changes when an
//     unrelated backend is added, and that breaks every agent which recorded the
//     old one.
package snapshotter

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	mcpadapter "github.com/mcpdoll/mcpdoll/internal/mcp"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	"github.com/mcpdoll/mcpdoll/internal/platform/canonical"
	"github.com/mcpdoll/mcpdoll/internal/platform/ids"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// Result is one snapshot build.
type Result struct {
	Snapshot *snapshotpb.Snapshot
	Signed   *snapshotpb.SignedSnapshot

	// Discovered records what each backend published, so the CLI can report
	// "3 tools from crm-prod (2026-07-28)" and drift can compare later.
	Discovered []BackendReport

	// Warnings are things worth telling an operator that do not justify failing
	// the build — a backend that negotiated down, a tool excluded by policy.
	Warnings []string
}

// BackendReport is what one discovery pass found.
type BackendReport struct {
	ServerID   string
	ServerName string
	// TenantSlug is which tenant's binding this pass discovered. The same
	// backend appears once per tenant, because each tenant's deployment is
	// discovered and admitted separately (ADR 0017).
	TenantSlug        string
	Endpoint          string
	NegotiatedVersion string
	ToolCount         int
	// Admitted is the tools that made it into the snapshot.
	Admitted []string
	// Excluded is tools the registry deliberately dropped.
	Excluded   []string
	ObservedAt time.Time
}

// Options configures a build.
type Options struct {
	Spec   *registry.Spec
	Signer *snapshot.Signer

	// DiscoverTimeout bounds each backend's discovery pass.
	DiscoverTimeout time.Duration

	// Concurrency bounds parallel discovery. Twenty backends discovered
	// sequentially at a few hundred milliseconds each makes a build feel broken;
	// discovered all at once it can overwhelm a shared dependency.
	Concurrency int

	// Tenants the snapshot serves. A binding naming a tenant absent from this
	// list is a build failure: the tools would be admitted for a tenant no
	// principal could belong to.
	Tenants []*snapshotpb.Tenant

	// Catalog and Principals are the compiled RBAC the snapshot carries, read
	// from the control plane's database. Empty is legal — a deployment with no
	// users yet serves nobody, which is correct rather than broken.
	Catalog    authz.Catalog
	Principals []*snapshotpb.Principal

	// AllowUnreachable builds a snapshot even when a backend cannot be reached,
	// omitting its tools.
	//
	// Off by default, and that default matters: silently shipping a catalog with
	// a backend's tools missing is exactly the prompt-cache-invalidating change
	// the grace window exists to prevent. An operator who genuinely wants to
	// publish without a backend has to say so.
	AllowUnreachable bool
}

// Build resolves, validates, signs, and returns a snapshot.
func Build(ctx context.Context, opts Options) (*Result, error) {
	if opts.Spec == nil {
		return nil, fmt.Errorf("snapshotter: a registry spec is required")
	}
	if opts.Signer == nil {
		return nil, fmt.Errorf("snapshotter: a signer is required; an unsigned snapshot cannot be activated")
	}
	if opts.DiscoverTimeout <= 0 {
		opts.DiscoverTimeout = 15 * time.Second
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}

	spec := opts.Spec
	result := &Result{}

	reports, err := discoverAll(ctx, spec, opts)
	if err != nil {
		return nil, err
	}

	b := snapshot.NewBuilder(spec.Version).
		WithID(ids.New(ids.KindSnapshot)).
		WithCatalogDefaults(spec.Catalog.TTL, spec.Catalog.DegradedTTL)

	// The registry digest lets a snapshot be traced back to the document that
	// produced it, which is what makes "rebuild and compare" a usable audit step.
	registryDigest, err := canonical.DigestOf(spec)
	if err != nil {
		return nil, fmt.Errorf("snapshotter: digesting the registry document: %w", err)
	}
	b.WithRegistryDigest(registryDigest.String())

	tenantsBySlug := map[string]*snapshotpb.Tenant{}
	for _, tenant := range opts.Tenants {
		if _, dup := tenantsBySlug[tenant.Slug]; dup {
			return nil, fmt.Errorf("snapshotter: tenant slug %q appears twice", tenant.Slug)
		}
		tenantsBySlug[tenant.Slug] = tenant
		b.AddTenant(tenant)
	}

	namespacesByID := map[string]registry.NamespaceSpec{}
	for _, ns := range spec.Namespaces {
		namespacesByID[ns.ID] = ns
		b.AddNamespace(&snapshotpb.Namespace{
			Id:            ns.ID,
			Name:          ns.Name,
			Prefix:        ns.Prefix,
			OwningTeamId:  ns.Team,
			ProjectId:     ns.Project,
			OwnerIdpGroup: ns.OwnerIdpGroup,
		})
	}

	// Which toolset contributes each tool. Resolved before admission because a
	// tool in no toolset cannot be granted by anything, and one claimed by two
	// toolsets would have two possible authorization scopes.
	toolsetFor, err := resolveToolsets(spec, namespacesByID)
	if err != nil {
		return nil, err
	}

	// Collisions are detected per tenant, not across the build: two tenants
	// publishing `crm.lookup_customer` is the normal case and the whole point
	// of per-tenant admission. Two *namespaces* colliding within one tenant is
	// still a failure.
	claimed := map[bindingKey]map[string]string{}

	for _, srv := range spec.Servers {
		ns := namespacesByID[srv.Namespace]
		mode, err := registry.ParseServingMode(srv.ServingMode)
		if err != nil {
			return nil, fmt.Errorf("snapshotter: server %q: %w", srv.ID, err)
		}

		bindings := make([]*snapshotpb.Binding, 0, len(srv.Bindings))
		for _, binding := range srv.Bindings {
			tenant, ok := tenantsBySlug[binding.Tenant]
			if !ok {
				return nil, fmt.Errorf(
					"snapshotter: server %q has a binding for tenant %q, which this "+
						"build does not carry; its tools would be admitted for a tenant "+
						"no principal could belong to", srv.ID, binding.Tenant)
			}
			bindings = append(bindings, &snapshotpb.Binding{
				TenantId: tenant.Id, Primary: binding.Primary, Replicas: binding.Replicas,
			})
		}

		b.AddServer(&snapshotpb.Server{
			Id:                    srv.ID,
			Name:                  srv.Name,
			NamespaceId:           srv.Namespace,
			Bindings:              bindings,
			PinnedProtocolVersion: srv.PinnedProtocolVersion,
			ServingMode:           mode,
			Criticality:           srv.Criticality,
			DataClassification:    srv.DataClassification,
			ComplianceScope:       srv.ComplianceScope,
			OwningTeamId:          srv.Team,
			ProjectId:             srv.Project,
			CanaryTool:            srv.CanaryTool,
			TokenExchange:         tokenExchange(srv.TokenExchange),
			Health:                healthPolicy(srv.Health),
		})

		defaultEffect, err := registry.ParseEffectClass(srv.DefaultEffectClass)
		if err != nil {
			return nil, fmt.Errorf("snapshotter: server %q: %w", srv.ID, err)
		}

		for _, binding := range srv.Bindings {
			tenant := tenantsBySlug[binding.Tenant]
			key := bindingKey{serverID: srv.ID, tenant: binding.Tenant}

			report := reports[key]
			if report == nil {
				// Unreachable and allowed. Only *this tenant* loses the tools:
				// refusing the whole build would take an unrelated tenant's
				// working configuration hostage to a third party's outage.
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"server %q was unreachable for tenant %q (%s); its tools are absent "+
						"from that tenant's catalog",
					srv.Name, binding.Tenant, binding.Primary))
				continue
			}

			// Sorted by name so the build is deterministic regardless of the
			// order a backend listed its tools in. Catalog order is a contract
			// (ADR 0010) and must not depend on a backend's map iteration.
			tools := append([]*mcpToolWithDef(nil), report.tools...)
			sort.Slice(tools, func(i, j int) bool { return tools[i].def.Name < tools[j].def.Name })

			summary := BackendReport{
				ServerID:          srv.ID,
				ServerName:        srv.Name,
				TenantSlug:        binding.Tenant,
				Endpoint:          binding.Primary,
				NegotiatedVersion: report.negotiated,
				ToolCount:         len(tools),
				ObservedAt:        report.observedAt,
			}

			if claimed[key] == nil {
				claimed[key] = map[string]string{}
			}

			for _, tool := range tools {
				override, hasOverride := srv.Tools[tool.def.Name]
				if hasOverride && override.Exclude {
					summary.Excluded = append(summary.Excluded, tool.def.Name)
					continue
				}

				effect := defaultEffect
				if hasOverride && override.EffectClass != "" {
					effect, err = registry.ParseEffectClass(override.EffectClass)
					if err != nil {
						return nil, fmt.Errorf("snapshotter: server %q tool %q: %w",
							srv.ID, tool.def.Name, err)
					}
				}

				qualified := ns.Prefix + "." + tool.def.Name
				if len(qualified) > registry.MaxQualifiedNameLength {
					return nil, fmt.Errorf(
						"snapshotter: qualified name %q is %d characters, over the %d-character "+
							"budget; shorten the namespace prefix or exclude the tool",
						qualified, len(qualified), registry.MaxQualifiedNameLength)
				}
				if prior, dup := claimed[key][qualified]; dup {
					return nil, fmt.Errorf(
						"snapshotter: %q is published for tenant %q by both %q and %q; "+
							"resolve the collision by excluding one or moving it to another "+
							"namespace (MCPDoll never auto-renames a tool, because clients "+
							"depend on the name)",
						qualified, binding.Tenant, prior, srv.Name)
				}

				toolsetID, inToolset := toolsetFor.For(qualified, srv.Namespace)
				if !inToolset {
					// A tool no toolset contributes cannot be granted to
					// anyone, so admitting it would put an unreachable entry in
					// a signed artifact. Reported rather than silently dropped.
					summary.Excluded = append(summary.Excluded, tool.def.Name)
					result.Warnings = append(result.Warnings, fmt.Sprintf(
						"tool %q is in no toolset, so nothing can grant it; it is not admitted",
						qualified))
					continue
				}
				claimed[key][qualified] = srv.Name

				b.AddTool(snapshot.ToolInput{
					ServerID:     srv.ID,
					NamespaceID:  srv.Namespace,
					TenantID:     tenant.Id,
					ToolsetID:    toolsetID,
					Prefix:       ns.Prefix,
					Name:         tool.def.Name,
					Title:        tool.def.Title,
					Description:  tool.def.Description,
					InputSchema:  rawSchema(tool.def.InputSchema),
					OutputSchema: rawSchema(tool.def.OutputSchema),
					Annotations:  tool.def.Annotations,
					EffectClass:  effect,
				})
				summary.Admitted = append(summary.Admitted, qualified)
			}

			if report.negotiated != "" && report.negotiated < "2026-07-28" {
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"server %q negotiated %s for tenant %q; capabilities added in "+
						"2026-07-28 are unavailable for it",
					srv.Name, report.negotiated, binding.Tenant))
			}
			result.Discovered = append(result.Discovered, summary)
		}
	}

	for _, ts := range spec.Toolsets {
		b.AddToolset(&snapshotpb.Toolset{
			Id:          ts.ID,
			Name:        ts.Name,
			Priority:    ts.Priority,
			TokenBudget: ts.TokenBudget,
			TtlMs:       int32(ts.TTL.Milliseconds()),
		})
	}

	for _, policy := range spec.Policies {
		rules := make([]*snapshotpb.PolicyRule, 0, len(policy.Rules))
		for i, rule := range policy.Rules {
			decision, err := registry.ParseDecision(rule.Decision)
			if err != nil {
				return nil, fmt.Errorf("snapshotter: policy %q rule %d: %w", policy.ID, i, err)
			}
			effectNames := make([]string, 0, len(rule.EffectClasses))
			for _, ec := range rule.EffectClasses {
				parsed, err := registry.ParseEffectClass(ec)
				if err != nil {
					return nil, fmt.Errorf("snapshotter: policy %q rule %d: %w", policy.ID, i, err)
				}
				effectNames = append(effectNames, parsed.String())
			}
			rules = append(rules, &snapshotpb.PolicyRule{
				EffectClasses:       effectNames,
				QualifiedNameGlobs:  rule.QualifiedNameGlobs,
				NamespaceIds:        rule.Namespaces,
				RequiredIdpGroups:   rule.RequiredIdpGroups,
				DataClassifications: rule.DataClassifications,
				Decision:            decision,
				Reason:              rule.Reason,
				MaxTtlMs:            int32(rule.MaxTTL.Milliseconds()),
				IdentitySpecific:    rule.IdentitySpecific,
			})
		}
		b.AddPolicy(&snapshotpb.Policy{
			Id: policy.ID, Name: policy.Name, Priority: policy.Priority, Rules: rules,
		})
	}

	for _, plugin := range spec.Plugins {
		manifest, err := pluginManifest(plugin)
		if err != nil {
			return nil, err
		}
		b.AddPlugin(manifest)
	}

	b.SetRBAC(opts.Catalog, opts.Principals)

	snap, err := b.Build()
	if err != nil {
		return nil, err
	}
	signed, err := opts.Signer.Sign(snap)
	if err != nil {
		return nil, err
	}

	result.Snapshot = snap
	result.Signed = signed
	sort.Strings(result.Warnings)
	return result, nil
}

// ---------------------------------------------------------------- discovery --

// discoveryReport is one backend's raw discovery result.
type discoveryReport struct {
	negotiated string
	observedAt time.Time
	endpoint   string
	tools      []*mcpToolWithDef
}

// mcpToolWithDef pairs the canonical definition with nothing else; the wrapper
// exists so the slice can be sorted by name without re-deriving it.
type mcpToolWithDef struct {
	def *canonical.ToolDefinition
}

// discoverAll probes every backend concurrently.
// bindingKey identifies one (server, tenant) pair — the unit of discovery and
// of admission.
type bindingKey struct {
	serverID string
	tenant   string
}

func discoverAll(
	ctx context.Context,
	spec *registry.Spec,
	opts Options,
) (map[bindingKey]*discoveryReport, error) {
	type outcome struct {
		key      bindingKey
		endpoint string
		report   *discoveryReport
		err      error
	}

	type job struct {
		srv     registry.ServerSpec
		binding registry.BindingSpec
	}
	var jobs []job
	for _, srv := range spec.Servers {
		for _, binding := range srv.Bindings {
			jobs = append(jobs, job{srv: srv, binding: binding})
		}
	}

	sem := make(chan struct{}, opts.Concurrency)
	results := make(chan outcome, len(jobs))
	var wg sync.WaitGroup

	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			key := bindingKey{serverID: j.srv.ID, tenant: j.binding.Tenant}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- outcome{key: key, endpoint: j.binding.Primary, err: ctx.Err()}
				return
			}

			// The primary is the definition source. Replicas are compared
			// against it by the prober at runtime, not admitted from here —
			// a replica mid-deploy would otherwise decide the catalog.
			discovered, err := mcpadapter.Discover(ctx, mcpadapter.DiscoverOptions{
				Endpoint: j.binding.Primary,
				Timeout:  opts.DiscoverTimeout,
			})
			if err != nil {
				results <- outcome{key: key, endpoint: j.binding.Primary, err: err}
				return
			}

			report := &discoveryReport{
				negotiated: discovered.NegotiatedVersion,
				observedAt: discovered.ObservedAt,
				endpoint:   j.binding.Primary,
			}
			for _, tool := range discovered.Tools {
				def, err := mcpadapter.ToCanonical(tool)
				if err != nil {
					results <- outcome{key: key, endpoint: j.binding.Primary,
						err: fmt.Errorf("tool %q: %w", tool.Name, err)}
					return
				}
				report.tools = append(report.tools, &mcpToolWithDef{def: def})
			}
			results <- outcome{key: key, endpoint: j.binding.Primary, report: report}
		}(j)
	}

	wg.Wait()
	close(results)

	reports := map[bindingKey]*discoveryReport{}
	var failures []string
	for out := range results {
		if out.err != nil {
			// Named by tenant as well as server: with per-tenant bindings,
			// "crm-prod is unreachable" is ambiguous across twenty tenants.
			failures = append(failures, fmt.Sprintf("%s for tenant %s (%s): %v",
				out.key.serverID, out.key.tenant, out.endpoint, out.err))
			continue
		}
		reports[out.key] = out.report
	}

	if len(failures) > 0 && !opts.AllowUnreachable {
		sort.Strings(failures)
		return nil, fmt.Errorf(
			"snapshotter: %d backend(s) could not be discovered:\n  - %s\n\n"+
				"Publishing without them would silently drop their tools from every catalog, "+
				"which invalidates clients' prompt caches. Pass --allow-unreachable if that is intended.",
			len(failures), strings.Join(failures, "\n  - "))
	}
	return reports, nil
}

// ------------------------------------------------------------- conversions --

// rawSchema recovers the raw JSON schema the MCP adapter produced.
//
// A Go type switch matches *exact* types, so `case []byte` does not match
// json.RawMessage even though they share an underlying type — and the adapter
// returns json.RawMessage. Missing that case silently dropped every schema,
// which produced a snapshot the data plane could not serve.
func rawSchema(v any) []byte {
	switch s := v.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return s
	case []byte:
		return s
	case string:
		if s == "" {
			return nil
		}
		return []byte(s)
	default:
		// Anything else is a structured value; marshal it rather than dropping it.
		raw, err := json.Marshal(v)
		if err != nil || string(raw) == "null" {
			return nil
		}
		return raw
	}
}

func tokenExchange(spec *registry.TokenExchangeSpec) *snapshotpb.TokenExchange {
	if spec == nil {
		return nil
	}
	return &snapshotpb.TokenExchange{
		TokenEndpoint:       spec.TokenEndpoint,
		Audience:            spec.Audience,
		Scopes:              spec.Scopes,
		ClientCredentialRef: spec.ClientCredentialRef,
		Header:              spec.Header,
		ClaimHeaders:        spec.ClaimHeaders,
	}
}

func healthPolicy(spec *registry.HealthSpec) *snapshotpb.HealthPolicy {
	if spec == nil {
		return nil
	}
	return &snapshotpb.HealthPolicy{
		ProbeIntervalMs:    int32(spec.ProbeInterval.Milliseconds()),
		ProbeTimeoutMs:     int32(spec.ProbeTimeout.Milliseconds()),
		GraceWindowMs:      int32(spec.GraceWindow.Milliseconds()),
		EjectAfterFailures: int32(spec.EjectAfterFailures),
	}
}

func pluginManifest(spec registry.PluginSpec) (*snapshotpb.PluginManifest, error) {
	runtime, err := registry.ParseRuntime(spec.Runtime)
	if err != nil {
		return nil, fmt.Errorf("snapshotter: plugin %q: %w", spec.ID, err)
	}
	rollout, err := registry.ParseRollout(spec.Rollout)
	if err != nil {
		return nil, fmt.Errorf("snapshotter: plugin %q: %w", spec.ID, err)
	}

	hooks := make([]snapshotpb.Hook, 0, len(spec.Hooks))
	for _, h := range spec.Hooks {
		hook, err := registry.ParseHook(h)
		if err != nil {
			return nil, fmt.Errorf("snapshotter: plugin %q: %w", spec.ID, err)
		}
		hooks = append(hooks, hook)
	}

	failurePolicy := map[string]snapshotpb.FailureMode{}
	for ec, mode := range spec.FailurePolicy {
		effect, err := registry.ParseEffectClass(ec)
		if err != nil {
			return nil, fmt.Errorf("snapshotter: plugin %q failure_policy: %w", spec.ID, err)
		}
		parsed, err := registry.ParseFailureMode(mode)
		if err != nil {
			return nil, fmt.Errorf("snapshotter: plugin %q failure_policy[%s]: %w", spec.ID, ec, err)
		}
		failurePolicy[effect.String()] = parsed
	}

	var configJSON string
	if len(spec.Config) > 0 {
		raw, err := canonical.Marshal(spec.Config)
		if err != nil {
			return nil, fmt.Errorf("snapshotter: plugin %q config: %w", spec.ID, err)
		}
		configJSON = string(raw)
	}

	return &snapshotpb.PluginManifest{
		Id:                spec.ID,
		Name:              spec.Name,
		Version:           spec.Version,
		Runtime:           runtime,
		Hooks:             hooks,
		Priority:          spec.Priority,
		BudgetMs:          int32(spec.Budget.Milliseconds()),
		Reads:             spec.Reads,
		Writes:            spec.Writes,
		FailurePolicy:     failurePolicy,
		IdentityDependent: spec.IdentityDependent,
		Rollout:           rollout,
		CanaryPercent:     spec.CanaryPercent,
		ArtifactRef:       spec.ArtifactRef,
		ArtifactDigest:    spec.ArtifactDigest,
		FuelLimit:         spec.FuelLimit,
		Endpoint:          spec.Endpoint,
		ConfigJson:        configJSON,
		ToolsetIds:        spec.Toolsets,
	}, nil
}

// toolsetIndex answers "which toolset contributes this tool?".
//
// A toolset draws from whole namespaces, from individually named tools, or
// both, minus its exclusions. Resolving membership up front rather than during
// admission answers two questions that must have exactly one answer each:
//
//   - A tool in no toolset cannot be granted by anything, so admitting it would
//     put an entry in a signed artifact that no principal could ever reach.
//   - A tool claimed by two toolsets would have two possible authorization
//     scopes, and a grant on one would silently not cover the other.
//
// The second is a build failure rather than a precedence rule. A precedence
// rule would mean an operator adding a tool to a second toolset silently
// changes which grants reach it.
type toolsetIndex struct {
	// byTool is an individually named tool -> toolset id.
	byTool map[string]string
	// byNamespace is a whole-namespace claim: namespace id -> toolset id.
	byNamespace map[string]string
	// excluded are qualified names a toolset explicitly dropped.
	excluded map[string]bool
}

// For returns the toolset contributing a tool, or "" if none does.
//
// An individually named tool wins over its namespace's claim, which is what
// lets a curated toolset take one tool out of a namespace another toolset owns.
func (i toolsetIndex) For(qualifiedName, namespaceID string) (string, bool) {
	if i.excluded[qualifiedName] {
		return "", false
	}
	if id, ok := i.byTool[qualifiedName]; ok {
		return id, true
	}
	id, ok := i.byNamespace[namespaceID]
	return id, ok
}

func resolveToolsets(
	spec *registry.Spec,
	namespaces map[string]registry.NamespaceSpec,
) (toolsetIndex, error) {
	index := toolsetIndex{
		byTool:      map[string]string{},
		byNamespace: map[string]string{},
		excluded:    map[string]bool{},
	}

	for _, ts := range spec.Toolsets {
		for _, nsID := range ts.Namespaces {
			if _, ok := namespaces[nsID]; !ok {
				return index, fmt.Errorf(
					"snapshotter: toolset %q references unknown namespace %q", ts.ID, nsID)
			}
			if prior, dup := index.byNamespace[nsID]; dup {
				return index, fmt.Errorf(
					"snapshotter: namespace %q is claimed by toolsets %q and %q; its tools "+
						"would have two authorization scopes, and a grant on one would "+
						"silently not cover the other",
					nsID, prior, ts.ID)
			}
			index.byNamespace[nsID] = ts.ID
		}

		for _, qualified := range ts.Tools {
			if prior, dup := index.byTool[qualified]; dup {
				return index, fmt.Errorf(
					"snapshotter: tool %q is named by toolsets %q and %q",
					qualified, prior, ts.ID)
			}
			index.byTool[qualified] = ts.ID
		}

		for _, qualified := range ts.Exclude {
			index.excluded[qualified] = true
		}
	}
	return index, nil
}
