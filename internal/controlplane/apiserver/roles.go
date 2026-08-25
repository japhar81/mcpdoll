// Copyright 2026 Henry Zektser.

package apiserver

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// *Caller satisfies the escalation check's view of a caller.
var _ authz.Holder = (*Caller)(nil)

// permissionList renders permissions for an error message.
func permissionList(perms []authz.Permission) string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, string(p))
	}
	return strings.Join(out, ", ")
}

// PutRoleRequest is putRole's body.
type PutRoleRequest struct {
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions"`
}

func (s *Server) handlePutRole(w http.ResponseWriter, r *http.Request) {
	st := s.requireStore(w)
	if st == nil {
		return
	}
	var req PutRoleRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	name := chi.URLParam(r, "role")

	permissions := make([]authz.Permission, 0, len(req.Permissions))
	for _, p := range req.Permissions {
		permissions = append(permissions, authz.Permission(p))
	}

	// The vocabulary first, then the escalation check. Order matters for the
	// message: a typo checked in the other order comes back as "you do not hold
	// tool:calll anywhere", which sends somebody looking at their own grants
	// for a permission that was never real.
	if unknown := authz.UnknownPermissions(permissions); len(unknown) > 0 {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"no such permission: "+permissionList(unknown)+
				". The set is closed — one nothing enforces would be a role that "+
				"appears to grant something and does not")
		return
	}

	// You cannot define a role that confers a permission you do not hold
	// anywhere (ADR 0028).
	//
	// Checked at the global scope, because a role is scope-independent — the
	// scope arrives when it is granted. So this is the weaker of the two
	// checks and it exists for the error message: refusing here says "you
	// cannot put snapshot:publish in a role" at the moment somebody types it,
	// rather than letting them build a role they will be refused when they try
	// to use it. The check that actually holds the line is at grant time.
	caller := CallerFrom(r.Context())
	var withheld []authz.Permission
	for _, p := range permissions {
		if !caller.CanAnywhere(p) {
			withheld = append(withheld, p)
		}
	}
	if len(withheld) > 0 {
		writeError(w, s.log, http.StatusForbidden, CodeForbidden,
			"you cannot put "+permissionList(withheld)+" in a role: you do not "+
				"hold it anywhere, and a role cannot confer more than the person "+
				"defining it has")
		return
	}

	role, err := st.PutRole(r.Context(), name, req.Description, permissions)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// The catalog travels in the principal set, so a role change reaches the
	// data plane at principal latency rather than waiting for a snapshot
	// (ADR 0024). Without this the role would be edited and nothing served
	// would reflect it until something else happened to publish.
	warnPrincipals(w, s.publishPrincipals(r.Context()))
	writeJSON(w, s.log, http.StatusOK, roleOf(role))
}

func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	st := s.requireStore(w)
	if st == nil {
		return
	}
	if err := st.DeleteRole(r.Context(), chi.URLParam(r, "role")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	_ = s.publishPrincipals(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func roleOf(in store.Role) api.Role {
	out := api.Role{
		Name: in.Name, Description: in.Description, Builtin: in.Builtin,
		Permissions: []string{},
	}
	for _, p := range in.Permissions {
		out.Permissions = append(out.Permissions, string(p))
	}
	return out
}
