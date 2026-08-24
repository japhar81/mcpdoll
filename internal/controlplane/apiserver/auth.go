// Copyright 2026 Henry Zektser.

package apiserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// Who is asking, and may they.
//
// The control plane used to compare a bearer token against one configured value
// and run everything past that point unchecked, so an operator who could mint a
// signing key and one who could only read the registry were the same principal.
// The role model existed and enforced nothing here (ADR 0022).
//
// It was recorded as blocked on an identity provider, and that was wrong: a
// local password is a principal. `VerifyPassword` returns a user, a user has
// grants, and grants compile to a decider. OIDC is a second *source* of
// identity, not a prerequisite for having one.

// Caller is the resolved principal behind a request.
type Caller struct {
	// Kind is how they authenticated: "session", "api_key", or "static".
	Kind    string
	Subject string
	Tenant  string
	UserID  string

	// decide answers one authorization question. Compiled once per request from
	// grants read fresh, so a grant taken away applies to the next call rather
	// than at the next sign-in.
	decide authz.Decider
	grants []authz.Grant
}

// Can reports whether this caller holds a permission at a scope.
func (c *Caller) Can(permission authz.Permission, scope string) bool {
	if c == nil || c.decide == nil {
		return false
	}
	return c.decide(permission, scope)
}

// Grants is what the caller holds, for the session endpoint to report.
func (c *Caller) Grants() []authz.Grant {
	if c == nil {
		return nil
	}
	return c.grants
}

type callerKey struct{}

// CallerFrom returns the resolved principal, if any.
func CallerFrom(ctx context.Context) *Caller {
	c, _ := ctx.Value(callerKey{}).(*Caller)
	return c
}

func withCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// staticCaller is the break-glass credential (ADR 0022).
//
// Total authority, deliberately. CI has to build a snapshot before any user
// exists, and a deployment whose database is down still needs somebody able to
// look at it. It is not a fallback the other two degrade into — a failed
// session lookup is a 401, never a silent promotion to this.
func staticCaller() *Caller {
	return &Caller{
		Kind:    "static",
		Subject: "static-token",
		grants:  []authz.Grant{{Role: authz.RolePlatformAdmin, Scope: authz.GlobalScope}},
		// Compiled against the built-in catalog rather than the stored one: the
		// point of this credential is that it works when the database does not.
		decide: mustDecide([]authz.Grant{
			{Role: authz.RolePlatformAdmin, Scope: authz.GlobalScope},
		}),
	}
}

func mustDecide(grants []authz.Grant) authz.Decider {
	d, err := authz.BuiltinEngine{}.Prepare(context.Background(), grants, authz.DefaultCatalog())
	if err != nil {
		// Cannot happen: the built-in catalog and a platform_admin grant are
		// both constants. Failing closed anyway, because a decider that could
		// not compile must never be a decider that allows.
		return authz.DenyAll()
	}
	return d
}

// authenticate resolves every request to a principal.
//
// Three credentials, tried in order of specificity. A credential that looks
// like one and fails is a 401 — never a fall-through to the next, which would
// make a wrong session token succeed as the static principal.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AllowAnonymous {
			// Development only, and [New] refuses to pair it with a token. The
			// caller is a platform administrator because there is nobody to be
			// otherwise, and the startup warning says so.
			next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), staticCaller())))
			return
		}

		presented := strings.TrimSpace(
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if presented == "" {
			s.unauthorized(w, "a bearer token is required")
			return
		}

		// A credential in `mcpd.<prefix>.<secret>` form is a session or an API
		// key, and the store decides which. Anything else can only be the
		// static token.
		if strings.HasPrefix(presented, "mcpd.") {
			caller, err := s.resolveStoreCredential(r, presented)
			if err != nil {
				s.unauthorized(w, "this credential is not valid")
				return
			}
			next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), caller)))
			return
		}

		// Constant time, so the comparison does not leak the token's prefix to
		// somebody willing to make a few million requests.
		if s.cfg.Token == "" ||
			subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.Token)) != 1 {
			s.unauthorized(w, "this credential is not valid")
			return
		}

		// Logged on every use. A total-authority credential that is used
		// without a trace is how "who did this" becomes unanswerable.
		s.log.Info("request authorized by the static token",
			"method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), staticCaller())))
	})
}

