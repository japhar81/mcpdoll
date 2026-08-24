-- Sessions for people, and revocations that do not wait for a snapshot.
--
-- See ADR 0022 (the control plane authenticates and enforces its own RBAC) and
-- ADR 0023 (revocation travels out of band, signed, and only subtracts).

-- ---------------------------------------------------------------- sessions --

-- A session is a row, not a signed token.
--
-- A signed token would be stateless and unrevocable, and revocation is the
-- subject of the adjacent ADR — shipping a credential that cannot be revoked
-- while building a revocation path would be absurd. This is a low-traffic
-- control plane with the database already in the request path.
--
-- Same `prefix` + SHA-256 shape as api_keys, for the same reason (ADR 0021):
-- the secret is CSPRNG output, so there is nothing for a KDF to defend and a
-- memory-hard hash would only cost latency on every request.
CREATE TABLE sessions (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  prefix       text NOT NULL UNIQUE,
  hash         text NOT NULL,
  -- Where it was created from. Not used for authorization — an attacker
  -- controls both — but it is what makes "was this me?" answerable.
  user_agent   text,
  ip           text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz,
  expires_at   timestamptz NOT NULL,
  revoked_at   timestamptz
);

CREATE INDEX idx_sessions_user ON sessions(user_id);
-- The authentication path is a lookup by prefix; the expiry sweep is a scan by
-- expires_at. Both are hot enough to index and neither is covered by the other.
CREATE INDEX idx_sessions_expiry ON sessions(expires_at);

-- ------------------------------------------------------------ revocations --

-- Principals the data plane must refuse, whatever the snapshot says.
--
-- Only subtraction. There are no grants, scopes, or roles here, and that is the
-- design rather than an omission: it is what keeps "why was this allowed?"
-- answerable from the snapshot alone (ADR 0023).
CREATE TABLE revocations (
  id            bigserial PRIMARY KEY,
  -- The id the *snapshot* addresses the principal by. For an API key that is
  -- the key id; the data plane never sees an email or a user id in a grant
  -- decision, so nothing else would match.
  principal_id  uuid NOT NULL,
  -- What was revoked, for the audit trail. The data plane does not read it.
  kind          text NOT NULL CHECK (kind IN ('api_key', 'session')),
  -- Who it belonged to, so "every credential this person held" is one query
  -- rather than a join through a table the cascade may already have emptied.
  user_id       uuid,
  reason        text,
  revoked_at    timestamptz NOT NULL DEFAULT now(),
  -- Set when a snapshot built after this revocation is published: from then on
  -- the entry is redundant, because that snapshot already omits the credential.
  -- Pruned rather than kept, or the list grows forever.
  superseded_by bigint
);

CREATE UNIQUE INDEX idx_revocations_principal ON revocations(principal_id);
CREATE INDEX idx_revocations_user ON revocations(user_id);
CREATE INDEX idx_revocations_time ON revocations(revoked_at);

-- The version of the list the control plane last published, and the snapshot
-- version that made pruning safe.
--
-- One row. A data plane serving a snapshot older than `pruned_through` would
-- lose denials it still needs, so it refuses the list and keeps its previous
-- one — strictly more denials, and self-correcting once the newer snapshot
-- lands.
CREATE TABLE revocation_state (
  id              boolean PRIMARY KEY DEFAULT true CHECK (id),
  version         bigint NOT NULL DEFAULT 0,
  pruned_through  bigint NOT NULL DEFAULT 0,
  updated_at      timestamptz NOT NULL DEFAULT now()
);

INSERT INTO revocation_state (id, version, pruned_through) VALUES (true, 0, 0);
