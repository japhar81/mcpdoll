// Copyright 2026 The MCPDoll Authors.

// Package backends manages the data plane's outbound MCP client connections.
//
// One pool entry per registered server, each holding a live MCP client session,
// a circuit breaker, and the token-exchange configuration for that backend. The
// pool is rebuilt lazily as the snapshot changes: an entry whose endpoint is
// unchanged keeps its session and its breaker state across a snapshot swap, so
// publishing an unrelated server does not reset every backend's health.
package backends

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoll/mcpdoll/internal/observability"
	"github.com/mcpdoll/mcpdoll/internal/platform/logging"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// ErrCircuitOpen reports a backend whose breaker is open.
//
// It is a distinct error so the edge can turn it into a *structured,
// model-legible* failure — "this tool is temporarily unavailable, do not retry
// immediately" — rather than an opaque transport error the model will retry in a
// loop.
type ErrCircuitOpen struct {
	Backend string
	Until   time.Time
}

func (e *ErrCircuitOpen) Error() string {
	return fmt.Sprintf("backends: circuit open for %q until %s",
		e.Backend, e.Until.Format(time.RFC3339))
}

// ErrNotConnected reports a backend the pool has not managed to reach.
type ErrNotConnected struct {
	Backend string
	Cause   error
}

func (e *ErrNotConnected) Error() string {
	return fmt.Sprintf("backends: %q is not connected: %v", e.Backend, e.Cause)
}

func (e *ErrNotConnected) Unwrap() error { return e.Cause }

// TokenSource produces the credential to present to a backend.
//
// This is the RFC 8693 seam. The interface is deliberately narrow so the IdP
// side can be swapped — and so the *absence* of a passthrough option is
// structural: there is no method here that accepts the caller's token and hands
// it onward, so token passthrough is not something a future change can add by
// accident.
type TokenSource interface {
	// Exchange returns a credential for the given backend on behalf of the
	// principal. Implementations must derive a new, narrower credential; they
	// must never return the inbound token.
	Exchange(ctx context.Context, backend *snapshotpb.Server, principal Principal) (Credential, error)
}

// Principal is who the request is on behalf of.
type Principal struct {
	// ID is how the snapshot addresses this principal — the user id, or the
	// API key id for an agent credential. Authorization keys on this; Subject
	// is for humans and may change.
	ID string
	// Subject is the human-readable identifier, typically an email.
	Subject string
	// Groups from the IdP, used for entitlement filtering and policy matching.
	Groups []string
	// Claims carries additional claims a backend may need mapped to headers.
	Claims map[string]string
	// Tenant slug the principal belongs to. Resolved from the credential, not
	// from the request path (ADR 0019).
	Tenant string
}

// Credential is what gets attached to an outbound backend request.
type Credential struct {
	// Header is where the token goes, defaulting to "Authorization".
	Header string
	// Value is the full header value, e.g. "Bearer eyJ…".
	Value string
	// Extra headers derived from claim mappings.
	Extra map[string]string
	// ExpiresAt lets the pool avoid re-exchanging on every call.
	ExpiresAt time.Time
}

// Options configures a Pool.
type Options struct {
	Logger      *slog.Logger
	Telemetry   *observability.Provider
	Metrics     *observability.Metrics
	TokenSource TokenSource

	// FailureThreshold is consecutive failures before a breaker opens.
	FailureThreshold int
	// Cooldown is how long a breaker stays open before allowing a probe.
	Cooldown time.Duration
	// DialTimeout bounds connecting to a backend.
	DialTimeout time.Duration
	// HTTPClient is used for outbound requests; nil means a sensible default.
	HTTPClient *http.Client
}