// resolveStoreCredential tries a session, then an API key.
func (s *Server) resolveStoreCredential(r *http.Request, presented string) (*Caller, error) {
	if s.cfg.Store == nil {
		return nil, errors.New("no database is configured")
	}

	if resolved, _, err := s.cfg.Store.ResolveSession(r.Context(), presented); err == nil {
		decide, err := s.cfg.Store.Decider(r.Context(), resolved.Grants)
		if err != nil {
			return nil, err
		}
		return &Caller{
			Kind: "session", Subject: resolved.User.Email,
			Tenant: resolved.Tenant.Slug, UserID: resolved.User.ID.String(),
			decide: decide, grants: resolved.Grants,
		}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		// A database failure is not a wrong password, and returning "invalid"
		// for it would send an operator to reset a credential that is fine.
		return nil, err
	}

	resolved, err := s.cfg.Store.ResolveAPIKey(r.Context(), presented)
	if err != nil {
		return nil, err
	}
	decide, err := s.cfg.Store.Decider(r.Context(), resolved.Grants)
	if err != nil {
		return nil, err
	}
	return &Caller{
		Kind: "api_key", Subject: resolved.User.Email,
		Tenant: resolved.Tenant.Slug, UserID: resolved.User.ID.String(),
		decide: decide, grants: resolved.Grants,
	}, nil
}

