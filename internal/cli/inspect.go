// Copyright 2026 The MCPDoll Authors.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
)

// The read-only inspection commands.
//
// Each is the CLI half of an API operation, and each answers a question an
// operator actually asks: what is registered, what is the gateway serving, which
// plugins are running and in what rollout state.

// -------------------------------------------------------------- registry -----

func newRegistryShowCmd(env *Env) *cobra.Command {
	var registryPath string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the registry document as structured data",
		Long: "Reads the registry and renders it as JSON, YAML, or a summary table.\n" +
			"Useful for piping into `jq`, and for seeing the resolved shape rather than\n" +
			"the source formatting.",
		Annotations: map[string]string{annotationOperation: "getRegistry"},
		RunE: func(_ *cobra.Command, _ []string) error {
			spec, err := registry.Load(registryPath)
			if err != nil {
				return configError(err)
			}
			return env.Emit(newRegistryView(spec))
		},
	}
	cmd.Flags().StringVarP(&registryPath, "registry", "r", "registry.yaml", "registry document")
	return cmd
}

type registryView struct {
	Org        string          `json:"org" yaml:"org"`
	Version    int64           `json:"version" yaml:"version"`
	Namespaces []namespaceView `json:"namespaces" yaml:"namespaces"`
	Servers    []serverView    `json:"servers" yaml:"servers"`
	Bundles    []bundleView    `json:"bundles" yaml:"bundles"`
	Audiences  []audienceView  `json:"audiences" yaml:"audiences"`
	Policies   []policyView    `json:"policies,omitempty" yaml:"policies,omitempty"`
	Plugins    []pluginView    `json:"plugins,omitempty" yaml:"plugins,omitempty"`
}

type namespaceView struct {
	ID            string `json:"id" yaml:"id"`
	Name          string `json:"name" yaml:"name"`
	Prefix        string `json:"prefix" yaml:"prefix"`
	OwnerIdpGroup string `json:"owner_idp_group,omitempty" yaml:"owner_idp_group,omitempty"`
	Team          string `json:"team,omitempty" yaml:"team,omitempty"`
	Project       string `json:"project,omitempty" yaml:"project,omitempty"`
}

type serverView struct {
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
	// Overrides names the tools whose classification the registry states
	// explicitly, which is where a reviewer's attention belongs.
	Overrides map[string]string `json:"tool_overrides,omitempty" yaml:"tool_overrides,omitempty"`
	Excluded  []string          `json:"excluded_tools,omitempty" yaml:"excluded_tools,omitempty"`
}

type bundleView struct {
	ID          string   `json:"id" yaml:"id"`
	Name        string   `json:"name" yaml:"name"`
	Priority    int32    `json:"priority" yaml:"priority"`
	TokenBudget int32    `json:"token_budget,omitempty" yaml:"token_budget,omitempty"`
	Namespaces  []string `json:"namespaces" yaml:"namespaces"`
}

type audienceView struct {
	ID               string   `json:"id" yaml:"id"`
	Slug             string   `json:"slug" yaml:"slug"`
	Name             string   `json:"name,omitempty" yaml:"name,omitempty"`
	Bundles          []string `json:"bundles" yaml:"bundles"`
	Policies         []string `json:"policies,omitempty" yaml:"policies,omitempty"`
	AllowedIdpGroups []string `json:"allowed_idp_groups,omitempty" yaml:"allowed_idp_groups,omitempty"`
}

type policyView struct {
	ID        string `json:"id" yaml:"id"`
	Name      string `json:"name" yaml:"name"`
	Priority  int32  `json:"priority" yaml:"priority"`
	RuleCount int    `json:"rule_count" yaml:"rule_count"`
}

type pluginView struct {
	ID                string   `json:"id" yaml:"id"`
	Name              string   `json:"name" yaml:"name"`
	Version           string   `json:"version,omitempty" yaml:"version,omitempty"`
	Runtime           string   `json:"runtime" yaml:"runtime"`
	Hooks             []string `json:"hooks" yaml:"hooks"`
	Priority          int32    `json:"priority" yaml:"priority"`
	Rollout           string   `json:"rollout" yaml:"rollout"`
	CanaryPercent     int32    `json:"canary_percent,omitempty" yaml:"canary_percent,omitempty"`
	Reads             []string `json:"reads,omitempty" yaml:"reads,omitempty"`
	Writes            []string `json:"writes,omitempty" yaml:"writes,omitempty"`
	IdentityDependent bool     `json:"identity_dependent,omitempty" yaml:"identity_dependent,omitempty"`
	ArtifactDigest    string   `json:"artifact_digest,omitempty" yaml:"artifact_digest,omitempty"`
}