// Pool holds one entry per backend.
type Pool struct {
	opts Options
	log  *slog.Logger

	// baseCtx bounds the lifetime of every pooled session. It is deliberately
	// *not* derived from any request context: the MCP SDK retains the context it
	// was connected with for the connection's whole life, so tying a session to
	// the request that happened to create it would tear the session down the
	// moment that request finished. That is invisible for a stateless backend
	// (each call is an independent POST) and fatal for a stateful one.
	baseCtx context.Context
	cancel  context.CancelFunc

	mu      sync.Mutex
	entries map[Target]*entry
}

// New builds a pool.
func New(opts Options) *Pool {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.FailureThreshold < 1 {
		opts.FailureThreshold = 5
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = 30 * time.Second
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	return &Pool{
		opts:    opts,
		log:     opts.Logger,
		baseCtx: baseCtx,
		cancel:  cancel,
		entries: map[Target]*entry{},
	}
}

// entry is one backend's connection state.
// Target identifies one connectable backend: a server *for a tenant*.
//
// The same server is a different host per tenant (ADR 0017), so a server id
// alone no longer names something a session can be opened to.
type Target struct {
	ServerID string
	TenantID string
}

// String renders a target for logs and errors.
func (t Target) String() string { return t.ServerID + "@" + t.TenantID }

type entry struct {
	server *snapshotpb.Server
	// endpoint is this tenant's primary host for the server.
	endpoint string
	target   Target

	mu      sync.Mutex
	session *mcp.ClientSession
	// negotiated records the protocol version actually in use, which the
	// console displays and the conformance tests assert.
	negotiated string
	connectErr error
	// cancelSession tears down this entry's session context on close.
	cancelSession context.CancelFunc
	breaker       *breaker
	// credentials cached per principal subject, so a burst of calls from one
	// agent does not hammer the token endpoint.
	creds map[string]Credential
}

// Sync reconciles the pool with a snapshot's server set.
//
// Entries for servers that are gone are closed. Entries whose endpoint is
// unchanged are kept *with their breaker state*: a snapshot publish is not
// evidence that an unhealthy backend recovered, and resetting every breaker on
// every publish would let a flapping backend look healthy for one request after
// each unrelated deploy.
func (p *Pool) Sync(servers []*snapshotpb.Server) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// One entry per (server, tenant): each tenant's binding is a separate
	// deployment with its own session, its own breaker, and its own health.
	// Sharing a breaker across tenants would let one tenant's outage shed
	// another tenant's traffic.
	type want struct {
		server   *snapshotpb.Server
		endpoint string
	}
	wanted := map[Target]want{}
	for _, s := range servers {
		for _, b := range s.Bindings {
			wanted[Target{ServerID: s.Id, TenantID: b.TenantId}] =
				want{server: s, endpoint: b.Primary}
		}
	}

	for target, e := range p.entries {
		w, keep := wanted[target]
		if !keep {
			e.close()
			delete(p.entries, target)
			continue
		}
		if w.endpoint != e.endpoint {
			// A moved endpoint is a different backend as far as health is
			// concerned, so the session and the breaker both reset.
			e.close()
			p.entries[target] = newEntry(target, w.server, w.endpoint, p.opts)
			continue
		}
		e.mu.Lock()
		e.server = w.server
		e.mu.Unlock()
	}

	for target, w := range wanted {
		if _, ok := p.entries[target]; !ok {
			p.entries[target] = newEntry(target, w.server, w.endpoint, p.opts)
		}
	}
}

func newEntry(target Target, s *snapshotpb.Server, endpoint string, opts Options) *entry {
	threshold := opts.FailureThreshold
	cooldown := opts.Cooldown
	if h := s.Health; h != nil && h.EjectAfterFailures > 0 {
		threshold = int(h.EjectAfterFailures)
	}
	return &entry{
		server:   s,
		endpoint: endpoint,
		target:   target,
		breaker:  newBreaker(threshold, cooldown),
		creds:    map[string]Credential{},
	}
}

func (e *entry) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session != nil {
		_ = e.session.Close()
		e.session = nil
	}
	if e.cancelSession != nil {
		e.cancelSession()
		e.cancelSession = nil
	}
}

