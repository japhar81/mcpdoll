// Copyright 2026 The MCPDoll Authors.

package edge_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// A realistic-looking inbound credential. Not a live secret; it exists so the
// test has something whose absence downstream is meaningful.
const inboundToken = "Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9." +
	"eyJzdWIiOiJhbGljZUBleGFtcGxlLmNvbSIsImF1ZCI6Im1jcGRvbGwifQ." +
	"c2lnbmF0dXJlLXRoYXQtbXVzdC1uZXZlci1yZWFjaC1hLWJhY2tlbmQ"

// TestInboundTokenNeverReachesABackend is the security assertion the build brief
// requires by name.
//
// Forwarding the caller's credential to a backend would make every registered
// backend a confused deputy holding a token scoped far wider than the one tool it
// needs — and any one of those backends could then act as the user anywhere. The
// gateway must present a credential *it* minted, or none at all.
func TestInboundTokenNeverReachesABackend(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	headers := http.Header{}
	headers.Set("Authorization", inboundToken)
	headers.Set("Cookie", "session=super-secret-session-value")
	headers.Set("X-Api-Key", "sk-inbound-api-key-abcdefghijklmnop")
	headers.Set(edge.HeaderSubject, "alice@example.com")
	headers.Set(edge.HeaderGroups, "support")

	session := h.Connect(t, "platform-agents", headers)

	// Reach every backend so each one's received headers are recorded.
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"crm.lookup_customer", map[string]any{"customer_id": "cus_1"}},
		{"hr.lookup_employee", map[string]any{"staff_number": "E-1"}},
		{"whs.check_stock", map[string]any{"sku": "SKU-1"}},
		{"web.search_web", map[string]any{"query": "anything"}},
	}
	for _, c := range calls {
		res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
			Name: c.tool, Arguments: c.args,
		})
		require.NoError(t, err, "calling %s", c.tool)
		require.False(t, res.IsError, "%s: %s", c.tool, contentText(res))
	}

	secrets := []string{
		inboundToken,
		"eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9",
		"c2lnbmF0dXJlLXRoYXQtbXVzdC1uZXZlci1yZWFjaC1hLWJhY2tlbmQ",
		"super-secret-session-value",
		"sk-inbound-api-key-abcdefghijklmnop",
	}

	for name, b := range map[string]interface{ LastHeaders() http.Header }{
		"crm":       h.Modern,
		"hr":        h.Legacy,
		"warehouse": h.Misbehaving.Backend,
		"websearch": h.Hostile,
	} {
		received := b.LastHeaders()
		require.NotEmpty(t, received, "%s recorded no request", name)

		// No header value anywhere may contain any inbound secret.
		for header, values := range received {
			for _, v := range values {
				for _, secret := range secrets {
					require.NotContains(t, v, secret,
						"backend %q received inbound credential material in header %q", name, header)
				}
			}
		}

		// And specifically: no Authorization, Cookie, or API-key header at all,
		// since no token exchange is configured for these backends.
		require.Empty(t, received.Get("Authorization"),
			"backend %q received an Authorization header it was never meant to see", name)
		require.Empty(t, received.Get("Cookie"), "backend %q received a Cookie", name)
		require.Empty(t, received.Get("X-Api-Key"), "backend %q received an API key", name)

		// The dev identity headers must not be forwarded either: a backend that
		// trusted them could impersonate any principal.
		require.Empty(t, received.Get(edge.HeaderSubject),
			"backend %q received the gateway's identity header", name)
		require.Empty(t, received.Get(edge.HeaderGroups))
	}
}

// TestTokenSourceIsTheOnlySourceOfBackendCredentials proves the positive half:
// when token exchange *is* configured, the backend receives the exchanged
// credential and nothing else.
func TestTokenSourceIsTheOnlySourceOfBackendCredentials(t *testing.T) {
	h := newHarness(t, harnessOptions{
		TokenSource: &recordingTokenSource{value: "Bearer minted-for-this-backend-only"},
	})

	headers := http.Header{}
	headers.Set("Authorization", inboundToken)
	headers.Set(edge.HeaderSubject, "alice@example.com")
	session := h.Connect(t, "platform-agents", headers)

	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "crm.lookup_customer", Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, contentText(res))

	received := h.Modern.LastHeaders()
	require.Equal(t, "Bearer minted-for-this-backend-only", received.Get("Authorization"),
		"the backend must see the exchanged credential")
	require.NotContains(t, received.Get("Authorization"), "eyJhbGciOiJSUzI1NiI",
		"the inbound token must not appear even when an exchange is configured")
	require.Equal(t, "alice@example.com", received.Get("X-Tenant-Subject"),
		"claim-mapped headers come from the exchange, not from the inbound request")
}

// recordingTokenSource is a stand-in for a real RFC 8693 exchange.
//
// Note the shape of the interface it satisfies: Exchange receives a Principal,
// not a token. There is no parameter it *could* use to pass the caller's
// credential through, which is what makes passthrough structurally impossible
// rather than merely discouraged.
type recordingTokenSource struct {
	value string
}