func newRegistryView(spec *registry.Spec) registryView {
	out := registryView{Org: spec.Org, Version: spec.Version}
	for _, ns := range spec.Namespaces {
		out.Namespaces = append(out.Namespaces, namespaceView{
			ID: ns.ID, Name: ns.Name, Prefix: ns.Prefix,
			OwnerIdpGroup: ns.OwnerIdpGroup, Team: ns.Team, Project: ns.Project,
		})
	}
	for _, srv := range spec.Servers {
		mode := srv.ServingMode
		if mode == "" {
			mode = "strict"
		}
		view := serverView{
			ID: srv.ID, Name: srv.Name, Namespace: srv.Namespace,
			Endpoint: srv.Endpoint, ServingMode: mode,
			Criticality: srv.Criticality, DataClassification: srv.DataClassification,
			ComplianceScope:    srv.ComplianceScope,
			DefaultEffectClass: srv.DefaultEffectClass, CanaryTool: srv.CanaryTool,
		}
		for name, tool := range srv.Tools {
			if tool.Exclude {
				view.Excluded = append(view.Excluded, name)
				continue
			}
			if view.Overrides == nil {
				view.Overrides = map[string]string{}
			}
			view.Overrides[name] = tool.EffectClass
		}
		sort.Strings(view.Excluded)
		out.Servers = append(out.Servers, view)
	}
	for _, b := range spec.Bundles {
		view := bundleView{ID: b.ID, Name: b.Name, Priority: b.Priority, TokenBudget: b.TokenBudget}
		for _, entry := range b.Entries {
			view.Namespaces = append(view.Namespaces, entry.Namespace)
		}
		out.Bundles = append(out.Bundles, view)
	}
	for _, a := range spec.Audiences {
		out.Audiences = append(out.Audiences, audienceView{
			ID: a.ID, Slug: a.Slug, Name: a.Name,
			Bundles: a.Bundles, Policies: a.Policies, AllowedIdpGroups: a.AllowedIdpGroups,
		})
	}
	for _, p := range spec.Policies {
		out.Policies = append(out.Policies, policyView{
			ID: p.ID, Name: p.Name, Priority: p.Priority, RuleCount: len(p.Rules),
		})
	}
	for _, p := range spec.Plugins {
		rollout := p.Rollout
		if rollout == "" {
			rollout = "shadow"
		}
		out.Plugins = append(out.Plugins, pluginView{
			ID: p.ID, Name: p.Name, Version: p.Version, Runtime: p.Runtime,
			Hooks: p.Hooks, Priority: p.Priority, Rollout: rollout,
			CanaryPercent: p.CanaryPercent, Reads: p.Reads, Writes: p.Writes,
			IdentityDependent: p.IdentityDependent, ArtifactDigest: p.ArtifactDigest,
		})
	}
	return out
}

func (r registryView) Table() Table {
	rows := make([][]string, 0, len(r.Audiences))
	for _, a := range r.Audiences {
		groups := "any authenticated"
		if len(a.AllowedIdpGroups) > 0 {
			groups = strings.Join(a.AllowedIdpGroups, ",")
		}
		rows = append(rows, []string{
			a.Slug, a.Name, strings.Join(a.Bundles, ","), groups,
		})
	}
	return Table{
		Title:   fmt.Sprintf("%s registry, version %d", r.Org, r.Version),
		Columns: []string{"AUDIENCE", "NAME", "BUNDLES", "ALLOWED GROUPS"},
		Rows:    rows,
		Notes: []string{
			fmt.Sprintf("%d namespace(s), %d server(s), %d bundle(s), %d policy(s), %d plugin(s)",
				len(r.Namespaces), len(r.Servers), len(r.Bundles), len(r.Policies), len(r.Plugins)),
			"use --output json for the full document",
		},
	}
}

func newRegistryServersCmd(env *Env) *cobra.Command {
	var registryPath string
	cmd := &cobra.Command{
		Use:         "servers",
		Aliases:     []string{"backends"},
		Short:       "List the registered backends",
		Annotations: map[string]string{annotationOperation: "listServers"},
		RunE: func(_ *cobra.Command, _ []string) error {
			spec, err := registry.Load(registryPath)
			if err != nil {
				return configError(err)
			}
			return env.Emit(serverList{Servers: newRegistryView(spec).Servers})
		},
	}
	cmd.Flags().StringVarP(&registryPath, "registry", "r", "registry.yaml", "registry document")
	cmd.AddCommand(newRegistryServerShowCmd(env))
	return cmd
}

type serverList struct {
	Servers []serverView `json:"servers" yaml:"servers"`
}

