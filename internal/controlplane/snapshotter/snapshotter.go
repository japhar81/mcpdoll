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
	ServerID          string
	ServerName        string
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

	b := snapshot.NewBuilder(spec.Org, spec.Version).
		WithID(ids.New(ids.KindSnapshot)).
		WithCatalogDefaults(spec.Catalog.TTL, spec.Catalog.DegradedTTL)

	// The registry digest lets a snapshot be traced back to the document that
	// produced it, which is what makes "rebuild and compare" a usable audit step.
	registryDigest, err := canonical.DigestOf(spec)
	if err != nil {
		return nil, fmt.Errorf("snapshotter: digesting the registry document: %w", err)
	}
	b.WithRegistryDigest(registryDigest.String())

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

	// Collisions are detected across the whole build, not per server: two
	// different backends in the same namespace can both publish `lookup`.
	claimed := map[string]string{}

	for _, srv := range spec.Servers {
		ns := namespacesByID[srv.Namespace]
		mode, err := registry.ParseServingMode(srv.ServingMode)
		if err != nil {
			return nil, fmt.Errorf("snapshotter: server %q: %w", srv.ID, err)
		}
		b.AddServer(&snapshotpb.Server{
			Id:                    srv.ID,
			Name:                  srv.Name,
			NamespaceId:           srv.Namespace,
			Endpoint:              srv.Endpoint,
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

		report := reports[srv.ID]
		if report == nil {
			// Unreachable and allowed: recorded as a warning, and the server is
			// still registered so its identity and configuration survive.
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("server %q (%s) was unreachable; its tools are absent from this snapshot",
					srv.Name, srv.Endpoint))
			continue
		}

		defaultEffect, err := registry.ParseEffectClass(srv.DefaultEffectClass)
		if err != nil {
			return nil, fmt.Errorf("snapshotter: server %q: %w", srv.ID, err)
		}

		// Sort by name so the build is deterministic regardless of the order a
		// backend happened to list its tools in. Catalog order is a contract
		// (ADR 0010), and it should not depend on a backend's map iteration.
		tools := make([]*mcpToolWithDef, 0, len(report.tools))
		tools = append(tools, report.tools...)
		sort.Slice(tools, func(i, j int) bool { return tools[i].def.Name < tools[j].def.Name })

		summary := BackendReport{
			ServerID:          srv.ID,
			ServerName:        srv.Name,
			Endpoint:          srv.Endpoint,
			NegotiatedVersion: report.negotiated,
			ToolCount:         len(tools),
			ObservedAt:        report.observedAt,
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
					"snapshotter: qualified name %q is %d characters, over the %d-character budget; "+
						"shorten the namespace prefix or exclude the tool",
					qualified, len(qualified), registry.MaxQualifiedNameLength)
			}
			if prior, dup := claimed[qualified]; dup {
				return nil, fmt.Errorf(
					"snapshotter: %q is published by both %q and %q; "+
						"resolve the collision by excluding one or moving it to another namespace "+
						"(MCPDoll never auto-renames a tool, because clients depend on the name)",
					qualified, prior, srv.Name)
			}
			claimed[qualified] = srv.Name

			b.AddTool(snapshot.ToolInput{
				ServerID:     srv.ID,
				NamespaceID:  srv.Namespace,
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
				"server %q negotiated %s; capabilities added in 2026-07-28 are unavailable for it",
				srv.Name, report.negotiated))
		}
		result.Discovered = append(result.Discovered, summary)
	}

	for _, bundle := range spec.Bundles {
		entries := make([]*snapshotpb.BundleEntry, 0, len(bundle.Entries))
		for _, entry := range bundle.Entries {
			ns := namespacesByID[entry.Namespace]
			pbEntry := &snapshotpb.BundleEntry{NamespaceId: entry.Namespace}
			// The document names tools unqualified; the snapshot uses qualified
			// names. Qualifying here means the document stays readable and the
			// snapshot stays unambiguous.
			for _, name := range entry.Tools {
				pbEntry.QualifiedNames = append(pbEntry.QualifiedNames, ns.Prefix+"."+name)
			}
			for _, name := range entry.Exclude {
				pbEntry.ExcludeQualifiedNames = append(pbEntry.ExcludeQualifiedNames, ns.Prefix+"."+name)
			}
			entries = append(entries, pbEntry)
		}
		b.AddBundle(&snapshotpb.Bundle{
			Id:          bundle.ID,
			Name:        bundle.Name,
			ProjectId:   bundle.Project,
			Priority:    bundle.Priority,
			Entries:     entries,
			TokenBudget: bundle.TokenBudget,
			TtlMs:       int32(bundle.TTL.Milliseconds()),
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

	for _, aud := range spec.Audiences {
		b.AddAudience(&snapshotpb.Audience{
			Id:               aud.ID,
			Slug:             aud.Slug,
			Name:             aud.Name,
			ProjectId:        aud.Project,
			BundleIds:        aud.Bundles,
			PolicyIds:        aud.Policies,
			AllowedIdpGroups: aud.AllowedIdpGroups,
			RateLimits:       rateLimits(aud.RateLimits),
		})
	}

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
	tools      []*mcpToolWithDef
}

// mcpToolWithDef pairs the canonical definition with nothing else; the wrapper
// exists so the slice can be sorted by name without re-deriving it.
type mcpToolWithDef struct {
	def *canonical.ToolDefinition
}

// discoverAll probes every backend concurrently.
func discoverAll(
	ctx context.Context,
	spec *registry.Spec,
	opts Options,
) (map[string]*discoveryReport, error) {
	type outcome struct {
		serverID string
		report   *discoveryReport
		err      error
	}

	sem := make(chan struct{}, opts.Concurrency)
	results := make(chan outcome, len(spec.Servers))
	var wg sync.WaitGroup

	for _, srv := range spec.Servers {
		wg.Add(1)
		go func(srv registry.ServerSpec) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- outcome{serverID: srv.ID, err: ctx.Err()}
				return
			}

			discovered, err := mcpadapter.Discover(ctx, mcpadapter.DiscoverOptions{
				Endpoint: srv.Endpoint,
				Timeout:  opts.DiscoverTimeout,
			})
			if err != nil {
				results <- outcome{serverID: srv.ID, err: err}
				return
			}

			report := &discoveryReport{
				negotiated: discovered.NegotiatedVersion,
				observedAt: discovered.ObservedAt,
			}
			for _, tool := range discovered.Tools {
				def, err := mcpadapter.ToCanonical(tool)
				if err != nil {
					results <- outcome{serverID: srv.ID,
						err: fmt.Errorf("tool %q: %w", tool.Name, err)}
					return
				}
				report.tools = append(report.tools, &mcpToolWithDef{def: def})
			}
			results <- outcome{serverID: srv.ID, report: report}
		}(srv)
	}

	wg.Wait()
	close(results)

	reports := map[string]*discoveryReport{}
	var failures []string
	for out := range results {
		if out.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", out.serverID, out.err))
			continue
		}
		reports[out.serverID] = out.report
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

func rateLimits(spec *registry.RateLimitSpec) *snapshotpb.RateLimits {
	if spec == nil {
		return nil
	}
	return &snapshotpb.RateLimits{
		RequestsPerMinute:  spec.RequestsPerMinute,
		ConcurrentRequests: spec.ConcurrentRequests,
		TokensPerMinute:    spec.TokensPerMinute,
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
		AudienceIds:       spec.Audiences,
	}, nil
}
