# ADR 0024: Principals Leave the Snapshot

## Status

**Accepted.** Supersedes the distribution half of
[0018](./0018-grants-in-the-snapshot.md). The authorization model it describes
is unchanged; where the data comes from is not.

## Context

ADR 0018 put grants in the signed snapshot, because ADR 0002 says the data plane
must not call the control plane at request time and the grants had to reach it
somehow. That reasoning is still right. The *packaging* was wrong.

Two things with completely different change rates ended up in one artifact:

| | changes when | costs |
|---|---|---|
| Serving configuration | an operator publishes | a discovery sweep of every backend |
| Principals — users, keys, grants | somebody is hired, a key is minted, access is granted | nothing |

Binding the second to the first means minting an API key requires re-probing
every tenant's backend. The symptoms were visible and I treated each as its own
wrinkle rather than as one cause:

- A freshly minted key returns 403 until somebody publishes.
- `mcpdoll inspector` had to build a snapshot and poll before it could launch.
- A tenant admin cannot make their own key work, because publishing is
  global-scope and their role is not.
- Toggling ten grants is ten discovery sweeps — which ADR 0018 itself listed as
  a consequence and proposed to fix by caching discovery, treating the symptom.

And it is wrong on its own terms. A grant names a **scope**, not a list of
tools. `t/acme/ts/support` means *whatever that toolset admits*, whenever it is
asked. The whole point of a hierarchical scope is that it survives the catalog
changing underneath it. Freezing the principal into the same artifact as the
catalog throws that away: it makes a stable statement about authority depend on
the publish cycle of something it was designed to be independent of.

> If you hold `crm.*`, you hold it against whatever `crm.*` currently is. That
> is what the scope says.

## Decision

**The snapshot carries serving configuration. A second signed artifact carries
principals. Each is published on its own clock.**

```
registry + backends ──► snapshot.pb      published by an operator, discovery
database (users…)  ──► principals.pb     published on every change, no discovery
database (revoked) ──► revocations.pb    published on every change (ADR 0023)
```

The data plane holds all three and composes a `PrincipalView` from them at
connect time. Nothing about the authorization model changes: the same grants,
the same scopes, the same compiled decider, the same conformance test.

### Why this and not caching discovery

ADR 0018's proposed fix was to skip discovery when the registry digest is
unchanged. That would have made a grant change fast while leaving it coupled —
a principal would still ride in an artifact whose version number means "the
catalog changed", and every consumer reasoning about that version would be
reasoning about the wrong thing. Making a wrong coupling cheap is not the same
as removing it.

### Why not put grants in the credential

A self-describing token — grants as signed claims — removes the artifact
entirely. Rejected: the grants would be frozen at mint time, so revoking access
would not reach a credential already issued until it expired. The whole reason
grants are read fresh at every resolution (ADR 0014) is that they change and
must take effect.

### Composition, and what invalidates it

A `PrincipalView` is now a function of two versions, so it is cached under
`(snapshot version, principal-set version, principal id)` and both swaps drop
it. Keying on one would serve a stale catalog after the other moved — which is
the bug this ADR is written to avoid, reintroduced one layer down.

### Revocation stays separate

It could be folded in — a key absent from the principal set is refused, so
removal is revocation. It is kept because it is a *subtractive* channel that
carries no grants (ADR 0023), and because two independent paths to "stop this
credential" is the right number for the one operation that must not fail
quietly. A stale principal set still contains the revoked key; the revocation
list does not depend on that set being fresh.

## Consequences

- **Minting a key works in about a second**, with no snapshot build. So does
  granting, revoking, disabling, and creating a user.
- **A tenant admin can do their whole job** without `snapshot:build`, which they
  should never have needed for it.
- **`mcpdoll inspector` stops publishing a snapshot** to make its own credential
  work.
- **Three artifacts to distribute rather than two.** They share a key and a
  transport, and each has a version and an age on the gateway status — but an
  operator now has three things that can be stale rather than two, and the
  console has to show all three.
- **The snapshot's version stops being high-churn.** ADR 0018 predicted a
  timestamp-per-grant-change and aggregated swap logging to cope. That pressure
  moves to the principal set, where a swap is cheap and carries no tools.
- **ADR 0018's title is now misleading.** Its reasoning about *why the data
  plane cannot ask at request time* stands and is load-bearing; only its answer
  to *which file* is superseded.
