-- Roles become rows an operator composes (ADR 0028).
--
-- The permission *vocabulary* stays closed — adding one is a schema change, a
-- seed, and a code change, and that friction is what keeps the set reviewable.
-- What opens up is composing roles out of it, which is ordinary administration
-- and was previously a code change for no good reason.
--
-- role_permissions already existed and already fell back to the built-in
-- catalog when empty. What was missing is a role that can exist before it has
-- any permissions, a way to tell a built-in from something somebody made, and
-- somewhere to say what a role is for.

CREATE TABLE roles (
  name        text PRIMARY KEY,
  description text NOT NULL DEFAULT '',
  -- A built-in cannot be deleted. Its permissions are editable — an operator
  -- who wants `viewer` to stop seeing the registry is entitled to that — but
  -- deleting one would leave grants pointing at a role nothing recreates, and
  -- the seed would put it back on the next boot anyway.
  builtin    boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- Every role named by an existing permission row, so a deployment that seeded
-- role_permissions before this migration keeps its catalog.
INSERT INTO roles (name, builtin)
SELECT DISTINCT role, true FROM role_permissions
ON CONFLICT (name) DO NOTHING;

-- Permissions belong to a role and die with it. A role_permissions row naming
-- no role is a permission nothing can grant, which reads as a catalog entry
-- while authorizing nobody.
ALTER TABLE role_permissions
  ADD CONSTRAINT role_permissions_role_fkey
  FOREIGN KEY (role) REFERENCES roles(name) ON DELETE CASCADE ON UPDATE CASCADE;
