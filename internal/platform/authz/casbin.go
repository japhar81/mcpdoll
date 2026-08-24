// Copyright 2026 The MCPDoll Authors.

package authz

import (
	"context"
	"fmt"
	"strings"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	stringadapter "github.com/casbin/casbin/v2/persist/string-adapter"
)

// Model is the Casbin model MCPDoll's RBAC compiles to.
//
// Identical in shape to RAGdoll's (ADR 0001): a request is
// (subject, domain, object) where the domain is the scope; `g` binds a subject
// to a role within a domain; `p` binds a role to a permission. The domain
// matcher is [ScopeCovers], registered below, which is what makes an
// ancestor-scope grant authorize a descendant-scope request.
//
// `p.obj == "*"` lets a role hold every permission without enumerating them,
// which is how platform_admin stays correct when a permission is added.
const Model = `
[request_definition]
r = sub, dom, obj

[policy_definition]
p = sub, obj

[role_definition]
g = _, _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub, r.dom) && (p.obj == r.obj || p.obj == "*")
`

// casbinSubject is the single synthetic subject every compilation uses.
//
// One principal is compiled at a time, so its identity carries no information —
// the `g` edges in the policy *are* this principal's grants. Using a synthetic
// name keeps a real principal id out of a policy document that may be logged.
const casbinSubject = "principal"

// CasbinEngine decides with the real Casbin library.
//
// It exists because operators asked for Casbin by name and because a policy
// language they can read is worth a dependency. It must agree with
// [BuiltinEngine] on every input; `casbin_test.go` is what enforces that, and
// if it is ever weakened to make a change land, one of these two engines should
// be deleted instead of both being kept.
type CasbinEngine struct{}

// Prepare compiles the grants through Casbin and returns a synchronous decider.
//
// The enforcer is built once here and captured. Building it per decision would
// put policy parsing on the serving path, where a catalog listing makes
// thousands of calls.
func (CasbinEngine) Prepare(_ context.Context, grants []Grant, catalog Catalog) (Decider, error) {
	m, err := model.NewModelFromString(Model)
	if err != nil {
		return nil, fmt.Errorf("authz: compiling the casbin model: %w", err)
	}

	var lines []string
	for _, role := range catalog.Roles() {
		for _, permission := range catalog.Permissions(role) {
			lines = append(lines, fmt.Sprintf("p, %s, %s", csv(role), csv(string(permission))))
		}
	}
	for _, g := range grants {
		lines = append(lines, fmt.Sprintf("g, %s, %s, %s",
			casbinSubject, csv(g.Role), csv(g.Scope)))
	}

	// With no policies at all there is nothing that could match, which is
	// exactly default-deny. Casbin's string adapter also rejects an empty
	// document, so this returns rather than constructing an enforcer that
	// would fail for a reason unrelated to the decision.
	if len(lines) == 0 {
		return DenyAll(), nil
	}

	enforcer, err := casbin.NewEnforcer(m, stringadapter.NewAdapter(strings.Join(lines, "\n")))
	if err != nil {
		return nil, fmt.Errorf("authz: building the casbin enforcer: %w", err)
	}

	// Casbin calls the domain matcher as fn(requestDomain, policyDomain); this
	// package's argument order is (grant, request). Getting these backwards
	// inverts the hierarchy — a tool-scoped grant would authorize tenant-wide
	// requests — and it is the kind of mistake that passes a casual test,
	// which is why the conformance suite drives asymmetric scope pairs.
	if !enforcer.AddNamedDomainMatchingFunc("g", "scopeCovers",
		func(requestScope, grantScope string) bool {
			return ScopeCovers(grantScope, requestScope)
		}) {
		return nil, fmt.Errorf(
			"authz: casbin refused the domain matcher; the `g` role manager is absent")
	}

	if err := enforcer.BuildRoleLinks(); err != nil {
		return nil, fmt.Errorf("authz: building casbin role links: %w", err)
	}

	return func(permission Permission, requestScope string) bool {
		allowed, err := enforcer.Enforce(casbinSubject, requestScope, string(permission))
		if err != nil {
			// A decider cannot return an error, and a decision that failed to
			// evaluate is not an allow.
			return false
		}
		return allowed
	}, nil
}

// NewCasbinEngine builds the engine and proves it works before returning it.
//
// The probe is a real compilation with a known input and a checked result, not
// a construction that could succeed while deciding wrongly. Failing here means
// a misconfigured deployment stops at boot rather than authorizing incorrectly
// under load — the same reasoning as RAGdoll's `createCasbinEngine`.
func NewCasbinEngine(ctx context.Context) (*CasbinEngine, error) {
	engine := &CasbinEngine{}

	probeCatalog := Catalog{"probe": {PermToolList: {}}}
	decide, err := engine.Prepare(ctx,
		[]Grant{{Role: "probe", Scope: TenantScope("probe")}}, probeCatalog)
	if err != nil {
		return nil, fmt.Errorf("authz: casbin probe: %w", err)
	}

	// Both directions. A matcher wired backwards passes the positive case.
	if !decide(PermToolList, ToolsetScope("probe", "any")) {
		return nil, fmt.Errorf(
			"authz: casbin probe: a tenant-scoped grant did not cover a toolset request")
	}
	if decide(PermToolList, TenantScope("other")) {
		return nil, fmt.Errorf(
			"authz: casbin probe: a grant in one tenant covered a request in another")
	}
	return engine, nil
}

// csv escapes a Casbin policy field.
//
// Role names and scopes are operator-supplied. A comma in either would
// otherwise shift every following field left — turning a scope into a
// permission — which is a policy injection rather than a parse error.
func csv(field string) string {
	if strings.ContainsAny(field, `",`+"\n") {
		return `"` + strings.ReplaceAll(field, `"`, `""`) + `"`
	}
	return field
}
