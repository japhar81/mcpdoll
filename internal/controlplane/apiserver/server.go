// Copyright 2026 Henry Zektser.

package apiserver

import (
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
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
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

	// RebuildInterval is how often the catalog is rebuilt from the backends.
	// Zero means [DefaultRebuildInterval].
	RebuildInterval time.Duration
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

	// RevocationsPath is where the signed revocation list is written, for the
	// data plane to pick up (ADR 0023). Empty means revocations still take
	// effect — at snapshot latency, which is the thing this artifact exists to
	// avoid — so [New] warns rather than failing.
	RevocationsPath string

	// PrincipalsPath is where the signed principal set is written (ADR 0024).
	// Empty means who-exists changes still take effect — at snapshot latency,
	// which is what this artifact exists to avoid.
	PrincipalsPath string

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

	// What the rebuild loop has been doing. Reported rather than only logged —
	// see [RebuildState].
	rebuilds rebuildTracker
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

	if cfg.PrincipalsPath == "" {
		s.log.Warn("no principal set path configured",
			slog.String("detail",
				"minting a key or issuing a grant will not take effect until the next "+
					"snapshot is published; set controlplane.principals_path (ADR 0024)"))
	}

	if cfg.RevocationsPath == "" {
		s.log.Warn("no revocation list path configured",
			slog.String("detail",
				"revoking a credential will not take effect until the next snapshot "+
					"is published; set controlplane.revocations_path (ADR 0023)"))
	}

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

	// Outside the auth wall, necessarily: signing in cannot require being
	// signed in. It was inside, and a test caught it — the comment claiming it
	// was fine was wrong about how chi's Use applies to a group.
	//
	// An unauthenticated endpoint that touches the database is a brute-force
	// target, and the rate limiting here is structural rather than a counter:
	// verification runs Argon2id at 64 MiB and ~50ms per attempt (ADR 0021),
	// so a few hundred guesses a second per instance is not reachable. That is
	// the one place a memory-hard KDF is exactly right, which is why passwords
	// kept it while key secrets did not.
	r.Post("/api/v1/auth/login", s.handleLogin)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.authenticate)

		// Reading and ending your own session need no permission — the
		// question is about the credential you already presented.
		r.Get("/auth/session", s.handleGetSession)
		r.Delete("/auth/session", s.handleLogout)

		// Every operation below states its permission and its scope here,
		// beside the route. Reading this table answers "who can do this"
		// without reading a handler, and a new route gets an explicit
		// permission rather than whatever a pattern happened to match
		// (ADR 0022).
		global := authz.GlobalScope

		r.With(s.require(authz.PermRegistryRead, global)).
			Get("/hooks", s.handleListHooks)

		r.With(s.require(authz.PermRegistryRead, global)).Group(func(r chi.Router) {
			r.Get("/registry", s.handleGetRegistry)
			r.Post("/registry:validate", s.handleValidateRegistry)
			r.Get("/registry/servers", s.handleListServers)
			r.Get("/registry/servers/{serverId}", s.handleGetServer)
			r.Get("/plugins", s.handleListPlugins)

			// Reading a snapshot is reading configuration. Building one is a
			// different permission, below, because preparing a change and
			// shipping it are the separation this permission set exists for.
			r.Get("/snapshots/current", s.handleGetCurrentSnapshot)
			r.Post("/snapshots:inspect", s.handleInspectSnapshot)
			r.Post("/snapshots:verify", s.handleVerifySnapshot)
		})

		r.With(s.require(authz.PermSnapshotBuild, global)).
			Post("/snapshots:build", s.handleBuildSnapshot)

		// The narrowest and most dangerous permission there is: it grants the
		// ability to sign configuration every data-plane instance will accept.
		r.With(s.require(authz.PermKeyGenerate, global)).
			Post("/keys:generate", s.handleGenerateSigningKey)

		r.With(s.require(authz.PermGatewayInspect, global)).Group(func(r chi.Router) {
			r.Get("/gateway/status", s.handleGatewayStatus)
			r.Get("/gateway/backends", s.handleListBackends)
			r.Get("/gateway/catalog", s.handleCatalog)
			r.Post("/gateway/tools/{toolName}:call", s.handleCallTool)
		})

		// Tenancy and RBAC.
		//
		// listTenants takes no permission check here: it filters to what the
		// caller can see rather than refusing outright. A control plane that
		// answers "forbidden" to a question the caller is partly entitled to
		// ask is useless to anyone who is not a platform administrator.
		r.Get("/tenants", s.handleListTenants)
		r.With(s.require(authz.PermTenantManage, global)).
			Post("/tenants", s.handleCreateTenant)
		r.With(s.requireScoped("tenantId", authz.PermTenantManage, s.tenantScopeOf)).
			Delete("/tenants/{tenantId}", s.handleDeleteTenant)

		// Who is granted into a tenant. Tenant-scoped, because that is a
		// question about the tenant.
		r.With(s.requireScoped("tenantId", authz.PermUserManage, s.tenantScopeOf)).
			Get("/tenants/{tenantId}/users", s.handleListUsers)

		// People are global now, so managing one is a platform-level act on a
		// platform-level object. Creating one authorizes nothing — a user with
		// no grants reaches nothing — and the operation that does authorize is
		// the grant, checked at the scope of each grant issued.
		r.With(s.require(authz.PermUserManage, global)).Group(func(r chi.Router) {
			r.Get("/users", s.handleListAllUsers)
			r.Post("/users", s.handleCreateUser)
		})

		r.With(s.requireScoped("userId", authz.PermUserManage, s.userScopeOf)).Group(func(r chi.Router) {
			r.Get("/users/{userId}", s.handleGetUser)
			r.Patch("/users/{userId}", s.handleUpdateUser)
			r.Delete("/users/{userId}", s.handleDeleteUser)
		})

		// Deciding what a user may do is not the same as creating one, and
		// role:manage is separate from user:manage for exactly that reason: an
		// operator who can onboard without it cannot promote themselves.
		// Anywhere, not at a fixed scope. A grant is authorized at the scope of
		// the grant being issued, which the route cannot know — and gating on
		// one scope would refuse a tenant admin before the check that actually
		// decides could run. `handlePutGrants` does that check per grant.
		r.With(s.requireAnywhere(authz.PermRoleManage)).Group(func(r chi.Router) {
			r.Get("/users/{userId}/grants", s.handleListGrants)
			r.Put("/users/{userId}/grants", s.handlePutGrants)
		})

		// Same shape: a key is authorized at the scope of the tenant it acts
		// in, which `handleMintAPIKey` checks.
		r.With(s.requireAnywhere(authz.PermKeyManage)).Group(func(r chi.Router) {
			r.Get("/users/{userId}/keys", s.handleListAPIKeys)
			r.Post("/users/{userId}/keys", s.handleMintAPIKey)
		})
		// Revoking is keyed by the key, whose owner's tenant is the scope.
		r.With(s.requireScoped("keyId", authz.PermKeyManage, s.keyScopeOf)).
			Delete("/keys/{keyId}", s.handleRevokeAPIKey)

		// No permission beyond being authenticated. The role catalog is the
		// same for everyone, it is in the source, and the grants editor is
		// unusable without it — so gating it behind registry:read made a
		// tenant admin unable to do the one thing their role exists for.
		r.Get("/roles", s.handleListRoles)

		// Reading the revocation state is an operational question — "is what I
		// revoked actually in effect?" — so it sits with gateway inspection
		// rather than with key management.
		r.With(s.require(authz.PermGatewayInspect, global)).
			Get("/revocations", s.handleGetRevocations)
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
	// Merged in whether or not the gateway answered. Catalog freshness is the
	// control plane's own knowledge — it is this process that rebuilds — and a
	// gateway that is down is exactly when somebody wants to know whether the
	// catalog behind it is still being rebuilt.
	s.mergeRebuildState(&status)
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

// mergeRebuildState stamps catalog freshness onto a gateway status.
func (s *Server) mergeRebuildState(status *api.GatewayStatus) {
	state := s.RebuildState()
	status.CatalogError = state.LastError
	if !state.LastBuiltAt.IsZero() {
		status.CatalogAgeSeconds = time.Since(state.LastBuiltAt).Seconds()
	}
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
