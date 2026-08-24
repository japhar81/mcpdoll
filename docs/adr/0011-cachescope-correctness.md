# ADR 0011: cacheScope Correctness

## Status

Accepted. **Amended by [ADR 0016](./0016-toolsets-replace-audiences.md):
`cacheScopeFor` now returns `private` unconditionally.** Every catalog is
derived from a principal's grants, so the condition that once permitted `public`
— nothing identity-specific applied — is never true. The function is kept rather
than inlined so the invariant retains a name and a test, and so any future
public case has to be argued for in one place. Everything below about *why*
`public` on a filtered catalog is a cross-tenant leak still stands; there is now
simply no filtered-or-not distinction left to get wrong.

## Context

A 2026-07-28 list result carries `cacheScope`, analogous to HTTP
`Cache-Control`: `public` means any client or intermediary may cache and serve
the response; `private` means only the requesting user's client may cache it.
Absent, it defaults to `public`.

For a gateway that filters catalogs per principal, that default is a
confidentiality bug waiting to happen. If an entitlement filter removes the
destructive billing tools for everyone outside `billing-admins`, and the result
is marked `public`, then any shared cache — a proxy, an agent-framework cache, a
CDN — may serve the *admin's* catalog to an intern. The intern now knows those
tools exist, and depending on the framework, may be able to name them in a call.

The SDK will not save us here: it sets `cacheScope: "public"` and leaves `ttlMs`
at zero unless the server says otherwise. Doing nothing is not neutral.

## Decision

**`private` whenever the catalog can differ between principals. Decided at
snapshot build time, in exactly one place.**

### One expression can produce "public"

`edge.cacheScopeFor(identityFiltered bool)` is the only function in the codebase
that returns `CacheScopePublic`, and it is guarded by the filtering flag. That
makes the invariant auditable by reading one function rather than by reasoning
about every code path that constructs a list result.

### The flag is computed conservatively, at build time

`AudienceView.IdentityFiltered` is set during snapshot indexing when any of:

- a policy rule is marked `identity_specific`;
- a policy rule can `HIDE` or `DENY` *and* is conditioned on IdP groups —
  because two principals can then legitimately receive different lists, whether
  or not the author remembered to set the flag;
- an `identity_dependent` plugin runs at `ON_CATALOG`.

Deciding this at build time rather than per request is deliberate: it means the
answer cannot depend on whether a particular request happened to be filtered.
A view that *can* differ between principals is private even for a principal
whose view happens to be complete. The alternative — marking it public when
nothing was actually removed — leaks entitlement information through the cache
header itself.

### Runtime can only tighten it

If an `ON_CATALOG` plugin reports that it filtered, the result is private even
if the snapshot thought otherwise. Nothing can loosen it.

### `ttlMs` is the merged minimum

The org default, every contributing bundle's override, and every applicable
policy's cap. A bundle or policy may only *narrow* the TTL, never widen it —
otherwise a permissive bundle could override the org-wide ceiling.

## Alternatives considered

- **Always `private`.** Safe, and rejected because it throws away real value: an
  unfiltered platform-wide catalog is identical for every principal, and letting
  intermediaries cache it is exactly what the field is for. Always-private also
  trains operators to ignore the field.
- **Decide per request, based on whether anything was actually removed.**
  Rejected: `public`/`private` would then vary between principals for the same
  audience, which leaks entitlement information through the header, and makes the
  correctness argument depend on the specific request rather than on the
  configuration.
- **Set `private` and rely on `ttlMs: 0` to prevent caching.** Rejected as
  belt-with-no-trousers: it conflates two independent controls, and gives up
  client-side caching entirely to compensate for getting the scope wrong.
- **Trust plugin manifests' `identity_dependent` flag alone.** Rejected: a
  manifest is authored by the plugin's publisher and can be wrong or stale. The
  group-conditioned-policy inference exists precisely so a mislabelled plugin or
  an unflagged policy rule cannot produce a shareable filtered catalog.

## Consequences

- Enabling an entitlement-filtering plugin on an audience makes that audience's
  catalog uncacheable by intermediaries. That is the correct trade and it is a
  visible cost, so the console should surface it where the plugin is enabled.
- The conservative inference will sometimes mark a catalog private that happens
  to be identical for everyone — for instance a group-conditioned rule whose
  group contains every principal. Over-caution here costs cache hits; the
  opposite costs confidentiality.
- Two tests hold the line: `TestFilteredCatalogIsNeverPublic` at the snapshot
  level (the flag is computed) and `TestFilteredCatalogIsNeverPublicOverTheWire`
  end to end (it survives to the wire). Both are named so a future reader knows
  they are load-bearing rather than incidental.
