# ADR 0010: Stable Catalog Ordering and Prompt-Cache Economics

## Status

Accepted. **Amended by [ADR 0016](./0016-toolsets-replace-audiences.md):**
the sort key is `(toolset priority, namespace prefix, tool name)` — `bundle`
became `toolset` — and ordering is now computed over each principal's *filtered*
set rather than over an audience's shared one. Filtering before sorting keeps
the cost proportional to what the principal can actually see; the determinism
requirement below is unchanged and now applies per principal.

## Context

`tools/list` returns an array, and array order looks like a presentation detail.
It is not. It is a cost control, and getting it wrong is expensive in a way that
does not show up in any functional test.

Model providers cache prompt *prefixes*. An agent's prompt is typically system
instructions, then the tool catalog, then the conversation. The catalog therefore
sits inside the cached prefix for every request that agent makes. If the catalog
bytes change, the cache entry is invalidated from that point on and every
subsequent request pays full input-token price and full prefill latency.

Now consider a gateway aggregating 20+ backends. If tool order is derived from
anything volatile — map iteration, insertion order, a registry timestamp, a
per-request filter pass — then publishing *any* backend reshuffles the catalog
for *every* audience that includes it. One team shipping a new tool becomes a
fleet-wide cache flush.

## Decision

**Total order on `(bundle_priority, namespace_prefix, tool_name)`. It is a
contract, and it is computed at snapshot build time.**

Every component of the key is fixed at admission or configuration time:

- `bundle_priority` — an operator-set integer on the bundle.
- `namespace_prefix` — assigned at namespace registration and immutable
  (changing it renames every tool in it, which is a different and much louder
  operation).
- `tool_name` — the backend's own name, fixed for the admitted definition.

Nothing in the key can change as a side effect of unrelated activity. The
consequences:

- **Adding a tool appends within its namespace partition.** Every
  already-present entry keeps its position, so the cached prefix up to the
  insertion point stays valid.
- **Adding a whole namespace appends a contiguous block** in prefix order,
  disturbing nothing before it.
- **A backend going down changes nothing** — which is a large part of why the
  grace window exists at all (ADR 0014).
- **Two `tools/list` calls return byte-identical arrays** for the same
  `(audience, snapshot, principal)`.

Ordering happens once per snapshot activation, in `snapshot.sortTools`, and the
serving path reads a pre-sorted slice. A per-request sort would be both wasteful
and an opportunity to sort differently.

Where a bundle contributes a tool another bundle already contributed, the
**first** bundle owns it: it appears once, at the first bundle's priority. Order
must not depend on how many bundles happen to mention a tool.

`TestAddingAToolDoesNotPerturbExistingOrder` and
`TestConformanceListToolsIsStableAcrossCalls` are the enforcement.

## Alternatives considered

- **Sort by tool name only, ignoring namespace.** Rejected: it interleaves
  backends, so adding one tool to one backend can shift entries belonging to
  five others. Namespace-major keeps each team's changes inside their own
  partition.
- **Preserve registry insertion order.** Rejected: it makes the order an artifact
  of history rather than of configuration, so it cannot be reproduced from the
  snapshot, cannot be reasoned about, and changes if anything is ever
  re-imported.
- **Order by usage frequency, most-used first.** Superficially attractive for
  model attention, and rejected firmly: the order would change continuously,
  which is the worst possible property. It would also create a feedback loop
  where popular tools become more popular because they are listed first.
- **Order by a hash of the tool digest** (stable, uniform). Rejected: stable but
  meaningless, so a human reading a catalog or a diff cannot find anything, and
  a re-admitted tool jumps position for no reason a user could explain.
- **Let the client sort.** Rejected: it does not help. The cache is invalidated
  by the bytes the gateway sent, whatever the client does afterwards.

## Consequences

- Bundle priority is a real operator lever with a cost attached: reordering
  bundles invalidates caches on purpose. That is fine — it is a deliberate,
  infrequent action — but the console should say so where the field is edited.
- A namespace prefix is immutable in practice. Renaming one is a rename of every
  tool in it: new digests, re-admission, and a cache flush for every audience
  that includes it. Treated as a migration, not a config edit.
- Identity filtering removes entries but never reorders them, so a filtered
  catalog is a subsequence of the unfiltered one. Two principals with different
  entitlements still share the longest common prefix, which preserves some cache
  value across them.
- Pagination inherits the order for free, so a cursor remains meaningful across
  requests within a snapshot version.
