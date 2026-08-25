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

-- Two rows with one email cannot both survive a global unique constraint, so
-- one of them goes. Which one is not worth deciding carefully: nothing is
-- deployed, the only databases this runs against are development ones, and a
-- development database that comes out wrong is recreated rather than repaired.
--
-- An earlier draft ranked the duplicates — active over disabled, with a
-- password over without — and moved the loser's keys and grants onto the
-- survivor. That is the right migration to write for real data and the wrong
-- one to write for this: forty lines of merge logic whose only test is a
-- scratch database, standing in for a `docker compose down -v`.
DELETE FROM users a USING users b WHERE a.email = b.email AND a.id > b.id;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_tenant_id_email_key;
ALTER TABLE users DROP COLUMN tenant_id;
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