func (p *Pool) Close() {
	p.mu.Lock()
	for id, e := range p.entries {
		e.close()
		delete(p.entries, id)
	}
	p.mu.Unlock()
	p.cancel()
}

// Servers lists the ids the pool currently holds.
func (p *Pool) Targets() []Target {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Target, 0, len(p.entries))
	for id := range p.entries {
		out = append(out, id)
	}
	return out
}

func (p *Pool) entryFor(target Target) (*entry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[target]
	return e, ok
}

// NegotiatedVersion reports the protocol version in use with a backend, or "".
func (p *Pool) NegotiatedVersion(target Target) string {
	e, ok := p.entryFor(target)
	if !ok {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.negotiated
}

// CircuitState reports a backend's breaker state.
func (p *Pool) CircuitState(target Target) State {
	e, ok := p.entryFor(target)
	if !ok {
		return StateClosed
	}
	return e.breaker.State()
}

// Call is one outbound tool invocation.
//
// ToolName is the backend's own name, not the gateway's qualified name: the
// prefix is a gateway concept and a backend has never heard of it.
//
// RequestState and InputResponses carry an MRTR retry. RequestState must already
// be the *backend's* own state, unwrapped from the gateway's envelope by the
// caller — the pool must never see the envelope, and a backend must never see
// anything but the bytes it produced.
type Call struct {
	// Target is the server *for a tenant*: the same server is a different host
	// per tenant, so a server id alone does not name something callable.
	Target         Target
	ToolName       string
	Arguments      any
	Meta           map[string]any
	RequestState   string
	InputResponses mcp.InputResponseMap
}

// CallTool dispatches a tool call to a backend.
func (p *Pool) CallTool(
	ctx context.Context,
	call Call,
	principal Principal,
) (*mcp.CallToolResult, error) {
	target, toolName := call.Target, call.ToolName
	e, ok := p.entryFor(target)
	if !ok {
		return nil, &ErrNotConnected{Backend: target.String(), Cause: errors.New("no pool entry")}
	}

	if !e.breaker.Allow() {
		return nil, &ErrCircuitOpen{Backend: e.server.Name, Until: e.breaker.OpenUntil()}
	}

	session, err := p.session(ctx, e, principal)
	if err != nil {
		e.breaker.Failure()
		return nil, err
	}

	params := &mcp.CallToolParams{
		Name:           toolName,
		Arguments:      call.Arguments,
		RequestState:   call.RequestState,
		InputResponses: call.InputResponses,
	}
	if len(call.Meta) > 0 {
		params.Meta = mcp.Meta(call.Meta)
	}

	// Dispatch on a context detached from the inbound request's values. See
	// [detachContext]: the SDK stores the *inbound* protocol version under an
	// unexported context key, and its outbound transport reads that same key to
	// stamp `Mcp-Protocol-Version`. Reusing the caller's context therefore tells
	// a 2025-11-25 backend that the request is 2026-07-28, and the backend
	// rejects it with 400.
	dispatchCtx, release := detachContext(ctx)
	defer release()

	start := time.Now()
	res, err := session.CallTool(dispatchCtx, params)
	elapsed := time.Since(start)

	if err != nil {
		// A transport or protocol failure is the backend's fault and counts
		// against the breaker.
		e.breaker.Failure()
		p.log.WarnContext(ctx, "backend call failed",
			logging.FieldBackend, e.server.Name,
			logging.FieldToolName, toolName,
			logging.FieldDurationMS, elapsed.Milliseconds(),
			"err", err)
		return nil, fmt.Errorf("backends: %s: calling %s: %w", e.server.Name, toolName, err)
	}

	// A tool-level error (`isError`) is the *tool* saying no, not the backend
	// failing. Counting it against the breaker would eject a healthy backend
	// because a client kept passing invalid arguments.
	e.breaker.Success()
	return res, nil
}

// ListTools fetches a backend's live catalog. Used by the prober for drift
// detection — never to answer a client's tools/list, which is served from the
// snapshot.
func (p *Pool) ListTools(ctx context.Context, target Target, principal Principal) ([]*mcp.Tool, error) {
	e, ok := p.entryFor(target)
	if !ok {
		return nil, &ErrNotConnected{Backend: target.String(), Cause: errors.New("no pool entry")}
	}
	session, err := p.session(ctx, e, principal)
	if err != nil {
		return nil, err
	}
	listCtx, release := detachContext(ctx)
	defer release()

	var out []*mcp.Tool
	var cursor string
	for {
		res, err := session.ListTools(listCtx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("backends: %s: listing tools: %w", e.server.Name, err)
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
		// A backend that pages forever would otherwise hang the prober.
		if len(out) > maxToolsPerBackend {
			return nil, fmt.Errorf("backends: %s: returned more than %d tools; refusing to page further",
				e.server.Name, maxToolsPerBackend)
		}
	}
}

// maxToolsPerBackend bounds a paginating backend. Admission caps a server far
// below this; the limit exists so a misbehaving backend cannot make the prober
// loop indefinitely.
const maxToolsPerBackend = 10000

// session returns a connected session, dialing if necessary.
func (p *Pool) session(ctx context.Context, e *entry, principal Principal) (*mcp.ClientSession, error) {
	e.mu.Lock()
	if e.session != nil {
		s := e.session
		e.mu.Unlock()
		return s, nil
	}
	server := e.server
	e.mu.Unlock()

	cred, err := p.credential(ctx, e, server, principal)
	if err != nil {
		return nil, err
	}

	// The session's context is pool-lifetime, not request-lifetime (see the
	// baseCtx comment). The handshake is still bounded: a watchdog cancels the
	// session context only if Connect has not returned within DialTimeout, so a
	// hung backend cannot block startup while a healthy one keeps its session.
	sessionCtx, cancelSession := context.WithCancel(p.baseCtx)
	handshakeDone := make(chan struct{})
	go func() {
		timer := time.NewTimer(p.opts.DialTimeout)
		defer timer.Stop()
		select {
		case <-handshakeDone:
		case <-timer.C:
			cancelSession()
		case <-p.baseCtx.Done():
			cancelSession()
		}
	}()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "mcpdoll",
		Title:   "MCPDoll Gateway",
		Version: Version,
	}, &mcp.ClientOptions{
		// MRTR is handled by the gateway, not automatically by the client: an
		// `input_required` result from a backend has to be surfaced to *our*
		// client through our own signed envelope, so the SDK must not silently
		// fulfil it on our behalf.
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})

	transport := &mcp.StreamableClientTransport{
		Endpoint:   e.endpoint,
		HTTPClient: p.credentialedClient(cred),
		// The gateway makes request/response calls and does not want
		// server-initiated messages: in stateless mode there is no client to
		// deliver them to anyway.
		DisableStandaloneSSE: true,
	}

	session, err := client.Connect(sessionCtx, transport, nil)
	close(handshakeDone)
	if err != nil {
		cancelSession()
		e.mu.Lock()
		e.connectErr = err
		e.mu.Unlock()
		return nil, &ErrNotConnected{Backend: server.Name, Cause: err}
	}

	negotiated := ""
	if init := session.InitializeResult(); init != nil {
		negotiated = init.ProtocolVersion
	}

	e.mu.Lock()
	// Another goroutine may have connected while we were dialing; keep whichever
	// session won and close ours, rather than leaking one.
	if e.session != nil {
		other := e.session
		e.mu.Unlock()
		_ = session.Close()
		cancelSession()
		return other, nil
	}
	e.session = session
	e.cancelSession = cancelSession
	e.negotiated = negotiated
	e.connectErr = nil
	e.mu.Unlock()

	p.log.InfoContext(ctx, "connected to backend",
		logging.FieldBackend, server.Name,
		logging.FieldProtocol, negotiated)
	return session, nil
}