func (s *recordingTokenSource) Exchange(
	_ context.Context,
	_ *snapshotpb.Server,
	principal backends.Principal,
) (backends.Credential, error) {
	return backends.Credential{
		Header: "Authorization",
		Value:  s.value,
		Extra:  map[string]string{"X-Tenant-Subject": principal.Subject},
	}, nil
}

// TestFilteredCatalogIsNeverPublicOverTheWire is the cacheScope assertion, made
// end to end rather than only against the snapshot view.
//
// A shared HTTP cache honouring `cacheScope: public` on an identity-filtered
// catalog would serve one principal's tool list to another. The snapshot-level
// test proves the flag is computed; this one proves it survives to the wire.
func TestFilteredCatalogIsNeverPublicOverTheWire(t *testing.T) {
	t.Run("unfiltered catalog is public", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		session := h.Connect(t, "platform-agents", nil)
		res, err := session.ListTools(context.Background(), nil)
		require.NoError(t, err)
		require.Equal(t, "public", res.CacheScope)
	})

	t.Run("a filtering pipeline forces private", func(t *testing.T) {
		// A pipeline that hides destructive tools from anyone not in
		// billing-admins — the ordinary entitlement case.
		h := newHarness(t, harnessOptions{
			Pipeline: &filteringPipeline{hidePrefix: "dep."},
		})
		session := h.Connect(t, "platform-agents", nil)

		res, err := session.ListTools(context.Background(), nil)
		require.NoError(t, err)

		require.NotContains(t, toolNames(res.Tools), "dep.promote_release",
			"precondition: the pipeline actually filtered something")
		require.Equal(t, "private", res.CacheScope,
			"an identity-filtered catalog must never be advertised as publicly cacheable")
	})
}

// filteringPipeline is a minimal identity-dependent catalog filter.
type filteringPipeline struct {
	hidePrefix string
}

func (p *filteringPipeline) OnCatalog(_ context.Context, req *edge.CatalogRequest) (*edge.CatalogDecision, error) {
	kept := make([]*sdk.Tool, 0, len(req.Tools))
	filtered := false
	for _, t := range req.Tools {
		if strings.HasPrefix(t.Name, p.hidePrefix) {
			filtered = true
			continue
		}
		kept = append(kept, t)
	}
	return &edge.CatalogDecision{Tools: kept, IdentityFiltered: filtered}, nil
}

func (p *filteringPipeline) OnToolCall(context.Context, *edge.ToolCallRequest) (*edge.ToolCallDecision, error) {
	return &edge.ToolCallDecision{Decision: "allow"}, nil
}

func (p *filteringPipeline) OnToolResult(context.Context, *edge.ToolResultRequest) (*edge.ToolResultDecision, error) {
	return &edge.ToolResultDecision{Decision: "allow"}, nil
}

// TestAudienceAuthorization: an audience restricted to a group must refuse a
// principal outside it, with 403 rather than 404 so a misconfigured group
// assignment does not look like a typo.
func TestAudienceAuthorization(t *testing.T) {
	h := newHarness(t, harnessOptions{RestrictAudienceTo: []string{"billing-admins"}})

	t.Run("a member is admitted", func(t *testing.T) {
		headers := http.Header{}
		headers.Set(edge.HeaderSubject, "admin@example.com")
		headers.Set(edge.HeaderGroups, "billing-admins,support")
		session := h.Connect(t, "platform-agents", headers)
		_, err := session.ListTools(context.Background(), nil)
		require.NoError(t, err)
	})

	t.Run("a non-member is forbidden", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, h.URL("platform-agents"),
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set(edge.HeaderSubject, "intern@example.com")
		req.Header.Set(edge.HeaderGroups, "interns")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

// TestHeaderIdentityResolverRefusesProduction: the dev resolver trusts
// client-supplied headers, so letting it run in production would be a total
// authorization bypass. A constructor error is a control; a comment is not.
func TestHeaderIdentityResolverRefusesProduction(t *testing.T) {
	for _, env := range []string{"production", "prod", "PRODUCTION", "Prod"} {
		_, err := edge.NewHeaderIdentityResolver(env, "u", nil)
		require.Error(t, err, "env %q must be refused", env)
		require.ErrorContains(t, err, "must never run")
	}
	for _, env := range []string{"development", "test", "staging", ""} {
		_, err := edge.NewHeaderIdentityResolver(env, "u", nil)
		require.NoError(t, err, "env %q should be allowed", env)
	}
}

// TestUnauthenticatedRequestIsRejected: with no default subject configured, a
// request carrying no identity must be refused rather than served as anonymous.
func TestUnauthenticatedRequestIsRejected(t *testing.T) {
	h := newHarness(t, harnessOptions{NoDefaultSubject: true})

	req, err := http.NewRequest(http.MethodPost, h.URL("platform-agents"),
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
