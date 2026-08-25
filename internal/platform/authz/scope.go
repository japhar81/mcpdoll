// Copyright 2026 Henry Zektser.

// Package authz is MCPDoll's RBAC: hierarchical scopes, roles that grant
// permissions, and grants that bind a role to a principal within a scope.
//
// The model is Casbin's, and deliberately the same one RAGdoll uses (ADR 0001):
// a grant is a `g` policy whose third field is the scope — Casbin's "domain" —
// and a role→permission row is a `p` policy. Two engines implement it: a
// dependency-free one that compiles the policy directly, and one backed by the
// real Casbin library. A conformance test pins them to identical decisions.
//
// See docs/adr/0015-rbac-scopes-and-engines.md.
package authz

import "strings"

// GlobalScope covers every request. Only a platform administrator holds a
// grant at this scope.
const GlobalScope = "*"

// The scope hierarchy, from widest to narrowest:
//
//   - platform-wide
//     t/<tenant>                          one whole tenant
//     t/<tenant>/ts/<toolset>             one toolset within a tenant
//     t/<tenant>/ts/<toolset>/<tool>      one tool within a toolset
//
// Unlike RAGdoll's — where `e/<env>` and `p/<pipeline>` are siblings under a
// tenant and neither covers the other — this hierarchy is linear. Every level
// covers the levels beneath it, which is what makes "grant Alice the CRM
// toolset" and "grant Bob one tool" the same operation at different depths.
const (
	tenantPrefix  = "t/"
	toolsetMarker = "/ts/"
)

// TenantScope covers everything in one tenant.
func TenantScope(tenant string) string {
	return tenantPrefix + tenant
}

// ToolsetScope covers every tool in one toolset of one tenant.
func ToolsetScope(tenant, toolset string) string {
	return tenantPrefix + tenant + toolsetMarker + toolset
}

// ToolScope covers exactly one tool.
//
// The tool is the backend's own name within the toolset, not the qualified
// name a client sees: a grant must survive the qualified name changing, and
// the prefix is a namespace property rather than a tool one.
func ToolScope(tenant, toolset, tool string) string {
	return tenantPrefix + tenant + toolsetMarker + toolset + "/" + tool
}

// ScopeCovers reports whether a grant at grantScope authorizes a request at
// requestScope.
//
// Because the hierarchy is linear, this is a prefix test — but a prefix test
// with a boundary, which is the whole subtlety. `t/acme` must not cover
// `t/acme-corp`: they are different tenants that happen to share seven
// characters, and a naive strings.HasPrefix would hand one tenant's admin
// authority over another's data.
//
// Allocation-free and on the hot path: a catalog listing calls this once per
// admitted tool per grant.
func ScopeCovers(grantScope, requestScope string) bool {
	if grantScope == GlobalScope {
		return true
	}
	if grantScope == requestScope {
		return true
	}
	// A grant is narrower than the request it is being tested against, so it
	// cannot cover it. Checked before the prefix test because the prefix test
	// would otherwise be asked to compare out of order.
	if len(grantScope) >= len(requestScope) {
		return false
	}
	if !strings.HasPrefix(requestScope, grantScope) {
		return false
	}
	// The character after the grant must be a separator. Without this,
	// `t/acme` "covers" `t/acme-corp`.
	return requestScope[len(grantScope)] == '/'
}

// ParsedScope is a scope broken into its parts. Empty fields mean the scope
// does not reach that depth.
type ParsedScope struct {
	// Global is true for "*", in which case every other field is empty.
	Global  bool
	Tenant  string
	Toolset string
	Tool    string
}

// ParseScope decomposes a scope string.
//
// Returns ok=false for anything that is not a well-formed scope, rather than a
// zero value that would read as a global grant. A malformed scope that parsed
// as `*` would be the worst possible failure mode here.
func ParseScope(scope string) (ParsedScope, bool) {
	if scope == GlobalScope {
		return ParsedScope{Global: true}, true
	}
	rest, ok := strings.CutPrefix(scope, tenantPrefix)
	if !ok || rest == "" {
		return ParsedScope{}, false
	}

	tenant, after, hasToolset := strings.Cut(rest, toolsetMarker)
	if tenant == "" || strings.Contains(tenant, "/") {
		return ParsedScope{}, false
	}
	if !hasToolset {
		return ParsedScope{Tenant: tenant}, true
	}

	toolset, tool, hasTool := strings.Cut(after, "/")
	if toolset == "" {
		return ParsedScope{}, false
	}
	if !hasTool {
		return ParsedScope{Tenant: tenant, Toolset: toolset}, true
	}
	// A tool name may not contain a separator: there is no level below tool,
	// so a slash here means the scope is malformed rather than deeper.
	if tool == "" || strings.Contains(tool, "/") {
		return ParsedScope{}, false
	}
	return ParsedScope{Tenant: tenant, Toolset: toolset, Tool: tool}, true
}

// TenantOf returns the tenant a scope belongs to, or "" for the global scope.
//
// Used to answer "which tenants can this principal reach at all?" before doing
// per-tool work — a principal with no grant touching a tenant needs none of
// that tenant's tools considered.
func TenantOf(scope string) string {
	parsed, ok := ParseScope(scope)
	if !ok || parsed.Global {
		return ""
	}
	return parsed.Tenant
}

// AnyReaches reports whether any grant touches a tenant at all.
//
// The "can this principal reach here" question, distinct from "may they do X
// here". Used where a tenant has to be *reachable* before something is bound
// to it — minting a key in a tenant its owner cannot reach produces a
// credential that resolves to an empty catalog, which reads as a bug rather
// than as the refusal it is.
//
// Reaching is not covering, and conflating the two is the bug this replaced.
// Coverage asks whether a grant is at least as wide as `t/acme`, which every
// narrowly-scoped grant fails: `tool_user@t/acme/ts/support` does not cover the
// tenant, it lives inside it. Testing coverage refused a key to exactly the
// least-privileged users keys exist for, and allowed one only to principals
// holding the whole tenant.
func AnyReaches(grants []Grant, tenant string) bool {
	for _, g := range grants {
		parsed, ok := ParseScope(g.Scope)
		if !ok {
			continue
		}
		if parsed.Global || parsed.Tenant == tenant {
			return true
		}
	}
	return false
}
