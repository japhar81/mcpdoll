// Copyright 2026 Henry Zektser.

package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/cli"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/apiserver"
)

// The tri-surface law says a feature exists on the API, the CLI, and the UI.
// tools/paritycheck proves each operation *has* a command and a route. These
// tests prove something the parity tool cannot see: that the command and the
// endpoint return the same thing.
//
// Without this, both surfaces exist and disagree, which is worse than one
// surface — an operator who checks the console and the CLI and gets different
// answers has no way to tell which is lying.

const token = "parity-test-token"

func TestCLIAndAPIReturnIdenticalRegistryJSON(t *testing.T) {
	t.Parallel()
	path := writeRegistry(t)

	fromCLI := runCLIJSON(t, "registry", "show", "-r", path, "--output", "json")
	fromAPI := getAPIJSON(t, path, "/api/v1/registry")

	require.JSONEq(t, string(fromAPI), string(fromCLI),
		"`mcpdoll registry show` and GET /api/v1/registry disagree")
}

func TestCLIAndAPIReturnIdenticalServerLists(t *testing.T) {
	t.Parallel()
	path := writeRegistry(t)

	fromCLI := runCLIJSON(t, "registry", "servers", "-r", path, "--output", "json")
	fromAPI := getAPIJSON(t, path, "/api/v1/registry/servers")

	require.JSONEq(t, string(fromAPI), string(fromCLI))
}

func TestCLIAndAPIReturnIdenticalSingleServers(t *testing.T) {
	t.Parallel()
	path := writeRegistry(t)

	fromCLI := runCLIJSON(t, "registry", "servers", "show", "srv_crm", "-r", path, "--output", "json")
	fromAPI := getAPIJSON(t, path, "/api/v1/registry/servers/srv_crm")

	require.JSONEq(t, string(fromAPI), string(fromCLI))
}

func TestCLIAndAPIReturnIdenticalPluginLists(t *testing.T) {
	t.Parallel()
	path := writeRegistry(t)

	fromCLI := runCLIJSON(t, "plugins", "list", "-r", path, "--output", "json")
	fromAPI := getAPIJSON(t, path, "/api/v1/plugins")

	require.JSONEq(t, string(fromAPI), string(fromCLI))
}

func TestCLIAndAPIReturnIdenticalHookLists(t *testing.T) {
	t.Parallel()
	path := writeRegistry(t)

	fromCLI := runCLIJSON(t, "registry", "hooks", "--output", "json")
	fromAPI := getAPIJSON(t, path, "/api/v1/hooks")

	require.JSONEq(t, string(fromAPI), string(fromCLI))
}

func runCLIJSON(t *testing.T, args ...string) []byte {
	t.Helper()
	var out, errOut bytes.Buffer
	root := cli.New(cli.Options{
		Stdout:     &out,
		Stderr:     &errOut,
		ConfigPath: filepath.Join(t.TempDir(), "absent.yaml"),
	})
	root.SetArgs(args)
	require.NoError(t, root.Execute(), errOut.String())

	require.True(t, json.Valid(out.Bytes()), "CLI did not emit valid JSON: %s", out.String())
	return out.Bytes()
}

func getAPIJSON(t *testing.T, registryPath, path string) []byte {
	t.Helper()
	server, err := apiserver.New(apiserver.Config{
		RegistryPath: registryPath,
		Token:        token,
		Version:      "test",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.Bytes()
}

func writeRegistry(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.yaml")
	require.NoError(t, os.WriteFile(path, []byte(registryYAML), 0o600))
	return path
}

const registryYAML = `org: org_test
version: 3

catalog:
  ttl: 90s

namespaces:
  - id: ns_crm
    name: crm
    prefix: crm
    team: revenue
  - id: ns_hr
    name: hr
    prefix: hr

servers:
  - id: srv_crm
    name: crm-prod
    namespace: ns_crm
    bindings:
      - tenant: acme
        primary: http://localhost:9101
    default_effect_class: read
    data_classification: confidential
    compliance_scope: [sox]
    serving_mode: advisory
    tools:
      update_customer:
        effect_class: write
      delete_customer:
        exclude: true
  - id: srv_hr
    name: hr-legacy
    namespace: ns_hr
    bindings:
      - tenant: acme
        primary: http://localhost:9102
    default_effect_class: read

toolsets:
  - id: ts_support
    name: support
    priority: 10
    namespaces: [ns_crm]
  - id: ts_people
    name: people
    priority: 20
    namespaces: [ns_hr]


plugins:
  - id: plg_ent
    name: entitlements
    version: 1.2.0
    runtime: wasm
    hooks: [on_catalog, on_tool_call]
    priority: 20
    identity_dependent: true
    reads: [principal, catalog]
    writes: [catalog]
    artifact_ref: file:///dev/null
    artifact_digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  - id: plg_redact
    name: redact
    runtime: wasm
    hooks: [on_tool_result]
    priority: 50
    rollout: enforce
    reads: [result]
    writes: [result.content]
    artifact_ref: file:///dev/null
    artifact_digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"
`
