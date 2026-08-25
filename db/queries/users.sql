-- name: CreateUser :one
-- No tenant: a user is a person, and which tenants they reach is what their
-- grants say (ADR 0014, amended). A user with no grants is inert, which is why
-- creating one is safe at any scope.
INSERT INTO users (email, display_name, password_hash)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
-- Globally unique, so signing in needs an email and a password and nothing
-- else.
SELECT * FROM users WHERE email = $1;

-- name: ListUsersInTenant :many
-- Users *granted into* a tenant rather than owned by one. A more useful
-- listing than ownership was: it answers "who can reach this tenant", which is
-- the question an administrator is actually asking.
SELECT DISTINCT u.* FROM users u
JOIN grants g ON g.user_id = u.id
WHERE g.scope = $1 OR g.scope LIKE $1 || '/%' OR g.scope = '*'
ORDER BY u.email;

-- name: ListAllUsers :many
SELECT * FROM users ORDER BY email;

-- name: UpdateUser :one
UPDATE users
SET display_name = $2, status = $3, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetUserPassword :exec
UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = $1;

-- name: LinkIdentity :one
INSERT INTO user_identities (user_id, provider, subject, email)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUserByIdentity :one
SELECT u.* FROM users u
JOIN user_identities i ON i.user_id = u.id
WHERE i.provider = $1 AND i.subject = $2;

-- name: ListIdentitiesByUser :many
SELECT * FROM user_identities WHERE user_id = $1 ORDER BY provider;

-- name: UnlinkIdentity :exec
DELETE FROM user_identities WHERE id = $1;

-- CountUsersByTenant answers the tenant list in one query rather than one per
-- tenant. A tenant list is the first screen an operator opens, and N+1 there is
-- N+1 forever.
-- name: CountUsersByTenant :many
-- Counted from grants, since a user no longer belongs to a tenant. A grant at
-- global scope reaches every tenant and is counted for each.
SELECT t.id AS tenant_id, count(DISTINCT g.user_id)::bigint AS users
FROM tenants t
LEFT JOIN grants g
  ON g.scope = 't/' || t.slug
  OR g.scope LIKE 't/' || t.slug || '/%'
  OR g.scope = '*'
GROUP BY t.id;
