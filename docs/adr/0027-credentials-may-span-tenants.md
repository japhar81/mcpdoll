# ADR 0027: A Credential May Span Tenants, and Then Its Tool Names Are Qualified

## Status

**Accepted.** Relaxes the one-session-one-tenant rule from
[0019](./0019-single-mcp-endpoint.md) into a default rather than an absolute.
Builds on [0014, as amended](./0014-tenancy-and-principals.md) — the tenant is
a property of the credential, which is exactly what makes this expressible.

## Context

One MCP session resolved to exactly one tenant. The reason was real: with a
single endpoint for everyone, two tenants' tools in one catalog collide on name,
and `crm.lookup_customer` would be ambiguous.

But "resolve to one tenant" solved that by forbidding the case rather than
naming it, and the case is legitimate. Asking a model to compare what the same
tool returns in test and in live is one question. Making somebody hold two
sessions to ask it is the tool getting in the way of the work.

## Decision

**A key may be minted as *spanning*. Its catalog covers every tenant its grants
reach, and every tool in it is named `<tenant>.<prefix>.<tool>`.**

```
ordinary key   ->  tenant_id = acme   ->  crm.lookup_customer
spanning key   ->  tenant_id = NULL   ->  acme.crm.lookup_customer
                   grants decide           globex.crm.lookup_customer
```

### The grants decide which tenants, and nothing else does

A spanning key carries no tenant list. The tenants it reaches are the tenants
its grants name — so narrowing it to test and live is `--grant tool_user@t/crm-test
--grant tool_user@t/crm-live`, using the mechanism that already exists.

A second list would be a second source of truth about which tenants a key
reaches, and the two would eventually disagree. Worse, they would disagree
*silently*: the list would say one thing and authorization another, and the
catalog would follow whichever the code happened to consult.

Spanning therefore widens **naming**, never access. A spanning key sees exactly
the tools its grants already allowed; it can just now address them.

### Every tool is qualified, not only the ambiguous ones

Qualifying on collision was the tempting version — shorter names, only
disambiguating where needed. It is wrong, and the reason is worth stating: a
tool's name would then depend on *what else is in the catalog*.

Grant somebody a second tenant and tools in the first would silently rename.
Every agent prompt naming `crm.lookup_customer` would break, at a moment
unrelated to the tool, for a reason invisible from the tool itself. A name has
to be a property of the thing named.

So a spanning key with access to exactly one tenant still sees qualified names.
There is a test for that specifically.

### Opt-in, because the isolation it gives up is structural

An ordinary key cannot address another tenant *at all*. Not "is denied" —
cannot express it. That is a stronger property than any filter, and it is what
the default keeps.

A spanning key turns that structural boundary into a filtered one: correctness
now depends on the grant intersection being right on every request rather than
on the tenant being unreachable. That is the same ownership-versus-filter
trade ADR 0014 made about users, and it lands differently here, because a bug
in a filter over tenants is a cross-tenant read.

Which is why it is per key, off by default, and why minting one takes
`key:manage` at the global scope rather than in any tenant.

### The tenant of a *call* comes from the tool

An ordinary session could read its tenant off the session. A spanning one has
none, so every place that needed "the tenant" now takes it from the tool being
called — telemetry, logs, the plugin payloads that policy is written against,
and the backend binding that decides which deployment is reached.

This is more correct for ordinary sessions too, and it was worth changing
there as well rather than keeping two paths: the tenant a call acts in has
always been a property of the tool, and reading it from the session was a
shortcut that happened to agree.

### What a spanning credential is told

The instructions block says the names are qualified and that the same tool may
appear twice under different names. Without it a model sees `acme.crm.lookup`
and reads `acme` as a namespace — it would have no way to know two tenants are
in play, which is the entire point of the credential.

## Alternatives considered

- **Always qualify, for everyone.** One naming rule, no mode. Rejected: it
  lengthens every tool name in every catalog for the benefit of a minority of
  credentials, and tool names are tokens in a context window on every call.
- **Let a spanning key carry an explicit tenant list.** More direct to read, and
  a second source of truth about reachability alongside the grants. Rejected
  above.
- **A separate endpoint per tenant, and let clients hold several sessions.**
  What the constraint implied. Rejected as ADR 0019 already did — it is the
  fan-out problem that ADR back out of, and it does not put the two answers in
  one context anyway, which is the actual ask.
- **Qualify only on collision.** Rejected above; names must not depend on the
  catalog around them.
- **Make it a property of the user rather than the key.** Then every one of a
  person's credentials would span, including ones minted for a narrow agent.
  The credential is the right granularity for the same reason the tenant is.

## Consequences

- **Two naming schemes exist**, and which one a client sees depends on its
  credential. Documented in the instructions block, but a person reading two
  catalogs side by side has to know why they differ.
- **Tenant isolation for spanning keys is a filter, not a boundary.** The
  grant intersection is what holds, and it is now load-bearing for
  cross-tenant separation rather than only for tool selection.
- **A spanning key is managed globally**, so a tenant admin cannot revoke one
  that reaches their tenant. The alternative — scoping it to any one of its
  tenants — is worse: it would let an admin of the least sensitive tenant a key
  touches revoke its access to all the others.
- **Catalogs get larger.** A spanning credential with broad grants sees every
  tenant's tools at once, and that is a real context-window cost the operator
  chooses by minting one.
