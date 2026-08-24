// Copyright 2026 The MCPDoll Authors.

// Package inspector connects to a running data plane as a chosen identity and
// reports what that identity sees.
//
// It exists as a package rather than as CLI code because the console needs the
// same thing, and two implementations of "connect as this principal and list
// the tools" would eventually disagree — at which point the console and the CLI
// would give different answers to an entitlement question, and neither would be
// obviously wrong.
//
// The whole approach rests on one idea: the only trustworthy answer to "which
// tools can this agent call?" is the one obtained by making the request the
// agent makes. Re-deriving it from policy is how people get it wrong.
package inspector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoll/mcpdoll/internal/api"
)

// ErrUnavailable marks a data plane that could not be reached or that is not
// serving. Callers map it to an exit code or an HTTP status; classifying it
// here keeps that decision out of the transport layer.
var ErrUnavailable = errors.New("data plane unavailable")

// ErrInvalidRequest marks a malformed request — an empty audience, an argument
// blob that is not an object.
var ErrInvalidRequest = errors.New("invalid request")

// ErrNoAdminURL marks a control plane with no data-plane admin address
// configured. A distinct error rather than a timeout against the wrong port,
// because the fix is a config line and nothing about a timeout says so.
var ErrNoAdminURL = errors.New(
	"no data-plane admin URL is configured (controlplane.admin_url); backend " +
		"health is served on the data plane's admin listener, not its MCP port")

// ErrForbidden marks a data plane that answered, and said no.
//
// Kept distinct from ErrUnavailable because they send an operator to different
// places. "The gateway is down" and "this principal is not entitled to this
// audience" look identical from inside an MCP client — both surface as a failed
// handshake — and reporting the second as the first is how an afternoon gets
// spent restarting a healthy service.
var ErrForbidden = errors.New("forbidden")

// ErrUnknownPrincipal marks a credential the serving snapshot does not carry —
// typically a user created since the last publish.
var ErrUnknownPrincipal = errors.New("principal not in the serving snapshot")

// DefaultTimeout bounds a single inspection.
//
// Generous, because a tool call travels through the pipeline to a backend and
// back, and a plugin may legitimately take most of a second. Not unbounded,
// because a hung backend must not hold a console request open forever.
const DefaultTimeout = 60 * time.Second

// Client inspects one data plane.
type Client struct {
	// GatewayURL is the data plane's base URL — the one agents connect to.
	GatewayURL string
	// AdminURL is the data plane's admin listener, which is a different port
	// on purpose: it serves an inventory of the backends behind the gateway,
	// and an agent that can call a tool has no business reading it.
	AdminURL string
	// Token is presented to the data plane as a bearer credential. It is the
	// *inspecting operator's* token, never a token belonging to the principal
	// being impersonated.
	Token string
	// ClientName identifies the inspector to the gateway, so its requests are
	// distinguishable in the audit trail from real agent traffic.
	ClientName string
	// Version is reported in the MCP handshake.
	Version string
	// HTTP is optional; the zero value uses a client with DefaultTimeout.
	HTTP *http.Client
}

// Identity is the principal to present.
//
// Presenting an identity is not the same as authenticating as one: the data
// plane decides whether the caller is allowed to inspect on someone's behalf.
// This type carries what to claim, not permission to claim it.
type Identity struct {
	Subject string
	Groups  []string
}

