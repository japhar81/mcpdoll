// Copyright 2026 The MCPDoll Authors.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mcpdoll/mcpdoll/internal/api"
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
			return env.Emit(registryView{api.NewRegistry(spec)})
		},
	}
	cmd.Flags().StringVarP(&registryPath, "registry", "r", "registry.yaml", "registry document")
	return cmd
}

// The CLI renders the same structs the API returns. Table methods cannot be
// attached to another package's types, so each is wrapped in a one-field struct
// whose only job is to carry a Table method — which keeps the *data* single-
// sourced while leaving presentation here, where it belongs.

type registryView struct{ api.Registry }

type serverList struct {
	Servers []api.Server `json:"servers" yaml:"servers"`
}

type serverView struct{ api.Server }

type pluginList struct {
	Plugins []api.Plugin `json:"plugins" yaml:"plugins"`
}

func (r registryView) Table() Table {
	rows := make([][]string, 0, len(r.Toolsets))
	for _, ts := range r.Toolsets {
		sources := strings.Join(ts.Namespaces, ",")
		if len(ts.Tools) > 0 {
			sources += fmt.Sprintf(" +%d named", len(ts.Tools))
		}
		rows = append(rows, []string{
			ts.Name, strconv.Itoa(int(ts.Priority)), sources,
		})
	}
	return Table{
		Title:   fmt.Sprintf("%s registry, version %d", r.Org, r.Version),
		Columns: []string{"TOOLSET", "PRIORITY", "DRAWS FROM"},
		Rows:    rows,
		Notes: []string{
			fmt.Sprintf("%d namespace(s), %d server(s), %d toolset(s), %d policy(s), %d plugin(s)",
				len(r.Namespaces), len(r.Servers), len(r.Toolsets),
				len(r.Policies), len(r.Plugins)),
			"a toolset is what a grant names: t/<tenant>/ts/<toolset>",
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
			return env.Emit(serverList{Servers: api.NewRegistry(spec).Servers})
		},
	}
	cmd.Flags().StringVarP(&registryPath, "registry", "r", "registry.yaml", "registry document")
	cmd.AddCommand(newRegistryServerShowCmd(env))
	return cmd
}

