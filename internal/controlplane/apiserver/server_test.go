// Copyright 2026 Henry Zektser.

package apiserver_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/apiserver"
)

const testToken = "test-token-not-a-real-secret"

func newServer(t *testing.T, mutate func(*apiserver.Config)) http.Handler {
	t.Helper()
	cfg := apiserver.Config{
		RegistryPath: writeRegistry(t, registryYAML),
		Token:        testToken,
		Version:      "test",
		// Discard: a passing test should not print, and a failing one is
		// diagnosed from its assertion rather than from server chatter.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := apiserver.New(cfg)
	require.NoError(t, err)
	return s
}

func do(t *testing.T, h http.Handler, method, path string, body any, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ------------------------------------------------------------ construction ---

func TestNewRefusesToServeWithoutACredential(t *testing.T) {
	t.Parallel()

	// The whole point: this API can mint a signing key and republish the
	// serving snapshot. Leaving the token line out of a config file must not
	// silently produce an open one.
	_, err := apiserver.New(apiserver.Config{RegistryPath: "registry.yaml"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires a bearer token")
}

func TestNewRefusesAmbiguousAuthConfiguration(t *testing.T) {
	t.Parallel()

	_, err := apiserver.New(apiserver.Config{
		RegistryPath:   "registry.yaml",
		Token:          testToken,
		AllowAnonymous: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to guess")
}

func TestAllowAnonymousIsHonoured(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(c *apiserver.Config) {
		c.Token = ""
		c.AllowAnonymous = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hooks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

// -------------------------------------------------------------------- auth ---

func TestOperationsRequireTheToken(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	for _, path := range []string{
		"/api/v1/hooks", "/api/v1/registry", "/api/v1/registry/servers",
		"/api/v1/plugins", "/api/v1/snapshots/current", "/api/v1/gateway/status",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, path)
		require.Equal(t, `Bearer realm="mcpdoll"`, rec.Header().Get("WWW-Authenticate"), path)
	}
}

func TestHealthIsReachableWithoutAToken(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	// A load balancer has no credential. The response says nothing that is not
	// already implied by the port accepting connections.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var health api.Health
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &health))
	require.Equal(t, "ok", health.Status)
}

func TestAWrongTokenIsRejected(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	rec := do(t, h, http.MethodGet, "/api/v1/hooks", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testToken+"x")
	})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// -------------------------------------------------------------------- CORS ---

func TestUnlistedOriginsGetNoCORSGrant(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(c *apiserver.Config) {
		c.AllowedOrigins = []string{"http://localhost:5173"}
	})

	rec := do(t, h, http.MethodGet, "/api/v1/hooks", nil, func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example.com")
	})
	// No wildcard and no reflection: a page on any origin must not be able to
	// drive this API with an operator's credentials.
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestAllowedOriginGetsAGrantThatVariesCorrectly(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(c *apiserver.Config) {
		c.AllowedOrigins = []string{"http://localhost:5173"}
	})

	rec := do(t, h, http.MethodGet, "/api/v1/hooks", nil, func(r *http.Request) {
		r.Header.Set("Origin", "http://localhost:5173")
	})
	require.Equal(t, "http://localhost:5173", rec.Header().Get("Access-Control-Allow-Origin"))
	// Without Vary, a shared cache would hand one origin's grant to another.
	require.Contains(t, rec.Header().Values("Vary"), "Origin")
}

// ---------------------------------------------------------------- registry ---

func TestGetRegistryResolvesDefaults(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	rec := do(t, h, http.MethodGet, "/api/v1/registry", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var reg api.Registry
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reg))
	require.Equal(t, "org_test", reg.Org)
	require.Len(t, reg.Servers, 1)
	require.Equal(t, "strict", reg.Servers[0].ServingMode)
	require.Equal(t, "shadow", reg.Plugins[0].Rollout)
}

func TestAnInvalidRegistryReportsEveryProblem(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(c *apiserver.Config) {
		c.RegistryPath = writeRegistry(t, brokenRegistryYAML)
	})

	rec := do(t, h, http.MethodGet, "/api/v1/registry", nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	var apiErr apiserver.Error
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiErr))
	require.Equal(t, apiserver.CodeValidation, apiErr.Code)
	// One problem per entry, not one paragraph: a console renders a list, and a
	// document with six errors should take one round trip to fix rather than
	// six. The document below has exactly five distinct faults.
	require.Len(t, apiErr.Problems, 5)
	joined := strings.Join(apiErr.Problems, "\n")
	require.Contains(t, joined, `share the prefix "crm"`)
	require.Contains(t, joined, `unknown namespace "ns_absent"`)
}

