// Copyright 2026 Henry Zektser.

package apiserver

import (
	"errors"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// Tenants, users, grants, and API keys.
//
// These are the operations that decide who exists and what they may reach. They
// are the only ones in this package backed by a database rather than by a file
// or by the data plane, which is why they all start by checking that there is
// one: a deployment can legitimately run without a store (a snapshot builder in
// CI has no business holding user records), and the honest answer there is that
// the operation is unavailable — not a panic on a nil pointer, and not an empty
// list that reads as "no users exist".

// requireStore returns the store, or writes the explanation and returns nil.
func (s *Server) requireStore(w http.ResponseWriter) *store.Store {
	if s.cfg.Store == nil {
		writeError(w, s.log, http.StatusServiceUnavailable, CodeUnavailable,
			"this control plane has no database configured, so it holds no "+
				"tenants or users; set database.url (or MCPDOLL_DATABASE_URL)")
		return nil
	}
	return s.cfg.Store
}

// writeStoreError maps the store's sentinels onto statuses.
//
// A uniqueness violation is a 409 and not a 500: the caller asked for something
// legitimate that conflicts with existing state, and telling them the server
// broke sends them to the wrong place.
func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, s.log, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, s.log, http.StatusConflict, CodeInvalidRequest, err.Error())
	case errors.Is(err, store.ErrInvalid):
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	default:
		s.log.Error("store operation failed", "error", err)
		writeError(w, s.log, http.StatusInternalServerError, CodeInternal, err.Error())
	}
}

// uuidParam reads a path parameter that must be a uuid.
func (s *Server) uuidParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			name+" must be a uuid, got "+raw)
		return uuid.Nil, false
	}
	return id, true
}

// ------------------------------------------------------------------ tenants --

