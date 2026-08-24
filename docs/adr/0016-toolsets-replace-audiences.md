# ADR 0016: Toolsets Replace Audiences; a Principal Is Its Own Audience

## Status

**Accepted.** Supersedes the `audiences` concept entirely. Amends
[0010](./0010-stable-catalog-ordering.md) (ordering is now per principal),
[0011](./0011-cachescope-correctness.md) (every catalog is now private),
and [0012](./0012-mrtr-requeststate-wrapping.md) (the envelope binds a
principal, not an audience slug).

## Context

An **audience** was a named, declared bundle of namespaces published at its own
URL: `/mcp/support-agents`. Access control was the URL plus an optional
`allowed_idp_groups` gate on connecting.

That model has a fixed granularity: everyone who reaches an audience sees
exactly the same catalog. "Give Alice one extra tool" has no expression in it
short of minting an audience for Alice — and then another for Bob, and the
declared-audience list becomes a per-user list with extra steps.

Meanwhile the pieces needed to do it properly already existed and were being
used for something narrower: the `on_catalog` hook filters a catalog per
principal, and the `entitlements` plugin already matched tool prefixes against
groups. The machinery for per-principal toolsets was there; it was applied
*within* an audience rather than *instead of* one.

## Decision

### A toolset is a named, grantable group of tools

It replaces `bundle`. The rename is not cosmetic — a bundle was a grouping of
*namespaces* for publication, whereas a toolset is **the unit an admin grants**.
That is its whole purpose, and the name should say so.

```yaml
toolsets:
  - id: ts_crm
    name: crm
    namespaces: [ns_crm]          # everything in these namespaces
    tools: [hr.lookup_employee]   # plus these named ones
```

A toolset draws from whole namespaces, individually named tools, or both. The
named-tool form is what makes a toolset a curated set rather than a mirror of
the namespace layout.

### `namespace` survives; `audience` does not

A namespace is still the tool-name prefix and the ownership boundary — `crm.*`
belongs to the CRM team. It was never an access-control concept and does not
become one.

An audience is gone. What replaces it is not another declared entity: it is the
principal, resolved at connect time.

### The catalog is the principal's grants, evaluated

```
principal ─> effective grants (ADR 0014: key ∩ owner)
          ─> for each admitted tool in the principal's tenant:
               scopeCovers(grant.scope, "t/<tenant>/ts/<toolset>/<tool>")
                 AND role grants tool:list
          ─> catalog
```

Two principals in the same tenant with different grants get different catalogs
from the same URL. That is the point, and it is why ADR 0019 can collapse the
per-audience URLs into one.

### Ordering is per principal, and still deterministic

ADR 0010 sorted by `(bundle priority, namespace prefix, tool name)` so a
catalog is byte-stable across instances and republishes — which is what lets a
client's prompt cache survive. That property must hold per principal now:
sorting is by `(toolset priority, namespace prefix, tool name)` over the
principal's *filtered* set, so two instances serving the same principal produce
identical bytes.

Filtering before sorting rather than after is what makes this true. Sorting the
full admitted set and then filtering would also be deterministic, but it would
be O(all tools) per listing instead of O(granted tools).

### Every catalog is `private`

ADR 0011 established that a catalog filtered for a principal must never be
advertised `public`, and made `cacheScopeFor` the single expression that could
return `"public"` — reachable only when nothing identity-specific applied.

Under this ADR nothing identity-specific *never* applies: the catalog is
defined by the principal's grants. `cacheScopeFor` therefore returns `"private"`
unconditionally, and the function is kept — rather than inlining the constant —
so that the invariant has a name, a test, and a place for a future public case
to be argued for rather than slipped in.

This is a real, permanent cost. Shared caching of catalogs is given up in
exchange for per-user access control. ADR 0011's `ttlMs` still applies, so a
client caches its *own* catalog for the advertised window.

## Alternatives considered

- **Keep audiences, add per-user overlays.** Rejected: two access-control
  mechanisms that must agree, and the question "why can Alice see this?" gets
  two possible answers. Worse, an overlay that *removes* a tool an audience
  grants is a deny rule, and deny rules interacting with grants is the
  complexity this design is trying not to have.
- **Audiences as saved grant templates.** This is genuinely appealing —
  "support-agents" as a named set of grants an admin applies to a user — and it
  is *not* rejected, it is deferred. It is sugar over `rbac_grants` and can be
  added without changing anything here. Recorded in `docs/deferred.md` so it is
  not reinvented as a core concept.
- **Toolsets as the only granularity** (no single-tool grants). Rejected: the
  requirement is explicitly "one or more tools or toolsets", and a model that
  cannot grant one tool forces an admin to mint a one-tool toolset — which is a
  single-tool grant with worse ergonomics and a polluted toolset list.
- **Leaving `cacheScope` sometimes public** for principals whose grants happen
  to be tenant-wide. Rejected. It is correct in the narrow case and one grant
  change away from being a cross-tenant leak, and the condition that made it
  safe is invisible at the point where the leak would occur.

## Consequences

- **`AudienceView` becomes `PrincipalView`**, and there are as many as there are
  principals rather than as there are declared audiences. ADR 0018 covers what
  that costs and where it is computed.
- **The `entitlements` plugin's job is now core.** Filtering a catalog by
  identity is no longer a plugin concern; the engine does it. The plugin remains
  useful for policy the grant model deliberately cannot express — time of day,
  request rate, an external risk signal — and its `identity_dependent` flag
  loses its effect on cache scope, because everything is private now.
- **No shared catalog caching, ever.** Stated here so it is not rediscovered as
  a performance surprise.
- **Existing registries break.** `audiences:` and `bundles:` no longer parse.
  A migration path is required and is part of the work, not an afterthought.
