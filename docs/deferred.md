# Deferred

Everything consciously **not** built, with the reasoning. This file exists so
that "missing" and "forgotten" are distinguishable.

Two categories: **out of scope for v1** (the brief said not to build it) and
**deferred by this run** (in scope, not reached — an honest statement of where
the build stopped).

---

## Out of scope for v1 (per the brief)

### `subscriptions/listen` fan-in across backends

Aggregating subscription streams from 20+ backends into one client subscription
has unsolved failure semantics upstream: it is not defined what a client should
conclude when three of twelve backends drop their streams. `ttlMs`-driven polling
is sufficient, and the `ttlMs` we emit is honoured by the SDK client today (see
`TestConformanceSnapshotSwapChangesCatalogWithoutRestart`, which asserts a client
legitimately keeps its cached catalog for the TTL).

### Code-mode / sandboxed programmatic tool calling

A different execution model with its own sandbox, resource, and audit design. Not
started.

### MCP Apps / server-rendered UI extensions

Requires a UI trust model — what a backend may render inside a client — that does
not exist yet.

### Progressive tool discovery meta-tool surface

**Designed for, not built.** The catalog layer is structured so it can be added:
`AudienceView` already carries `TokenEstimate`, bundles already carry
`token_budget`, and ordering is a stable contract (ADR 0010) rather than an
accident. A discovery meta-tool would be a synthetic tool at `ON_CATALOG` that
replaces the full list — the hook and the ordering guarantees it needs are both
in place.

### Full EMA / ID-JAG

The token-exchange **seam** is built and load-bearing:
`backends.TokenSource` is an interface, and its shape is itself the control —
`Exchange(ctx, backend, principal)` has no parameter through which a caller
*could* pass the inbound token, so passthrough is structurally impossible rather
than merely discouraged. `TestInboundTokenNeverReachesABackend` asserts the
result. The IdP side (real RFC 8693 exchange against an authorization server) is
stubbed behind that interface.

### Multi-region routing, `x-mcp-header` promotion

Single-region only. The SDK supports `x-mcp-header` annotations; MCPDoll does not
promote them.

### Behavioural drift detection (output sampling/eval)

Structural and semantic drift are addressed by the digest pair (ADR 0005).
Detecting that a tool's *outputs* changed character requires sampling real
results and evaluating them, which needs a corpus, a budget, and a privacy model.

---

## Deferred by this run

Ordered by how much their absence matters.

### Control plane: admission pipeline and the job queue

**Persistence is built.** The Postgres schema, `sqlc` queries, and the
repository layer exist for tenants, users, identities, grants, API keys, and
identity providers, with an embedded migration runner and tests that run
against a real database (`internal/controlplane/store`).

Still missing: the admission stages (`ON_SUBMIT` … `ON_PROMOTE`), human
approval with publisher ≠ approver, and the `river` job queue.

The *permissions* for approval exist — `snapshot:build` and `snapshot:publish`
are separate, and the default `publisher` role deliberately holds the second
without the first — but nothing yet enforces that the two were exercised by
different people.

Consequence: a publish today is programmatic, not reviewed. The gateway serves
admitted definitions (ADR 0006) but "admitted" currently means "built into the
snapshot" rather than "approved by a human who is not the publisher".

### The restructure is partly landed

ADRs 0014–0020 are the design of record. Built so far: `internal/platform/authz`
(scopes, roles, two engines, conformance) and `internal/controlplane/store`
(tenancy, credentials, grants).

Not yet built, and each is a breaking change when it lands:

- **Toolsets replacing bundles and audiences** in the registry (ADR 0016).
- **Per-tenant backend bindings with pools and a primary** (ADR 0017).
- **A tenant-partitioned snapshot carrying grants** (ADR 0018).
- **The single `/mcp` endpoint** and `PrincipalView` (ADR 0019). Until this
  lands the gateway still serves `/mcp/{audience}`, which the ADRs say no
  longer exists.
- **Tenant/user/grant/key management on all three surfaces** (ADR 0004). The
  store has the operations; nothing exposes them yet.
- **Identity providers and the gRPC SPI** (ADR 0020). Local passwords are
  implemented in the store; OIDC, SAML, and the pluggable transport are not.

Consequence: the running system is still the pre-restructure one. The ADRs
describe where it is going, not where it is.

### The console: built, but read-and-inspect only

`make parity` passes: all sixteen operations reach the API, the CLI, and the
console. The tri-surface law is satisfied *for the operations that exist*.

What the console does not have, because the underlying capability does not
exist either (see the entries above and below):

- **No approval or publish workflow.** There is nothing to approve until
  admission and human sign-off exist.
