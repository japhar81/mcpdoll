# ADR 0019: One MCP Endpoint; Tenant and Toolset Come From the Credential

## Status

**Accepted.** Supersedes the `/mcp/{audience}` routing. Amends
[0012](./0012-mrtr-requeststate-wrapping.md): the MRTR envelope binds a
principal and tenant rather than an audience slug.

## Context

Audiences were URLs. With the catalog derived from a principal's grants (ADR
0016), the URL has nothing left to select — every principal reaching
`/mcp/support-agents` would get a different catalog anyway, and the slug would
be a label that no longer determined anything.

## Decision

**One endpoint: `/mcp`. The tenant and the principal come from the credential.**

```
POST /mcp
Authorization: Bearer <api key>

  key prefix ─> api_key ─> user ─> tenant
                       └─> effective grants (key ∩ owner)   ADR 0014
                                    ↓
                            PrincipalView                   ADR 0018
```

An agent's configuration is one URL and one credential, for every tenant.

### The cost, stated plainly

A misconfigured client — right URL, wrong credential — gets a **smaller
toolset rather than an error**. The per-tenant URL form (`/mcp/{tenant}`) would
have caught that class: URL asserts a tenant, credential asserts a tenant, a
mismatch is a loud 403.

Two things reduce the exposure, and neither eliminates it:

- **The failure is never a cross-tenant leak.** A credential resolves to
  exactly one tenant, so a wrong credential yields *that* credential's tenant.
  The risk is an agent silently doing less than intended, not one reading
  another tenant's data.
- **`server/discover` and the initialize result name the resolved principal and
  tenant**, so a client that checks can tell immediately which identity it is
  operating as, and the console's inspector shows it directly.

This is a real ergonomic loss accepted for a real ergonomic gain. It is recorded
here rather than discovered later, and `/mcp/{tenant}` remains available as a
strictly additive future change if silent under-provisioning proves to be the
more expensive failure.

### An unauthenticated request is refused, not defaulted

There is no anonymous principal and no default tenant. Without a resolvable
credential the connection is refused at initialize. The previous model's
"default subject for development" is removed: it was harmless when the URL
selected the catalog and would now be a way to get a catalog without proving
who you are.

### The MRTR envelope binds tenant and principal

ADR 0012's `requestState` bound `(tool, principal, audience, argument digest)`.
With audiences gone it binds `(tool, principal, tenant, argument digest)`.

The tenant is not redundant with the principal. Principal ids are unique, but
binding the tenant explicitly means a confirmation cannot be replayed across a
tenant boundary even if a principal id were ever reissued or a user moved — and
it makes the envelope self-describing when read in an audit trail.

## Alternatives considered

- **`/mcp/{tenant}`, principal from the credential.** The safer option, and the
  one to revisit if the silent-under-provisioning failure above proves
  expensive. Rejected here for client simplicity: one URL for every agent in
  every tenant.
- **Both forms.** Rejected: two code paths to test and two ways an audit trail
  describes the same request, for a convenience one of the two already provides.
- **Tenant from a header** (`X-MCPDoll-Tenant`). Rejected: it is `/mcp/{tenant}`
  with worse discoverability and no standard behaviour on mismatch.
- **Keeping `/mcp/{audience}` as a compatibility alias.** Rejected: an audience
  no longer determines a catalog, so the alias would accept a slug and ignore
  it — the most confusing possible behaviour.

## Consequences

- **Every existing client reconfigures.** `/mcp/support-agents` → `/mcp` plus a
  credential. There is no compatibility shim, and the removal is a breaking
  change that belongs in release notes rather than in a footnote.
- **The data plane can no longer build one MCP server per audience at swap
  time.** It builds per principal, lazily, cached by snapshot version (ADR
  0018). The `buildServer` panic guard from the audience path stays: a malformed
  admitted tool now fails one principal's connection rather than refusing the
  whole snapshot, which is a *weaker* protection than before and is why
  admission-time validation of every tool becomes more important, not less.
- **Readiness reports differently.** "Serving N audiences" is meaningless; the
  data plane reports tenants and admitted tool counts instead.
- **The gateway inspector must take a credential**, not an audience slug and a
  subject. Impersonating a principal for inspection becomes an explicit,
  audited control-plane operation rather than a header an operator sets.
