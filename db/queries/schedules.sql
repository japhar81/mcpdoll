-- name: ListSchedules :many
SELECT * FROM schedules ORDER BY name;

-- name: GetScheduleByJobType :one
SELECT * FROM schedules WHERE job_type = $1;

-- name: UpsertSystemSchedule :one
-- Registration, run at startup for every job the binary knows how to do.
--
-- The cadence is NOT overwritten on conflict. A schedule an operator has
-- retuned or disabled must survive a restart — otherwise every deploy silently
-- reverts their decision, which is worse than not letting them make it.
INSERT INTO schedules (job_type, name, kind, spec, system, next_run_at)
VALUES ($1, $2, $3, $4, true, now())
ON CONFLICT (job_type) DO UPDATE
  SET name = EXCLUDED.name, updated_at = now()
RETURNING *;

-- name: ClaimDueSchedule :one
-- Atomically take one due schedule and push its next run out.
--
-- The `next_run_at <= now()` in the WHERE clause is what makes this safe with
-- more than one control-plane replica: the row lock serializes the update, and
-- the loser's WHERE no longer matches, so it claims nothing. Ragdoll's
-- scheduler documented the opposite — listDue then markRun, unfenced, with a
-- note that two instances double-fire and would need leader election. Claiming
-- in the UPDATE costs nothing and removes that whole class of problem.
UPDATE schedules s
SET next_run_at = now() + $2::interval, last_run_at = now(), updated_at = now()
WHERE s.id = (
  SELECT d.id FROM schedules d
  WHERE d.enabled AND d.next_run_at IS NOT NULL AND d.next_run_at <= now()
    AND d.job_type = $1
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING *;

-- name: RecordScheduleOutcome :exec
UPDATE schedules
SET last_error = $2, last_duration_ms = $3, updated_at = now()
WHERE id = $1;

-- name: UpdateScheduleCadence :one
UPDATE schedules
SET spec = COALESCE(sqlc.narg('spec'), spec),
    enabled = COALESCE(sqlc.narg('enabled'), enabled),
    -- Re-arm from now. Without this, lengthening a cadence leaves the old
    -- next_run_at in place and the change appears not to have taken; and
    -- re-enabling a schedule whose next_run_at is years stale would fire it
    -- immediately and then keep firing to catch up.
    next_run_at = now(),
    updated_at = now()
WHERE job_type = $1
RETURNING *;

-- name: DueNow :exec
-- Bring a schedule forward so the next tick takes it. "Run now" without a
-- second execution path: the same loop, the same claim, the same outcome
-- recording. A separate synchronous run would be a second way for a job to
-- happen, and the two would drift.
UPDATE schedules SET next_run_at = now(), updated_at = now() WHERE job_type = $1;
