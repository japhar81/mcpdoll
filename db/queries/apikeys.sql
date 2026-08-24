-- name: CreateAPIKey :one
INSERT INTO api_keys (user_id, name, prefix, hash, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetAPIKeyByPrefix :one
-- The authentication path. Looks up by the public prefix; the caller then
-- verifies the secret against `hash`. Revoked and expired keys are returned
-- rather than filtered, so the caller can tell "revoked" from "no such key" —
-- the two deserve different log lines and the same response.
SELECT * FROM api_keys WHERE prefix = $1;

-- name: ListAPIKeysByUser :many
SELECT * FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC;

-- name: ListAPIKeysByTenant :many
SELECT k.* FROM api_keys k
JOIN users u ON u.id = k.user_id
WHERE u.tenant_id = $1
ORDER BY k.created_at DESC;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1;

-- name: RevokeAPIKey :exec
UPDATE api_keys SET revoked_at = now() WHERE id = $1;

-- name: DeleteAPIKey :exec
DELETE FROM api_keys WHERE id = $1;

-- name: AddAPIKeyGrant :one
INSERT INTO api_key_grants (api_key_id, role, scope)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: ListAPIKeyGrants :many
SELECT * FROM api_key_grants WHERE api_key_id = $1 ORDER BY scope, role;

-- name: ListActiveAPIKeys :many
-- Every key that could authenticate right now, across every tenant. This is
-- what a snapshot build reads: the data plane holds no database, so the keys
-- have to travel in the artifact (ADR 0021). Revoked and expired keys are
-- excluded here rather than filtered later — publishing a key that cannot
-- authenticate would put a dead credential's digest into a signed file for no
-- reason.
SELECT * FROM api_keys
WHERE revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY created_at;

-- name: ListAPIKeyIDsByUser :many
SELECT id FROM api_keys WHERE user_id = $1;

-- name: GetAPIKey :one
SELECT * FROM api_keys WHERE id = $1;