func (l serverList) Table() Table {
	rows := make([][]string, 0, len(l.Servers))
	for _, s := range l.Servers {
		rows = append(rows, []string{
			s.Name, s.Namespace, s.ServingMode, s.DefaultEffectClass,
			s.DataClassification, strconv.Itoa(len(s.Bindings)),
		})
	}
	return Table{
		Columns: []string{"BACKEND", "NAMESPACE", "MODE", "DEFAULT EFFECT", "CLASSIFICATION", "TENANTS"},
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
			for _, srv := range api.NewRegistry(spec).Servers {
				if srv.ID == args[0] || srv.Name == args[0] {
					return env.Emit(serverView{srv})
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

		{"serving_mode", s.ServingMode},
		{"default_effect_class", s.DefaultEffectClass},
	}
	for _, b := range s.Bindings {
		hosts := b.Primary
		if n := len(b.Replicas); n > 0 {
			hosts += fmt.Sprintf(" (+%d replica)", n)
		}
		rows = append(rows, []string{"tenant " + b.Tenant, hosts})
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
	for name, effect := range s.ToolOverrides {
		rows = append(rows, []string{"tool " + name, effect})
	}
	for _, name := range s.ExcludedTools {
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
			return env.Emit(pluginList{Plugins: api.NewRegistry(spec).Plugins})
		},
	}
	cmd.Flags().StringVarP(&registryPath, "registry", "r", "registry.yaml", "registry document")
	return cmd
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

func newGatewayTenantsCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "audiences",
		Short:       "List the audiences a data plane serves",
		Annotations: map[string]string{annotationOperation: "listTenants"},
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
				Version int64 `json:"snapshot_version"`
				Tenants int   `json:"tenants"`
				Tools   int   `json:"tools"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				return unavailableError(fmt.Errorf("%s returned an unreadable body: %w", url, err))
			}
			if resp.StatusCode != http.StatusOK {
				return unavailableError(fmt.Errorf("the data plane is not ready"))
			}

			// A count, not names: the data plane deliberately exposes no
			// enumeration of its tenants, since that would tell an
			// unauthenticated caller who is hosted here. Names come from the
			// snapshot, which is the authoritative source anyway.
			return env.Emit(tenantListReport{
				GatewayURL:      env.GatewayURL(),
				SnapshotVersion: payload.Version,
				Tenants:         payload.Tenants,
				Tools:           payload.Tools,
			})
		},
	}
	return cmd
}

type tenantListReport struct {
	GatewayURL      string `json:"gateway_url" yaml:"gateway_url"`
	SnapshotVersion int64  `json:"snapshot_version" yaml:"snapshot_version"`
	Tenants         int    `json:"tenants" yaml:"tenants"`
	Tools           int    `json:"tools" yaml:"tools"`
}

func (r tenantListReport) Table() Table {
	return Table{
		Columns: []string{"GATEWAY", "SNAPSHOT", "TENANTS", "ADMITTED TOOLS"},
		Rows: [][]string{{
			r.GatewayURL, strconv.FormatInt(r.SnapshotVersion, 10),
			strconv.Itoa(r.Tenants), strconv.Itoa(r.Tools),
		}},
		Notes: []string{
			"a count, not names — enumerating tenants to an unauthenticated caller",
			"would be an information leak. Use `mcpdoll snapshot inspect` for names.",
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

// -------------------------------------------------------- gateway backends ---

func newGatewayBackendsCmd(env *Env) *cobra.Command {
	var showDrift bool
	cmd := &cobra.Command{
		Use:   "backends",
		Short: "Report what the gateway's prober knows about each backend",
		Long: "The gateway serves admitted definitions and never live backend output, so a\n" +
			"backend changing its catalog is a detectable event rather than a silent\n" +
			"change to what clients see. This is what the detector found.\n\n" +
			"Read the DRIFT and BLOCKED columns together: drift on a strict backend costs\n" +
			"tool calls, drift on an advisory one costs nothing but is still worth fixing.",
		Annotations: map[string]string{annotationOperation: "listBackends"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()

			url := strings.TrimRight(env.adminURL, "/") + "/admin/backends"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			if token := env.Token(); token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return unavailableError(fmt.Errorf(
					"cannot reach the data plane's admin listener at %s: %w\n\n"+
						"Backend health is served on the admin port, not the MCP one. "+
						"Set --admin-url or the profile's admin_url.", env.adminURL, err))
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return unavailableError(fmt.Errorf("%s returned %d", url, resp.StatusCode))
			}
			var report backendReport
			if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
				return unavailableError(fmt.Errorf("%s returned an unreadable body: %w", url, err))
			}
			report.showDrift = showDrift

			if err := env.Emit(report); err != nil {
				return err
			}
			// Non-zero when something is refusing calls, so a deploy gate can
			// branch on it without parsing the table.
			if report.Summary.BlockedTools > 0 {
				return validationError(fmt.Errorf(
					"%d tool(s) are refused because their backend drifted",
					report.Summary.BlockedTools))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showDrift, "drift", false,
		"list every drifted tool rather than counting them")
	return cmd
}

type backendReport struct {
	Summary  api.BackendHealthSummary `json:"summary" yaml:"summary"`
	Backends []api.BackendHealth      `json:"backends" yaml:"backends"`

	showDrift bool
}

func (r backendReport) Table() Table {
	if r.showDrift {
		rows := [][]string{}
		for _, b := range r.Backends {
			for _, d := range b.Drift {
				name := d.QualifiedName
				if name == "" {
					// An added tool has no qualified name, and printing a blank
					// cell reads as a bug rather than as the deliberate refusal
					// to invent one.
					name = d.Name + " (unadmitted)"
				}
				rows = append(rows, []string{b.ServerName, name, d.Kind, d.Detail})
			}
		}
		return Table{
			Columns: []string{"BACKEND", "TOOL", "KIND", "WHAT CHANGED"},
			Rows:    rows,
			Notes: []string{
				"semantic and removed drift block calls on a strict backend; " +
					"cosmetic and added never do",
			},
		}
	}

	rows := make([][]string, 0, len(r.Backends))
	for _, b := range r.Backends {
		blocked := 0
		for _, d := range b.Drift {
			if (d.Kind == "semantic" || d.Kind == "removed") && b.ServingMode == "strict" {
				blocked++
			}
		}
		latency := "—"
		if b.LatencyEWMAMs > 0 {
			latency = strconv.FormatInt(b.LatencyEWMAMs, 10) + "ms"
		}
		rows = append(rows, []string{
			b.ServerName, b.State, b.ServingMode,
			strconv.Itoa(len(b.Drift)), strconv.Itoa(blocked),
			latency, b.NegotiatedVersion,
		})
	}

	notes := []string{
		fmt.Sprintf("%d backend(s): %d healthy, %d degraded, %d unavailable, %d drifted, %d unknown",
			r.Summary.Total, r.Summary.Healthy, r.Summary.Degraded,
			r.Summary.Unavailable, r.Summary.Drifted, r.Summary.Unknown),
	}
	if r.Summary.BlockedTools > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d tool(s) are refused. Publish a snapshot built from the backends' "+
				"current catalogs, or roll the backends back.", r.Summary.BlockedTools))
	}
	notes = append(notes, "use --drift to see exactly what changed")

	return Table{
		Columns: []string{"BACKEND", "STATE", "MODE", "DRIFT", "BLOCKED", "LATENCY", "PROTOCOL"},
		Rows:    rows,
		Notes:   notes,
	}
}
