# ADR 0022: The Control Plane Authenticates People and Enforces Its Own RBAC

## Status

**Accepted.** Corrects a claim recorded in `docs/deferred.md` and in ADR 0020's
consequences.

## Context

`internal/platform/authz` has a role model, a closed permission set, and two
engines pinned to identical decisions. `internal/controlplane/store` has users,
grants, and `VerifyPassword`. The data plane compiles those grants into a
`Decider` and enforces them on every catalog and every call.

The control plane does none of it. `apiserver.authenticate` compares a bearer
token against one value from configuration, and every operation past that point
runs unchecked. So an operator who can mint a signing key and one who can only
read the registry are the same principal, and separation of duties — which the
permission set was built to express — is expressible and not enforced.

This was recorded as blocked on an identity provider. **That was wrong**, and
stating why matters more than the fix:

> A local password is a principal. `VerifyPassword` returns a `User`, a user has
> grants, and grants compile to a `Decider`. Nothing in that chain needs OIDC.

OIDC is a second *source* of identity, not a prerequisite for having one. The
mistake was treating "the enterprise-grade way to authenticate people" as the
only way to authenticate people, which deferred a control the system already had
every part of.

## Decision

**The control plane resolves every request to a principal and runs every
operation through a `Decider`. Three credentials resolve, and the static
configuration token is the one that is deliberately blunt.**

### Three ways in

| Credential | Resolves to | Grants |
|---|---|---|
| Session token from a password login | the user | the user's grants |
| API key (`mcpd.…`) | the key | key ∩ owner, recomputed per request |
| The static config token | a synthetic principal | `platform_admin` at `*` |

The first two go through the store, which means the control plane enforces
exactly the grants the data plane does — one authorization model, not two that
drift.

The third is break-glass and automation: CI has to build a snapshot before any
user exists, and a deployment whose database is down still needs somebody able
to look at it. It is a documented total-authority credential, it is logged as
such on every use, and `New` warns at startup when one is configured. It is not
a fallback the other two degrade into — a failed session lookup is a 401, never
a silent promotion.

### Sessions are opaque and stored, not signed

A signed session token would be stateless and unrevocable. This is a
low-traffic control plane with a database already in the request path, and the
whole subject of the adjacent ADR is that revocation must not wait — so a
session is a row, with the same `prefix` + SHA-256 shape as an API key and for
the same reason (ADR 0021: the secret is CSPRNG output, so there is nothing for
a KDF to defend).

### Every operation declares a permission and a scope

Not a middleware that guesses from the path. The permission and the scope are
stated at the route, next to the handler, so reading the route table answers
"who can do this" without reading any handler.

Scope comes from what is being acted on: a tenant operation is scoped to that
tenant, a registry or snapshot operation to `*`. That is what makes a tenant
admin real — they hold `tenant_admin` at `t/acme` and every operation on `acme`
passes while the same operation on `globex` does not.

### You cannot grant what you do not hold

`putGrants` additionally requires that the caller hold `role:manage` at a scope
covering **each grant being issued**. Without it, a tenant admin could grant
themselves `platform_admin` at `*`, and the permission set's whole structure
would be decoration. This is the one check that is not derivable from the
route's declared scope, so it lives in the handler with a comment saying why.

### Reads are filtered, not refused

`listTenants` returns the tenants the caller's grants cover rather than 403-ing
a tenant admin who can legitimately see one of them. A control plane that
answers "forbidden" to a question the caller is partly entitled to ask is
useless to anyone who is not a platform administrator.

## Alternatives considered

- **Wait for OIDC.** The mistake this ADR corrects. It would have left the
  control plane unauthorized for however long that took, for no gain — OIDC
  slots in as another way to produce a `User`, behind the same `Decider`.
- **A signed, stateless session token.** Cheaper per request and unrevocable.
  Rejected: revocation is the subject of ADR 0023, and shipping a credential
  that cannot be revoked while building a revocation path would be absurd.
- **Middleware that infers the permission from the URL.** Rejected: the mapping
  becomes implicit, a new route gets whatever the pattern happens to match, and
  the failure is silent over-permission.
- **Dropping the static token entirely.** Rejected: a snapshot build in CI has
  no user, and a control plane with an unreachable database must still be
  inspectable. Making it explicit and loud is better than making it absent and
  reinvented badly.

## Consequences

- **A fresh install must print a password an operator can use.**
  `SeedPlatformAdmin` already does; it now matters, because it is the only way
  in that is not the break-glass token.
- **The console signs in as a person.** It stops holding the shared token, and
  it must render from what the session says the user may do — a button that
  403s is worse than a button that is not there.
- **The CLI can authenticate as an API key**, which means an operator's laptop
  holds a credential scoped to them rather than the deployment's master token.
- **Two authorization call sites now exist** (control plane, data plane) against
  one model. `authz`'s conformance test is what keeps them honest; a third
  engine or a divergent check would break the property this ADR depends on.
