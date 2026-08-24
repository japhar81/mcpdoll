// Copyright 2026 The MCPDoll Authors.

// Package edge is MCPDoll's MCP server: the single endpoint each audience talks
// to.
//
// The edge is deliberately thin. Protocol concerns — version negotiation,
// `server/discover`, `Mcp-Method`/`Mcp-Name` validation, MRTR result typing —
// are the SDK's job, and reimplementing them here would mean tracking a moving
// spec in two places. What the edge owns is the mapping from an audience to a
// catalog, the dispatch of a call to the right backend, and the correctness of
// the fields the gateway is uniquely responsible for: `ttlMs`, `cacheScope`, and
// the stable ordering.
//
// Stateless-mode semantics are still settling upstream, so all transport
// coupling lives in this one package behind [Edge.Handler]. A spec change should
// be a single-package edit.
package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	"github.com/mcpdoll/mcpdoll/internal/observability"
	"github.com/mcpdoll/mcpdoll/internal/platform/logging"
)

// GatewayName and GatewayVersion identify the gateway to clients.
const (
	GatewayName    = "mcpdoll"
	GatewayTitle   = "MCPDoll Gateway"
	GatewayVersion = "0.1.0"
)

// Options configures an Edge.
type Options struct {
	Store    *snapshot.Store
	Pool     *backends.Pool
	Identity IdentityResolver
	Logger   *slog.Logger

	Telemetry *observability.Provider
	Metrics   *observability.Metrics

	// Pipeline runs the hook engine. Nil means no plugins, which is the
	// phase-1 configuration and a legitimate production one for an org that
	// wants pure aggregation.
	Pipeline Pipeline

	// GraceWindow is how long a tool from an unreachable backend stays listed.
	GraceWindow time.Duration

	// DriftGuard, when set, is consulted before every tool call and may refuse
	// one whose backend definition has changed since admission. Optional: a
	// gateway with no prober serves whatever the snapshot admitted, which is
	// the behaviour without this and remains correct — just blind.
	DriftGuard DriftGuard

	// StateSigner signs MRTR requestState envelopes. Required only if a
	// backend or plugin can ask for client input; without it such a call is
	// refused rather than served with an unsigned state a client could forge.
	StateSigner *StateSigner
}

// Pipeline is the hook engine as the edge sees it.
//
// Declared here as an interface rather than imported concretely so the edge can
// be tested — and run — without a plugin host, and so the pipeline package does
// not have to know about MCP types.
type Pipeline interface {
	// OnCatalog may filter or annotate a catalog for a principal. It returns
	// the tools to serve and whether the result became identity-specific.
	OnCatalog(ctx context.Context, req *CatalogRequest) (*CatalogDecision, error)
	// OnToolCall runs before dispatch. A denial or a deferral stops dispatch.
	OnToolCall(ctx context.Context, req *ToolCallRequest) (*ToolCallDecision, error)
	// OnToolResult runs after dispatch and may mutate the result.
	OnToolResult(ctx context.Context, req *ToolResultRequest) (*ToolResultDecision, error)
}

// Edge serves the MCP endpoints.
type Edge struct {
	opts Options
	log  *slog.Logger

	// mu guards the served version.
	mu      sync.RWMutex
	version int64

	// principals caches one MCP server per principal, built on first
	// connection. Dropped wholesale on a snapshot swap: a cached server holds
	// a catalog composed from grants that the new snapshot may have revoked,
	// and per-entry invalidation is a chance to miss one (ADR 0018).
	principals *principalCache

	streamable *mcp.StreamableHTTPHandler
	router     chi.Router
}

// principalServer is one principal's MCP server plus the view it was built from.
type principalServer struct {
	view   *snapshot.PrincipalView
	server *mcp.Server
}

// principalCache holds per-principal servers for the life of one snapshot.
type principalCache struct {
	mu      sync.RWMutex
	entries map[string]*principalServer
}

func newPrincipalCache() *principalCache {
	return &principalCache{entries: map[string]*principalServer{}}
}

func (c *principalCache) get(id string) (*principalServer, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ps, ok := c.entries[id]
	return ps, ok
}

// put stores a server, returning whichever one wins if two connections for the
// same principal raced. Both are equivalent; returning the stored one keeps a
// single instance per principal so the SDK's server state is not split.
func (c *principalCache) put(id string, ps *principalServer) *principalServer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[id]; ok {
		return existing
	}
	c.entries[id] = ps
	return ps
}

// Purge drops every cached server. Called on snapshot activation.
func (c *principalCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]*principalServer{}
}

