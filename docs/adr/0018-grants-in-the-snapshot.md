# ADR 0018: Grants Travel in the Signed Snapshot

## Status

**Accepted.** Upholds [0002](./0002-control-data-plane-split.md) by choosing
the option that preserves it.

## Context

The catalog is now the principal's grants, evaluated (ADR 0016). So the data
plane needs the grants. There are three places they could come from, and the
choice decides whether ADR 0002's central property survives:

> The data plane has no dependency on the control plane at request time. A
> control-plane outage is invisible to clients.

The tension is revocation latency. An admin revokes Alice's access to a
destructive tool; how soon must her next call fail?

## Decision

**Grants are compiled into the signed snapshot. A grant change triggers a
republish. Revocation takes effect at snapshot latency — a few seconds.**

```
admin revokes  ->  control plane rebuilds + signs  ->  data plane swaps
                            ~2-5 seconds
```

### Why this and not a live check

A per-request authorization call to the control plane would make revocation
immediate and would reverse ADR 0002: a control-plane outage would stop tool
calls. That trade is not worth a few seconds, and it would undo the property
the whole architecture is built to provide.

### Why this and not a fast revocation side channel

A live "revoke now" channel alongside the snapshot is defensible and was
rejected on a narrower ground: it produces denials that are **not in the signed
artifact**. Today "why was this denied?" is answered by reading one signed
snapshot. With a side channel there are two possible answers and an operator has
to know which path applied. One auditable source of truth is worth more than the
seconds, and if sub-second revocation is ever genuinely required it should
arrive as an explicit, audited second mechanism rather than as an optimization.

### What a snapshot carries per principal

Not a fully-expanded catalog per user. The snapshot carries:

- the **compiled grant set** per principal (role → permission catalog, and the
  principal's `(role, scope)` edges, already intersected for API keys), and
- the **admitted tools** per `(toolset, tenant)`.

The data plane composes them at connect time into a `PrincipalView`, and caches
it keyed by `(snapshot version, principal id)`. Composition is a scope-prefix
match per tool against a compiled decider (ADR 0015), which is a map lookup and
a string compare.

Precomputing the fully-expanded catalog for every principal was offered and is
not needed: for 1,000 principals × 2,000 tools it is two million tool references
in a signed artifact that must be verified on every swap, to save work that the
first connection does in under a millisecond and every later one skips entirely.
Carrying the grants and composing on demand is smaller to sign, faster to
verify, and — because the cache is keyed by snapshot version — self-invalidating
on publish.

### The cache is keyed by snapshot version

A `PrincipalView` is valid for exactly one snapshot. On swap, the whole cache is
dropped rather than invalidated entry by entry. A stale view surviving a swap
would serve revoked access, and per-entry invalidation is a chance to miss one.

## Alternatives considered

- **Live evaluation against the control plane.** Rejected above: reverses ADR
  0002.
- **A fast revocation channel alongside the snapshot.** Rejected above: two
  sources of truth for one question.
- **Fully-expanded per-principal catalogs in the snapshot.** Rejected on size
  and verification cost, not on principle. The offer to "precompute a thousand
  audience views" is taken up — the views are computed, just at the data plane
  and lazily, which is strictly cheaper for the same result.
- **Grants in the snapshot, but unsigned** (a sidecar). Rejected: grants are
  precisely the part an attacker would want to modify, and signing the tools
  while leaving the access rules loose would be signing the wrong half.

## Consequences

- **Revocation is not instant, and this must be documented where an admin can
  see it** — the console's grant UI says so at the moment of revoking, rather
  than in a document nobody opens.
- **Grant changes republish.** With many small changes that is a lot of
  snapshots. The build must therefore be cheap when nothing about backends
  changed: a grants-only rebuild reuses the last discovery pass rather than
  re-probing every tenant's backends, or an admin toggling ten grants triggers
  ten full discovery sweeps.
- **Snapshot version becomes high-churn.** It is already a monotonic counter and
  a Unix timestamp works; but the data plane logs every swap, and a busy admin
  session should not flood the log. Swap logging is aggregated.
- **A principal with no grants gets an empty catalog, not an error.** That is
  the correct behaviour for `open_no_access` signup (ADR 0014) — the user exists
  and holds nothing. The console must distinguish "no grants" from "not
  provisioned" or the state looks like a bug.
