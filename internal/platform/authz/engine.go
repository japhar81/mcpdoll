// Copyright 2026 The MCPDoll Authors.

package authz

import "context"

// Decider answers one authorization question, synchronously and without I/O.
//
// This is the type the serving path holds. A catalog listing for a principal
// with 2,000 admitted tools is 2,000 calls to this, so it must be a lookup and
// a comparison — never a policy evaluation, and certainly never a network hop.
// See ADR 0015.
type Decider func(permission Permission, requestScope string) bool

// Engine compiles a principal's grants into a [Decider].
//
// Compilation is where the work goes: it happens once per principal per
// snapshot, off the request path, and may be async and expensive. That
// separation is what makes an out-of-process engine viable at all (ADR 0020) —
// one RPC per principal, not one per decision.
type Engine interface {
	Prepare(ctx context.Context, grants []Grant, catalog Catalog) (Decider, error)
}

// DenyAll refuses everything. The value a failed compilation must produce, and
// the correct answer for a principal with no grants.
func DenyAll() Decider {
	return func(Permission, string) bool { return false }
}

// BuiltinEngine implements the model directly, with no dependencies.
//
// It is not a stub or a fallback: it is the engine the test suite runs and the
// one a deployment that does not want the Casbin dependency uses in
// production. `casbin_test.go` pins it to identical decisions with the real
// engine, which is what makes shipping two of these defensible rather than
// merely convenient.
type BuiltinEngine struct{}

// Prepare compiles the grants into a decider.
//
// The compiled form is a map from permission to the scopes at which the
// principal holds it. That shape is chosen for the serving path's actual
// question — "may this principal list this tool?" — which is one map lookup
// followed by a short scan of scopes that are almost always one or two entries
// long. Indexing by scope instead would put the linear scan on the permission
// side, where the sets are larger.
func (BuiltinEngine) Prepare(_ context.Context, grants []Grant, catalog Catalog) (Decider, error) {
	byPermission := map[Permission][]string{}

	for _, g := range grants {
		perms, known := catalog[g.Role]
		if !known {
			// A grant naming a role the catalog does not define authorizes
			// nothing. Silently, on purpose: roles can be deleted while grants
			// referencing them survive, and that must fail closed rather than
			// failing the whole compilation and locking out every principal.
			continue
		}
		for permission := range perms {
			byPermission[permission] = appendUnique(byPermission[permission], g.Scope)
		}
	}

	if len(byPermission) == 0 {
		return DenyAll(), nil
	}

	return func(permission Permission, requestScope string) bool {
		for _, granted := range byPermission[permission] {
			if ScopeCovers(granted, requestScope) {
				return true
			}
		}
		return false
	}, nil
}

// appendUnique keeps the scope list short.
//
// A principal commonly holds several roles at one scope — tenant_admin and
// tool_user at `t/acme` — and every one of them would otherwise contribute a
// duplicate entry that the hot-path scan walks for nothing.
func appendUnique(list []string, scope string) []string {
	for _, existing := range list {
		if existing == scope {
			return list
		}
	}
	return append(list, scope)
}

// Intersect narrows a set of grants by another, per ADR 0014: an API key's
// effective grants are its declared grants intersected with its owner's.
//
// A key grant survives only where the owner holds the same role at a scope
// that covers it. So a key may name a *narrower* scope than the owner's grant
// — that is the point — but may not reach a scope the owner cannot.
//
// Recomputed at every resolution rather than stored at mint time. That is what
// makes revoking a user revoke every key they hold, with no key-by-key cleanup
// and no chance of missing one.
func Intersect(keyGrants, ownerGrants []Grant) []Grant {
	if len(keyGrants) == 0 || len(ownerGrants) == 0 {
		return nil
	}

	out := make([]Grant, 0, len(keyGrants))
	for _, kg := range keyGrants {
		for _, og := range ownerGrants {
			if og.Role != kg.Role {
				continue
			}
			if ScopeCovers(og.Scope, kg.Scope) {
				out = append(out, kg)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
