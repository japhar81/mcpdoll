-- name: CreateSession :one
INSERT INTO sessions (user_id, prefix, hash, user_agent, ip, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSessionByPrefix :one
-- The authentication path. Looks up by the public prefix; the caller verifies
-- the secret against `hash`. Revoked and expired rows are returned rather than
-- filtered, so the caller can tell "expired" from "no such session" — the two
-- deserve different log lines and the same response.
SELECT * FROM sessions WHERE prefix = $1;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = now() WHERE id = $1;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = now() WHERE id = $1;

-- name: RevokeSessionsByUser :many
-- Signing somebody out everywhere. Returns the ids so each becomes a revocation
-- entry: a session already handed out is a live credential, and marking the row
-- is not the same as stopping it.
UPDATE sessions SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL
RETURNING id;

-- name: ListSessionsByUser :many
SELECT * FROM sessions WHERE user_id = $1 ORDER BY created_at DESC;

-- name: DeleteExpiredSessions :exec
-- Rows whose credential could not authenticate anyway. Kept for a grace period
-- so a session that expired an hour ago still explains itself in an audit.
DELETE FROM sessions WHERE expires_at < now() - interval '30 days';
