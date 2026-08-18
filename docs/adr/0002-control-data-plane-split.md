# ADR 0002: Control Plane / Data Plane Split

## Status

Accepted

## Context

MCPDoll sits on the hot path of every tool call every agent in the enterprise
makes. That gives it two responsibilities with incompatible characters:

- **Serving.** Latency-sensitive, must never be down, must behave predictably
  under load, changes rarely.
- **Governing.** Registry writes, admission review, human approvals, drift
  scans. Transactional, human-paced, changes constantly.

Building these as one process means an admission run competes with tool calls
for CPU, a schema migration is a serving outage, and a bug in the approval UI
can take down the gateway.

## Decision

**Two binaries, and the data plane never depends on the control plane to serve
a request.**

| | Control plane (`mcpdoll-cp`) | Data plane (`mcpdoll-dp`) |
|---|---|---|
| Owns | Postgres, registry, admission, policy authoring, probes, console API | The MCP endpoints |
| Reads | The database | One signed snapshot, held in memory |
| Writes | The database, snapshots | Nothing durable on the request path |
| If the other is down | Cannot publish; nothing else breaks | Keeps serving the last good snapshot indefinitely |

The only channel between them is the snapshot: a signed, versioned, complete
serving configuration (ADR 0009). The data plane subscribes to a gRPC stream,
verifies each snapshot's signature, indexes it, and swaps it in behind an
`atomic.Pointer`.

Three properties fall out of this, and they are the reason for the design:

1. **No control-plane dependency on the serving path.** There is no code path
   in the data plane that queries Postgres, calls the control-plane API, or
   blocks on anything the control plane owns. A control-plane outage is
   invisible to clients. This is enforced by the package layout — the data plane
   packages do not import `internal/controlplane` — and asserted by
   `TestControlPlaneAbsenceDoesNotStopServing`, which runs the whole data plane
   with no control plane at all.

2. **Configuration changes are atomic and reversible.** A request sees exactly
   one snapshot version for its whole life; there is no window where half a
   policy change is in effect. A bad publish is a `Rollback(version)` away, and
   the data plane retains the last N snapshots locally, so rollback works
   precisely when the control plane is the thing that broke.

3. **The data plane can run in a different trust domain.** It verifies a
   signature rather than trusting a connection, so it can sit in a DMZ, a
   customer VPC, or an air-gapped network fed by a file.

### What this costs

- **Configuration changes are not instant.** A publish takes a snapshot build
  plus a stream round trip. That is the right trade: instant propagation would
  mean the data plane reading the database.
- **The snapshot must be self-contained.** It cannot reference anything it does
  not carry, which makes it larger and makes the snapshotter's build-time
  validation load-bearing (a dangling reference is a build failure, not a
  runtime surprise).
- **Two deployables.** More to operate, but they scale independently — which is
  the point, since the data plane scales with traffic and the control plane
  does not.

## Alternatives considered

- **One binary with an in-process cache of the database.** Rejected: the cache
  is a snapshot with none of its guarantees. No version identity, no signature,
  no atomic swap, no rollback, and a cold start that needs the database up.
- **Data plane reads Postgres directly with a short TTL cache.** Rejected: it
  makes the database a hard dependency of every tool call, couples serving
  latency to database health, and means a schema migration is a serving
  incident. It also makes the "different trust domain" deployment impossible.
- **Push configuration over the same MCP endpoint (an admin tool).** Rejected:
  it would put a privileged write path on the same surface untrusted agents
  talk to.
- **etcd or Consul as the configuration channel.** Rejected: one more
  stateful system to operate, and it solves distribution — which was not the
  hard part — while solving neither signing nor atomic multi-entity
  consistency. A snapshot is one object, so it is atomic by construction.

## Consequences

- Data-plane instances are stateless apart from the snapshot in memory, so they
  scale horizontally with no coordination and can be replaced freely.
- Anything the data plane needs at request time must be *in* the snapshot. That
  is a real design constraint on every later feature: a proposal that requires
  a database lookup per request has to be redesigned or rejected.
- Audit records are produced by the data plane and shipped asynchronously. A
  data-plane instance that dies with unflushed audit records loses them; the
  alternative — synchronous audit writes — would reintroduce the dependency
  this ADR exists to remove. Mitigated by batching with a short interval and
  recorded in `docs/deferred.md`.
- Two signing-key concerns rather than one: the snapshot signing key (control
  plane holds the private half) and the MRTR `requestState` key (shared among
  data-plane instances). Both are documented in
  `docs/operations/key-rotation.md`.
