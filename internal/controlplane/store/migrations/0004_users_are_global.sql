-- Users stop belonging to a tenant; API keys start naming one.
--
-- This is ragdoll's shape, and I diverged from it without a good enough reason.
-- ADR 0014 rejected "tenant as a scope on a flat user table" because it makes
-- deleting a tenant a query rather than a cascade. That is true and it is a
-- small cost, and it bought a large one: a person had to sign in *to a tenant*,
-- their catalog came from whichever tenant happened to own their row, and the
-- same email in two tenants became two different people who could not be told
-- apart at the login screen.
--
-- What MCPDoll genuinely needs that ragdoll does not is for one MCP session to
-- resolve to exactly one tenant — tool names would otherwise collide across
-- tenants in a single catalog. That is a property of the *credential*, not of
-- the person, which is exactly how ragdoll scopes its own keys
-- (`api_keys.environment_id`). So the key names the tenant.

-- ------------------------------------------------------------- api keys ----

-- Backfilled from the owner's tenant before the column it comes from is
-- dropped. Every existing key keeps resolving to the tenant it already did.
ALTER TABLE api_keys ADD COLUMN tenant_id uuid REFERENCES tenants(id) ON DELETE CASCADE;

UPDATE api_keys k
SET tenant_id = u.tenant_id
FROM users u
WHERE u.id = k.user_id AND k.tenant_id IS NULL;

-- A key with no tenant cannot pick a backend binding, so there is no sensible
-- default and NULL must not be reachable.
DELETE FROM api_keys WHERE tenant_id IS NULL;
ALTER TABLE api_keys ALTER COLUMN tenant_id SET NOT NULL;

CREATE INDEX idx_api_keys_tenant ON api_keys(tenant_id);

-- ---------------------------------------------------------------- users ----

-- Two rows with one email cannot both survive a global unique constraint, and
-- which one survives is not arbitrary. Deleting the *newer* was the obvious
-- rule and it is wrong here: this deployment's `dev-admin@mcpdoll.local` exists
-- twice, and the older of the two is a disabled decoy an earlier seed left in
-- the `platform` tenant. "Newer loses" keeps the dead row and deletes the
-- account somebody signs in with.
--
-- So: an active row beats a disabled one, a row with a password beats one
-- without, and only then does the older win. `id` breaks the remaining tie, so
-- two rows created in the same transaction still resolve to exactly one
-- survivor rather than failing the constraint.
-- A plain table, not a TEMP ... ON COMMIT DROP one. The runner wraps each
-- migration in a transaction so the temp form would work here, but it would
-- work *only* here: applied statement by statement through psql — which is how
-- anyone debugs a migration that failed — the table would disappear after the
-- first statement and every line below it would error on a missing relation.
CREATE TABLE user_merge AS
SELECT
  id AS loser,
  first_value(id) OVER (
    PARTITION BY email
    ORDER BY (status = 'disabled'), (password_hash IS NULL), created_at, id
  ) AS survivor
FROM users;

DELETE FROM user_merge WHERE loser = survivor;

-- What the loser held moves rather than dying with them. The same email is one
-- person now, so their keys are their keys — and a key names its own tenant, so
-- it keeps resolving exactly where it did. Dropping them instead would break
-- live credentials to tidy up a row.
UPDATE api_keys k SET user_id = m.survivor FROM user_merge m WHERE k.user_id = m.loser;

-- Grants move the same way, minus any the survivor already holds: the unique
-- constraint is (user_id, role, scope) and a merge can easily produce a
-- duplicate of a grant both rows had.
UPDATE grants g SET user_id = m.survivor
FROM user_merge m
WHERE g.user_id = m.loser
  AND NOT EXISTS (
    SELECT 1 FROM grants existing
    WHERE existing.user_id = m.survivor AND existing.role = g.role AND existing.scope = g.scope
  );

-- `granted_by` is audit, so it is repointed rather than nulled by the cascade:
-- "who granted this" losing its answer is the one thing that table exists for.
UPDATE grants g SET granted_by = m.survivor FROM user_merge m WHERE g.granted_by = m.loser;

DELETE FROM users u USING user_merge m WHERE u.id = m.loser;

DROP TABLE user_merge;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_tenant_id_email_key;
ALTER TABLE users DROP COLUMN tenant_id;
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
