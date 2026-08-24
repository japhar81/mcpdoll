// Copyright 2026 The MCPDoll Authors.

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The read commands are thin, so the tests are about the two things that are
// easy to get wrong: which fields reach the operator, and whether the command
// tree still satisfies the tri-surface rule.

func TestRegistryShowRendersTheDocument(t *testing.T) {
	t.Parallel()
	path := writeRegistryFixture(t)

	out, errOut, code := runCLI(t, "registry", "show", "-r", path, "--output", "json")
	require.Equal(t, ExitOK, code, errOut)

	var view registryView
	require.NoError(t, json.Unmarshal([]byte(out), &view))
	require.Equal(t, "org_test", view.Org)
	require.Len(t, view.Servers, 1)
	require.Equal(t, "crm-prod", view.Servers[0].Name)

	// An unset serving_mode reads as "strict" everywhere else in the system;
	// showing an empty cell here would suggest it is unset at runtime too.
	require.Equal(t, "strict", view.Servers[0].ServingMode)
}

func TestRegistryShowSurfacesExclusionsSeparatelyFromOverrides(t *testing.T) {
	t.Parallel()
	path := writeRegistryFixture(t)

	out, errOut, code := runCLI(t, "registry", "show", "-r", path, "--output", "json")
	require.Equal(t, ExitOK, code, errOut)

	var view registryView
	require.NoError(t, json.Unmarshal([]byte(out), &view))

	// An excluded tool is not an override with an empty effect class: conflating
	// them would show "" as a classification and read as unclassified.
	require.Equal(t, []string{"delete_customer"}, view.Servers[0].ExcludedTools)
	require.Equal(t, map[string]string{"update_customer": "write"}, view.Servers[0].ToolOverrides)
}

func TestRegistryServersShowReportsMissingServerAsNotFound(t *testing.T) {
	t.Parallel()
	path := writeRegistryFixture(t)

	_, errOut, code := runCLI(t, "registry", "servers", "show", "absent", "-r", path)
	require.Equal(t, ExitNotFound, code)
	require.Contains(t, errOut, "no server")
}

func TestRegistryServersShowAcceptsIDOrName(t *testing.T) {
	t.Parallel()
	path := writeRegistryFixture(t)

	byID, _, code := runCLI(t, "registry", "servers", "show", "srv_crm", "-r", path, "--output", "json")
	require.Equal(t, ExitOK, code)
	byName, _, code := runCLI(t, "registry", "servers", "show", "crm-prod", "-r", path, "--output", "json")
	require.Equal(t, ExitOK, code)
	require.JSONEq(t, byID, byName)
}

func TestPluginsListDefaultsRolloutToShadow(t *testing.T) {
	t.Parallel()
	path := writeRegistryFixture(t)

	out, errOut, code := runCLI(t, "plugins", "list", "-r", path, "--output", "json")
	require.Equal(t, ExitOK, code, errOut)

	var list pluginList
	require.NoError(t, json.Unmarshal([]byte(out), &list))
	require.Len(t, list.Plugins, 1)

	// The registry omits `rollout`. Reporting "" would let an operator believe a
	// plugin is inert when the engine will in fact run it in shadow — and worse,
	// would make an *enforcing* omission indistinguishable from a shadow one.
	require.Equal(t, "shadow", list.Plugins[0].Rollout)
}

func TestPluginsListTableFlagsIdentityDependence(t *testing.T) {
	t.Parallel()
	path := writeRegistryFixture(t)

	out, _, code := runCLI(t, "plugins", "list", "-r", path)
	require.Equal(t, ExitOK, code)

	// Identity dependence is the reason a catalog cannot be cached publicly, so
	// it belongs in the human output rather than only in JSON.
	require.Contains(t, out, "identity-dependent")
	require.Contains(t, out, "cacheScope: private")
}

func TestSystemHealthReportsAnUnreachableControlPlane(t *testing.T) {
	t.Parallel()

	// Port 1 is reserved and never listening, so this exercises the dial failure
	// rather than depending on nothing running on a common port.
	_, errOut, code := runCLI(t, "system", "health", "--api-url", "http://127.0.0.1:1")
	require.Equal(t, ExitUnavailable, code)
	require.Contains(t, errOut, "cannot reach the control plane")
}

func TestEveryCommandWithAnOperationIsUnique(t *testing.T) {
	t.Parallel()

	out, _, code := runCLI(t, "__commands", "--json")
	require.Equal(t, ExitOK, code)

	var tree CommandTree
	require.NoError(t, json.Unmarshal([]byte(out), &tree))

	// Two commands claiming one operation makes the parity check pass while
	// leaving the CLI ambiguous about which one is the real surface.
	seen := map[string]string{}
	for _, c := range tree.Commands {
		if c.Operation == "" {
			continue
		}
		if prev, dup := seen[c.Operation]; dup {
			t.Fatalf("operation %q is claimed by both %q and %q", c.Operation, prev, c.Path)
		}
		seen[c.Operation] = c.Path
	}
	require.NotEmpty(t, seen)
}

func TestCommandsIncludesRunnableParents(t *testing.T) {
	t.Parallel()

	out, _, code := runCLI(t, "__commands", "--json")
	require.Equal(t, ExitOK, code)

	var tree CommandTree
	require.NoError(t, json.Unmarshal([]byte(out), &tree))

	// `registry servers` both lists and holds `show`. An earlier walker treated
	// any parent as navigation, which silently cost listServers its CLI surface.
	var found bool
	for _, c := range tree.Commands {
		if c.Path == "mcpdoll registry servers" {
			found = true
			require.Equal(t, "listServers", c.Operation)
		}
	}
	require.True(t, found, "a runnable parent command was dropped from the manifest")
}

func TestEmptyConfigFileIsNotAnError(t *testing.T) {
	t.Parallel()

	// `touch ~/.mcpdoll/config.yaml` is a thing people do, and it used to break
	// every command with a bare "EOF".
	empty := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))

	_, errOut, code := runCLI(t, "--config", empty, "registry", "hooks")
	require.Equal(t, ExitOK, code, errOut)
}

func writeRegistryFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.TrimSpace(`
org: org_test
version: 1

catalog:
  ttl: 60s

namespaces:
  - id: ns_crm
    name: crm
    prefix: crm

servers:
  - id: srv_crm
    name: crm-prod
    namespace: ns_crm
    bindings:
      - tenant: acme
        primary: http://localhost:9101
    default_effect_class: read
    data_classification: confidential
    tools:
      update_customer:
        effect_class: write
      delete_customer:
        exclude: true

toolsets:
  - id: bnd_support
    name: support
    priority: 10
    namespaces: [ns_crm]


plugins:
  - id: plg_ent
    name: entitlements
    runtime: wasm
    hooks: [on_catalog]
    priority: 20
    identity_dependent: true
    reads: [principal, catalog]
    writes: [catalog]
    artifact_ref: file:///dev/null
    artifact_digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
`)+"\n"), 0o600))
	return path
}