// New builds an Edge and wires it to the snapshot store.
func New(opts Options) (*Edge, error) {
	if opts.Store == nil {
		return nil, errors.New("edge: a snapshot store is required")
	}
	if opts.Pool == nil {
		return nil, errors.New("edge: a backend pool is required")
	}
	if opts.Identity == nil {
		return nil, errors.New("edge: an identity resolver is required; " +
			"a gateway that cannot identify its caller cannot enforce anything")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Telemetry == nil {
		opts.Telemetry = observability.NoopProvider()
	}
	if opts.Metrics == nil {
		m, err := observability.NewMetrics(opts.Telemetry.Meter)
		if err != nil {
			return nil, fmt.Errorf("edge: %w", err)
		}
		opts.Metrics = m
	}
	if opts.GraceWindow <= 0 {
		opts.GraceWindow = 10 * time.Minute
	}

	e := &Edge{
		opts:       opts,
		log:        opts.Logger,
		principals: newPrincipalCache(),
	}

	// One streamable handler for everyone. The principal is resolved from the
	// credential, not the path (ADR 0019). Stateless is mandatory for 2026-07-28 over HTTP — the
	// SDK rejects the modern protocol on a stateful transport — and it is also
	// what makes the data plane horizontally scalable with no shared session
	// state.
	e.streamable = mcp.NewStreamableHTTPHandler(e.serverForRequest, &mcp.StreamableHTTPOptions{
		Stateless: true,
		Logger:    opts.Logger,
		// A gateway is fronted by an ingress that rewrites Host, so the SDK's
		// loopback/Host check would reject legitimate traffic. Origin
		// protection belongs at the ingress.
		DisableLocalhostProtection: true,
	})

	r := chi.NewRouter()
	// One endpoint. There is deliberately no /mcp/{audience} alias: an audience
	// no longer determines a catalog, so accepting a slug and ignoring it would
	// be the most confusing possible behaviour (ADR 0019).
	r.Handle("/mcp", http.HandlerFunc(e.serveMCP))
	r.Handle("/mcp/*", http.HandlerFunc(e.serveMCP))
	r.Get("/healthz", e.serveHealth)
	r.Get("/readyz", e.serveReady)
	e.router = r

	// Rebuild on activation rather than checking for a new snapshot per
	// request: building a server per audience costs real work, and a request
	// should never pay for a configuration change.
	//
	// Registered as the preparer, not an observer, so a snapshot the edge cannot
	// serve is refused instead of committed.
	opts.Store.SetPreparer(e.rebuild)
	if current := opts.Store.Current(); current != nil {
		if err := e.rebuild(current); err != nil {
			return nil, fmt.Errorf("edge: the snapshot already in the store cannot be served: %w", err)
		}
	}

	return e, nil
}

// Handler is the edge's HTTP handler.
func (e *Edge) Handler() http.Handler { return e.router }

// rebuild constructs a fresh MCP server per audience from a snapshot view.
//
// Registered as the store's *preparer*, so it runs before the swap and its
// failure refuses the snapshot. That ordering is the point: constructing the
// servers is where a malformed definition is actually discovered, and
// discovering it after the commit would mean a bad publish had already taken
// effect on every instance.
//
// Everything is built into a new map first and installed with a single
// assignment, so a partially-built set is never visible to a request.
func (e *Edge) rebuild(view *snapshot.View) error {
	// Nothing is precomputed here any more. With one endpoint and a catalog
	// per principal (ADR 0019), there is no fixed set of servers to build: a
	// principal's MCP server is constructed on its first connection and cached
	// with the view, which dies on the next swap.
	//
	// This is a weaker guard than the audience version, which discovered a
	// malformed definition at publish time and refused the snapshot. Now a bad
	// tool fails one principal's connection instead. Admission-time validation
	// of every tool therefore matters more, not less — the builder rejects a
	// tool with no input schema for exactly this reason.
	e.principals.Purge()

	e.opts.Pool.Sync(view.Servers())

	e.mu.Lock()
	e.version = view.Version
	e.mu.Unlock()

	ctx := context.Background()
	e.opts.Metrics.SnapshotVersion.Record(ctx, view.Version)
	e.opts.Metrics.SnapshotSwaps.Add(ctx, 1)
	e.log.InfoContext(ctx, "serving new snapshot",
		logging.FieldSnapshot, view.Version,
		"tenants", len(view.TenantSlugs()),
		"backends", len(view.Servers()))
	return nil
}

// buildServer registers every tool in an audience's catalog on a new MCP server.
//
// The SDK's AddTool *panics* on a tool it considers malformed. A panic here would
// take the whole gateway down over one bad definition, so it is recovered and
// converted into a refused activation. Defensive rather than expected: the view
// builder already rejects the definitions we know of, and this catches the ones a
// future SDK version decides it dislikes.
func (e *Edge) buildServer(view *snapshot.View, av *snapshot.PrincipalView) (srv *mcp.Server, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("building the MCP server panicked: %v", r)
		}
	}()

	srv = mcp.NewServer(&mcp.Implementation{
		Name:    GatewayName,
		Title:   GatewayTitle,
		Version: GatewayVersion,
	}, &mcp.ServerOptions{
		Instructions: principalInstructions(av),
		Logger:       e.log,
		// Advertise tools even before any are registered, so an audience whose
		// bundles are currently empty still presents a tools capability rather
		// than looking like a server that does not do tools at all.
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{ListChanged: true},
		},
	})

	for _, tool := range av.Tools {
		srv.AddTool(toolDefinitionToMCP(tool), e.toolHandler(view, av, tool))
	}

	// Middleware, not a wrapped handler, because `tools/list` is generated by
	// the SDK from the registered set and there is no handler of ours to wrap.
	srv.AddReceivingMiddleware(e.catalogMiddleware(view, av))

	return srv, nil
}

