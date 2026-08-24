// Copyright 2026 Henry Zektser.

package apiserver_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/apiserver"
)

// The control plane enforces its own RBAC now (ADR 0022). What is worth testing
// without a database is the shape of the wall: that a credential is required,
// that a wrong one never falls through to the static token, and that the static
// token is what it says it is.

func TestNoCredentialIsRefused(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	rec := do(t, h, http.MethodGet, "/api/v1/registry", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
	})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
}

func TestAWrongTokenDoesNotFallThroughToTheStaticOne(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	// A credential in `mcpd.` form is a session or a key, and with no database
	// neither resolves. It must be a 401 rather than a quiet promotion to the
	// static principal, which is the failure mode this ordering exists to
	// prevent.
	rec := do(t, h, http.MethodGet, "/api/v1/registry", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer mcpd.abcdefgh.notarealsecret")
	})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTheStaticTokenReportsItselfAsSuch(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	rec := do(t, h, http.MethodGet, "/api/v1/auth/session", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var me api.SessionInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))

	// Naming it is the point. An operator who cannot tell they are holding the
	// break-glass credential will assume a permission check passed for a
	// reason.
	require.Equal(t, "static", me.Kind)
	require.Contains(t, me.Permissions, "signingkey:generate")
	require.Contains(t, me.Permissions, "tenant:manage")
}

func TestTheStaticTokenHoldsEverything(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	// Deliberate, and the reason it is documented and logged rather than
	// removed: CI has to build a snapshot before any user exists.
	for _, path := range []string{
		"/api/v1/registry",
		"/api/v1/roles",
		"/api/v1/revocations",
	} {
		rec := do(t, h, http.MethodGet, path, nil)
		require.NotEqual(t, http.StatusForbidden, rec.Code, path)
		require.NotEqual(t, http.StatusUnauthorized, rec.Code, path)
	}
}

func TestAMalformedIDIsABadRequestNotANotFound(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	// A uuid's format is public, so saying "that is not a uuid" leaks nothing
	// and is far more useful than "not found". The 404 is reserved for a
	// resource that does not exist *or* that the caller may not see, which is
	// what stops it enumerating other tenants' resources.
	rec := do(t, h, http.MethodGet, "/api/v1/users/not-a-uuid/grants", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "must be a uuid")
}

func TestRevocationsReportSaysWhenNothingWillTakeEffect(t *testing.T) {
	t.Parallel()
	h := newServer(t, func(*apiserver.Config) {})

	rec := do(t, h, http.MethodGet, "/api/v1/revocations", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var report api.RevocationReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))

	// No revocations_path configured: revoking still works, it simply waits for
	// a snapshot. Saying so is the difference between a known trade and a
	// surprise during an incident.
	require.Contains(t, report.Warning, "no revocations_path")
	require.NotNil(t, report.Revocations)
}
