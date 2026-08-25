// Copyright 2026 Henry Zektser.

package authz

import (
	"fmt"
	"sort"
)

// Permission is something a role may allow, in `resource:verb` form.
//
// The set is closed. Adding one is a schema change, a seed migration, and a UI
// change — friction that exists because a permission set which grows casually
// stops being reviewable. See ADR 0015.
type Permission string

// Platform and tenant administration.
const (
	// PermTenantManage creates, renames, and archives tenants.
	PermTenantManage Permission = "tenant:manage"
	// PermUserManage creates users and manages their credentials.
	PermUserManage Permission = "user:manage"
	// PermRoleManage edits the role→permission catalog and issues grants.
	//
	// Separate from user:manage on purpose: creating a user is routine, and
	// deciding what a user may do is not. An operator who can do the first
	// without the second cannot promote themselves.
	PermRoleManage Permission = "role:manage"
	// PermKeyManage mints and revokes API keys.
	PermKeyManage Permission = "key:manage"
	// PermIdpManage configures identity providers.
	PermIdpManage Permission = "idp:manage"
	// PermAuthSettings changes signup mode and the default role.
	PermAuthSettings Permission = "auth:settings"
)

// The registry and publishing.
const (
	// PermRegistryRead reads the registry document.
	PermRegistryRead Permission = "registry:read"
	// PermRegistryWrite edits backends, toolsets, and classifications.
	PermRegistryWrite Permission = "registry:write"
	// PermSnapshotBuild resolves the registry into a signed snapshot.
	PermSnapshotBuild Permission = "snapshot:build"
	// PermSnapshotPublish makes a built snapshot the one being served.
	//
	// Distinct from building it, so an operator can prepare a change that
	// somebody else puts into production — which is what makes
	// publisher ≠ approver expressible at all.
	PermSnapshotPublish Permission = "snapshot:publish"
	// PermKeyGenerate mints a snapshot signing key. The narrowest and most
	// dangerous permission here: it grants the ability to sign configuration
	// every data-plane instance will accept.
	PermKeyGenerate Permission = "signingkey:generate"
)

// Tools. These are the permissions a grant at a toolset or tool scope carries.
const (
	// PermToolList makes a tool appear in a principal's catalog.
	PermToolList Permission = "tool:list"
	// PermToolCall makes a tool invocable.
	//
	// Separate from listing because seeing and invoking are different
	// privileges — and because the *reverse* must be impossible: a principal
	// with call but not list would hold a capability that never appears in its
	// catalog. [ValidateCatalog] refuses that combination.
	PermToolCall Permission = "tool:call"
)

// Observability.
const (
	// PermGatewayInspect connects to the data plane as another principal to
	// see what they see.
	PermGatewayInspect Permission = "gateway:inspect"
	// PermAuditView reads the audit trail.
	PermAuditView Permission = "audit:view"
)

// AllPermissions is every permission, sorted. The closed set, enumerated once.
func AllPermissions() []Permission {
	return []Permission{
		PermAuditView,
		PermAuthSettings,
		PermGatewayInspect,
		PermIdpManage,
		PermKeyManage,
		PermRegistryRead,
		PermRegistryWrite,
		PermRoleManage,
		PermSnapshotBuild,
		PermSnapshotPublish,
		PermKeyGenerate,
		PermTenantManage,
		PermToolCall,
		PermToolList,
		PermUserManage,
	}
}

// Role names shipped by default. The catalog is editable, so these are seeds
// rather than a closed set — unlike permissions, which are a contract.
const (
	RolePlatformAdmin = "platform_admin"
	RoleTenantAdmin   = "tenant_admin"
	RoleToolsetAdmin  = "toolset_admin"
	RolePublisher     = "publisher"
	RoleToolUser      = "tool_user"
	RoleViewer        = "viewer"
	RoleAuditor       = "auditor"
)

