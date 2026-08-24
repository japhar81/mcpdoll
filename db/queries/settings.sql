-- name: GetAuthSettings :one
SELECT * FROM auth_settings WHERE id = true;

-- name: UpdateAuthSettings :one
UPDATE auth_settings
SET signup_mode = $1, default_role = $2, updated_at = now()
WHERE id = true
RETURNING *;

-- name: CreateIdentityProvider :one
INSERT INTO identity_providers (slug, kind, display_name, enabled, tenant_id, config)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetIdentityProviderBySlug :one
SELECT * FROM identity_providers WHERE slug = $1;

-- name: ListIdentityProviders :many
SELECT * FROM identity_providers ORDER BY slug;

-- name: ListEnabledIdentityProviders :many
-- The login screen's list. A tenant-scoped provider (tenant_id set) is offered
-- only to that tenant; a null tenant_id serves every tenant.
SELECT * FROM identity_providers
WHERE enabled = true AND (tenant_id IS NULL OR tenant_id = $1)
ORDER BY display_name;

-- name: UpdateIdentityProvider :one
UPDATE identity_providers
SET kind = $2, display_name = $3, enabled = $4, config = $5, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteIdentityProvider :exec
DELETE FROM identity_providers WHERE id = $1;