func (l serverList) Table() Table {
	rows := make([][]string, 0, len(l.Servers))
	for _, s := range l.Servers {
		rows = append(rows, []string{
			s.Name, s.Namespace, s.ServingMode, s.DefaultEffectClass,
			s.DataClassification, s.Endpoint,
		})
	}
	return Table{
		Columns: []string{"BACKEND", "NAMESPACE", "MODE", "DEFAULT EFFECT", "CLASSIFICATION", "ENDPOINT"},
		Rows:    rows,
	}
}

func newRegistryServerShowCmd(env *Env) *cobra.Command {
	var registryPath string
	cmd := &cobra.Command{
		Use:         "show <server-id>",
		Short:       "Read one backend's registration",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperation: "getServer"},
		RunE: func(_ *cobra.Command, args []string) error {
			spec, err := registry.Load(registryPath)
			if err != nil {
				return configError(err)
			}
			for _, s := range newRegistryView(spec).Servers {
				if s.ID == args[0] || s.Name == args[0] {
					return env.Emit(s)
				}
			}
			return notFoundError(fmt.Errorf("no server %q in %s", args[0], registryPath))
		},
	}
	cmd.Flags().StringVarP(&registryPath, "registry", "r", "registry.yaml", "registry document")
	return cmd
}

func (s serverView) Table() Table {
	rows := [][]string{
		{"id", s.ID},
		{"name", s.Name},
		{"namespace", s.Namespace},
		{"endpoint", s.Endpoint},
		{"serving_mode", s.ServingMode},
		{"default_effect_class", s.DefaultEffectClass},
	}
	if s.Criticality != "" {
		rows = append(rows, []string{"criticality", s.Criticality})
	}
	if s.DataClassification != "" {
		rows = append(rows, []string{"data_classification", s.DataClassification})
	}
	if len(s.ComplianceScope) > 0 {
		rows = append(rows, []string{"compliance_scope", strings.Join(s.ComplianceScope, ",")})
	}
	if s.CanaryTool != "" {
		rows = append(rows, []string{"canary_tool", s.CanaryTool})
	}
	for name, effect := range s.Overrides {
		rows = append(rows, []string{"tool " + name, effect})
	}
	for _, name := range s.Excluded {
		rows = append(rows, []string{"tool " + name, "excluded"})
	}
	return Table{Columns: []string{"FIELD", "VALUE"}, Rows: rows}
}

// --------------------------------------------------------------- plugins -----

func newPluginsCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Inspect pipeline plugins and their rollout state",
	}
	cmd.AddCommand(newPluginsListCmd(env))
	return cmd
}

func newPluginsListCmd(env *Env) *cobra.Command {
	var registryPath string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the plugins and their rollout state",
		Long: "Rollout is the field to read: `shadow` means a plugin runs and is recorded\n" +
			"but changes nothing, `enforce` means it acts. Promote only after reading a\n" +
			"plugin's shadow divergences.",
		Annotations: map[string]string{annotationOperation: "listPlugins"},
		RunE: func(_ *cobra.Command, _ []string) error {
			spec, err := registry.Load(registryPath)
			if err != nil {
				return configError(err)
			}
			return env.Emit(pluginList{Plugins: newRegistryView(spec).Plugins})
		},
	}
	cmd.Flags().StringVarP(&registryPath, "registry", "r", "registry.yaml", "registry document")
	return cmd
}

type pluginList struct {
	Plugins []pluginView `json:"plugins" yaml:"plugins"`
}

func (l pluginList) Table() Table {
	rows := make([][]string, 0, len(l.Plugins))
	var enforcing int
	for _, p := range l.Plugins {
		rollout := p.Rollout
		if rollout == "canary" {
			rollout = fmt.Sprintf("canary(%d%%)", p.CanaryPercent)
		}
		if p.Rollout == "enforce" {
			enforcing++
		}
		writes := strings.Join(p.Writes, ",")
		if writes == "" {
			writes = "— (read-only)"
		}
		rows = append(rows, []string{
			p.Name, p.Runtime, strings.Join(p.Hooks, ","),
			strconv.Itoa(int(p.Priority)), rollout, writes,
		})
	}
	notes := []string{
		fmt.Sprintf("%d plugin(s), %d enforcing", len(l.Plugins), enforcing),
	}
	for _, p := range l.Plugins {
		if p.IdentityDependent {
			notes = append(notes, fmt.Sprintf(
				"%s is identity-dependent; at on_catalog it forces cacheScope: private", p.Name))
		}
	}
	return Table{
		Columns: []string{"PLUGIN", "RUNTIME", "HOOKS", "PRIORITY", "ROLLOUT", "WRITES"},
		Rows:    rows,
		Notes:   notes,
	}
}

// --------------------------------------------------------------- gateway -----

func newGatewayAudiencesCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "audiences",
		Short:       "List the audiences a data plane serves",
		Annotations: map[string]string{annotationOperation: "listAudiences"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()

			url := strings.TrimRight(env.GatewayURL(), "/") + "/readyz"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return unavailableError(fmt.Errorf("cannot reach the data plane at %s: %w",
					env.GatewayURL(), err))
			}
			defer resp.Body.Close()

			var payload struct {
				Version   int64 `json:"snapshot_version"`
				Audiences int   `json:"audiences"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				return unavailableError(fmt.Errorf("%s returned an unreadable body: %w", url, err))
			}
			if resp.StatusCode != http.StatusOK {
				return unavailableError(fmt.Errorf("the data plane is not ready"))
			}

			// The readiness endpoint reports a count, not names: the data plane
			// deliberately exposes no enumeration of its audiences, since that
			// would tell an unauthenticated caller which endpoints exist. Names
			// come from the snapshot, which is the authoritative source anyway.
			return env.Emit(audienceListReport{
				GatewayURL:      env.GatewayURL(),
				SnapshotVersion: payload.Version,
				Count:           payload.Audiences,
			})
		},
	}
	return cmd
}

type audienceListReport struct {
	GatewayURL      string `json:"gateway_url" yaml:"gateway_url"`
	SnapshotVersion int64  `json:"snapshot_version" yaml:"snapshot_version"`
	Count           int    `json:"audiences" yaml:"audiences"`
}

func (r audienceListReport) Table() Table {
	return Table{
		Columns: []string{"GATEWAY", "SNAPSHOT", "AUDIENCES"},
		Rows: [][]string{{
			r.GatewayURL, strconv.FormatInt(r.SnapshotVersion, 10), strconv.Itoa(r.Count),
		}},
		Notes: []string{
			"the data plane reports a count, not names — enumerating endpoints to an",
			"unauthenticated caller would be an information leak. Use",
			"`mcpdoll snapshot inspect` for the names.",
		},
	}
}

// ---------------------------------------------------------------- system -----

func newSystemCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Control-plane health and metadata",
	}
	cmd.AddCommand(newSystemHealthCmd(env))
	return cmd
}

func newSystemHealthCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "health",
		Short:       "Check that the control-plane API is alive",
		Annotations: map[string]string{annotationOperation: "getHealth"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()

			url := strings.TrimRight(env.APIURL, "/") + "/healthz"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return unavailableError(fmt.Errorf("cannot reach the control plane at %s: %w",
					env.APIURL, err))
			}
			defer resp.Body.Close()

			var health healthReport
			if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
				return unavailableError(fmt.Errorf("%s returned an unreadable body: %w", url, err))
			}
			health.APIURL = env.APIURL
			if resp.StatusCode != http.StatusOK {
				_ = env.Emit(health)
				return unavailableError(fmt.Errorf("the control plane reported %q", health.Status))
			}
			return env.Emit(health)
		},
	}
	return cmd
}

type healthReport struct {
	APIURL  string `json:"api_url" yaml:"api_url"`
	Status  string `json:"status" yaml:"status"`
	Version string `json:"version" yaml:"version"`
}

func (r healthReport) Table() Table {
	return Table{
		Columns: []string{"CONTROL PLANE", "STATUS", "VERSION"},
		Rows:    [][]string{{r.APIURL, r.Status, r.Version}},
	}
}

// -------------------------------------------------------- snapshot current ---

func newSnapshotCurrentCmd(env *Env) *cobra.Command {
	var (
		showTools bool
		override  string
	)
	cmd := &cobra.Command{
		Use:   "current",
		Short: "Show the snapshot the gateway is serving",
		Long: "Reads the snapshot file the local data plane is configured with. Distinct\n" +
			"from `snapshot inspect <file>`, which reads any file you point it at.",
		Annotations: map[string]string{annotationOperation: "getCurrentSnapshot"},
		RunE: func(_ *cobra.Command, _ []string) error {
			path := env.snapshotPath
			if override != "" {
				path = override
			}
			signed, err := snapshot.ReadSignedSnapshot(path)
			if err != nil {
				return notFoundError(fmt.Errorf(
					"%w\n\nPoint --snapshot at the file your data plane is serving "+
						"(dataplane.snapshot_path in its config)", err))
			}
			snap, err := snapshot.ParseUnverified(signed)
			if err != nil {
				return err
			}
			view, err := snapshot.Build(snap)
			if err != nil {
				env.Printf("warning: this snapshot would not activate: %v\n", err)
			}
			return env.Emit(newInspectReport(path, signed, snap, view, showTools))
		},
	}
	cmd.Flags().BoolVar(&showTools, "tools", false, "list every tool rather than summarising")
	cmd.Flags().StringVar(&override, "snapshot", "",
		"snapshot file (default: the profile's snapshot_path, or ./snapshot.pb)")
	return cmd
}
