-- name: ListRolePermissions :many
SELECT * FROM role_permissions ORDER BY role, permission;

-- name: AddRolePermission :exec
INSERT INTO role_permissions (role, permission)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: RemoveRolePermission :exec
DELETE FROM role_permissions WHERE role = $1 AND permission = $2;

-- name: DeleteRole :exec
DELETE FROM role_permissions WHERE role = $1;

-- name: CreateGrant :one
INSERT INTO grants (user_id, role, scope, granted_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, role, scope) DO UPDATE
  SET granted_by = EXCLUDED.granted_by
RETURNING *;

-- name: ListGrantsByUser :many
SELECT * FROM grants WHERE user_id = $1 ORDER BY scope, role;

-- name: ListGrantsByTenant :many
-- Grants *within* a tenant's scope, which is now the only sense in which a
-- grant belongs to a tenant.
SELECT * FROM grants
WHERE scope = $1 OR scope LIKE $1 || '/%'
ORDER BY user_id, scope, role;

-- name: RevokeGrant :exec
DELETE FROM grants WHERE user_id = $1 AND role = $2 AND scope = $3;

-- name: RevokeGrantByID :exec
DELETE FROM grants WHERE id = $1;

-- name: ListAllGrants :many
-- Every user's grants in one query. A snapshot build needs all of them, and
-- asking per user turns a publish into one round trip per person on staff.
SELECT * FROM grants ORDER BY user_id, scope, role;

-- name: ListAllAPIKeyGrants :many
SELECT * FROM api_key_grants ORDER BY api_key_id, scope, role;

-- name: BumpPrincipalVersion :one
-- The principal set's version. Monotonic, and what the data plane compares
-- against what it holds — a set that did not bump would publish a file nobody
-- applies.
UPDATE revocation_state
SET principal_version = principal_version + 1, updated_at = now()
WHERE id = true
RETURNING *;

-- name: DeleteGrantsInTenant :exec
-- Grants name a scope as text, not a tenant by foreign key, so nothing cascades
-- when a tenant is deleted. Left behind they are dormant *and* dangerous: a
-- tenant recreated with the same slug would silently re-authorize everyone who
-- was granted into the old one.
DELETE FROM grants WHERE scope = $1 OR scope LIKE $1 || '/%';

-- name: ListRoles :many
SELECT * FROM roles ORDER BY name;

-- name: GetRole :one
SELECT * FROM roles WHERE name = $1;

-- name: UpsertRole :one
-- Registration and creation in one. `builtin` is only ever set true here, by
-- the seed: nothing promotes an operator's role into a built-in, and a restart
-- must not demote a built-in either.
INSERT INTO roles (name, description, builtin)
VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE
  -- Only fills a description in; never replaces one. The seed runs on every
  -- boot, so overwriting here would silently revert an operator's edit every
  -- time the process restarted — the same trap the schedule cadences avoid.
  SET description = CASE
        WHEN roles.description = '' THEN EXCLUDED.description
        ELSE roles.description
      END,
      updated_at = now()
RETURNING *;

-- name: SetRoleDescription :one
UPDATE roles SET description = $2, updated_at = now() WHERE name = $1 RETURNING *;

-- name: ClearRolePermissions :exec
DELETE FROM role_permissions WHERE role = $1;

-- name: DeleteRoleRow :exec
DELETE FROM roles WHERE name = $1 AND NOT builtin;

-- name: CountGrantsOfRole :one
-- Whether anybody holds this role. Deleting one that is granted would revoke
-- access from people the operator was not looking at.
SELECT count(*) FROM grants WHERE role = $1;

-- name: CountKeyGrantsOfRole :one
-- The same question for keys, which narrow themselves with their own grants.
SELECT count(*) FROM api_key_grants WHERE role = $1;
