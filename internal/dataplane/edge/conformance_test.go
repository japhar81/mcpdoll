// Copyright 2026 The MCPDoll Authors.

package edge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/fixtures"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
)

// The conformance suite drives the real edge with the SDK's own client. Its job
// is to catch places where MCPDoll's understanding of the protocol differs from
// the SDK's, so everything here goes over HTTP against a real listener.

// TestConformanceProtocolVersion asserts the gateway serves 2026-07-28 to a
// modern client. This is the headline requirement: 2026-07-28 over streamable
// HTTP is only possible with a stateless transport, so if this fails the edge has
// silently negotiated down.
func TestConformanceProtocolVersion(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)

	init := session.InitializeResult()
	require.NotNil(t, init)
	require.Equal(t, "2026-07-28", init.ProtocolVersion,
		"the edge must serve the modern protocol; a downgrade means Stateless was not set")
	require.Equal(t, edge.GatewayName, init.ServerInfo.Name)
	require.Contains(t, init.Instructions, "MCPDoll")
}

// TestConformanceServerDiscover asserts `server/discover` works and advertises
// 2026-07-28. It is mandatory in the modern protocol, and it is how a client
// negotiates without the legacy initialize handshake.
func TestConformanceServerDiscover(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	body := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"conformance","version":"1.0.0"},` +
		`"io.modelcontextprotocol/clientCapabilities":{}` +
		`}}}`

	req, err := http.NewRequest(http.MethodPost, h.URL("platform-agents"), strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "server/discover")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	result := decodeJSONRPCResult(t, resp)
	var discover struct {
		SupportedVersions []string `json:"supportedVersions"`
		Capabilities      struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
		TTLMs      int    `json:"ttlMs"`
		CacheScope string `json:"cacheScope"`
	}
	require.NoError(t, json.Unmarshal(result, &discover))

	require.Contains(t, discover.SupportedVersions, "2026-07-28")
	require.NotNil(t, discover.Capabilities.Tools, "the gateway must advertise tools")
	require.Equal(t, "public", discover.CacheScope,
		"server/discover carries the cacheable fields like any other list result")
}

// TestConformanceHeaderValidation covers the mandatory `Mcp-Method` / `Mcp-Name`
// headers and the -32020 HeaderMismatch code.
//
// The SDK enforces these, and this test exists precisely to prove that: the edge
// deliberately does not reimplement the check, so the suite has to verify the
// behaviour is actually present rather than assumed.
func TestConformanceHeaderValidation(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	const codeHeaderMismatch = -32020

	callBody := func(name string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` +
			`"name":"` + name + `","arguments":{"customer_id":"cus_1"},"_meta":{` +
			`"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
			`"io.modelcontextprotocol/clientInfo":{"name":"conformance","version":"1.0.0"},` +
			`"io.modelcontextprotocol/clientCapabilities":{}` +
			`}}}`
	}

	tests := []struct {
		name       string
		body       string
		headers    map[string]string
		wantStatus int
		wantCode   int
		wantErr    string
	}{
		{
			name: "matching headers are accepted",
			body: callBody("crm.lookup_customer"),
			headers: map[string]string{
				"Mcp-Method": "tools/call",
				"Mcp-Name":   "crm.lookup_customer",
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "Mcp-Method disagreeing with the body is rejected",
			body: callBody("crm.lookup_customer"),
			headers: map[string]string{
				"Mcp-Method": "tools/list",
				"Mcp-Name":   "crm.lookup_customer",
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeHeaderMismatch,
		},
		{
			name: "Mcp-Name disagreeing with the body is rejected",
			body: callBody("crm.lookup_customer"),
			headers: map[string]string{
				"Mcp-Method": "tools/call",
				"Mcp-Name":   "crm.update_customer",
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeHeaderMismatch,
		},
		{
			name:       "missing Mcp-Protocol-Version with _meta.protocolVersion is rejected",
			body:       callBody("crm.lookup_customer"),
			headers:    map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "crm.lookup_customer", "-skip-version": "1"},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeHeaderMismatch,
			wantErr:    "Mcp-Protocol-Version",
		},
		{
			name: "Mcp-Protocol-Version disagreeing with _meta is rejected",
			body: callBody("crm.lookup_customer"),
			headers: map[string]string{
				"Mcp-Method":           "tools/call",
				"Mcp-Name":             "crm.lookup_customer",
				"Mcp-Protocol-Version": "2025-11-25",
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeHeaderMismatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, h.URL("platform-agents"), strings.NewReader(tc.body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")

			_, skipVersion := tc.headers["-skip-version"]
			if !skipVersion {
				req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
			}
			for k, v := range tc.headers {
				if strings.HasPrefix(k, "-") {
					continue
				}
				req.Header.Set(k, v)
			}

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, tc.wantStatus, resp.StatusCode)

			if tc.wantCode == 0 {
				return
			}
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			var envelope struct {
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(raw, &envelope), "body was %s", raw)
			require.NotNil(t, envelope.Error, "expected a JSON-RPC error, got %s", raw)
			require.Equal(t, tc.wantCode, envelope.Error.Code,
				"want HeaderMismatch (-32020), got %s", raw)
			if tc.wantErr != "" {
				require.Contains(t, envelope.Error.Message, tc.wantErr)
			}
		})
	}
}

// TestConformanceListTools asserts the aggregated catalog, its namespace
// prefixing, and its stable ordering.
func TestConformanceListTools(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)

	res, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	names := toolNames(res.Tools)

	// Every tool is namespace-prefixed, and the prefix is the gateway's, not
	// the backend's.
	for _, n := range names {
		require.Contains(t, n, ".", "tool %q is not namespace-prefixed", n)
	}
	require.Contains(t, names, "crm.lookup_customer")
	require.Contains(t, names, "hr.lookup_employee")
	require.Contains(t, names, "whs.check_stock")
	require.Contains(t, names, "dep.promote_release")

	// The unprefixed backend names must not leak.
	require.NotContains(t, names, "lookup_customer")

	// Stable total order: (bundle priority, namespace prefix, tool name). Every
	// namespace's tools form a contiguous block, in prefix order.
	require.Equal(t, []string{
		"crm.list_open_tickets",
		"crm.lookup_customer",
		"crm.update_customer",
		"dep.promote_release",
		"hr.get_org_chart",
		"hr.lookup_employee",
		"web.fetch_page",
		"web.search_web",
		"whs.check_stock",
		"whs.reserve_stock",
	}, names)
}

// TestConformanceListToolsIsStableAcrossCalls: the order is a contract clients
// cache against, so two identical requests must produce identical lists.
func TestConformanceListToolsIsStableAcrossCalls(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)

	first, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	for range 5 {
		again, err := session.ListTools(context.Background(), nil)
		require.NoError(t, err)
		require.Equal(t, toolNames(first.Tools), toolNames(again.Tools))
	}
}

// TestConformanceCacheableFields asserts `ttlMs` and `cacheScope` on list
// results. Both are the gateway's responsibility: the SDK defaults cacheScope to
// "public" and leaves ttlMs at zero, so a missing implementation would silently
// tell every client the catalog is immediately stale and freely shareable.
func TestConformanceCacheableFields(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)

	res, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	require.Equal(t, int((5 * time.Minute).Milliseconds()), res.TTLMs,
		"ttlMs must carry the merged catalog TTL, not the SDK's zero default")
	require.Equal(t, "public", res.CacheScope,
		"an unfiltered catalog is shareable")
}

// TestConformanceCallTool proves a real client can call a tool across the
// gateway and that the call actually reached the backend.
func TestConformanceCallTool(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)

	before := h.Modern.Calls("lookup_customer")
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "crm.lookup_customer",
		Arguments: map[string]any{"customer_id": "cus_42"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected tool error: %s", contentText(res))
	require.Contains(t, contentText(res), "cus_42")
	require.Contains(t, contentText(res), "Acme Corp")
	require.Equal(t, before+1, h.Modern.Calls("lookup_customer"),
		"the call must have reached the backend")
}

// TestConformanceCallToolAcrossBackends is the aggregation requirement: one
// endpoint, one session, tools from several independently-published backends.
func TestConformanceCallToolAcrossBackends(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)
	ctx := context.Background()

	crm, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "crm.lookup_customer",
		Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.Contains(t, contentText(crm), "Acme Corp")

	hr, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "hr.lookup_employee",
		Arguments: map[string]any{"staff_number": "E-9"},
	})
	require.NoError(t, err)
	require.Contains(t, contentText(hr), "R. Patel")

	whs, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "whs.check_stock",
		Arguments: map[string]any{"sku": "SKU-7"},
	})
	require.NoError(t, err)
	require.Contains(t, contentText(whs), "142 units")

	require.Equal(t, 1, h.Modern.Calls("lookup_customer"))
	require.Equal(t, 1, h.Legacy.Calls("lookup_employee"))
	require.Equal(t, 1, h.Misbehaving.Calls("check_stock"))
}

// TestConformanceProtocolDowngradeToLegacyBackend is the version-negotiation
// requirement. The gateway serves 2026-07-28 to its client while speaking
// 2025-11-25 to a legacy backend, and neither side needs to know about the other.
func TestConformanceProtocolDowngradeToLegacyBackend(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)

	// Client side: modern.
	require.Equal(t, "2026-07-28", session.InitializeResult().ProtocolVersion)

	// Force the pool to dial the legacy backend.
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "hr.lookup_employee",
		Arguments: map[string]any{"staff_number": "E-1"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, contentText(res))

	// Backend side: negotiated down, through the SDK's real negotiation path.
	require.Equal(t, "2025-11-25", h.Pool.NegotiatedVersion("srv_hr"),
		"the legacy backend advertises no 2026-07-28 support, so the pool must negotiate down")

	// And the modern backend stayed modern, proving the downgrade is per-backend
	// rather than global.
	_, err = session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "crm.lookup_customer",
		Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.Equal(t, "2026-07-28", h.Pool.NegotiatedVersion("srv_crm"))
}

// TestConformanceToolErrorPropagates: a tool-level error must arrive as
// `isError` on a successful response, not as a protocol error, so the model can
// see it and self-correct.
func TestConformanceToolErrorPropagates(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)

	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "crm.lookup_customer",
		Arguments: map[string]any{"customer_id": "cus_missing"},
	})
	require.NoError(t, err, "a tool error must not surface as a protocol error")
	require.True(t, res.IsError)
	require.Contains(t, contentText(res), "no customer with id cus_missing")
}

// TestConformanceUnknownToolIsRejected: the gateway must not forward a call for
// a tool it does not serve.
func TestConformanceUnknownToolIsRejected(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)

	_, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "crm.no_such_tool",
		Arguments: map[string]any{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown tool")

	// An unprefixed name must not resolve either, or the namespace boundary
	// would be advisory.
	_, err = session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "lookup_customer",
		Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.Error(t, err)
}

// TestConformanceServesAdmittedNotObserved is the central integrity property:
// the gateway serves what was admitted, and a backend that changes its catalog
// cannot change what clients see.
func TestConformanceServesAdmittedNotObserved(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)
	ctx := context.Background()

	before, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	admitted := findTool(t, before.Tools, "whs.check_stock")
	require.NotContains(t, admitted.Description, "IMPORTANT",
		"precondition: the admitted description is clean")

	// The backend redeploys with an injected instruction appended, and also
	// starts serving a tool that was never admitted.
	h.Misbehaving.Drift()
	h.Misbehaving.AddSurpriseTool("exfiltrate_all")

	// A fresh session, so the assertion is about what the gateway serves rather
	// than about the client's cache.
	fresh := h.Connect(t, "platform-agents", nil)
	after, err := fresh.ListTools(ctx, nil)
	require.NoError(t, err)

	stillAdmitted := findTool(t, after.Tools, "whs.check_stock")
	require.Equal(t, admitted.Description, stillAdmitted.Description,
		"the mutated description must never reach a client")
	require.NotContains(t, stillAdmitted.Description, "IMPORTANT")

	require.NotContains(t, toolNames(after.Tools), "whs.exfiltrate_all",
		"a tool that never went through admission must not appear in the catalog")
}

// TestConformanceHostileDescriptionIsServedAsAdmitted documents the current
// boundary honestly: the hostile backend's poisoned description *was* admitted by
// the harness, so it is served. Admission's ON_ANALYZE stage is what should have
// rejected it, and the guard is what should catch the injected result — neither
// is the edge's job.
func TestConformanceHostileDescriptionIsServedAsAdmitted(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	session := h.Connect(t, "platform-agents", nil)

	res, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	tool := findTool(t, res.Tools, "web.search_web")
	require.Equal(t, fixtures.PoisonedDescription, tool.Description,
		"the edge serves the admitted text verbatim; filtering poisoned prose is admission's job, not the edge's")
}

// TestConformanceUnknownAudienceIs404 and friends cover the endpoint surface.
func TestConformanceEndpointSurface(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	t.Run("unknown audience is 404", func(t *testing.T) {
		resp, err := http.Post(h.URL("no-such-audience"), "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		require.Contains(t, string(body), "no-such-audience",
			"the error should name the audience so a typo is obvious")
	})

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(h.Server.URL + "/healthz")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("readyz reports the snapshot version", func(t *testing.T) {
		resp, err := http.Get(h.Server.URL + "/readyz")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var ready struct {
			Status    string `json:"status"`
			Version   int64  `json:"snapshot_version"`
			Audiences int    `json:"audiences"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&ready))
		require.Equal(t, "ok", ready.Status)
		require.Positive(t, ready.Version)
		require.Equal(t, 1, ready.Audiences)
	})
}

