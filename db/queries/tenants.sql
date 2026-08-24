-- name: CreateTenant :one
INSERT INTO tenants (slug, name, metadata)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = $1;

-- name: GetTenantBySlug :one
SELECT * FROM tenants WHERE slug = $1;

-- name: ListTenants :many
SELECT * FROM tenants ORDER BY slug;

-- name: UpdateTenant :one
UPDATE tenants
SET name = $2, status = $3, metadata = $4, updated_at = now()
WHERE id = $1
RETURNING *;

-- The slug is deliberately absent from UpdateTenant: it appears in every scope
-- string, so renaming a tenant would orphan every grant naming it.

-- name: DeleteTenant :exec
DELETE FROM tenants WHERE id = $1;
