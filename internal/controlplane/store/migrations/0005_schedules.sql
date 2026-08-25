-- Timed work becomes a row (ADR 0026).
--
-- Three things ran on hardcoded tickers: the revocation heartbeat, the
-- principal heartbeat, and the catalog rebuild. Each was a `time.NewTicker` in
-- a goroutine with its cadence baked into a Go constant or a config file, which
-- means an operator asking "what does this system do on its own, and when?" had
-- to read the source, and changing a cadence needed a deploy.
--
-- A schedule is a row instead: visible, editable, and able to say when it last
-- ran and whether it worked.

CREATE TABLE schedules (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

  -- The dispatch key. A job type with no handler registered is a startup
  -- error, not a row that quietly never fires.
  job_type text NOT NULL UNIQUE,
  name     text NOT NULL,

  -- Cadence. `kind` is a discriminator so calendar schedules can be added
  -- later without rewriting this table: today the only kind is 'interval', and
  -- `spec` holds a Go duration ('30s', '1m').
  --
  -- Interval rather than cron, deliberately. Cron's finest granularity is one
  -- minute and the revocation heartbeat runs every thirty seconds — the
  -- cadence that most matters here is one cron cannot express at all.
  kind text NOT NULL DEFAULT 'interval' CHECK (kind IN ('interval')),
  spec text NOT NULL,

  enabled boolean NOT NULL DEFAULT true,

  -- A system schedule is one the platform needs in order to be correct.
  -- Its cadence is editable and it can be disabled — an operator who wants the
  -- catalog rebuilt hourly is entitled to that — but it cannot be deleted,
  -- because deleting it would remove a job nothing else would ever recreate.
  system boolean NOT NULL DEFAULT false,

  next_run_at timestamptz,
  last_run_at timestamptz,
  -- Whether the last run worked, and why not. Held on the row rather than only
  -- logged: a job that has been failing since Tuesday is invisible in a log
  -- nobody is tailing, and the whole point of making these rows is that
  -- somebody can look at them.
  last_error    text,
  last_duration_ms integer,

  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- The claim query orders by this and filters on enabled.
CREATE INDEX idx_schedules_due ON schedules(next_run_at) WHERE enabled;