// Catalog maps a role to the permissions it grants — Casbin `p` policies.
type Catalog map[string]map[Permission]struct{}

// DefaultCatalog seeds a fresh install.
//
// A new install is immediately usable rather than requiring an operator to
// invent a role model before they can do anything. Every one of these is
// editable afterwards.
func DefaultCatalog() Catalog {
	catalog := Catalog{}
	add := func(role string, perms ...Permission) {
		set := make(map[Permission]struct{}, len(perms))
		for _, p := range perms {
			set[p] = struct{}{}
		}
		catalog[role] = set
	}

	// Everything. Held at global scope by whoever runs the platform.
	add(RolePlatformAdmin, AllPermissions()...)

	// Runs one tenant: its users, its keys, its registry, its publishing.
	// Deliberately without signingkey:generate — a tenant admin should not be
	// able to mint a key that every data plane in the deployment trusts.
	add(RoleTenantAdmin,
		PermUserManage, PermRoleManage, PermKeyManage,
		PermRegistryRead, PermRegistryWrite,
		PermSnapshotBuild, PermSnapshotPublish,
		PermGatewayInspect, PermAuditView,
		PermToolList, PermToolCall,
	)

	// Curates toolsets without administering people.
	add(RoleToolsetAdmin,
		PermRegistryRead, PermRegistryWrite, PermSnapshotBuild,
		PermToolList, PermToolCall,
	)

	// Puts a built snapshot into production without being able to edit what
	// went into it — the approver half of publisher ≠ approver.
	add(RolePublisher, PermRegistryRead, PermSnapshotPublish, PermAuditView)

	// The role most grants use: may see and call the tools in its scope.
	add(RoleToolUser, PermToolList, PermToolCall)

	// Reads without acting. Includes tool:list so a viewer can see what a
	// toolset contains without being able to fire anything in it.
	add(RoleViewer, PermRegistryRead, PermToolList)

	// Reads the audit trail and nothing else. Separate from viewer because
	// audit access is frequently granted to people who should see no
	// configuration at all.
	add(RoleAuditor, PermAuditView)

	return catalog
}

// Grant binds a role to a principal within a scope — a Casbin `g` policy.
type Grant struct {
	Role  string `json:"role"`
	Scope string `json:"scope"`
}

// Validate checks a grant is well-formed.
func (g Grant) Validate() error {
	if g.Role == "" {
		return fmt.Errorf("grant has no role")
	}
	if g.Scope == "" {
		return fmt.Errorf("grant for role %q has no scope", g.Role)
	}
	if _, ok := ParseScope(g.Scope); !ok {
		return fmt.Errorf("grant for role %q has malformed scope %q", g.Role, g.Scope)
	}
	return nil
}