- **No request-trace waterfall.** The trace data is produced; nothing stores it.
- **No React Flow bundle composer or drift diff.** ADR 0001 records the design
  language for these in detail; none of it is built. `reactflow` is not even a
  dependency yet.
- **No live events.** Every screen polls or refetches on demand. RAGdoll's SSE
  bus is not ported.
- **No login.** The API token is typed into the sidebar and kept in
  `localStorage`. A real deployment needs a session from the identity provider,
  which does not exist either.

### TypeScript types generated from the spec

`web/src/lib/types.ts` is hand-written, so it is a second definition of shapes
that `internal/api` also defines, and it can drift. The Go side is protected —
`internal/api/schema_test.go` holds every struct against the spec's schemas by
field name — but nothing checks the TypeScript.

Missing: `openapi-typescript` in `make generate`, plus a `verify-generated` gate.
The work is small; it was not done because the schema check bought most of the
same protection for the half where the drift would be silent.

Consequence: a field renamed in the spec and in Go will typecheck fine in the
console and render `undefined` at runtime.

### The gRPC plugin host and the LLM guard

**The WASM half is built; the gRPC half is not.** The hook engine, the wazero
host, the SDK, and two first-party plugins (`redact`, `entitlements`) all work
end to end — see PROGRESS.md.

Missing: the gRPC plugin host and therefore the LLM guard, which needs it because
it has to reach a model. The proto contract (`pluginpb`) is defined, and
`wiring.HostRegistry` **refuses** a gRPC plugin with an explanatory error rather
than ignoring it — a configured security control that silently did nothing would
be worse than one that says it cannot run.

Also missing from the first-party set: `header-map` (claim → backend header
mapping). The mechanism it needs exists — `backends.Credential.Extra` already
carries per-request headers — so it is a plugin to write, not a capability to
add.

### WASM fuel limits are not enforced

`wazero` has no instruction metering. The plugin manifest carries a `fuel_limit`
field and the host **does not enforce it**; termination is by wall-clock deadline
via `WithCloseOnContextDone`.

A runaway plugin *is* stopped, but the limit is load-dependent: the same plugin
may complete on a quiet host and be cut off on a busy one. The host logs a
warning when a manifest sets `fuel_limit`, rather than letting an operator
believe a limit is in force. Switching to wasmtime or wasmer would fix it at the
cost of cgo — see ADR 0008.

### Prober, drift detection, health state machine

**Inputs built, loop not.** `mcp.Discover` and `mcp.DigestTools` produce exactly
what drift classification consumes, and the misbehaving fixture can drift,
disappear, and grow tools on command — `TestConformanceServesAdmittedNotObserved`
already proves the mutated text never reaches a client. What is missing is the
periodic prober, the drift classification taxonomy applied to its output, and the
`healthy → degraded → ejected → quarantined` state machine.

Partial credit where it is real: the circuit breaker *is* built and tested,
including the distinction that matters most — a tool-level error is the tool
working correctly and does not count against backend health
(`TestCircuitDoesNotOpenOnToolErrors`).

### Snapshot distribution over gRPC

The service is defined (`SnapshotDistribution`) and the generated code exists;
the server and the subscribing client are not implemented. The data plane loads
snapshots in-process today. `Store.Activate` / `Rollback` / `Signed` — the hard
parts — are complete and tested.

### Redis-backed caching and idempotency

The catalog cache key design is settled (`(entitlement_set, bundle,
snapshot_version)`) and the snapshot marks which tools require an idempotency key
(`requires_idempotency_key`, derived from effect class at build time). Neither
cache is implemented.

### RBAC roles and separation of duties

Roles are named in the design (publisher, approver, operator, consumer, auditor)
but there is no role model, because there is no control-plane API to enforce it
at. Audience-level authorization against IdP groups *is* enforced and tested
(`TestAudienceAuthorization`).

### Real identity provider

Only `HeaderIdentityResolver` exists, which trusts client-supplied headers. It
**refuses to be constructed** when the environment is production
(`TestHeaderIdentityResolverRefusesProduction`) — a header-trusting resolver
reaching production would be a total authorization bypass, so that is a
constructor error rather than a documented caveat. OIDC/JWT validation and SCIM
are not built.

### Deployment: no Helm chart

`make dev` and `make up` both work. What is missing is the production side: a
Helm chart, and Kubernetes manifests of any kind.

One dashboard is provisioned as code (`deploy/observability/dashboards/`) —
serving, backends, and the plugin pipeline. There is no dashboard for the
control plane, and there are no alert rules: `mcpdoll.drift.events` and
`mcpdoll.snapshot.rejects` are the two that most obviously want one.

### Instrumentation that had no dashboard, and metrics that had no feature

Seven instruments were declared and never recorded, so every panel built on
them read "No data" forever. They are wired now — probe runs and latency,
backend health state, drift events, circuit state, snapshot age and rejects.