func TestAMissingRegistryIsNotFoundRatherThanInvalid(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(c *apiserver.Config) {
		c.RegistryPath = filepath.Join(t.TempDir(), "absent.yaml")
	})

	rec := do(t, h, http.MethodGet, "/api/v1/registry", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetServerAcceptsIDOrNameAnd404sOtherwise(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	byID := do(t, h, http.MethodGet, "/api/v1/registry/servers/srv_crm", nil)
	require.Equal(t, http.StatusOK, byID.Code)
	byName := do(t, h, http.MethodGet, "/api/v1/registry/servers/crm-prod", nil)
	require.Equal(t, http.StatusOK, byName.Code)
	require.JSONEq(t, byID.Body.String(), byName.Body.String())

	missing := do(t, h, http.MethodGet, "/api/v1/registry/servers/nope", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)
}

func TestValidateRegistryChecksTheSuppliedDocument(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	ok := do(t, h, http.MethodPost, "/api/v1/registry:validate",
		apiserver.ValidateRegistryRequest{Content: registryYAML})
	require.Equal(t, http.StatusOK, ok.Code)

	var summary api.RegistrySummary
	require.NoError(t, json.Unmarshal(ok.Body.Bytes(), &summary))
	require.True(t, summary.Valid)
	require.Equal(t, 1, summary.Servers)

	// The server's own registry is valid; the *supplied* one is not. Validating
	// the wrong document would make this endpoint useless as a PR check.
	bad := do(t, h, http.MethodPost, "/api/v1/registry:validate",
		apiserver.ValidateRegistryRequest{Content: "org: x\nversion: 1\n"})
	require.Equal(t, http.StatusUnprocessableEntity, bad.Code)
}

func TestUnknownRequestFieldsAreRejected(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	// Silently ignoring `allowUnreachable` when the field is `allow_unreachable`
	// means a build that was meant to tolerate a down backend fails instead,
	// and the caller has no way to see why.
	rec := do(t, h, http.MethodPost, "/api/v1/snapshots:build",
		map[string]any{"allowUnreachable": true})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "allowUnreachable")
}

