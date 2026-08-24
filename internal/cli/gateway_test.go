// Copyright 2026 The MCPDoll Authors.

package cli

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
)

// The gateway does not tell a client who it decided the credential is anywhere
// in the MCP protocol — there is no field for it, and the `instructions` string
// is prose written for a model. It says so in a response header instead, and
// both inspection surfaces read it there.
//
// Without this, `mcpdoll gateway catalog` prints `for tenant ""` while the API
// reports the tenant correctly. That is the tri-surface law failing in the way
// `make parity` cannot see: both surfaces exist and disagree.

func TestTheResolvedIdentityIsReadFromTheResponse(t *testing.T) {
	t.Parallel()

	var r resolvedIdentity
	header := http.Header{}
	header.Set(edge.HeaderResolvedTenant, "acme")
	header.Set(edge.HeaderResolvedSubject, "support@acme.example")
	r.record(header)

	tenant, subject := r.get()
	require.Equal(t, "acme", tenant)
	require.Equal(t, "support@acme.example", subject)
}

func TestALaterResponseWithoutTheHeadersDoesNotEraseIt(t *testing.T) {
	t.Parallel()

	var r resolvedIdentity
	first := http.Header{}
	first.Set(edge.HeaderResolvedTenant, "acme")
	first.Set(edge.HeaderResolvedSubject, "support@acme.example")
	r.record(first)

	// A streamable session makes several requests, and not every response
	// carries these. Blanking on an absent header would leave the report empty
	// depending on which round trip happened last.
	r.record(http.Header{})

	tenant, subject := r.get()
	require.Equal(t, "acme", tenant)
	require.Equal(t, "support@acme.example", subject)
}
