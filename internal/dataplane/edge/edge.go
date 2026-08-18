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

	// mu guards the per-audience server cache, which is replaced wholesale on
	// each snapshot activation.
	mu        sync.RWMutex
	version   int64
	audiences map[string]*audienceServer

	streamable *mcp.StreamableHTTPHandler
	router     chi.Router
}

// audienceServer is one audience's MCP server plus the view it was built from.
type audienceServer struct {
	slug   string
	view   *snapshot.AudienceView
	server *mcp.Server
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
		opts:      opts,
		log:       opts.Logger,
		audiences: map[string]*audienceServer{},
	}

	// One streamable handler for every audience; the audience is resolved from
	// the request path. Stateless is mandatory for 2026-07-28 over HTTP — the
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
	r.Route("/mcp/{audience}", func(sub chi.Router) {
		sub.Handle("/", http.HandlerFunc(e.serveMCP))
		sub.Handle("/*", http.HandlerFunc(e.serveMCP))
	})
	r.Get("/healthz", e.serveHealth)
	r.Get("/readyz", e.serveReady)
	e.router = r

	// Rebuild on activation rather than checking for a new snapshot per
	// request: building a server per audience costs real work, and a request
	// should never pay for a configuration change.
	if current := opts.Store.Current(); current != nil {
		e.rebuild(current)
	}
	opts.Store.Observe(e.rebuild)

	return e, nil
}

// Handler is the edge's HTTP handler.
func (e *Edge) Handler() http.Handler { return e.router }

// rebuild constructs a fresh MCP server per audience from a snapshot view.
func (e *Edge) rebuild(view *snapshot.View) {
	next := make(map[string]*audienceServer, len(view.AudienceSlugs()))
	for _, slug := range view.AudienceSlugs() {
		av := view.Audience(slug)
		next[slug] = &audienceServer{
			slug:   slug,
			view:   av,
			server: e.buildServer(view, av),
		}
	}

	e.opts.Pool.Sync(view.Servers())

	e.mu.Lock()
	e.audiences = next
	e.version = view.Version
	e.mu.Unlock()

	ctx := context.Background()
	e.opts.Metrics.SnapshotVersion.Record(ctx, view.Version)
	e.opts.Metrics.SnapshotSwaps.Add(ctx, 1)
	e.log.InfoContext(ctx, "serving new snapshot",
		logging.FieldSnapshot, view.Version,
		"audiences", len(next),
		"backends", len(view.Servers()))
}

// buildServer registers every tool in an audience's catalog on a new MCP server.
func (e *Edge) buildServer(view *snapshot.View, av *snapshot.AudienceView) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    GatewayName,
		Title:   GatewayTitle,
		Version: GatewayVersion,
	}, &mcp.ServerOptions{
		Instructions: audienceInstructions(av),
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

	return srv
}

// audienceInstructions is the `instructions` string clients receive. It names
// the gateway and the audience so an operator reading a client's logs can tell
// which endpoint produced a catalog.
func audienceInstructions(av *snapshot.AudienceView) string {
	name := av.Audience.Name
	if name == "" {
		name = av.Audience.Slug
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Aggregated tools for %s, served by MCPDoll.\n", name)
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

// serveMCP resolves the audience, authorizes the principal, and delegates to the
// SDK's streamable handler.
func (e *Edge) serveMCP(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "audience")

	e.mu.RLock()
	as, ok := e.audiences[slug]
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
	if !ok {
		http.Error(w, fmt.Sprintf("no audience %q is served by this gateway", slug),
			http.StatusNotFound)
		return
	}

	principal, err := e.opts.Identity.Resolve(r.Header)
	if err != nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := authorizeAudience(as.view.Audience, principal); err != nil {
		// 403 rather than 404: the audience exists, and pretending otherwise
		// would make a misconfigured group assignment look like a typo.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	e.streamable.ServeHTTP(w, r.WithContext(withRequestScope(r.Context(), requestScope{
		Audience:  slug,
		Principal: principal,
	})))
}

// serverForRequest is the SDK's getServer callback.
func (e *Edge) serverForRequest(r *http.Request) *mcp.Server {
	slug := chi.URLParam(r, "audience")
	e.mu.RLock()
	defer e.mu.RUnlock()
	if as, ok := e.audiences[slug]; ok {
		return as.server
	}
	return nil
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
	count := len(e.audiences)
	e.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if version == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"no snapshot"}`))
		return
	}
	fmt.Fprintf(w, `{"status":"ok","snapshot_version":%d,"audiences":%d}`, version, count)
}

// SnapshotVersion reports what the edge is serving, for the admin surface.
func (e *Edge) SnapshotVersion() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.version
}

// AudienceSlugs lists the endpoints currently served.
func (e *Edge) AudienceSlugs() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.audiences))
	for slug := range e.audiences {
		out = append(out, slug)
	}
	return out
}

// ---------------------------------------------------------- request scope ----

type requestScopeKey struct{}

type requestScope struct {
	Audience  string
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
