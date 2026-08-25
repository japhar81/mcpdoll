# ADR 0025: The Catalog Rebuilds Itself

## Status

**Accepted.** Removes publishing as a user-facing step. Keeps every property
[0002](./0002-control-data-plane-split.md) (serving through a control-plane
outage), [0006](./0006-serve-admitted-not-observed.md) (admitted, not observed),
and [0009](./0009-snapshot-signing-and-distribution.md) (signing and
distribution) depend on.

## Context

A snapshot was something a person published. Edit the registry, press build,
watch the data plane pick it up.

Nobody was making a decision at that moment. The registry says what should be
served; the backends say what they have. The correct catalog is a function of
those two things, and a function does not need a button. What the button
actually produced was a system with two states — *what is configured* and *what
is serving* — and an operator who had to remember they could differ.

The failure that follows is quiet. Somebody adds a binding, does not rebuild,
and the gateway keeps serving the old catalog indefinitely. Nothing is broken,
nothing alerts, and the registry screen shows the change that is not in effect.

ADR [0024](./0024-principals-leave-the-snapshot.md) already removed the worst
instance of this, where a grant change
needed a full discovery sweep to take effect. This finishes the job for the
part that genuinely does require discovery.

## Decision

**The control plane rebuilds the catalog on a timer. Publishing stops being a
step. The snapshot remains exactly what it was underneath.**

```
timer ──▶ discover backends ──▶ admit ──▶ sign ──▶ publish *if changed*
                                                        │
                                                        ▼
                                              data plane picks it up
```

### What does not change, and this is the point

The word "snapshot" leaves the UI. Nothing leaves the architecture:

- **Admission still happens at build time.** Collisions are still rejected
  there, never auto-renamed at runtime. Serving live backend output was the
  other way to read "just pull the latest", and it would have made a collision
  unresolvable at the only moment it could be noticed.
- **The gateway still serves admitted definitions, never live backend output.**
  A rebuild produces a signed artifact; the data plane verifies it as before.
- **Drift detection still works**, because there is still something to drift
  *from*. If live output were the definition, a backend quietly changing a tool
  description — a prompt-injection vector, not a cosmetic edit — would simply
  take effect with nothing to compare against.
- **An unreachable backend does not empty the catalog.** The rebuild fails, the
  previously published snapshot keeps serving, and the next tick tries again.
  That is ADR 0002 unchanged.

### Publishing only on change, and reporting freshness separately

Publishing an identical snapshot is not free: the data plane recomposes every
principal's view on a version change, so a timer that republished every minute
would churn that cache all day to deliver nothing. So a rebuild that produces
the serving catalog publishes nothing.

Which creates exactly the trap [ADR
0023](./0023-out-of-band-revocation.md) documented for revocations. If the only
number reported is one that moves on *change*, then a deployment where nothing
has changed and a deployment whose rebuild loop has died report the same thing.
That is worse than reporting nothing, because it looks like a control.

So there are two numbers, and they answer different questions:

| | answers | moves when |
|---|---|---|
| `snapshot_version` | what is serving | the catalog changes |
| `catalog_age_seconds` | is this still being checked | every rebuild, changed or not |

A growing `catalog_age_seconds` means the loop has stopped. That is the alert.
`catalog_error` carries the last failure, because a rebuild that has been
failing for an hour is invisible in a log nobody is tailing.

A dry run does not count as a rebuild. It resolved nothing and published
nothing, and counting it would let `--dry-run` on a timer hold the gauge green
over a catalog nobody has actually rebuilt.

### Sixty seconds

A rebuild is a discovery sweep of every backend behind the gateway, so the cost
scales with how many there are. Seconds would make a large deployment spend its
life discovering; an hour would make "I added a tool and nothing happened" the
normal experience. Sixty is the default and `rebuild_interval` is the knob.

`Rebuild now` still exists for the case where somebody has just changed
something and would rather not wait out the interval. It is a convenience, not
a step — the distinction being that skipping it costs you a minute rather than
leaving the gateway wrong.

## Alternatives considered

- **Serve live backend output; drop the snapshot entirely.** The simplest thing
  that could work, and it gives up admission, drift detection, signature
  verification, and last-known-good serving. Each of those is a rule in the
  brief. Rejected.
- **Rebuild on change instead of on a timer.** Correct for registry and tenancy
  edits, which the control plane observes. Useless for the case that actually
  motivates this — a *backend* adding a tool, which the control plane learns
  about only by asking. A timer covers both; watching changes as well would be
  an optimization on top, not a replacement.
- **Keep the publish button and warn when the catalog is stale.** Considered
  seriously, because it preserves an audit story where every publish has a
  person attached. Rejected: it keeps both states and adds a nag. The audit
  story is preserved anyway — every rebuild is signed and versioned, and the
  question "what was serving on Tuesday" still has an answer.
- **Republish unconditionally on every tick, like the revocation heartbeat.**
  That is what makes the revocation list's age meaningful, so it was the
  obvious symmetry. Rejected because the artifacts differ in cost: a revocation
  list is a set of ids, and a snapshot version bump invalidates every composed
  principal view in the data plane. Reporting freshness directly costs nothing
  and does not churn a cache.

## Consequences

- **Two numbers to understand instead of one.** `snapshot_version` and
  `catalog_age_seconds` mean different things, and somebody has to know which
  they are looking at. That is the price of being able to tell "quiet" from
  "dead", and it is worth it.
- **Backends are polled whether or not anything changed.** A deployment with
  many backends generates steady discovery traffic against its own upstreams.
  `rebuild_interval` is the knob; lowering it on a large deployment is how you
  build a thundering herd.
- **Admission failures need a surface that is not a build screen.** Nobody is
  watching a build any more, so "3 tools excluded, 1 collision" has to reach
  somebody through the catalog view and `catalog_error` rather than through the
  output of a command they ran. A silent exclusion is the one outcome this must
  not produce.
