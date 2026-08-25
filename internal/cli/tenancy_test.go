// Copyright 2026 Henry Zektser.

package cli_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/cli"
)

// The tenancy commands talk to the control-plane API rather than to the
// database, so what is worth testing here is the translation: a slug becomes a
// uuid, a role@scope string becomes a grant, and a refusal from the server
// becomes the right exit code.

// fakeAPI serves canned responses and records what was asked.
type fakeAPI struct {
	*httptest.Server
	requests []recorded
}

type recorded struct {
	Method string
	Path   string
	Body   string
}

func newFakeAPI(t *testing.T, routes map[string]any) *fakeAPI {
	t.Helper()
	api := &fakeAPI{}
	api.Server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var body bytes.Buffer
			_, _ = body.ReadFrom(r.Body)
			api.requests = append(api.requests, recorded{
				Method: r.Method, Path: r.URL.Path, Body: body.String(),
			})

			payload, ok := routes[r.Method+" "+r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"code": "not_found", "message": "no route " + r.Method + " " + r.URL.Path,
				})
				return
			}
			if payload == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(payload)
		}))
	t.Cleanup(api.Close)
	return api
}

func runCLI(t *testing.T, api *fakeAPI, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := cli.New(cli.Options{
		Stdout:     &out,
		Stderr:     &errOut,
		ConfigPath: filepath.Join(t.TempDir(), "absent.yaml"),
	})
	root.SetArgs(append(args, "--api-url", api.URL))
	err := root.Execute()
	return out.String(), err
}

// tenantsPayload is the listing every command resolves a slug against.
func tenantsPayload() map[string]any {
	return map[string]any{
		"gateway_url": "http://localhost:8080", "status": "ready", "ready": true,
		"snapshot_version": 7, "tenants": 1, "tools": 12,
		"registered": []map[string]any{{
			"id": "11111111-1111-1111-1111-111111111111", "slug": "acme",
			"name": "Acme", "status": "active", "users": 1, "backends": 2, "tools": 12,
		}},
	}
}

func usersPayload() map[string]any {
	return map[string]any{
		"tenant": "acme",
		"users": []map[string]any{{
			"id":        "22222222-2222-2222-2222-222222222222",
			"tenant_id": "11111111-1111-1111-1111-111111111111",
			"tenant":    "acme", "email": "alice@example.com", "status": "active",
			"has_password": true, "created_at": "2026-01-01T00:00:00Z",
		}},
	}
}

func TestGrantsSetSendsTheWholeSet(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, map[string]any{
		"GET /api/v1/tenants": tenantsPayload(),
		"GET /api/v1/users":   usersPayload(),
		"PUT /api/v1/users/22222222-2222-2222-2222-222222222222/grants": map[string]any{
			"user_id": "22222222-2222-2222-2222-222222222222",
			"grants": []map[string]string{
				{"role": "tool_user", "scope": "t/acme/ts/support"},
			},
		},
	})

	_, err := runCLI(t, api,
		"users", "grants", "set", "alice@example.com",
		"--grant", "tool_user@t/acme/ts/support",
		"--output", "json")
	require.NoError(t, err)

	var put *recorded
	for i, req := range api.requests {
		if req.Method == http.MethodPut {
			put = &api.requests[i]
		}
	}
	require.NotNil(t, put, "no PUT was made")
	require.JSONEq(t,
		`{"grants":[{"role":"tool_user","scope":"t/acme/ts/support"}]}`, put.Body)
}

func TestGrantsSetWithNoGrantsRevokesEverything(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, map[string]any{
		"GET /api/v1/tenants": tenantsPayload(),
		"GET /api/v1/users":   usersPayload(),
		"PUT /api/v1/users/22222222-2222-2222-2222-222222222222/grants": map[string]any{
			"user_id": "22222222-2222-2222-2222-222222222222", "grants": []any{},
		},
	})

	// Not an error and not a no-op: it is how an account is stripped without
	// being deleted, and the empty array has to actually reach the server.
	_, err := runCLI(t, api,
		"users", "grants", "set", "alice@example.com",
		"--output", "json")
	require.NoError(t, err)

	var put *recorded
	for i, req := range api.requests {
		if req.Method == http.MethodPut {
			put = &api.requests[i]
		}
	}
	require.NotNil(t, put)
	require.JSONEq(t, `{"grants":[]}`, put.Body)
}

func TestAMalformedGrantIsRejectedBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, map[string]any{
		"GET /api/v1/tenants": tenantsPayload(),
		"GET /api/v1/users":   usersPayload(),
	})

	_, err := runCLI(t, api,
		"users", "grants", "set", "alice@example.com",
		"--grant", "tool_user")
	require.Error(t, err)
	require.Contains(t, err.Error(), "role@scope")

	for _, req := range api.requests {
		require.NotEqual(t, http.MethodPut, req.Method,
			"a malformed grant reached the server")
	}
}

func TestAScopeContainingAnAtSignSplitsOnTheLastOne(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, map[string]any{
		"GET /api/v1/tenants": tenantsPayload(),
		"GET /api/v1/users":   usersPayload(),
		"PUT /api/v1/users/22222222-2222-2222-2222-222222222222/grants": map[string]any{
			"user_id": "22222222-2222-2222-2222-222222222222", "grants": []any{},
		},
	})

	_, err := runCLI(t, api,
		"users", "grants", "set", "alice@example.com",
		"--grant", "tool_user@t/acme/ts/mail@corp")
	require.NoError(t, err)

	var put *recorded
	for i, req := range api.requests {
		if req.Method == http.MethodPut {
			put = &api.requests[i]
		}
	}
	require.NotNil(t, put)
	require.Contains(t, put.Body, `"scope":"t/acme/ts/mail@corp"`)
	require.Contains(t, put.Body, `"role":"tool_user"`)
}

func TestDeletingATenantRequiresConfirmation(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, map[string]any{
		"GET /api/v1/tenants": tenantsPayload(),
	})

	_, err := runCLI(t, api, "tenants", "delete", "acme")
	require.Error(t, err)
	require.Contains(t, err.Error(), "--yes")
	// It names what would be lost, so the confirmation is informed rather than
	// ceremonial.
	require.Contains(t, err.Error(), "1 user(s)")

	for _, req := range api.requests {
		require.NotEqual(t, http.MethodDelete, req.Method)
	}
}

func TestARegistryOnlyTenantSaysSoRatherThanNotFound(t *testing.T) {
	t.Parallel()
	payload := tenantsPayload()
	payload["registered"] = []map[string]any{{
		"id": "", "slug": "ghost", "name": "ghost", "status": "unregistered",
		"users": 0, "backends": 1, "tools": 4,
	}}
	api := newFakeAPI(t, map[string]any{"GET /api/v1/tenants": payload})

	// "no tenant with slug ghost" would be misleading when the slug is visibly
	// in the registry. The error has to explain which half is missing.
	_, err := runCLI(t, api, "users", "list", "--tenant", "ghost")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no tenant record exists")
	require.Contains(t, err.Error(), "mcpdoll tenants create ghost")
}

func TestTheMintedSecretIsPrintedExactlyOnce(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, map[string]any{
		"GET /api/v1/tenants": tenantsPayload(),
		"GET /api/v1/users":   usersPayload(),
		"POST /api/v1/users/22222222-2222-2222-2222-222222222222/keys": map[string]any{
			"key": map[string]any{
				"id":      "33333333-3333-3333-3333-333333333333",
				"user_id": "22222222-2222-2222-2222-222222222222",
				"name":    "bot", "prefix": "abc123", "declared_grants": []any{},
				"active": true, "created_at": "2026-01-01T00:00:00Z",
			},
			"secret": "mcpd.abc123.thesecretpart",
		},
	})

	out, err := runCLI(t, api,
		"users", "keys", "mint", "alice@example.com",
		"--tenant", "acme", "--name", "bot")
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(out, "mcpd.abc123.thesecretpart"),
		"the secret must appear once — a second copy is a second thing to leak")
}

func TestARefusalIsNotReportedAsAnOutage(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{}
	api.Server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "forbidden", "message": "this token may not manage tenants",
			})
		}))
	t.Cleanup(api.Close)

	_, err := runCLI(t, api, "tenants", "list")
	require.Error(t, err)
	require.Contains(t, err.Error(), "may not manage tenants")
	// A refusal and an outage are different problems with different fixes.
	// Reporting the first as the second sends an operator to restart a service
	// that is working exactly as configured.
	require.Equal(t, cli.ExitConfig, cliExitCode(t, err))
}

// cliExitCode runs the error through the same mapping Execute uses.
func cliExitCode(t *testing.T, err error) int {
	t.Helper()
	return cli.ExitCodeFor(err)
}
