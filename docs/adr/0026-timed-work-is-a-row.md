# ADR 0026: Timed Work Is a Row, Not a Ticker

## Status

**Accepted.** Generalizes the loop [0025](./0025-the-catalog-rebuilds-itself.md)
introduced, and moves the heartbeats from
[0023](./0023-out-of-band-revocation.md) and
[0024](./0024-principals-leave-the-snapshot.md) onto the same footing. Ports the
scheduler concept from ragdoll's ADR 0009, with three deliberate divergences.

## Context

Three things ran on their own: the revocation heartbeat, the principal
heartbeat, and the catalog rebuild. Each was a `time.NewTicker` in a goroutine,
with its cadence in a Go constant or a config file.

That has three costs, and none of them are about correctness:

- **"What does this system do when nobody asks it to?"** was answerable only by
  reading `main.go` and following three `go` statements.
- **Changing a cadence was a deploy.** An operator who wanted the catalog
  rebuilt every five minutes instead of every minute had to edit a config file
  and restart the control plane.
- **A job that had been failing since Tuesday said so only in a log.** Each of
  these loops logged a warning and carried on, which is the right behaviour and
  the wrong place to leave the only evidence.

ADR 0025 added the third loop and, in the process, made the pattern obvious
enough to be worth naming.

## Decision

**A schedule is a row. The control plane registers the jobs it knows how to
run, and a loop claims due ones and dispatches them.**

```
startup ──▶ register jobs (cadence only if the row is new)
                    │
   tick (5s) ──▶ read cadence from the row ──▶ claim atomically ──▶ run
                                                       │
                                              record outcome on the row
```

### The row wins, including over the binary

Registration is `ON CONFLICT DO UPDATE` on the name only. A cadence somebody
retuned, or a schedule they switched off, survives a restart — otherwise every
deploy silently reverts their decision, which is worse than not offering the
knob.

Config still matters for a *new* deployment: `rebuild_interval` seeds the row
the first time it is created. After that it is a seed, not a setting.

### Divergences from ragdoll

**Intervals, not cron.** Ragdoll built a 5-field Vixie cron evaluator. Cron's
finest granularity is one minute, and the revocation heartbeat runs every
thirty seconds — its cadence *is* the exposure window for a leaked credential
(ADR 0023). A scheduler that could not express the most important schedule it
has would be worse than the tickers it replaced. `kind` is a discriminator so
calendar cadences can be added when something needs one; nothing does yet, and
scaffolding an evaluator for no caller is what `docs/deferred.md` is for.

**Claiming is atomic, so there is no single-instance caveat.** Ragdoll's
scheduler documented that `listDue` + `markRun` are unfenced, that two
instances double-enqueue, and that multi-worker deployments would need leader
election. MCPDoll already contemplates more than one control-plane replica —
the migration runner takes an advisory lock for exactly that reason — so the
claim is a conditional `UPDATE ... WHERE next_run_at <= now()` with `FOR UPDATE
SKIP LOCKED`. The row lock serializes it and the loser's predicate no longer
matches. It costs nothing and removes the whole class of problem, along with
the leader election that would otherwise be owed.

**"Run now" moves a timestamp.** It does not execute the job. A synchronous
path would be a second way for the work to happen — one that claims the row and
records an outcome, and one that does not — and the two would drift. Bringing
`next_run_at` forward means the next tick runs it through exactly the same code
as every other run.

### The data plane keeps its own timers, and that is not an oversight

Backend health probing, drift scanning, and probe timeouts stay in the data
plane's config file. They are not schedules and must not become them.

A data plane whose probe cadence lived in the control plane's database would
need that database to keep probing — which means a control-plane outage would
stop it from noticing an unhealthy backend, during precisely the outage ADR
0002 exists to make survivable. The line is: **the process that runs the work
owns its cadence, unless it can afford to lose the database.** The control
plane already cannot run without one. The data plane already must.

They are still *shown*, though. The data plane reports its cadences on its
admin listener and the schedules surfaces list them read-only, because a page
titled "Schedules" that quietly listed only half the platform's timed work
would leave a reader concluding nothing else runs. Seen, not owned — and when
the data plane cannot be reached, the list says so rather than silently
shortening.

### Cadence lives in one place

`RebuildState` used to report the configured interval. It no longer reports a
cadence at all, because the moment cadences moved into rows that field became a
lie waiting to happen: it would have gone on reporting the default while a
retuned schedule ran at something else.

## Alternatives considered

- **Leave the tickers and add a read-only "what runs" screen.** Cheaper, and it
  answers the discovery question without the retuning one. Rejected because the
  cadence would still be in two places — the screen and the constant — and
  keeping them honest is exactly the work a row does for free.
- **One row per run, as a job queue.** What ragdoll's `run_pipeline` jobs are.
  Right when runs are user-triggered, have inputs, and need history. These jobs
  are periodic maintenance with no arguments; a queue would add a table that
  grows forever to schedule three things.
- **Leader election instead of atomic claiming.** The documented evolution
  ragdoll deferred to. Rejected as strictly more machinery for a weaker
  guarantee: a lease can expire mid-run, and the conditional update cannot.
- **Put the data plane's probes here too, for one list.** Rejected on ADR 0002,
  above. A complete list is not worth a data plane that stops probing when the
  control plane is down.

## Consequences

- **The cadence of every scheduled job is operator-editable at runtime**,
  including cadences it would be unwise to change. Switching off the revocation
  heartbeat widens the exposure window for every credential in the deployment.
  That is a decision an operator is entitled to make, and the CLI says so where
  they would make it.
- **A 5-second tick means a job's cadence is accurate to ±5s.** Deliberate: a
  30-second job checked only every 30 seconds drifts to a 60-second effective
  period half the time. Checking is one indexed query; running is what costs.
- **The floor is 5 seconds.** Not against typos so much as against plausible
  numbers — a rebuild is a discovery sweep of every backend.
- **Two lists of timed things exist**, one in rows and one in the data plane's
  config. Somebody looking for "everything this system does on a timer" has to
  know that. The alternative was worse.
