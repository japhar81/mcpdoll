-- The principal set's published version (ADR 0024).
--
-- Alongside the revocation list's, in the same one-row table, because both are
-- "what did the control plane last publish" and splitting them would be two
-- tables with one row each.
ALTER TABLE revocation_state
  ADD COLUMN principal_version bigint NOT NULL DEFAULT 0;
