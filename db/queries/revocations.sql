-- name: AddRevocation :one
INSERT INTO revocations (principal_id, kind, user_id, reason)
VALUES ($1, $2, $3, $4)
ON CONFLICT (principal_id) DO UPDATE SET revoked_at = now(), reason = EXCLUDED.reason
RETURNING *;

-- name: ListActiveRevocations :many
-- Everything the data plane must still refuse. A superseded entry is one a
-- published snapshot already omits, so carrying it would only make the signed
-- list bigger.
SELECT * FROM revocations WHERE superseded_by IS NULL ORDER BY id;

-- name: SupersedeRevocationsBefore :exec
-- Called when a snapshot is published: every revocation committed before the
-- build read the database is already reflected in it.
UPDATE revocations
SET superseded_by = $1
WHERE superseded_by IS NULL AND revoked_at < $2;

-- name: BumpRevocationVersion :one
-- Monotonic, and the version the data plane compares against what it holds.
UPDATE revocation_state
SET version = version + 1, updated_at = now()
WHERE id = true
RETURNING *;

-- name: SetRevocationPrunedThrough :one
UPDATE revocation_state
SET version = version + 1, pruned_through = $1, updated_at = now()
WHERE id = true
RETURNING *;

-- name: GetRevocationState :one
SELECT * FROM revocation_state WHERE id = true;