func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	out := api.TenantList{Registered: []api.TenantSummary{}}

	// A gateway that cannot be reached is not a failure of this operation: the
	// tenants are still the useful answer, and the zero status says the gateway
	// is not ready rather than that there are no tenants.
	status, err := s.inspectorClient(r).Status(r.Context())
	out.GatewayStatus = status
	if err != nil {
		s.log.Warn("listing tenants without live gateway state", "error", err)
	}

	// Three sources, joined by slug. Each one alone is a half-truth: the store
	// knows who exists, the registry knows who has backends, and the snapshot
	// knows what is actually being served.
	byBackends := s.tenantBackendCounts()
	byTools := s.tenantToolCounts()

	seen := map[string]bool{}

	if st := s.cfg.Store; st != nil {
		tenants, err := st.ListTenants(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		users, err := st.CountUsersByTenant(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		for _, t := range tenants {
			seen[t.Slug] = true
			out.Registered = append(out.Registered, api.TenantSummary{
				ID:        t.ID.String(),
				Slug:      t.Slug,
				Name:      t.Name,
				Status:    t.Status,
				Users:     users[t.ID],
				Backends:  byBackends[t.Slug],
				Tools:     byTools[t.Slug],
				CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
	}

	// A slug the registry binds but nobody created. It has no users, so nothing
	// can authenticate into it — worth showing precisely because it is the
	// mistake that produces a working-looking config serving nobody.
	for slug := range byBackends {
		if seen[slug] {
			continue
		}
		out.Registered = append(out.Registered, api.TenantSummary{
			Slug:     slug,
			Name:     slug,
			Status:   "unregistered",
			Backends: byBackends[slug],
			Tools:    byTools[slug],
		})
	}

	// Filtered, not refused. A tenant admin can legitimately see one of these,
	// and answering "forbidden" to a question the caller is partly entitled to
	// ask is useless to anyone who is not a platform administrator (ADR 0022).
	caller := CallerFrom(r.Context())
	visible := out.Registered[:0]
	for _, t := range out.Registered {
		if caller.Can(authz.PermTenantManage, authz.TenantScope(t.Slug)) ||
			caller.Can(authz.PermUserManage, authz.TenantScope(t.Slug)) ||
			caller.Can(authz.PermGatewayInspect, authz.TenantScope(t.Slug)) {
			visible = append(visible, t)
		}
	}
	out.Registered = visible

	sort.Slice(out.Registered, func(i, j int) bool {
		return out.Registered[i].Slug < out.Registered[j].Slug
	})
	writeJSON(w, s.log, http.StatusOK, out)
}

// tenantBackendCounts counts registry bindings per tenant slug.
func (s *Server) tenantBackendCounts() map[string]int {
	out := map[string]int{}
	spec, err := registry.Load(s.cfg.RegistryPath)
	if err != nil {
		s.log.Warn("counting tenant bindings without a readable registry", "error", err)
		return out
	}
	for _, srv := range spec.Servers {
		for _, b := range srv.Bindings {
			out[b.Tenant]++
		}
	}
	return out
}

// tenantToolCounts counts admitted tools per tenant in the serving snapshot.
func (s *Server) tenantToolCounts() map[string]int {
	out := map[string]int{}
	if s.cfg.SnapshotPath == "" {
		return out
	}
	signed, err := snapshot.ReadSignedSnapshot(s.cfg.SnapshotPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("counting tenant tools without a readable snapshot", "error", err)
		}
		return out
	}
	snap, err := snapshot.ParseUnverified(signed)
	if err != nil {
		return out
	}
	view, err := snapshot.Build(snap)
	if err != nil {
		return out
	}
	for _, slug := range view.TenantSlugs() {
		out[slug] = len(view.ToolsForTenant(view.Tenant(slug).Id))
	}
	return out
}

// CreateTenantRequest is createTenant's body.
type CreateTenantRequest struct {
	// Slug appears verbatim in every scope string this tenant's grants use, so
	// it is immutable once created. There is no renameTenant for that reason.
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	st := s.requireStore(w)
	if st == nil {
		return
	}
	var req CreateTenantRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	tenant, err := st.CreateTenant(r.Context(), req.Slug, req.Name)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, tenantOf(tenant))
}

func (s *Server) handleDeleteTenant(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uuidParam(w, r, "tenantId")
	if !ok {
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}
	// Revoke before deleting. The cascade removes the rows, so afterwards
	// there is nothing left to enumerate — and the keys those rows described
	// are still in the serving snapshot, working.
	users, err := st.ListUsersByTenant(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	for _, u := range users {
		if _, err := st.RevokeUser(r.Context(), u.ID, "tenant deleted"); err != nil {
			s.writeStoreError(w, err)
			return
		}
	}
	problem := s.publishRevocations(r.Context())

	// Cascades to the tenant's users, their grants, and their keys. That is the
	// schema's doing rather than this handler's, which is what makes it
	// complete: nobody has to remember every table.
	if err := st.DeleteTenant(r.Context(), id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	if problem != "" {
		writeJSON(w, s.log, http.StatusAccepted, Error{
			Code:     CodeUnavailable,
			Message:  "the tenant was deleted, but its credentials are not refused yet",
			Problems: []string{problem},
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -------------------------------------------------------------------- users --

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uuidParam(w, r, "tenantId")
	if !ok {
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}
	tenant, err := st.GetTenant(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	users, err := st.ListUsersByTenant(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	out := api.UserList{Tenant: tenant.Slug, Users: []api.User{}}
	for _, u := range users {
		out.Users = append(out.Users, userOf(u, tenant.Slug))
	}
	writeJSON(w, s.log, http.StatusOK, out)
}

// CreateUserRequest is createUser's body.
type CreateUserRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	// Password is optional: a user who signs in through an identity provider
	// has none, and a service identity that only ever holds API keys does not
	// need one either.
	Password string `json:"password,omitempty"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.uuidParam(w, r, "tenantId")
	if !ok {
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}
	var req CreateUserRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	tenant, err := st.GetTenant(r.Context(), tenantID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	user, err := st.CreateUser(r.Context(), tenantID, req.Email, req.DisplayName, req.Password)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// A new user holds nothing. That is the correct starting state and not an
	// omission: an account that could see tools the moment it existed would
	// make onboarding the thing that grants access.
	writeJSON(w, s.log, http.StatusCreated, userOf(user, tenant.Slug))
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uuidParam(w, r, "userId")
	if !ok {
		return
	}
	if s.requireStore(w) == nil {
		return
	}
	user, slug, err := s.userWithTenant(r, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, userOf(user, slug))
}

// UpdateUserRequest is updateUser's body.
type UpdateUserRequest struct {
	DisplayName string `json:"display_name,omitempty"`
	// Status is active or disabled. Disabling stops the user's API keys too,
	// because effective grants are recomputed from the owner at every
	// resolution (ADR 0014) — which is what makes offboarding one operation
	// rather than a hunt through credentials.
	Status string `json:"status"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uuidParam(w, r, "userId")
	if !ok {
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}
	var req UpdateUserRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	if _, err := st.UpdateUser(r.Context(), id, req.DisplayName, req.Status); err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Disabling is the offboarding path, and it is the more common of the two
	// cases ADR 0023 exists for. Revoking only their *sessions* would leave it
	// incomplete in the way that is hardest to notice: the person is gone from
	// the console and their automation is still running.
	problem := ""
	if req.Status == "disabled" {
		if _, err := st.RevokeUser(r.Context(), id, "user disabled"); err != nil {
			s.writeStoreError(w, err)
			return
		}
		problem = s.publishRevocations(r.Context())
	}

	user, slug, err := s.userWithTenant(r, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if problem != "" {
		w.Header().Set("X-MCPDoll-Warning", problem)
	}
	writeJSON(w, s.log, http.StatusOK, userOf(user, slug))
}

// userWithTenant reads a user and the slug their scopes are written with.
func (s *Server) userWithTenant(r *http.Request, id uuid.UUID) (store.User, string, error) {
	user, err := s.cfg.Store.GetUser(r.Context(), id)
	if err != nil {
		return store.User{}, "", err
	}
	tenant, err := s.cfg.Store.GetTenant(r.Context(), user.TenantID)
	if err != nil {
		return store.User{}, "", err
	}
	return user, tenant.Slug, nil
}

// --------------------------------------------------------- scope validation --

// knownScopes is what the serving snapshot can actually be granted at.
//
// A grant at a scope nothing matches is not an error the system would ever
// report: it stores fine, compiles fine, and authorizes nothing. The operator
// sees an empty catalog and no reason for it. Checking here is the only place
// the mistake is still attributable to the change that caused it.
type knownScopes struct {
	// loaded is false when no snapshot could be read. Every check then passes:
	// refusing grants because the gateway has not published yet would make a
	// fresh install unusable, and the check is a courtesy rather than a
	// security boundary — the scope is enforced at serving time regardless.
	loaded   bool
	tenants  map[string]bool
	toolsets map[string]bool // "tenant/toolset"
	tools    map[string]bool // "tenant/toolset/tool"
	// qualified maps the name a client sees back to the name a scope uses, so
	// the error can say what the operator meant.
	qualified map[string]string
}

func (s *Server) servingScopes() knownScopes {
	out := knownScopes{
		tenants: map[string]bool{}, toolsets: map[string]bool{},
		tools: map[string]bool{}, qualified: map[string]string{},
	}
	if s.cfg.SnapshotPath == "" {
		return out
	}
	signed, err := snapshot.ReadSignedSnapshot(s.cfg.SnapshotPath)
	if err != nil {
		return out
	}
	snap, err := snapshot.ParseUnverified(signed)
	if err != nil {
		return out
	}
	view, err := snapshot.Build(snap)
	if err != nil {
		return out
	}

	out.loaded = true
	for _, slug := range view.TenantSlugs() {
		out.tenants[slug] = true
		tenant := view.Tenant(slug)
		for _, tool := range view.ToolsForTenant(tenant.Id) {
			out.toolsets[slug+"/"+tool.Toolset.Name] = true
			key := slug + "/" + tool.Toolset.Name + "/" + tool.Def.Name
			out.tools[key] = true
			out.qualified[slug+"/"+tool.Toolset.Name+"/"+tool.Def.QualifiedName] = tool.Def.Name
		}
	}
	return out
}

// check returns a problem description, or "" if the scope is grantable.
func (k knownScopes) check(scope string) string {
	parsed, ok := authz.ParseScope(scope)
	if !ok {
		return "scope " + scope + " is malformed; it must be *, t/<tenant>, " +
			"t/<tenant>/ts/<toolset>, or t/<tenant>/ts/<toolset>/<tool>"
	}
	if !k.loaded || scope == authz.GlobalScope || parsed.Tenant == "" {
		return ""
	}
	if !k.tenants[parsed.Tenant] {
		return "scope " + scope + " names tenant " + parsed.Tenant +
			", which the serving snapshot does not carry; it would authorize nothing"
	}
	if parsed.Toolset == "" {
		return ""
	}
	if !k.toolsets[parsed.Tenant+"/"+parsed.Toolset] {
		return "scope " + scope + " names toolset " + parsed.Toolset +
			", which admits nothing for tenant " + parsed.Tenant +
			"; it would authorize nothing"
	}
	if parsed.Tool == "" {
		return ""
	}
	if k.tools[parsed.Tenant+"/"+parsed.Toolset+"/"+parsed.Tool] {
		return ""
	}
	// The commonest way to get this wrong, by a wide margin: a tool scope
	// names the backend's own tool name, and what an operator sees in every
	// catalog is the *qualified* name with the namespace prefix on it. The two
	// differ by exactly the thing that is invisible in the UI.
	if actual, ok := k.qualified[parsed.Tenant+"/"+parsed.Toolset+"/"+parsed.Tool]; ok {
		return "scope " + scope + " uses the qualified name; a tool scope names " +
			"the backend's own tool, so this should end in " + actual +
			" — the namespace prefix is not part of it"
	}
	return "scope " + scope + " names a tool that toolset does not carry for " +
		"tenant " + parsed.Tenant + "; it would authorize nothing"
}

// ------------------------------------------------------------------- grants --

func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uuidParam(w, r, "userId")
	if !ok {
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}
	grants, err := st.GrantsForUser(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, grantListOf(id, grants))
}

// PutGrantsRequest is putGrants' body: the complete set the user should hold.
//
// Declarative rather than add-one/remove-one. The question an operator answers
// is "what should this person hold", and expressing that as a sequence of
// deltas is exactly how a revocation gets forgotten.
type PutGrantsRequest struct {
	Grants []api.Grant `json:"grants"`
}

func (s *Server) handlePutGrants(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uuidParam(w, r, "userId")
	if !ok {
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}
	var req PutGrantsRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}

	catalog, err := st.Catalog(r.Context())
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	caller := CallerFrom(r.Context())
	scopes := s.servingScopes()
	want := make([]authz.Grant, 0, len(req.Grants))
	for _, g := range req.Grants {
		if _, known := catalog[g.Role]; !known {
			// Refused rather than stored. A grant naming an unknown role
			// authorizes nothing, so accepting it would report success for a
			// change that does not take effect.
			writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
				"no role named "+g.Role+" exists; it would authorize nothing")
			return
		}
		if problem := scopes.check(g.Scope); problem != "" {
			writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest, problem)
			return
		}
		// You cannot grant what you do not hold.
		//
		// The route already checked role:manage at the *target user's* tenant.
		// This checks it at the scope of each grant being issued, which is a
		// different question — without it, a tenant admin could grant
		// themselves platform_admin at `*` and the permission set's whole
		// structure would be decoration (ADR 0022).
		if !caller.Can(authz.PermRoleManage, g.Scope) {
			writeError(w, s.log, http.StatusForbidden, CodeForbidden,
				"you cannot grant "+g.Role+" at "+g.Scope+
					": issuing a grant requires role:manage at a scope covering it, "+
					"and you do not hold it there")
			return
		}
		want = append(want, authz.Grant{Role: g.Role, Scope: g.Scope})
	}

	if err := st.SetGrants(r.Context(), id, want, nil); err != nil {
		s.writeStoreError(w, err)
		return
	}
	grants, err := st.GrantsForUser(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, grantListOf(id, grants))
}

func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	catalog := authz.DefaultCatalog()
	if s.cfg.Store != nil {
		stored, err := s.cfg.Store.Catalog(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		catalog = stored
	}

	out := api.RoleCatalog{Roles: []api.Role{}, Permissions: []string{}}
	for _, role := range catalog.Roles() {
		perms := []string{}
		for _, p := range catalog.Permissions(role) {
			perms = append(perms, string(p))
		}
		out.Roles = append(out.Roles, api.Role{Name: role, Permissions: perms})
	}
	// Every permission that exists, not only the ones some role happens to use.
	// A UI offered only the latter could never grant a new one.
	for _, p := range authz.AllPermissions() {
		out.Permissions = append(out.Permissions, string(p))
	}
	writeJSON(w, s.log, http.StatusOK, out)
}

// ----------------------------------------------------------------- api keys --

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uuidParam(w, r, "userId")
	if !ok {
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}
	keys, err := st.ListAPIKeysByUser(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	out := api.APIKeyList{UserID: id.String(), Keys: []api.APIKey{}}
	now := time.Now()
	for _, k := range keys {
		out.Keys = append(out.Keys, apiKeyOf(k, now))
	}
	writeJSON(w, s.log, http.StatusOK, out)
}

// MintAPIKeyRequest is mintAPIKey's body.
type MintAPIKeyRequest struct {
	Name string `json:"name"`
	// Grants are what the key asks for. They are intersected with the owner's
	// at every resolution, so a key can narrow its owner's access but never
	// widen it — declaring more than the owner holds is not an error, it simply
	// has no effect (ADR 0014).
	Grants []api.Grant `json:"grants,omitempty"`
	// ExpiresAt is RFC 3339. Absent means the key does not expire, which is a
	// choice rather than a default worth hiding.
	ExpiresAt string `json:"expires_at,omitempty"`
}

func (s *Server) handleMintAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uuidParam(w, r, "userId")
	if !ok {
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}
	var req MintAPIKeyRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}

	var expires *time.Time
	if req.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
				"expires_at must be RFC 3339: "+err.Error())
			return
		}
		expires = &parsed
	}

	declared := make([]authz.Grant, 0, len(req.Grants))
	for _, g := range req.Grants {
		declared = append(declared, authz.Grant{Role: g.Role, Scope: g.Scope})
	}

	key, secret, err := st.MintAPIKey(r.Context(), id, req.Name, declared, expires)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// The one response that carries a secret. It is stored only as an Argon2id
	// hash, so a caller who does not capture it has to mint another key —
	// which is the property that makes a leaked log harmless.
	writeJSON(w, s.log, http.StatusCreated, api.MintedAPIKey{
		Key: apiKeyOf(key, time.Now()), Secret: secret,
	})
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uuidParam(w, r, "keyId")
	if !ok {
		return
	}
	st := s.requireStore(w)
	if st == nil {
		return
	}
	if err := st.RevokeAPIKey(r.Context(), id); err != nil {
		s.writeStoreError(w, err)
		return
	}

	// The database row is marked, and that alone stops nothing: the data plane
	// verifies against the snapshot it holds. Publishing the revocation list is
	// what makes this immediate (ADR 0023).
	if _, err := st.Revoke(r.Context(), id, "api_key", nil, "revoked via the API"); err != nil {
		s.writeStoreError(w, err)
		return
	}
	if problem := s.publishRevocations(r.Context()); problem != "" {
		// The key *is* revoked. It simply takes effect at snapshot latency
		// instead of immediately, and saying so is better than a 204 that
		// implies more than happened.
		writeJSON(w, s.log, http.StatusAccepted, Error{
			Code:     CodeUnavailable,
			Message:  "the key was revoked, but not immediately",
			Problems: []string{problem},
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- rendering --

func tenantOf(t store.Tenant) api.Tenant {
	return api.Tenant{
		ID:        t.ID.String(),
		Slug:      t.Slug,
		Name:      t.Name,
		Status:    t.Status,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func userOf(u store.User, tenantSlug string) api.User {
	return api.User{
		ID:          u.ID.String(),
		TenantID:    u.TenantID.String(),
		Tenant:      tenantSlug,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Status:      u.Status,
		HasPassword: u.HasPassword,
		CreatedAt:   u.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func grantListOf(userID uuid.UUID, grants []authz.Grant) api.GrantList {
	out := api.GrantList{UserID: userID.String(), Grants: []api.Grant{}}
	for _, g := range grants {
		out.Grants = append(out.Grants, api.Grant{Role: g.Role, Scope: g.Scope})
	}
	sort.Slice(out.Grants, func(i, j int) bool {
		if out.Grants[i].Scope != out.Grants[j].Scope {
			return out.Grants[i].Scope < out.Grants[j].Scope
		}
		return out.Grants[i].Role < out.Grants[j].Role
	})
	return out
}

func apiKeyOf(k store.APIKey, now time.Time) api.APIKey {
	out := api.APIKey{
		ID:        k.ID.String(),
		UserID:    k.UserID.String(),
		Name:      k.Name,
		Prefix:    k.Prefix,
		Declared:  []api.Grant{},
		Active:    k.Active(now),
		CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339),
	}
	for _, g := range k.Declared {
		out.Declared = append(out.Declared, api.Grant{Role: g.Role, Scope: g.Scope})
	}
	if k.LastUsedAt != nil {
		out.LastUsedAt = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	if k.ExpiresAt != nil {
		out.ExpiresAt = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if k.RevokedAt != nil {
		out.RevokedAt = k.RevokedAt.UTC().Format(time.RFC3339)
	}
	return out
}