func (s *Server) unauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="mcpdoll"`)
	writeError(w, s.log, http.StatusUnauthorized, CodeInvalidRequest, message)
}

// ------------------------------------------------------------ authorization --

// require builds middleware enforcing one permission at a fixed scope.
//
// Declared at the route rather than inferred from the path. Reading the route
// table then answers "who can do this" without reading any handler, and a new
// route gets an explicit permission rather than whatever a pattern happened to
// match.
func (s *Server) require(permission authz.Permission, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.allow(w, r, permission, scope) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireScoped enforces a permission at a scope derived from the request.
//
// A tenant operation is scoped to that tenant, which is what makes a tenant
// admin real: they hold their role at `t/acme`, and the same operation against
// `globex` is refused by the same check.
//
// Two distinct failures, and keeping them apart matters:
//
//   - A path parameter that is not a uuid is a 400. Its format is public, so
//     nothing leaks, and telling a caller their id is malformed is far more
//     useful than "not found".
//   - A resource that does not exist, or whose scope cannot be resolved, is a
//     404 — the same answer a caller gets for one they may not see, which is
//     what stops this enumerating other tenants' resources.
func (s *Server) requireScoped(
	param string, permission authz.Permission, scopeOf func(*http.Request, uuid.UUID) (string, bool),
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := chi.URLParam(r, param)
			id, err := uuid.Parse(raw)
			if err != nil {
				writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
					param+" must be a uuid, got "+raw)
				return
			}
			// No database at all is a deployment-level fact, not a missing
			// resource. Reporting it as 404 would send an operator hunting for
			// a tenant that could not exist.
			if s.requireStore(w) == nil {
				return
			}
			scope, ok := scopeOf(r, id)
			if !ok {
				// Never falls back to a global check: that would grant more
				// than the route intended, which is the failure worth designing
				// against.
				writeError(w, s.log, http.StatusNotFound, CodeNotFound,
					"no such resource, or its scope could not be determined")
				return
			}
			if !s.allow(w, r, permission, scope) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) allow(
	w http.ResponseWriter, r *http.Request, permission authz.Permission, scope string,
) bool {
	caller := CallerFrom(r.Context())
	if caller.Can(permission, scope) {
		return true
	}
	subject := "unauthenticated"
	if caller != nil {
		subject = caller.Subject
	}
	s.log.Warn("refused",
		"subject", subject, "permission", string(permission), "scope", scope,
		"method", r.Method, "path", r.URL.Path)
	writeError(w, s.log, http.StatusForbidden, CodeForbidden,
		"this credential does not hold "+string(permission)+" at "+scope)
	return false
}

// --------------------------------------------------------------- operations --

// LoginRequest is login's body.
type LoginRequest struct {
	// Tenant is part of the identity, not a lookup hint: the same email may
	// exist in two tenants and they are different people (ADR 0014).
	Tenant   string `json:"tenant"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	st := s.requireStore(w)
	if st == nil {
		return
	}
	var req LoginRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}

	session, secret, user, err := st.SignIn(
		r.Context(), req.Tenant, req.Email, req.Password,
		r.UserAgent(), clientIP(r))
	if err != nil {
		// One answer for every failure. A caller learns whether they signed in
		// and nothing else — not whether the tenant exists, not whether the
		// email does, not whether the account is disabled.
		s.log.Warn("failed sign-in",
			"tenant", req.Tenant, "email", req.Email, "error", err.Error())
		s.unauthorized(w, "the tenant, email, or password is wrong")
		return
	}

	grants, err := st.GrantsForUser(r.Context(), user.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, api.Session{
		// The one time this is knowable. Stored as a SHA-256 digest of CSPRNG
		// output, so nothing can show it again (ADR 0021).
		Token:     secret,
		ExpiresAt: session.ExpiresAt.UTC().Format(time.RFC3339),
		User:      userOf(user, req.Tenant),
		Grants:    grantsOf(grants),
	})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	caller := CallerFrom(r.Context())
	if caller == nil {
		s.unauthorized(w, "no credential")
		return
	}

	out := api.SessionInfo{
		Kind:    caller.Kind,
		Subject: caller.Subject,
		Tenant:  caller.Tenant,
		UserID:  caller.UserID,
		Grants:  grantsOf(caller.Grants()),
		// What the console renders from. A button that 403s is worse than a
		// button that is not there, so the answer to "what may I do" has to be
		// available before anything is attempted.
		Permissions: []string{},
	}
	for _, p := range authz.AllPermissions() {
		if caller.Can(p, authz.GlobalScope) {
			out.Permissions = append(out.Permissions, string(p))
		}
	}
	writeJSON(w, s.log, http.StatusOK, out)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	caller := CallerFrom(r.Context())
	if caller == nil || caller.Kind != "session" {
		// Nothing to sign out of. A 204 rather than an error: the caller's
		// intent is that the session should not work, and it does not.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}

	presented := strings.TrimSpace(
		strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	_, session, err := st.ResolveSession(r.Context(), presented)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := st.SignOut(r.Context(), session.ID); err != nil {
		s.writeStoreError(w, err)
		return
	}
	_ = s.publishRevocations(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func grantsOf(grants []authz.Grant) []api.Grant {
	out := make([]api.Grant, 0, len(grants))
	for _, g := range grants {
		out = append(out, api.Grant{Role: g.Role, Scope: g.Scope})
	}
	return out
}

// clientIP is best-effort and never used for authorization.
//
// Both the header and the socket address are attacker-influenced behind a
// proxy, so this is an audit breadcrumb rather than a control — which is why it
// takes the first forwarded value without validating it.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, found := strings.Cut(fwd, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	return host
}

// ------------------------------------------------------------ scope lookups --

// The scope of a tenancy operation is the tenant it acts on, which means
// resolving a uuid in the path to a slug before the permission is checked.
// That costs a query per request on these routes, and it is what makes a tenant
// admin real: they hold their role at `t/acme`, so the same operation against
// `globex` is refused by the same check rather than by a special case.
//
// Each returns ok=false when the scope cannot be determined, and the middleware
// turns that into a 404. Falling back to a global check would grant more than
// the route intended, which is the failure mode worth designing against.

func (s *Server) tenantScopeOf(r *http.Request, id uuid.UUID) (string, bool) {
	if s.cfg.Store == nil {
		return "", false
	}
	tenant, err := s.cfg.Store.GetTenant(r.Context(), id)
	if err != nil {
		return "", false
	}
	return authz.TenantScope(tenant.Slug), true
}

func (s *Server) userScopeOf(r *http.Request, id uuid.UUID) (string, bool) {
	if s.cfg.Store == nil {
		return "", false
	}
	user, err := s.cfg.Store.GetUser(r.Context(), id)
	if err != nil {
		return "", false
	}
	tenant, err := s.cfg.Store.GetTenant(r.Context(), user.TenantID)
	if err != nil {
		return "", false
	}
	return authz.TenantScope(tenant.Slug), true
}

func (s *Server) keyScopeOf(r *http.Request, id uuid.UUID) (string, bool) {
	if s.cfg.Store == nil {
		return "", false
	}
	key, err := s.cfg.Store.GetAPIKey(r.Context(), id)
	if err != nil {
		return "", false
	}
	user, err := s.cfg.Store.GetUser(r.Context(), key.UserID)
	if err != nil {
		return "", false
	}
	tenant, err := s.cfg.Store.GetTenant(r.Context(), user.TenantID)
	if err != nil {
		return "", false
	}
	return authz.TenantScope(tenant.Slug), true
}
