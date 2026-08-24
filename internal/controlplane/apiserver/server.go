// Copyright 2026 The MCPDoll Authors.

package apiserver

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/inspector"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
)

// Config is what a control-plane API server needs to run.
type Config struct {
	// RegistryPath is the document served by the registry operations. It is
	// re-read per request rather than cached: a GitOps registry changes under
	// the server, and a console showing yesterday's document is worse than one
	// that costs a file read.
	RegistryPath string
	// SnapshotPath is the file the local data plane serves.
	SnapshotPath string
	// GatewayURL is the data plane the gateway operations inspect.
	GatewayURL string
	// AdminURL is the data plane's admin listener, where backend health lives.
	// Separate from GatewayURL because it is a separate port serving a
	// different trust level.
	AdminURL string

	// SigningKeyPath and SigningKeyID let this control plane build snapshots.
	// Both empty is a legitimate deployment: a control plane that only reads is
	// a smaller thing to secure, and buildSnapshot reports that it holds no key
	// rather than pretending the operation does not exist.
	SigningKeyPath string
	SigningKeyID   string
	// KeyDir is where generateSigningKey writes new keypairs.
	KeyDir string

	// Token is the bearer credential every operation except /healthz requires.
	Token string
	// AllowAnonymous disables that check. It exists so local development is not
	// a token-management exercise, and it is never a default: [New] refuses to
	// build a server with neither a token nor this flag.
	AllowAnonymous bool

	// AllowedOrigins are the browser origins permitted to call this API. Empty
	// means no cross-origin access, which is correct for a same-origin console.
	AllowedOrigins []string

	// Store is the control plane's durable state. Nil when no database is
	// configured, in which case the tenant and user operations report that
	// plainly rather than panicking on a nil pointer.
	Store *store.Store

	Version string
	Logger  *slog.Logger
}

// Server is the control plane's HTTP surface.
type Server struct {
	cfg Config
	log *slog.Logger
	mux *chi.Mux
}

// New builds a server, or refuses to.
//
// The refusal is the point. An API that hands out signing keys and rebuilds the
// serving snapshot must not be reachable without a credential, and the way that
// mistake normally happens is a config file with the token line missing. Making
// it a startup error rather than a warning means the unsafe state cannot be
// reached by omission — only by writing --allow-anonymous, which is a thing
// somebody has to type.
func New(cfg Config) (*Server, error) {
	if cfg.Token == "" && !cfg.AllowAnonymous {
		return nil, errors.New(
			"the control-plane API requires a bearer token: set api.token (or " +
				"MCPDOLL_CP_TOKEN), or pass --allow-anonymous for local development")
	}
	if cfg.Token != "" && cfg.AllowAnonymous {
		return nil, errors.New(
			"--allow-anonymous was passed alongside a token; refusing to guess " +
				"which one you meant")
	}
	if cfg.RegistryPath == "" {
		return nil, errors.New("a registry path is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}

	s := &Server{cfg: cfg, log: cfg.Logger}
	s.routes()

	if cfg.AllowAnonymous {
		s.log.Warn("control-plane API is unauthenticated",
			slog.String("detail",
				"every operation is reachable without a credential, including "+
					"snapshot builds and signing-key generation. Bind to localhost."))
	}
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(s.recoverer)
	r.Use(middleware.Timeout(90 * time.Second))
	r.Use(s.cors)

	// api.Health is outside the auth wall: a load balancer has no credential, and
	// the response says nothing an unauthenticated caller could not learn by
	// observing that the port accepts connections.
	r.Get("/healthz", s.handleHealth)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.authenticate)

		r.Get("/hooks", s.handleListHooks)

		r.Get("/registry", s.handleGetRegistry)
		r.Post("/registry:validate", s.handleValidateRegistry)
		r.Get("/registry/servers", s.handleListServers)
		r.Get("/registry/servers/{serverId}", s.handleGetServer)

		r.Get("/plugins", s.handleListPlugins)

		r.Get("/snapshots/current", s.handleGetCurrentSnapshot)
		r.Post("/snapshots:inspect", s.handleInspectSnapshot)
		r.Post("/snapshots:verify", s.handleVerifySnapshot)
		r.Post("/snapshots:build", s.handleBuildSnapshot)

		r.Post("/keys:generate", s.handleGenerateSigningKey)

		r.Get("/gateway/status", s.handleGatewayStatus)
		r.Get("/gateway/backends", s.handleListBackends)
		r.Get("/gateway/catalog", s.handleCatalog)
		r.Post("/gateway/tools/{toolName}:call", s.handleCallTool)

		// Tenancy and RBAC. Everything below is backed by the database rather
		// than by a file, and reports plainly when there is not one.
		r.Get("/tenants", s.handleListTenants)
		r.Post("/tenants", s.handleCreateTenant)
		r.Delete("/tenants/{tenantId}", s.handleDeleteTenant)
		r.Get("/tenants/{tenantId}/users", s.handleListUsers)
		r.Post("/tenants/{tenantId}/users", s.handleCreateUser)

		r.Get("/users/{userId}", s.handleGetUser)
		r.Patch("/users/{userId}", s.handleUpdateUser)
		r.Get("/users/{userId}/grants", s.handleListGrants)
		r.Put("/users/{userId}/grants", s.handlePutGrants)
		r.Get("/users/{userId}/keys", s.handleListAPIKeys)
		r.Post("/users/{userId}/keys", s.handleMintAPIKey)
		r.Delete("/keys/{keyId}", s.handleRevokeAPIKey)

		r.Get("/roles", s.handleListRoles)
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, s.log, http.StatusNotFound, CodeNotFound,
			fmt.Sprintf("no operation at %s %s", r.Method, r.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, s.log, http.StatusMethodNotAllowed, CodeInvalidRequest,
			fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path))
	})

	s.mux = r
}

