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
