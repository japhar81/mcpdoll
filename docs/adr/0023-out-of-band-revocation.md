# ADR 0023: Revocation Travels Out of Band, Signed, and Only Subtracts

## Status

**Accepted.** Builds the mechanism [0018](./0018-grants-in-the-snapshot.md)
named and deferred, and closes the exposure
[0021](./0021-offline-credential-verification.md) recorded.

## Context

Everything in this system takes effect at snapshot latency. ADR 0018 argued that
is right for grants — a few seconds is worth keeping a control-plane outage
invisible to a tool call — and named the one case where it is not:

> A leaked credential is different: the reason somebody revokes a key is that it
> is being used *right now* by someone who should not have it, and "a few
> seconds" is the wrong answer to that.

Today `revokeAPIKey` writes to the database and the key keeps working until a
snapshot is published. Seconds if somebody publishes; forever if nobody does.
The same is true of disabling a user, which is the offboarding case and the more
common one.

ADR 0018 also said what the fix must not be:

> A live "revoke now" channel alongside the snapshot… produces denials that are
> **not in the signed artifact**. Today "why was this denied?" is answered by
> reading one signed snapshot. With a side channel there are two possible
> answers and an operator has to know which path applied.

That objection is to an *unsigned, unauditable* channel. It is answerable.

## Decision

**A signed revocation list, distributed the way the snapshot is, that can only
remove principals and never add them. Two signed artifacts, both auditable, and
one rule for reading them: the snapshot says who may do what, the list says who
may not do anything.**

```
revoke  ->  row written  ->  list rebuilt + signed  ->  data plane applies
                                    ~immediately
```

### Only subtraction, and that is the whole design

The list is a set of principal ids. A principal in it is refused, whatever the
snapshot says. It carries no grants, no scopes, and no roles — there is nothing
in it that could *widen* anything.

That is what answers ADR 0018's objection. "Why was this denied?" still has one
answer per direction: an *allowed* action is explained by the snapshot alone,
and a *denied* one by the snapshot or by a revocation list that names the
principal. Neither artifact can contradict the other into a permission, because
only one of them can produce one.

### Failing open is the correct failure, and it is the residual risk

If the list cannot be loaded, the last one successfully loaded stays in effect
and the data plane keeps serving. Failing closed would mean a control-plane
outage stops tool calls, which reverses ADR 0002 — the property the entire
architecture exists to provide.

So the exposure is not eliminated, it is **bounded and measurable**: it equals
the age of the last list the data plane loaded — the window in which a
revocation issued now would not yet be enforced. That age is a gauge
(`mcpdoll.revocations.age`), it is on the gateway status every surface reports,
and it is the thing to alert on.

### The list is republished on a timer, and that is what makes the age mean
something

Publishing only when something is revoked was the first design, and it does not
work. A deployment that revoked something last Tuesday would report an
eight-day-old list, and an operator could not tell that from a data plane that
had stopped receiving the file — the number would look identical in both cases.
It was measurable and not actionable, which is a worse failure than not
measuring it, because it looks like a control.

So the control plane republishes every thirty seconds whether or not anything
changed, bumping the version each time. In a working deployment the age stays
under a minute; a *growing* age means distribution has broken, which is the only
failure this artifact has. The alert is `age > 5 × heartbeat`, and it fires on
the thing that actually matters.

The version doubles as the heartbeat counter. It has no meaning beyond
monotonicity — the data plane refuses anything not newer — so reusing it costs
nothing and avoids a second freshness field with its own comparison rules.

### Pruning, so the list does not grow forever

A revocation is redundant once a snapshot built after it is serving, because
that snapshot already omits the credential. So a build prunes every revocation
committed before the build read the database, and the list records
`pruned_through_snapshot_version`.

A data plane serving a snapshot **older** than that would lose denials it still
needs, so it refuses the list and keeps its previous one. Refusing keeps
strictly more denials than accepting, which is the safe direction, and it
self-corrects the moment the newer snapshot lands.

### Its own signing context

`mcpdoll.revocations.v1\x00`, not the snapshot's. The domain separation prefix
already exists for exactly this: without a distinct context, a signature minted
over one artifact would verify against the other, and an attacker who could get
any snapshot signed could present its bytes as a revocation list.

### Disabling a user revokes them, not just their keys

`updateUser status=disabled` writes revocations for the user and every key they
own. Revoking the keys alone would leave the offboarding incomplete in the way
that is hardest to notice — the person is gone from the console and their
automation is still running.

## Alternatives considered

- **Push, over the snapshot's gRPC channel.** Lower latency than polling a file,
  and it makes the data plane's correctness depend on a live connection to the
  control plane. Rejected on ADR 0002. The file-plus-watch path is the same one
  the snapshot uses, and reusing it means one distribution story to operate.
- **Short-lived credentials instead of revocation.** Genuinely the better answer
  in the long run, and orthogonal: it shrinks the window rather than closing it,
  and an agent credential that expires every five minutes needs a refresh path
  this system does not have. Worth revisiting; not a reason to leave revocation
  unbuilt.
- **A bloom filter, to keep the list small.** Rejected: false positives are
  denials of legitimate principals, and "this agent stopped working and nobody
  can say why" is a worse failure than a list a few hundred kilobytes larger.
- **Putting revocations in the snapshot and publishing more often.** That is the
  status quo with a shorter timer. It still means a full discovery sweep per
  revocation, and it still cannot beat the build's own latency.

## Consequences

- **A second artifact to distribute, sign, and operate.** Its key is the
  snapshot's, so no new trust anchor — but a deployment that copies snapshots
  around now has two files to copy, and a data plane with a stale list is a
  state somebody has to be able to see. Hence the gauge and the status field.
- **The exposure window is now the list's age, not "until somebody publishes".**
  Smaller, bounded, and visible. Not zero.
- **A revocation is not a delete.** The row stays, and the list is derived from
  it, so "when did this stop working and who stopped it" survives.
