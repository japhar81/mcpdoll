# ADR 0020: Pluggable Identity and Authorization, over gRPC

## Status

**Accepted.** The Go counterpart to RAGdoll's
`docs/adr/0035-pluggable-identity-and-authz-providers.md`.
Extends [0008](./0008-dual-plugin-runtime.md) with a third plugin *purpose*
on the existing gRPC runtime.

## Context

Enterprises do not agree on how to authenticate a user or how to authorize one.
MCPDoll ships OIDC, SAML, and local passwords for authentication, and Casbin
for authorization — and every one of those will be the wrong answer somewhere.

RAGdoll solves this by importing a module at boot: point
`RAGDOLL_IDENTITY_PROVIDER` at a package, and it replaces the built-in. Loading
is import-at-boot and fail-closed — a configured-but-unloadable provider crashes
the process rather than silently falling back to a different engine.

**That mechanism does not exist in Go.** There is no runtime module import, and
`buildmode=plugin` is forbidden by this project's constraints for good reasons
already recorded in ADR 0008: it requires byte-identical toolchain and
dependency versions, it cannot be unloaded, and a mismatch is a segfault rather
than an error.

## Decision

**A provider is a gRPC service the control plane calls. Same SPI shape as
RAGdoll's, same fail-closed loading, different transport.**

```
MCPDOLL_IDENTITY_PROVIDER_ADDR=dns:///ldap-identity.internal:9443
MCPDOLL_AUTHZ_PROVIDER_ADDR=dns:///opa-authz.internal:9443
```

Unset means the built-in. Set means the built-in is **replaced**, not
supplemented.

### Two SPIs, mirroring RAGdoll's

**IdentityProvider** — `Kinds()`, `Start()`, `Callback()`. A provider declares
which `identity_providers.kind` values it serves and turns an external login
into an `SsoIdentity` (`subject`, `email`, `name`). MCPDoll handles user
linking and session issuance from there, so the behaviour after identification
is identical across every provider. Registering `kinds: ["oidc"]` overrides the
built-in, exactly as in RAGdoll.

**PolicyEngine** — `Prepare(grants, catalog) → decider`. ADR 0015 already
compiles decisions off the hot path, which is what makes an out-of-process
engine viable at all: `Prepare` is one RPC per principal per snapshot, and the
returned decisions are then local.

That last point is the load-bearing one. A remote call *per authorization
decision* would be indefensible — a catalog listing is thousands of decisions.
A remote call per *compilation* is one RPC, and the result is a table.

### The remote engine returns a decision table, not a callback

`Prepare` returns the set of `(permission, scope)` pairs the principal holds,
which the data plane compiles into the same decider shape the built-in produces.
The provider is asked once, exhaustively, rather than being called back.

An exhaustive answer is only possible because the question is bounded: the
permission set is a closed contract (ADR 0015) and the scopes are enumerable
from the tenant's toolsets. A provider that wanted to decide on unbounded input
could not be supported this way — and that is a deliberate limit, because a
provider that must be consulted per request would put a network hop in the
serving path.

### Fail-closed, and verified at boot

A configured provider that cannot be reached, or that fails a probe, **stops
the process**. It does not fall back to the built-in.

Falling back would mean an operator who configured OPA and mistyped the address
gets Casbin's decisions while believing they have OPA's — the failure mode
where the system appears to work and is enforcing the wrong policy. The probe
is a real `Prepare` call with a known input and a checked result, matching
RAGdoll's `createCasbinEngine` probe rather than a bare connectivity check.

### Providers run in the control plane only

Neither SPI is on the data plane's serving path. Identity providers are used at
login; the policy engine is used at snapshot build. A provider outage stops
publishing and stops new logins — it does not stop tool calls, preserving ADR
0002.

## Alternatives considered

- **`buildmode=plugin`.** Forbidden by ADR 0008 and by the project's
  constraints. Byte-identical builds, no unloading, segfault on mismatch.
- **WASM, reusing the existing host.** Genuinely considered — the runtime is
  already built and the containment story is excellent. Rejected because both
  SPIs need the network: an identity provider must reach the IdP, and a policy
  engine typically reaches a policy service. A WASM plugin cannot, by
  construction, and that containment is the entire reason to prefer WASM (ADR
  0008). Granting it network access would give up the property while keeping the
  ABI awkwardness.
- **A subprocess speaking JSON over stdio.** Simpler to write a provider for,
  and rejected because process lifecycle, restart, and health become MCPDoll's
  problem. gRPC pushes those to the deployment, where they are already solved.
- **HTTP + JSON instead of gRPC.** Defensible. gRPC chosen for the typed
  contract and because ADR 0008 already commits to a gRPC plugin host — one
  transport for out-of-process extensions rather than two.
- **Compile-time registration** (a build tag and a Go import). Rejected: it
  requires rebuilding MCPDoll to change a provider, which is not pluggability,
  it is a fork.

## Consequences

- **Writing a provider is harder than in RAGdoll.** A published npm package
  becomes a deployed gRPC service. That is the real cost of the runtime
  difference, and it should be stated to anyone comparing the two products
  rather than glossed.
- **The gRPC plugin host must exist**, and it currently does not — ADR 0008
  defines the contract and `docs/deferred.md` records that only the WASM half is
  built. This ADR depends on that work.
- **`Prepare` latency is on the publish path.** A slow provider makes snapshot
  builds slow, and a build already discovers every tenant's backends (ADR 0017).
  Both are bounded and both belong in the publish workflow's expectations.
- **An exhaustive decision table has a size.** A principal with grants across
  many tenants and toolsets produces a large table. It is bounded by the
  permission set × the principal's reachable scopes, which is small in practice
  and worth measuring before it is not.