// principalInstructions is the `instructions` string clients receive.
//
// It names the tenant and subject, which is how a client can tell *which
// identity it is operating as*. With one endpoint for everyone, a misconfigured
// credential yields a smaller toolset rather than an error (ADR 0019), and this
// is the line that makes that visible rather than silent.
func principalInstructions(av *snapshot.PrincipalView) string {
	name := av.Tenant.Name
	if name == "" {
		name = av.Tenant.Slug
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Aggregated tools for %s, served by MCPDoll.\n", name)
	fmt.Fprintf(&b, "You are acting as %s in tenant %s.\n",
		av.Principal.Subject, av.Tenant.Slug)
	fmt.Fprintf(&b, "Tool names are namespaced as <prefix>.<tool>. ")
	b.WriteString("Every definition here has been reviewed and admitted; ")
	b.WriteString("it is not read live from the upstream server.")
	return b.String()
}

// toolDefinitionToMCP converts an admitted definition into the SDK's tool type.
//
// The qualified name is what the client sees. The schema is the *admitted*
// document, canonicalized at admission — never whatever the backend is currently
// serving.
func toolDefinitionToMCP(t *snapshot.Tool) *mcp.Tool {
	out := &mcp.Tool{
		Name:        t.Def.QualifiedName,
		Title:       t.Def.Title,
		Description: t.Def.Description,
	}
	// json.RawMessage, not []byte: encoding/json base64-encodes a plain []byte,
	// which the SDK then rejects as "can't marshal input schema to a JSON
	// object". The distinction is invisible until a schema actually goes over the
	// wire.
	if t.Def.InputSchemaJson != "" {
		out.InputSchema = json.RawMessage(t.Def.InputSchemaJson)
	}
	if t.Def.OutputSchemaJson != "" {
		out.OutputSchema = json.RawMessage(t.Def.OutputSchemaJson)
	}
	return out
}

// serveMCP resolves the principal from its credential and delegates to the
// SDK's streamable handler.
func (e *Edge) serveMCP(w http.ResponseWriter, r *http.Request) {
	e.mu.RLock()
	version := e.version
	e.mu.RUnlock()

	if version == 0 {
		// Started but no snapshot yet. 503 with Retry-After is honest about the
		// state and tells an ingress not to mark the instance permanently bad.
		w.Header().Set("Retry-After", "5")
		http.Error(w, "gateway has not yet loaded a configuration snapshot",
			http.StatusServiceUnavailable)
		return
	}

	principal, err := e.opts.Identity.Resolve(r.Header)
	if err != nil {
		// No anonymous principal and no default tenant. Without a resolvable
		// credential there is nothing to compose a catalog from, and defaulting
		// would be a way to get a catalog without proving who you are.
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcpdoll"`)
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	if _, err := e.serverFor(r.Context(), principal); err != nil {
		// The credential resolved but the snapshot does not carry this
		// principal — it was created after the last publish. A 403 rather than
		// a 500: nothing is broken, the grants simply have not been published.
		e.log.WarnContext(r.Context(), "no principal in the serving snapshot",
			logging.FieldPrincipal, principal.Subject,
			logging.FieldSnapshot, version, "err", err)
		http.Error(w,
			"this credential is not in the serving snapshot; publish a snapshot "+
				"to pick up recent grant changes", http.StatusForbidden)
		return
	}

	e.streamable.ServeHTTP(w, r.WithContext(withRequestScope(r.Context(), requestScope{
		Principal: principal,
	})))
}

// serverFor returns the principal's MCP server, building it on first use.
func (e *Edge) serverFor(ctx context.Context, principal backends.Principal) (*principalServer, error) {
	if cached, ok := e.principals.get(principal.ID); ok {
		return cached, nil
	}

	view := e.opts.Store.Current()
	if view == nil {
		return nil, errors.New("edge: no snapshot")
	}

	pv, err := view.Principal(ctx, principal.ID)
	if err != nil {
		return nil, err
	}

	server, err := e.buildServer(view, pv)
	if err != nil {
		return nil, err
	}
	return e.principals.put(principal.ID, &principalServer{view: pv, server: server}), nil
}

// serverForRequest is the SDK's getServer callback.
func (e *Edge) serverForRequest(r *http.Request) *mcp.Server {
	scope, ok := scopeFromContext(r.Context())
	if !ok {
		// serveMCP always installs the scope before delegating, so this means
		// the handler was reached another way.
		return nil
	}
	ps, err := e.serverFor(r.Context(), scope.Principal)
	if err != nil {
		return nil
	}
	return ps.server
}

func (e *Edge) serveHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// serveReady reports readiness, which for the data plane means "has a snapshot".
// An instance with no snapshot must not receive traffic: it has nothing to serve
// and would 503 every request.
func (e *Edge) serveReady(w http.ResponseWriter, _ *http.Request) {
	e.mu.RLock()
	version := e.version
	e.mu.RUnlock()

	tenants, tools := 0, 0
	if view := e.opts.Store.Current(); view != nil {
		slugs := view.TenantSlugs()
		tenants = len(slugs)
		tools = len(view.Proto().Tools)
	}

	w.Header().Set("Content-Type", "application/json")
	if version == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"no snapshot"}`))
		return
	}
	// Tenants and admitted tools, not audiences: "serving N audiences" no
	// longer describes anything (ADR 0019). Still a count rather than names —
	// enumerating tenants to an unauthenticated caller is the same information
	// leak enumerating audiences was.
	fmt.Fprintf(w, `{"status":"ok","snapshot_version":%d,"tenants":%d,"tools":%d}`,
		version, tenants, tools)
}

// SnapshotVersion reports what the edge is serving, for the admin surface.
func (e *Edge) SnapshotVersion() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.version
}

// TenantSlugs lists the tenants the serving snapshot carries.
func (e *Edge) TenantSlugs() []string {
	view := e.opts.Store.Current()
	if view == nil {
		return nil
	}
	return view.TenantSlugs()
}

// ---------------------------------------------------------- request scope ----

type requestScopeKey struct{}

type requestScope struct {
	Principal backends.Principal
}

func withRequestScope(ctx context.Context, rs requestScope) context.Context {
	return context.WithValue(ctx, requestScopeKey{}, rs)
}

// scopeFromContext recovers the request scope. It returns ok=false rather than a
// zero value so a caller cannot silently proceed with an empty principal.
func scopeFromContext(ctx context.Context) (requestScope, bool) {
	rs, ok := ctx.Value(requestScopeKey{}).(requestScope)
	return rs, ok
}

// principalFor recovers the principal for a request.
//
// The context is the fast path, but the HTTP headers on the request are the
// authoritative fallback: the SDK owns the handler context, and depending on it
// propagating a value across a transport boundary would make identity resolution
// silently fail if that ever changed. Re-resolving from headers is cheap.
func (e *Edge) principalFor(ctx context.Context, extra *mcp.RequestExtra) (backends.Principal, error) {
	if rs, ok := scopeFromContext(ctx); ok && rs.Principal.Subject != "" {
		return rs.Principal, nil
	}
	if extra != nil && extra.Header != nil {
		return e.opts.Identity.Resolve(extra.Header)
	}
	return backends.Principal{}, ErrUnauthenticated
}

// ------------------------------------------------------------- telemetry -----

func (e *Edge) startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return e.opts.Telemetry.Tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...))
}

func recordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func metricAttrs(kv ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributes(kv...)
}

// DriftGuard reports tools that must not be served.
//
// An interface, not the health registry itself, so the edge does not depend on
// the prober: a data plane can run without one, and the edge's tests do not
// need a probe loop to exercise a refusal.
type DriftGuard interface {
	// Blocked reports whether a qualified tool must be refused, and why.
	//
	// The reason is returned alongside the answer because it ends up in an
	// error a model reads, and a refusal without a cause produces a retry loop.
	Blocked(qualifiedName string) (reason string, blocked bool)
}