Seven more were declared for features that do not exist: the guard's verdict
cache, Redis idempotency, token and cost accounting, rate limiting, and the two
admission stages. Those were deleted rather than left in place. A metric arrives
with its feature; one that cannot emit is an alert rule that never fires and a
panel that looks quiet rather than absent.

`TestEveryInstrumentIsRecordedSomewhere` now fails the build on either mistake.

### The container images are development images

`make up` runs the stack in Docker, and what it runs is not what would ship:

- **The console runs Vite's dev server**, not a production build behind a static
  server. Deliberate — this stack exists to be worked in, and a source edit
  should be visible on refresh. A production image would `npm run build`.
- **Everything is one image.** Three binaries, the fixtures, and the plugins
  share `mcpdoll/mcpdoll:dev`. Fine for a stack that starts together; a real
  deployment wants the data plane's image to contain the data plane.
- **The fixture backends are in the compose file at all.** They are test
  doubles. A deployment fronts real backends and would not ship these.
- **Secrets are fixed development values** in the compose file: the API token
  and the MRTR envelope key. Both are marked as such where they appear.
- **No resource limits, no read-only root filesystem, no dropped capabilities.**
  The containers run as a non-root user and nothing more.

There is no Helm chart and no Kubernetes manifest.

### Health probing: built; ejection and the LLM guard's inputs are not

The prober runs, classifies drift against the admitted digests, and blocks
drifted tools on a strict backend. What it does not do:

- **Eject a backend after N consecutive invocation failures.**
  `health.eject_after_failures` is configured and unused. Circuit breaking on
  the *call* path exists (`backends.Breaker`), which covers most of the same
  ground; ejection would additionally stop probing a backend that is clearly
  gone.
- **Feed drift into an alert.** Drift is logged on transition and served on the
  admin listener. Nothing pages.
- **Probe with a canary tool.** `Server.canary_tool` is carried in the snapshot
  and displayed in the console; the prober uses `tools/list` for every backend
  rather than calling the canary. Listing proves the session works, which is
  weaker than proving a tool works.

### Audit trail persistence

`pipeline.Trace` records everything the console's request-trace waterfall needs —
every hook, every plugin, every verdict, every skip and its reason, budget
consumption, and shadow divergences. The data-plane binary currently logs a
one-line summary of each trace.

Missing: the append-only, queryable store. The `TraceSink` seam is where it
plugs in, and the trace type is already the shape the waterfall would render.

### Playwright e2e and `testcontainers-go` integration tests

Neither exists, and `make test-e2e` was removed rather than left pointing at a
Playwright config that is not there.

The console's own tests (`web/src/lib/*.test.ts`, vitest) cover the API client's
error handling and the pure helpers — the parts where a bug is silent. The
screens themselves have been driven by hand through a browser against the live
stack, including the full MRTR confirmation round trip, but that is a person
checking rather than a suite.

The testcontainers suite needs the Postgres schema. The in-process integration
coverage that does exist is substantial and real (the edge suite runs the full
data plane over HTTP against five live fixture backends) but it is not the same
as a containerized control-plane→snapshot→data-plane→backend path.

---

## Smaller known limitations

- **`$anchor` refs are rejected, not resolved.** `{"$ref": "#SomeName"}` fails
  canonicalization. Silently hashing an unresolved reference would defeat the
  purpose of a content address, so it fails loudly. Schemas using anchors cannot
  be admitted.
- **JCS number precision.** Per RFC 8785, numbers are IEEE-754 doubles, so two
  schemas differing only in a numeric keyword beyond ±2^53 would collide. Real
  but not reachable in practice; admission's schema linter is where to reject
  such a keyword if it ever matters.
- **MRTR approvals are not single-use.** Within the 10-minute TTL an approval can
  be redeemed more than once for the *same* call. Single-use redemption needs
  shared state; the argument binding already prevents redirecting an approval to a
  different target. See ADR 0012.
- **Audit records are not durable.** No append-only store; the data plane would
  ship them asynchronously and an instance dying with unflushed records loses
  them. Synchronous writes would reintroduce the control-plane dependency ADR 0002
  exists to remove.
- **No SVAR Grid.** RAGdoll uses `@svar-ui/react-grid` for tall data grids.
  MCPDoll's plan is plain `table.grid`. A deferral, not a rejection (ADR 0001).
- **RAGdoll's live-events bus is not ported.** The console would poll. SSE across
  a horizontally-scaled control plane needs a broker, and `ttlMs` polling is
  adequate for v1 (ADR 0001).
- **Snapshot distribution is full-artifact, not delta.** A few hundred kilobytes
  per publish at 20 backends. The version field already makes deltas expressible
  if it becomes a problem.