// Status reports what the data plane says about itself.
func (c *Client) Status(ctx context.Context) (api.GatewayStatus, error) {
	out := api.GatewayStatus{GatewayURL: c.GatewayURL}

	url := strings.TrimRight(c.GatewayURL, "/") + "/readyz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return out, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return out, fmt.Errorf("%w: cannot reach %s: %v", ErrUnavailable, c.GatewayURL, err)
	}
	defer resp.Body.Close()

	var payload struct {
		Status  string `json:"status"`
		Version int64  `json:"snapshot_version"`
		Tenants int    `json:"tenants"`
		Tools   int    `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return out, fmt.Errorf("%w: %s returned an unreadable body: %v", ErrUnavailable, url, err)
	}

	out.Status = payload.Status
	if resp.StatusCode != http.StatusOK {
		// Not-ready is a real, reportable state rather than a transport
		// failure, so the populated status is returned alongside the error and
		// callers can render both.
		return out, fmt.Errorf("%w: %s", ErrUnavailable, payload.Status)
	}
	out.Ready = true
	out.SnapshotVersion = payload.Version
	out.Tenants = payload.Tenants
	out.Tools = payload.Tools
	return out, nil
}

// CatalogRequest asks what one credential can see.
//
// A credential, not an audience and a subject. With one endpoint and
// per-principal catalogs (ADR 0019), the only way to see what a principal sees
// is to present what they present — anything else would be re-deriving policy,
// which is exactly the mistake this tool exists to avoid.
type CatalogRequest struct {
	// Credential is the API key to inspect as.
	Credential string
	Identity   Identity
	// FullDescriptions keeps whole descriptions rather than first lines. Off by
	// default because a poisoned description is often long, and a console list
	// that renders it in full is doing the attacker's layout work.
	FullDescriptions bool
}

// Catalog lists the tools an identity actually receives.
func (c *Client) Catalog(ctx context.Context, req CatalogRequest) (api.Catalog, error) {
	out := api.Catalog{
		Subject: req.Identity.Subject,
		Tools:   []api.CatalogTool{},
	}

	session, observed, err := c.connect(ctx, req.Credential)
	if err != nil {
		return out, err
	}
	defer session.Close()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return out, classify(observed.status(), c.GatewayURL,
			fmt.Errorf("listing tools: %w", err))
	}

	out.TTLMs = res.TTLMs
	out.CacheScope = res.CacheScope
	if init := session.InitializeResult(); init != nil {
		out.ProtocolVersion = init.ProtocolVersion
		out.ServerName = init.ServerInfo.Name
	}

	// Who the gateway decided this credential is. The request could not say —
	// the tenant comes from the key, and reporting back what the caller claimed
	// would answer a different question than the one this screen asks.
	tenant, subject := observed.resolved()
	out.Tenant = tenant
	if subject != "" {
		out.Subject = subject
	}
	for _, tool := range res.Tools {
		namespace, _, _ := strings.Cut(tool.Name, ".")
		description := tool.Description
		if !req.FullDescriptions {
			description = firstLine(description)
		}
		out.Tools = append(out.Tools, api.CatalogTool{
			Name: tool.Name, Namespace: namespace,
			Title: tool.Title, Description: description,
		})
	}
	return out, nil
}

// CallRequest exercises one tool as one identity.
type CallRequest struct {
	Credential string
	Tool       string
	Arguments  map[string]any
	Identity   Identity

	// RequestState and Responses continue a deferred (MRTR) call. Both or
	// neither: a response map without the state it was issued against cannot be
	// bound to the original call.
	RequestState string
	Responses    sdk.InputResponseMap
}

// Call invokes a tool through the gateway and reports what came back.
func (c *Client) Call(ctx context.Context, req CallRequest) (api.CallResult, error) {
	out := api.CallResult{Tool: req.Tool}
	if strings.TrimSpace(req.Tool) == "" {
		return out, fmt.Errorf("%w: a tool name is required", ErrInvalidRequest)
	}
	if len(req.Responses) > 0 && req.RequestState == "" {
		return out, fmt.Errorf(
			"%w: responses were supplied without the requestState they answer; "+
				"the gateway cannot bind them to the original call", ErrInvalidRequest)
	}

	session, observed, err := c.connect(ctx, req.Credential)
	if err != nil {
		return out, err
	}
	defer session.Close()

	params := &sdk.CallToolParams{
		Name:           req.Tool,
		Arguments:      req.Arguments,
		RequestState:   req.RequestState,
		InputResponses: req.Responses,
	}

	started := time.Now()
	res, err := session.CallTool(ctx, params)
	elapsed := time.Since(started)
	if err != nil {
		return out, classify(observed.status(), c.GatewayURL,
			fmt.Errorf("calling %s: %w", req.Tool, err))
	}

	out.IsError = res.IsError
	out.NeedsInput = res.NeedsInput()
	out.DurationMS = elapsed.Milliseconds()
	out.Text = resultText(res)

	if detail, ok := res.Meta["mcpdoll"]; ok {
		if raw, err := json.Marshal(detail); err == nil {
			out.GatewayDetail = raw
		}
	}
	if res.NeedsInput() {
		for id := range res.InputRequests {
			out.InputRequests = append(out.InputRequests, id)
		}
		// Map order is randomised; a console that re-renders the same deferral
		// with the questions shuffled looks broken.
		sort.Strings(out.InputRequests)
		out.RequestState = res.RequestState
	}
	return out, nil
}

// connect opens a session and returns the recorder that saw its HTTP statuses.
//
// The recorder outlives the handshake on purpose: a tools/call that the gateway
// rejects with a 400 fails the same opaque way a network partition does, and
// the status is again the only thing that tells them apart.
func (c *Client) connect(
	ctx context.Context,
	credential string,
) (*sdk.ClientSession, *statusRecorder, error) {
	if strings.TrimSpace(credential) == "" {
		return nil, nil, fmt.Errorf(
			"%w: a credential is required; inspection presents what the principal "+
				"presents rather than re-deriving what they should see",
			ErrInvalidRequest)
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+credential)

	name := c.ClientName
	if name == "" {
		name = "mcpdoll-inspector"
	}
	version := c.Version
	if version == "" {
		version = "dev"
	}

	client := sdk.NewClient(&sdk.Implementation{
		Name: name, Title: "MCPDoll inspector", Version: version,
	}, &sdk.ClientOptions{
		// The inspector reports a deferral rather than fulfilling it. An
		// operator asking what a tool does wants to see that it demanded
		// confirmation, not to have the inspector confirm on their behalf.
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})

	// One endpoint for everyone (ADR 0019). The tenant and the toolset both
	// come from the credential.
	endpoint := strings.TrimRight(c.GatewayURL, "/") + "/mcp"

	httpClient, observed := c.identityClient(header)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, nil, classify(observed.status(), endpoint, err)
	}
	return session, observed, nil
}

// classify turns a failed handshake into the error that names what happened.
func classify(status int, endpoint string, err error) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s rejected the credential presented to it",
			ErrForbidden, endpoint)
	case http.StatusForbidden:
		return fmt.Errorf(
			"%w: the gateway refused this credential; it is reachable and "+
				"healthy, and this principal may simply not be in the serving "+
				"snapshot yet", ErrForbidden)
	case http.StatusBadRequest:
		// The gateway understood the request and rejected it — an unknown tool,
		// arguments that fail the schema. That is the caller's problem, and
		// calling it an outage points the investigation at the wrong system.
		return fmt.Errorf("%w: the gateway rejected the request: %v",
			ErrInvalidRequest, err)
	default:
		return fmt.Errorf("%w: connecting to %s: %v", ErrUnavailable, endpoint, err)
	}
}

// httpClient is for the plain HTTP endpoints, which carry no identity.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}

// identityClient returns a client that stamps one identity onto every request,
// alongside the recorder holding the first non-2xx status it observes.
func (c *Client) identityClient(header http.Header) (*http.Client, *statusRecorder) {
	base := http.DefaultTransport
	timeout := DefaultTimeout
	if c.HTTP != nil {
		if c.HTTP.Transport != nil {
			base = c.HTTP.Transport
		}
		if c.HTTP.Timeout != 0 {
			timeout = c.HTTP.Timeout
		}
	}
	recorder := &statusRecorder{}
	return &http.Client{
		Timeout:   timeout,
		Transport: &staticHeaders{base: base, header: header, observed: recorder},
	}, recorder
}

// statusRecorder remembers the first failing HTTP status on a connection.
//
// First rather than last: the handshake may retry, and a later transport error
// would otherwise overwrite the 403 that actually explains the failure.
type statusRecorder struct {
	mu    sync.Mutex
	code  int
	isSet bool
	// The gateway names who it decided the credential is, on every response.
	// Read here rather than parsed out of the `instructions` string: the
	// instructions are prose written for a model, and an inspector that scraped
	// them would break the first time somebody improved the wording.
	tenant  string
	subject string
}

func (r *statusRecorder) record(code int, header http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if tenant := header.Get(HeaderResolvedTenant); tenant != "" {
		r.tenant = tenant
	}
	if subject := header.Get(HeaderResolvedSubject); subject != "" {
		r.subject = subject
	}

	if code < 400 {
		return
	}
	if !r.isSet {
		r.code, r.isSet = code, true
	}
}

// resolved returns who the gateway says the credential is.
func (r *statusRecorder) resolved() (tenant, subject string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tenant, r.subject
}

// The gateway stamps these on every MCP response. They are the answer to "who
// did this credential turn out to be", which is not derivable from the request:
// the tenant comes from the key, not from the path (ADR 0019).
const (
	HeaderResolvedTenant  = "X-MCPDoll-Tenant"
	HeaderResolvedSubject = "X-MCPDoll-Subject-Resolved"
)

func (r *statusRecorder) status() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code
}

// staticHeaders stamps a fixed header set onto every request.
//
// The presented principal and the inspecting operator's bearer token travel in
// different headers, and both are set here at connection time. Keeping them
// separate is what makes it structurally impossible to forward an inbound token
// as though it were the caller's own.
type staticHeaders struct {
	base     http.RoundTripper
	header   http.Header
	observed *statusRecorder
}

func (t *staticHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	for k, values := range t.header {
		for _, v := range values {
			out.Header.Set(k, v)
		}
	}
	resp, err := t.base.RoundTrip(out)
	if resp != nil && t.observed != nil {
		t.observed.record(resp.StatusCode, resp.Header)
	}
	return resp, err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func resultText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// Backends reports what the data plane's prober knows.
//
// It reads the data plane's **admin** listener, not its MCP endpoint. That
// separation is deliberate on the serving side — the report names every backend
// and its address — and it means a control plane configured with only a gateway
// URL cannot fetch this, which [ErrNoAdminURL] says plainly rather than timing
// out against the wrong port.
func (c *Client) Backends(ctx context.Context) (api.BackendHealthReport, error) {
	var out api.BackendHealthReport

	if strings.TrimSpace(c.AdminURL) == "" {
		return out, ErrNoAdminURL
	}

	url := strings.TrimRight(c.AdminURL, "/") + "/admin/backends"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return out, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return out, fmt.Errorf("%w: cannot reach the data plane's admin listener at %s: %v",
			ErrUnavailable, c.AdminURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("%w: %s returned %d", ErrUnavailable, url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("%w: %s returned an unreadable body: %v", ErrUnavailable, url, err)
	}
	if out.Backends == nil {
		out.Backends = []api.BackendHealth{}
	}
	return out, nil
}