func TestTrailingContentInABodyIsRejected(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/registry:validate",
		strings.NewReader(`{"content":"org: x"}{"content":"org: y"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Two documents in one body means the client believes both were applied.
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "trailing content")
}

// --------------------------------------------------------------- snapshots ---

func TestGetCurrentSnapshotIs404WithoutAConfiguredPath(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	rec := do(t, h, http.MethodGet, "/api/v1/snapshots/current", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestInspectSnapshotRejectsNonSnapshotContent(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	rec := do(t, h, http.MethodPost, "/api/v1/snapshots:inspect",
		apiserver.InspectSnapshotRequest{
			Content: base64.StdEncoding.EncodeToString([]byte("this is not a snapshot")),
		})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestVerifySnapshotDemandsATrustedKey(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	rec := do(t, h, http.MethodPost, "/api/v1/snapshots:verify",
		apiserver.VerifySnapshotRequest{Content: base64.StdEncoding.EncodeToString([]byte("x"))})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Defaulting to the server's own key would answer a different question
	// than the one asked, and answer it reassuringly.
	require.Contains(t, rec.Body.String(), "would answer a different question")
}

func TestBuildSnapshotWithoutAKeySaysSoPlainly(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	rec := do(t, h, http.MethodPost, "/api/v1/snapshots:build",
		apiserver.BuildSnapshotRequest{DryRun: true})
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "holds no signing key")
}

func TestGenerateSigningKeyNeverReturnsThePrivateHalf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newServer(t, func(c *apiserver.Config) { c.KeyDir = dir })

	rec := do(t, h, http.MethodPost, "/api/v1/keys:generate",
		apiserver.GenerateSigningKeyRequest{KeyID: "rotation-test"})
	require.Equal(t, http.StatusCreated, rec.Code)

	var key api.SigningKey
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &key))
	require.Equal(t, "rotation-test", key.KeyID)
	require.NotEmpty(t, key.PublicKey)

	// The private key was written to disk and must not also be in the response:
	// whoever holds it can publish configuration to every data-plane instance,
	// and an HTTP response lands in browser memory and proxy logs.
	priv, err := os.ReadFile(filepath.Join(dir, "rotation-test.key"))
	require.NoError(t, err)
	require.NotContains(t, rec.Body.String(), strings.TrimSpace(string(priv)))
}

func TestGenerateSigningKeyRefusesAKeyIDThatEscapesTheDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newServer(t, func(c *apiserver.Config) { c.KeyDir = dir })

	for _, id := range []string{"../escape", "a/b", "with space", ""} {
		rec := do(t, h, http.MethodPost, "/api/v1/keys:generate",
			apiserver.GenerateSigningKeyRequest{KeyID: id})
		require.Equal(t, http.StatusBadRequest, rec.Code, id)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestGenerateSigningKeyRefusesWithoutAKeyDirectory(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	// Minting a key and then failing to store it would leave a public key
	// advertised for a private key nobody has.
	rec := do(t, h, http.MethodPost, "/api/v1/keys:generate",
		apiserver.GenerateSigningKeyRequest{KeyID: "orphan"})
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// ----------------------------------------------------------------- gateway ---

func TestGatewayOperationsReportAnUnreachableDataPlane(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(c *apiserver.Config) {
		// Port 1 is reserved and never listening.
		c.GatewayURL = "http://127.0.0.1:1"
	})

	rec := do(t, h, http.MethodGet, "/api/v1/gateway/status", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code)

	var apiErr apiserver.Error
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiErr))
	require.Equal(t, apiserver.CodeUnavailable, apiErr.Code)
}

func TestListTenantsStillAnswersWhenTheGatewayIsDown(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(c *apiserver.Config) {
		c.GatewayURL = "http://127.0.0.1:1"
	})

	rec := do(t, h, http.MethodGet, "/api/v1/tenants", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var list api.TenantList
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	// The live half is missing; Ready:false says so rather than hiding it.
	require.False(t, list.Ready)
	// The registry half still answers. A tenant slug the registry binds is
	// knowable without a database and without the gateway, and reporting
	// nothing here would make a running deployment look empty.
	require.NotEmpty(t, list.Registered)
	require.Equal(t, "unregistered", list.Registered[0].Status,
		"no store is configured, so every slug here comes from the registry alone")
}

func TestTenancyOperationsReportTheAbsentDatabase(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	// A control plane with no database is a legitimate deployment — a snapshot
	// builder in CI has no business holding user records. What it must not do
	// is answer with an empty list, which reads as "no users exist".
	for _, path := range []string{
		"/api/v1/tenants/" + uuid.Nil.String() + "/users",
		"/api/v1/users/" + uuid.Nil.String(),
		"/api/v1/users/" + uuid.Nil.String() + "/grants",
		"/api/v1/users/" + uuid.Nil.String() + "/keys",
	} {
		rec := do(t, h, http.MethodGet, path, nil)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, path)

		var apiErr apiserver.Error
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiErr))
		require.Contains(t, apiErr.Message, "no database configured")
	}
}

func TestListRolesAnswersWithoutADatabase(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	// The one tenancy operation that does not need a store: the built-in
	// catalog is what a fresh install authorizes against before anybody has
	// seeded it, so it is a real answer rather than a placeholder.
	rec := do(t, h, http.MethodGet, "/api/v1/roles", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var catalog api.RoleCatalog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &catalog))
	require.NotEmpty(t, catalog.Roles)
	require.NotEmpty(t, catalog.Permissions)

	// tool:list and tool:call are separate permissions. A catalog that merged
	// them would let an agent call something it was never shown.
	require.Contains(t, catalog.Permissions, "tool:list")
	require.Contains(t, catalog.Permissions, "tool:call")
}

func TestUUIDPathsAreRejectedBeforeTheDatabaseIsAsked(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	rec := do(t, h, http.MethodGet, "/api/v1/users/not-a-uuid", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCallToolRejectsAnUnknownResponseAction(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(c *apiserver.Config) { c.GatewayURL = "http://127.0.0.1:1" })

	rec := do(t, h,
		http.MethodPost, "/api/v1/gateway/tools/crm.get:call",
		apiserver.CallToolRequest{
			RequestState: "opaque",
			Responses:    map[string]string{"req_1": "maybe"},
		})
	// Rejected before any network attempt: a malformed request is the caller's
	// problem, not the gateway's, and reporting it as a 502 would send someone
	// looking at the wrong system.
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "accept, decline, cancel")
}

// ------------------------------------------------------------------ misc ----

func TestUnknownRoutesReturnTheStandardErrorShape(t *testing.T) {
	t.Parallel()
	h := newServer(t, nil)

	rec := do(t, h, http.MethodGet, "/api/v1/nonexistent", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	var apiErr apiserver.Error
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiErr))
	require.Equal(t, apiserver.CodeNotFound, apiErr.Code)
	require.Contains(t, apiErr.Message, "/api/v1/nonexistent")
}

func TestListsAreEmptyArraysRatherThanNull(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(c *apiserver.Config) {
		c.RegistryPath = writeRegistry(t, strings.Replace(registryYAML, pluginsBlock, "", 1))
	})

	rec := do(t, h, http.MethodGet, "/api/v1/plugins", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// `null` and `[]` are different values to a TypeScript client, and one of
	// them makes `.map` throw.
	require.Contains(t, rec.Body.String(), `"plugins":[]`)
}

func writeRegistry(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

const pluginsBlock = `
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
`

const brokenRegistryYAML = `org: org_test
version: 1
namespaces:
  - id: ns_a
    name: a
    prefix: crm
  - id: ns_b
    name: b
    prefix: crm
servers:
  - id: srv_1
    name: one
    namespace: ns_missing
    bindings:
      - tenant: acme
        primary: http://localhost:9101
    default_effect_class: nonsense
toolsets:
  - id: ts_1
    name: b
    namespaces: [ns_absent]
`

const registryYAML = `org: org_test
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

` + pluginsBlock

func TestGrantScopesAreCheckedAgainstTheServingSnapshot(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	// No store, so this stops at the 503 rather than reaching the check — the
	// point of this case is that a malformed scope is reported as such rather
	// than stored and silently authorizing nothing. The full check has a live
	// snapshot to work from; see the scope tests in internal/platform/authz.
	rec := do(t, h, http.MethodPut,
		"/api/v1/users/"+uuid.Nil.String()+"/grants",
		apiserver.PutGrantsRequest{Grants: []api.Grant{{Role: "tool_user", Scope: "nonsense"}}})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