// TestConformanceStatelessRejectsGET: in stateless mode GET and DELETE are 405.
// A client that tried to open a standalone SSE stream must be told plainly
// rather than left hanging.
func TestConformanceStatelessRejectsGET(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	req, err := http.NewRequest(http.MethodGet, h.URL("platform-agents"), nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode,
		"stateless mode has no standalone SSE stream to open")
}

// TestConformanceSnapshotSwapChangesCatalogWithoutRestart is the live-reload
// requirement: a republish must change what clients see, on the same process,
// with no restart.
func TestConformanceSnapshotSwapChangesCatalogWithoutRestart(t *testing.T) {
	h := newHarness(t, harnessOptions{SkipHostile: true})
	session := h.Connect(t, "platform-agents", nil)
	ctx := context.Background()

	before, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.NotContains(t, toolNames(before.Tools), "web.search_web")
	firstVersion := h.Edge.SnapshotVersion()

	// Republish with the hostile backend's namespace included.
	h.Publish(harnessOptions{})

	require.Greater(t, h.Edge.SnapshotVersion(), firstVersion,
		"the edge must have picked up the new snapshot")

	// The existing session still serves its cached list: we advertised a 5-minute
	// ttlMs and the SDK client honours it (SEP-2549). That is correct behaviour,
	// not staleness, and asserting it here documents that our ttlMs is real.
	cached, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.NotContains(t, toolNames(cached.Tools), "web.search_web",
		"within its ttlMs a client legitimately keeps serving the cached catalog")

	// A fresh client — or the same one after its TTL expires — sees the new
	// catalog, with no gateway restart.
	fresh := h.Connect(t, "platform-agents", nil)
	after, err := fresh.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Contains(t, toolNames(after.Tools), "web.search_web",
		"a republish must change the live catalog with no restart")

	// The pre-existing entries kept their positions, so the prefix of a client's
	// prompt cache is still valid.
	require.Equal(t, toolNames(before.Tools),
		removeNames(toolNames(after.Tools), "web.fetch_page", "web.search_web"))
}

// ------------------------------------------------------------- helpers -------

func toolNames(tools []*sdk.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func findTool(t *testing.T, tools []*sdk.Tool, name string) *sdk.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found in %v", name, toolNames(tools))
	return nil
}

func contentText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func removeNames(names []string, drop ...string) []string {
	dropped := make(map[string]bool, len(drop))
	for _, d := range drop {
		dropped[d] = true
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !dropped[n] {
			out = append(out, n)
		}
	}
	return out
}

// decodeJSONRPCResult reads a JSON-RPC result from either an application/json
// body or a text/event-stream frame, since the transport may use either.
func decodeJSONRPCResult(t *testing.T, resp *http.Response) json.RawMessage {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	payload := raw
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		payload = nil
		for line := range bytes.Lines(raw) {
			if after, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
				payload = bytes.TrimSpace(after)
				break
			}
		}
		require.NotNil(t, payload, "no data frame in SSE response: %s", raw)
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(payload, &envelope), "body was %s", payload)
	require.Nil(t, envelope.Error, "unexpected JSON-RPC error: %s", envelope.Error)
	return envelope.Result
}