// Roles returns the catalog's role names, sorted.
func (c Catalog) Roles() []string {
	out := make([]string, 0, len(c))
	for role := range c {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

// Permissions returns one role's permissions, sorted.
func (c Catalog) Permissions(role string) []Permission {
	perms := c[role]
	out := make([]Permission, 0, len(perms))
	for p := range perms {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ValidateCatalog reports every problem with a role catalog at once.
//
// All problems rather than the first, for the same reason the registry
// validator does it: an operator fixing a role model one error per attempt is
// a bad afternoon.
func ValidateCatalog(c Catalog) error {
	known := make(map[Permission]struct{}, len(AllPermissions()))
	for _, p := range AllPermissions() {
		known[p] = struct{}{}
	}

	var problems []string
	for _, role := range c.Roles() {
		perms := c[role]

		for _, p := range c.Permissions(role) {
			if _, ok := known[p]; !ok {
				problems = append(problems, fmt.Sprintf(
					"role %q grants unknown permission %q", role, p))
			}
		}

		// The invariant from ADR 0015. A principal able to call a tool that
		// never appears in its catalog holds a hidden capability, which is
		// precisely what a gateway exists to prevent — so it is refused at
		// admission rather than served.
		_, canCall := perms[PermToolCall]
		_, canList := perms[PermToolList]
		if canCall && !canList {
			problems = append(problems, fmt.Sprintf(
				"role %q grants %s without %s: the tool would be invocable but "+
					"never appear in a catalog, which is a hidden capability",
				role, PermToolCall, PermToolList))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("role catalog: %d problem(s):\n  - %s",
			len(problems), joinLines(problems))
	}
	return nil
}

func joinLines(items []string) string {
	out := items[0]
	for _, item := range items[1:] {
		out += "\n  - " + item
	}
	return out
}

// Holder is what an escalation check needs from a caller: whether they hold a
// permission at a scope.
//
// An interface so this package can express the rule without importing the API
// server's Caller, and so a test can pass a bare function.
type Holder interface {
	Can(permission Permission, scope string) bool
}

// HolderFunc adapts a function to [Holder].
type HolderFunc func(Permission, string) bool

// Can implements [Holder].
func (f HolderFunc) Can(p Permission, scope string) bool { return f(p, scope) }

// WithheldBy reports the permissions `role` confers that `holder` does not have
// at `scope`. Empty means the grant is safe to issue.
//
// This is the rule that makes user-defined roles survivable, and it is not the
// same as checking `role:manage`. Holding role:manage says you may administer
// grants there; it says nothing about *what* you may confer. Without this a
// tenant admin defines a role carrying any permission they like, grants it to
// themselves in their own tenant, and holds it — the permission set becomes
// decoration, which is exactly what ADR 0022 refused for the fixed catalog.
//
// It is a real hole in the fixed catalog too, not only with editable roles:
// `tenant_admin` deliberately lacks `tenant:manage`, and could grant itself
// `platform_admin` at its own tenant to get it.
//
// The check is per scope because permissions are. Conferring `tool:call` at
// `t/acme` is safe for somebody who holds it there and an escalation for
// somebody who only holds it in `t/globex`.
func (c Catalog) WithheldBy(holder Holder, role, scope string) []Permission {
	var missing []Permission
	for _, p := range c.Permissions(role) {
		if !holder.Can(p, scope) {
			missing = append(missing, p)
		}
	}
	return missing
}

// UnknownPermissions returns the entries of `want` that are not in the closed
// vocabulary.
//
// The vocabulary is closed on purpose: a permission nothing enforces is a role
// that looks like it grants something and does not, and a typo would be
// indistinguishable from a real restriction.
func UnknownPermissions(want []Permission) []Permission {
	known := map[Permission]struct{}{}
	for _, p := range AllPermissions() {
		known[p] = struct{}{}
	}
	var out []Permission
	for _, p := range want {
		if _, ok := known[p]; !ok {
			out = append(out, p)
		}
	}
	return out
}

// DescribeRole is the one-line explanation of a built-in role.
//
// Held here beside the definitions rather than in the seed, so a role and the
// sentence describing it cannot drift apart. Empty for anything an operator
// made: their description is theirs to write.
func DescribeRole(role string) string {
	switch role {
	case RolePlatformAdmin:
		return "Everything. Held at the global scope by whoever runs the platform."
	case RoleTenantAdmin:
		return "Runs one tenant: its users, keys, registry, and publishing. " +
			"Deliberately without signingkey:generate — a tenant admin should not " +
			"be able to mint a key every data plane in the deployment trusts."
	case RoleToolsetAdmin:
		return "Curates toolsets without administering people."
	case RolePublisher:
		return "Puts a built snapshot into production without being able to edit " +
			"what went into it — the approver half of publisher ≠ approver."
	case RoleToolUser:
		return "May see and call the tools in its scope. The role most grants use."
	case RoleViewer:
		return "Reads without acting, including seeing what a toolset contains."
	case RoleAuditor:
		return "Reads the audit trail and nothing else."
	default:
		return ""
	}
}