// credential obtains (and caches) the credential for a backend and principal.
func (p *Pool) credential(
	ctx context.Context,
	e *entry,
	server *snapshotpb.Server,
	principal Principal,
) (Credential, error) {
	// No token-exchange configuration means the backend needs no credential —
	// an internal service on a trusted network. It emphatically does *not* mean
	// "forward the caller's token".
	if server.TokenExchange == nil || p.opts.TokenSource == nil {
		return Credential{}, nil
	}

	e.mu.Lock()
	if cached, ok := e.creds[principal.Subject]; ok {
		// Re-exchange a little before expiry so a call does not fail on a token
		// that expired between the check and the request.
		if cached.ExpiresAt.IsZero() || time.Until(cached.ExpiresAt) > 30*time.Second {
			e.mu.Unlock()
			return cached, nil
		}
	}
	e.mu.Unlock()

	cred, err := p.opts.TokenSource.Exchange(ctx, server, principal)
	if err != nil {
		return Credential{}, fmt.Errorf("backends: %s: token exchange for %q: %w",
			server.Name, principal.Subject, err)
	}

	e.mu.Lock()
	e.creds[principal.Subject] = cred
	e.mu.Unlock()
	return cred, nil
}

// credentialedClient wraps the HTTP client so the exchanged credential is
// attached to every outbound request.
//
// Note what is *not* here: nothing copies a header from the inbound request.
// The only Authorization header an outbound request can carry is one this
// function put there, from a credential the token source minted.
func (p *Pool) credentialedClient(cred Credential) *http.Client {
	if cred.Value == "" && len(cred.Extra) == 0 {
		return p.opts.HTTPClient
	}
	base := p.opts.HTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone := *p.opts.HTTPClient
	clone.Transport = &credentialTransport{base: base, cred: cred}
	return &clone
}