// authenticate enforces the bearer token.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AllowAnonymous {
			next.ServeHTTP(w, r)
			return
		}

		header := r.Header.Get("Authorization")
		token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		// Constant time, so the comparison does not leak the token's prefix to
		// somebody willing to make a few million requests.
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcpdoll"`)
			writeError(w, s.log, http.StatusUnauthorized, CodeInvalidRequest,
				"a bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cors permits exactly the configured origins.
//
// No wildcard, and no reflection of arbitrary Origin headers: this API can
// build a snapshot and mint a signing key, so a page on any origin being able
// to call it with the operator's credentials is not a theoretical problem.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			// The response varies by Origin, so a cache that ignored this would
			// serve one origin's permission to another.
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(origin string) bool {
	for _, allowed := range s.cfg.AllowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

// recoverer turns a panic into a 500 rather than a dropped connection.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Any("panic", rec))
				// Deliberately opaque: a panic message can contain a file path
				// or a fragment of config, and the caller can do nothing with
				// it. The log has the detail.
				writeError(w, s.log, http.StatusInternalServerError, CodeInternal,
					"the control plane failed to handle this request")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------------ system ---

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.log, http.StatusOK, api.Health{
		Status:       "ok",
		Version:      s.cfg.Version,
		RegistryPath: s.cfg.RegistryPath,
		SnapshotPath: s.cfg.SnapshotPath,
	})
}

func (s *Server) handleListHooks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.log, http.StatusOK, api.HookList{Hooks: registry.HookNames()})
}

// ---------------------------------------------------------------- registry ---

// loadRegistry reads and validates the configured document.
//
// A registry that no longer validates is reported as a 422 with every problem
// listed, not a 500: somebody edited the file, and the response should say what
// is wrong with it.
func (s *Server) loadRegistry(w http.ResponseWriter) (*registry.Spec, bool) {
	spec, err := registry.Load(s.cfg.RegistryPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, s.log, http.StatusNotFound, CodeNotFound,
				fmt.Sprintf("no registry document at %s", s.cfg.RegistryPath))
			return nil, false
		}
		writeProblems(w, s.log,
			fmt.Sprintf("%s is not a valid registry document", s.cfg.RegistryPath), err)
		return nil, false
	}
	return spec, true
}

func (s *Server) handleGetRegistry(w http.ResponseWriter, _ *http.Request) {
	spec, ok := s.loadRegistry(w)
	if !ok {
		return
	}
	writeJSON(w, s.log, http.StatusOK, api.NewRegistry(spec))
}

