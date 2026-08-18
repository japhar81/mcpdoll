# ADR 0003: Protocol Version Strategy and SDK Dependency

## Status

Accepted

## Context

MCPDoll must speak MCP 2026-07-28 to its clients while fronting backends that
speak anything from 2024-11-05 upward, and the 2026-07-28 spec is still
settling. Getting this wrong in either direction is expensive: implementing from
memory produces a gateway that subtly disagrees with real clients, and coupling
the whole codebase to the wire format means every spec revision is a
refactor.

Before writing any protocol code, the SDK source was read to establish what is
actually true rather than what seemed likely. The findings are recorded in
`PROGRESS.md` with file and line references; the ones that shaped this ADR:

- `github.com/modelcontextprotocol/go-sdk` v1.7.0 supports 2026-07-28 and
  simultaneously 2025-11-25, 2025-06-18, 2025-03-26, and 2024-11-05.
- **2026-07-28 over streamable HTTP requires `Stateless = true`.** A stateful
  transport returns 400 with an explicit message, and its
  `SupportsProtocolVersion` reports no 2026-07-28 support, so clients negotiate
  down.
- In stateless mode the server cannot make client requests, so every
  interactive flow must go through MRTR.
- The SDK already implements `server/discover`, `Mcp-Method`/`Mcp-Name`
  validation with `-32020`, and the `ttlMs`/`cacheScope` fields.

## Decision

**Depend on the official SDK for the protocol. Own only what the SDK cannot
know.**

### 1. The SDK is the authority on the wire

The edge does not reimplement version negotiation, header validation, result
typing, or pagination. Concretely: `Mcp-Method`/`Mcp-Name` mismatch produces
`-32020` because the SDK produces it, not because MCPDoll checks.

This is a deliberate reversal of the instinct to validate defensively. Two
implementations of a moving spec disagree, and when they do, the bug is in the
disagreement rather than in either one. The conformance suite therefore
*verifies* the SDK's behaviour end to end rather than trusting it — which is why
`TestConformanceHeaderValidation` exists even though no MCPDoll code
implements header validation.

### 2. What MCPDoll owns

The fields a gateway is uniquely responsible for, because the SDK has no way to
know them:

- **`ttlMs`** — the merged minimum of org, bundle, and policy TTLs.
- **`cacheScope`** — `private` whenever the catalog can differ between
  principals.
- **Catalog ordering** — stable across publishes (ADR 0010).
- **The MRTR `requestState` envelope** — the SDK passes it through opaquely; the
  gateway has to sign it (ADR 0012).

The SDK defaults `cacheScope` to `"public"` and leaves `ttlMs` at zero, so
omitting our part is not a no-op: it would tell every client that a filtered
catalog is freely shareable and immediately stale.

### 3. Stateless mode, and transport coupling in one package

`Stateless = true` is not optional; it is what makes 2026-07-28 possible at all.
It is also what makes the data plane horizontally scalable with no shared
session state, so the two requirements coincide.

Because stateless semantics are still being refined upstream, **all transport
coupling lives in `internal/dataplane/edge`**. Nothing outside that package
imports the SDK's transport types. A spec change should be a single-package
edit, and the conformance suite is the thing that tells us whether the edit was
right.

### 4. Per-backend version negotiation

Each backend negotiates independently, and the gateway records what was agreed.
A 2026-07-28 client talking through the gateway to a 2025-11-25 backend is
normal and neither side needs to know.

**One consequence of this deserves its own note**, because it was found by a
failing test rather than by reading. The SDK's streamable *server* stores the
inbound request's negotiated version under an unexported context key so handlers
can read it — and its streamable *client* reads that same unexported key to
decide which `Mcp-Protocol-Version` header to send. A gateway that hands an
inbound handler context to an outbound client therefore announces the inbound
version to the backend: a request that arrived as 2026-07-28 gets sent to a
2025-11-25 backend claiming to be 2026-07-28, and the backend rejects it with
400.

The key is unexported, so it cannot be deleted. The backend pool therefore
builds a **fresh context** for every outbound call, inheriting cancellation and
deadline but no values (`backends.detachContext`). Trace context is not lost by
this because it propagates explicitly in `_meta`, which is where MCP carries it
anyway.

## Alternatives considered

- **Implement the protocol directly on `net/http` and `encoding/json`.**
  Rejected: 2026-07-28 is new and moving, the SDK is the reference
  implementation, and the parts that look simple (header/body agreement, MRTR
  result typing, cacheable-field defaults) are exactly where a hand-rolled
  version drifts.
- **Vendor the SDK and patch it.** Rejected: it converts every upstream fix
  into a merge. The one place we work around SDK behaviour (the context key) is
  handled in our own code at our own boundary.
- **Support only 2026-07-28 and refuse older backends.** Rejected: the premise
  of the product is fronting 20+ independently-published backends, most of which
  will not have upgraded. Refusing them would leave nothing to aggregate.
- **Pin every backend to a single version organisation-wide.** Rejected for the
  same reason, and it removes the per-backend pin that exists so a backend
  cannot silently change result shapes under an already-admitted catalog.
- **Reimplement header validation defensively "in case the SDK misses it".**
  Rejected as actively harmful: two checks that can disagree are worse than one
  check plus a test that proves it fires.

## Consequences

- An SDK upgrade can change wire behaviour. The conformance suite is the gate,
  and it runs against real fixture backends over real HTTP, so a behavioural
  change surfaces as a test failure rather than in production.
- Stateless mode forbids server→client requests, so elicitation and sampling are
  only available through MRTR. That is a genuine capability reduction, and it is
  why the MRTR envelope (ADR 0012) had to be built properly rather than deferred.
- `internal/mcp` converts SDK tool types into MCPDoll's canonical definition
  rather than storing the SDK type. A field the SDK adds later is ignored rather
  than folded in, so an SDK upgrade cannot silently change the digest of every
  admitted definition. Adding a field to the canonical form is a deliberate,
  versioned migration.
- The `detachContext` workaround is load-bearing and non-obvious. It is
  documented at length at the call site because a well-meaning simplification
  back to "just pass ctx through" would break every legacy backend, and the
  symptom (a 400 from the backend) points nowhere near the cause.