type credentialTransport struct {
	base http.RoundTripper
	cred Credential
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating: a RoundTripper must not modify the request it is
	// given, and the caller may retry with the same one.
	out := req.Clone(req.Context())
	if t.cred.Value != "" {
		header := t.cred.Header
		if header == "" {
			header = "Authorization"
		}
		out.Header.Set(header, t.cred.Value)
	}
	for k, v := range t.cred.Extra {
		out.Header.Set(k, v)
	}
	return t.base.RoundTrip(out)
}

// Version is the gateway version reported to backends during initialize.
var Version = "0.1.0"

// detachContext builds an outbound context that inherits cancellation and any
// deadline from ctx but *none* of its values.
//
// This exists because of a specific, non-obvious hazard. The MCP SDK's
// streamable server stashes the inbound request's negotiated protocol version
// under an unexported context key so handlers can read it. Its streamable
// *client* reads that same unexported key to decide what `Mcp-Protocol-Version`
// header to send. A gateway therefore cannot hand an inbound handler context
// straight to an outbound client: a request that arrived as 2026-07-28 would be
// announced as 2026-07-28 to a backend that only speaks 2025-11-25, and the
// backend rejects it outright.
//
// The key is unexported, so it cannot be deleted — the only way to be sure no
// inbound value crosses the boundary is to start from a fresh context. Trace
// context is not lost by this: it is propagated explicitly in the request's
// `_meta`, which is where MCP carries it anyway.
//
// The returned release func must always be called, or the AfterFunc registration
// leaks until ctx is done.
func detachContext(ctx context.Context) (context.Context, func()) {
	var (
		out    context.Context
		cancel context.CancelFunc
	)
	if deadline, ok := ctx.Deadline(); ok {
		out, cancel = context.WithDeadline(context.Background(), deadline)
	} else {
		out, cancel = context.WithCancel(context.Background())
	}
	// Keep cancellation linked: if the client disconnects, the backend call
	// should stop rather than run to completion for nobody.
	stop := context.AfterFunc(ctx, cancel)
	return out, func() {
		stop()
		cancel()
	}
}