func (s *Server) handleListServers(w http.ResponseWriter, _ *http.Request) {
	spec, ok := s.loadRegistry(w)
	if !ok {
		return
	}
	writeJSON(w, s.log, http.StatusOK, api.ServerList{Servers: api.NewRegistry(spec).Servers})
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.loadRegistry(w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "serverId")
	for _, srv := range api.NewRegistry(spec).Servers {
		if srv.ID == id || srv.Name == id {
			writeJSON(w, s.log, http.StatusOK, srv)
			return
		}
	}
	writeError(w, s.log, http.StatusNotFound, CodeNotFound,
		fmt.Sprintf("no server %q in %s", id, s.cfg.RegistryPath))
}

func (s *Server) handleListPlugins(w http.ResponseWriter, _ *http.Request) {
	spec, ok := s.loadRegistry(w)
	if !ok {
		return
	}
	plugins := api.NewRegistry(spec).Plugins
	if plugins == nil {
		plugins = []api.Plugin{}
	}
	writeJSON(w, s.log, http.StatusOK, api.PluginList{Plugins: plugins})
}

// ----------------------------------------------------------------- gateway ---

func (s *Server) inspectorClient(r *http.Request) *inspector.Client {
	return &inspector.Client{
		GatewayURL: s.cfg.GatewayURL,
		AdminURL:   s.cfg.AdminURL,
		// The data-plane credential is the caller's own, forwarded deliberately
		// rather than swapped for a service identity: an operator inspecting
		// the gateway should reach exactly what their own token reaches.
		Token:      bearerOf(r),
		ClientName: "mcpdoll-console",
		Version:    s.cfg.Version,
	}
}

func bearerOf(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func (s *Server) handleGatewayStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.inspectorClient(r).Status(r.Context())
	if err != nil {
		// The populated status travels with the error, so a not-ready gateway
		// renders as a state rather than as a blank page.
		writeJSON(w, s.log, http.StatusBadGateway, Error{
			Code:     CodeUnavailable,
			Message:  err.Error(),
			Problems: []string{fmt.Sprintf("gateway reported status %q", status.Status)},
		})
		return
	}
	writeJSON(w, s.log, http.StatusOK, status)
}

func (s *Server) handleListBackends(w http.ResponseWriter, r *http.Request) {
	report, err := s.inspectorClient(r).Backends(r.Context())
	if err != nil {
		if errors.Is(err, inspector.ErrNoAdminURL) {
			writeError(w, s.log, http.StatusNotFound, CodeNotFound, err.Error())
			return
		}
		s.writeInspectorError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, report)
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.inspectorClient(r).Catalog(r.Context(), inspector.CatalogRequest{
		Credential:       r.Header.Get("X-MCPDoll-Inspect-Credential"),
		Identity:         identityFromQuery(r),
		FullDescriptions: r.URL.Query().Get("full") == "true",
	})
	if err != nil {
		s.writeInspectorError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, catalog)
}

func identityFromQuery(r *http.Request) inspector.Identity {
	q := r.URL.Query()
	id := inspector.Identity{Subject: q.Get("subject")}
	if raw := q.Get("groups"); raw != "" {
		for _, g := range strings.Split(raw, ",") {
			if g = strings.TrimSpace(g); g != "" {
				id.Groups = append(id.Groups, g)
			}
		}
	}
	return id
}

func (s *Server) writeInspectorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, inspector.ErrInvalidRequest):
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, inspector.ErrForbidden):
		// 403, not 502. The gateway is healthy and made a decision; telling the
		// operator it was unavailable would send them to restart a service that
		// is working exactly as configured.
		writeError(w, s.log, http.StatusForbidden, CodeForbidden, err.Error())
	case errors.Is(err, inspector.ErrUnknownPrincipal):
		writeError(w, s.log, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, inspector.ErrUnavailable):
		writeError(w, s.log, http.StatusBadGateway, CodeUnavailable, err.Error())
	default:
		s.log.Error("gateway inspection failed", slog.String("error", err.Error()))
		writeError(w, s.log, http.StatusInternalServerError, CodeInternal,
			"the gateway inspection failed")
	}
}
