-- 0001_tenancy_and_rbac.sql
--
-- Tenants, users, credentials, and RBAC. See:
--   docs/adr/0014-tenancy-and-principals.md
--   docs/adr/0015-rbac-scopes-and-engines.md
--
-- The RBAC tables map 1:1 onto Casbin, exactly as RAGdoll's do:
--   role_permissions -> `p` policies (role -> permission)
--   grants           -> `g` policies (user -> role @ scope)
-- The scope string is the Casbin domain: `*`, `t/<tenant>`,
-- `t/<tenant>/ts/<toolset>`, or `t/<tenant>/ts/<toolset>/<tool>`. A grant at an
-- ancestor scope covers every descendant.

-- ----------------------------------------------------------------- tenants --

CREATE TABLE tenants (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- The slug appears in every scope string, so it is immutable in practice:
  -- renaming one would orphan every grant that names it. Enforced in the
  -- repository rather than here, because the check needs to explain itself.
  slug       text NOT NULL UNIQUE,
  name       text NOT NULL,
  status     text NOT NULL DEFAULT 'active'
             CHECK (status IN ('active', 'suspended', 'archived')),
  metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------------- users --

CREATE TABLE users (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- Users belong to a tenant and die with it. Ownership rather than a filter:
  -- "delete this tenant" is then a cascade rather than a query somebody can
  -- forget to write correctly.
  tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  email         text NOT NULL,
  display_name  text,
  -- Null for a user who only ever signs in through an identity provider.
  password_hash text,
  status        text NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'disabled')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  -- Unique per tenant, not globally: the same person may hold accounts in two
  -- tenants of one deployment, and they are different principals.
  UNIQUE (tenant_id, email)
);

CREATE INDEX idx_users_tenant ON users(tenant_id);

-- One row per (provider, external subject). Linking an external identity to a
-- local user is what lets the same person arriving through two providers still
-- be one user with one set of grants.
CREATE TABLE user_identities (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider   text NOT NULL,
  subject    text NOT NULL,
  email      text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, subject)
);

CREATE INDEX idx_user_identities_user ON user_identities(user_id);

-- Configurable SSO connections. `config` holds non-secret connection metadata;
-- client secrets and SP private keys live outside the database.
--
-- `kind` is deliberately not a CHECK constraint: a third-party identity
-- provider registers its own kind (ADR 0020), and a constraint here would mean
-- a schema migration to install a plugin.
CREATE TABLE identity_providers (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug         text NOT NULL UNIQUE,
  kind         text NOT NULL,
  display_name text NOT NULL,
  enabled      boolean NOT NULL DEFAULT true,
  -- Null means the provider serves every tenant. A tenant-scoped provider is
  -- how one deployment hosts customers with their own IdPs.
  tenant_id    uuid REFERENCES tenants(id) ON DELETE CASCADE,
  config       jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);

-- -------------------------------------------------------------------- rbac --

-- Role -> permission (Casbin `p`). Editable, and seeded from the built-in
-- defaults so a fresh install is immediately usable.
CREATE TABLE role_permissions (
  role       text NOT NULL,
  permission text NOT NULL,
  PRIMARY KEY (role, permission)
);

-- User -> role @ scope (Casbin `g`).
CREATE TABLE grants (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       text NOT NULL,
  scope      text NOT NULL DEFAULT '*',
  -- Who granted it and when. An access-control table without this cannot
  -- answer the only question an auditor ever asks about one.
  granted_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, role, scope)
);

CREATE INDEX idx_grants_user ON grants(user_id);

-- ---------------------------------------------------------------- api keys --

-- How an agent authenticates. `prefix` is the public, lookup-able half;
-- `hash` covers the secret remainder. The plaintext is shown once at mint and
-- never stored, so the prefix is what appears in an audit trail — enough to
-- identify which key acted, useless for acting as it.
CREATE TABLE api_keys (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         text NOT NULL,
  prefix       text NOT NULL UNIQUE,
  hash         text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  expires_at   timestamptz,
  revoked_at   timestamptz
);

CREATE INDEX idx_api_keys_user ON api_keys(user_id);

-- A key's declared grants. Its *effective* grants are these intersected with
-- its owner's, recomputed at every resolution — which is what makes revoking
-- the user revoke the key. See ADR 0014.
CREATE TABLE api_key_grants (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  api_key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  role       text NOT NULL,
  scope      text NOT NULL,
  UNIQUE (api_key_id, role, scope)
);

CREATE INDEX idx_api_key_grants_key ON api_key_grants(api_key_id);

-- ---------------------------------------------------------------- settings --

CREATE TABLE auth_settings (
  id           boolean PRIMARY KEY DEFAULT true CHECK (id),
  signup_mode  text NOT NULL DEFAULT 'admin_only'
               CHECK (signup_mode IN ('admin_only', 'open_default_role', 'open_no_access')),
  default_role text,
  updated_at   timestamptz NOT NULL DEFAULT now()
);

-- admin_only by default: a fresh install must not let anyone who can reach the
-- identity provider create themselves an account.
INSERT INTO auth_settings (id, signup_mode, default_role)
VALUES (true, 'admin_only', 'viewer');
