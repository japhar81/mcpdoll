# ADR 0015: RBAC — Hierarchical Scopes, Two Engines, One Decision

## Status

**Accepted.**

Companion ADRs: [0014](./0014-tenancy-and-principals.md) (principals),
[0016](./0016-toolsets-replace-audiences.md) (what is being authorized),
[0020](./0020-pluggable-identity-and-authz.md) (replacing the engine).

## Context

With access decided per user rather than per URL, MCPDoll needs an
authorization model. It needs to express both halves of the requirement:

- **Platform administration.** Who may create a tenant, mint a signing key,
  publish a snapshot, read an audit trail.
- **Tool access.** Which tools a given user may see and call — down to a single
  tool, not just a whole toolset.

Those look like different problems and are not. Both are "does this principal
hold permission P within scope S", and RAGdoll already answers it with Casbin
over hierarchical domains. Adopting that model rather than inventing a second
one means an operator who knows one product knows the other, and it means the
tool-access half gets a decision procedure that has already been thought about.

## Decision

### Scope is a hierarchical string, and it is the Casbin domain

```
*                                   platform-wide
t/<tenant>                          one whole tenant
t/<tenant>/ts/<toolset>             one toolset within a tenant
t/<tenant>/ts/<toolset>/<tool>      one tool within a toolset
```

A grant at an ancestor scope covers every descendant request. `t/acme` admits a
request scoped `t/acme/ts/crm/lookup_customer`; `t/acme/ts/crm` does not admit
one scoped `t/globex/ts/crm`.

This is RAGdoll's scheme with its `e/<env>` and `p/<pipeline>` levels replaced
by `ts/<toolset>` and `<tool>`. The *mechanism* — `scopeCovers` registered as
Casbin's named domain-matching function — is identical, which is what lets the
conformance approach below be borrowed intact.

### One grant type, whether it is administration or tool access

A grant is `(user, role, scope)` — Casbin `g`. A role grants permissions —
Casbin `p`. Tool access is not a special case:

```
p, tenant_admin,   tenant:manage
p, tool_user,      tool:call
p, tool_user,      tool:list

g, alice, tenant_admin, t/acme                       # runs the tenant
g, alice, tool_user,    t/acme/ts/crm                # may use every CRM tool
g, bob,   tool_user,    t/acme/ts/crm/lookup_customer # may use exactly one
```

"Give Alice the CRM toolset" and "give Bob one tool" are the same operation at
different scopes. No second grant table, no second decision path, and the
question "why can Bob call this?" has one answer with one shape.

### `tool:list` and `tool:call` are separate permissions

Seeing a tool and invoking it are different privileges. A reviewer who should
know what exists without being able to fire it holds `tool:list` alone. More
importantly the *reverse* must be impossible to express by accident: a
principal holding `tool:call` without `tool:list` would have a tool it can
invoke but that never appears in its catalog — a hidden capability, which is
exactly the thing this system exists to prevent. Admission rejects that
combination rather than serving it.

### Two engines, pinned to identical decisions

`BuiltinEngine` is a dependency-free Go implementation of the same model.
`CasbinEngine` wraps the real policy engine. A conformance test drives both
with the same generated grant/permission/scope matrix and fails on any
disagreement.

This is RAGdoll's arrangement and the reason is the same there and here: the
builtin engine is what runs in tests and in a deployment that does not want the
dependency, and an engine that *nearly* agrees is worse than one engine,
because the disagreement surfaces as an authorization difference between test
and production. The conformance test is not a nicety; it is the thing that
makes having two engines defensible.

### Decisions are compiled, not evaluated per call

An engine's `Prepare(grants, catalog)` returns a synchronous decider. The
serving path calls the decider, never the engine.

This matters more in MCPDoll than in RAGdoll. RAGdoll enforces at ~129 API call
sites; MCPDoll must decide **per tool per catalog listing** — a principal with
2,000 admitted tools is 2,000 decisions for one `tools/list`. Compiling the
grants once per principal turns that from a policy evaluation into a map
lookup, which is what makes ADR 0018's per-principal precomputation affordable.

## Alternatives considered

- **A bespoke allow/deny list per user.** Rejected. It is what everyone builds
  first and it cannot express "every tool in this toolset" without either
  enumerating at grant time — so a new tool is silently ungranted — or
  inventing a wildcard, at which point the scope hierarchy has been rebuilt
  badly.
- **Casbin only, no builtin engine.** Tempting, and rejected for the reason
  RAGdoll rejected it: the test suite should not need the dependency, and a
  deployment that cannot take it should still be able to authorize. The cost is
  the conformance test, which is bounded.
- **Builtin only, no Casbin.** Rejected: operators asked for Casbin by name,
  and a policy language they can read and extend is worth more than one fewer
  dependency.
- **OPA / Rego as the only engine.** Rejected as a *default* — it is a large
  operational dependency for a decision this shaped — but it is precisely what
  ADR 0020's pluggable engine exists to accommodate.
- **Permissions on the tool rather than scopes on the grant** (each tool
  declaring who may use it). Rejected: it puts access control in the registry
  document, so granting one person one tool becomes a config commit and a
  publish rather than an admin action.

## Consequences

- **The permission set is a contract.** Adding one is a schema change, a seed
  migration, and a UI change. That friction is deliberate — a permission set
  that grows casually stops being reviewable.
- **A catalog listing is O(tools) decisions**, mitigated by compilation above
  and by precomputation in ADR 0018. A principal with a very large admitted set
  is the case to watch.
- **Scope strings are parsed in the hot path.** `scopeCovers` must be
  allocation-free; it is a prefix comparison with a separator check, and it has
  a benchmark to keep it that way.
- **Two engines mean two implementations of one idea.** The conformance test is
  load-bearing. If it is ever weakened to make a change land, the second engine
  should be deleted instead.
