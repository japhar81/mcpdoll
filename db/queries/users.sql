-- name: CreateUser :one
INSERT INTO users (tenant_id, email, display_name, password_hash)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE tenant_id = $1 AND email = $2;

-- name: ListUsersByTenant :many
SELECT * FROM users WHERE tenant_id = $1 ORDER BY email;

-- name: ListAllUsers :many
SELECT * FROM users ORDER BY tenant_id, email;

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
