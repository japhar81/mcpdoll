-- A credential may span tenants (ADR 0027).
--
-- One MCP session resolving to exactly one tenant was the right default and the
-- wrong absolute. Comparing the same tool's answers in test and in live is one
-- question, and making somebody hold two sessions to ask it is the tool getting
-- in the way of the work.
--
-- Spanning is opt-in and per key, so the structural isolation an ordinary
-- credential has — it cannot address another tenant at all — is what you get
-- unless you ask for something else.

ALTER TABLE api_keys ADD COLUMN spans_tenants boolean NOT NULL DEFAULT false;

-- A spanning key names no single tenant: which tenants it reaches is decided by
-- its grants, and a second column saying so would be a second source of truth
-- that could disagree with them.
ALTER TABLE api_keys ALTER COLUMN tenant_id DROP NOT NULL;

-- Exactly one of the two shapes. Without this a key could be spanning *and*
-- carry a tenant — a row where two mechanisms both claim to decide the answer,
-- and nothing says which wins.
ALTER TABLE api_keys
  ADD CONSTRAINT api_keys_tenant_or_spanning_check
  CHECK (
    (spans_tenants = false AND tenant_id IS NOT NULL)
    OR
    (spans_tenants = true AND tenant_id IS NULL)
  );
