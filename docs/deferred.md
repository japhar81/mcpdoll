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
`PrincipalView` already carries `TokenEstimate`, toolsets already carry
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

### The restructure has landed; two pieces of it have not

ADRs 0014–0023 are the design of record, and the system now matches them.
Built: `internal/platform/authz` (scopes, roles, two engines, conformance),
`internal/controlplane/store` (tenancy, credentials, grants), toolsets replacing
bundles and audiences, per-tenant bindings with a declared primary, a
tenant-partitioned snapshot carrying grants and credentials, the single `/mcp`
endpoint with lazily-composed `PrincipalView`s, tenant/user/grant/key management
on all three surfaces, offline credential verification in the data plane,
control-plane sessions with RBAC enforced per operation, and out-of-band signed
revocation.

What is still missing from the design:

- **Identity providers and the gRPC SPI** (ADR 0020). Local passwords and API
  keys work, the control plane authenticates people with them and enforces
  their grants (ADR 0022), and OIDC slots in as another way to produce a `User`
  behind the same decider. SAML, SCIM, and the pluggable transport do not
  exist. This is the largest remaining gap for an enterprise deployment, and it
  is now an addition rather than a prerequisite.
- **~~A grants-only rebuild that skips discovery.~~** Done, and not the way
  ADR 0018 proposed. Caching discovery would have made a wrong coupling cheap;
  principals left the snapshot instead (ADR 0024), so a grant change costs no
  build at all.

### Revocation is built; short-lived credentials are not

A leaked key used to stay valid until the next snapshot. It no longer does:
`revokeAPIKey`, disabling a user, and deleting a tenant all publish a signed
revocation list the data plane applies within a couple of seconds
(ADR 0023).

What remains is the residual risk that design accepts and states. The list is
distributed, not pushed, and failing closed on an unreachable one would let a
control-plane outage stop tool calls — so the exposure is the gateway's list
age, bounded by a thirty-second heartbeat and visible as
`mcpdoll.revocations.age`, on `/readyz`, and on the console's revocations
screen. Alert on `age > 5 × heartbeat`.

Short-lived credentials would shrink that window rather than closing it, and
they are the better long-run answer: an agent key that expires every few minutes
makes revocation latency mostly irrelevant. It needs a refresh path this system
does not have, and it is orthogonal to what is built rather than blocked by it.

### The console: built, but read-and-inspect only

`make parity` passes: all twenty-nine operations reach the API, the CLI, and the
console. The tri-surface law is satisfied *for the operations that exist*.

What the console does not have, because the underlying capability does not
exist either (see the entries above and below):

- **No approval or publish workflow.** There is nothing to approve until
  admission and human sign-off exist.
- **No request-trace waterfall.** The trace data is produced; nothing stores it.
- **No React Flow toolset composer or drift diff.** ADR 0001 records the design
  language for these in detail; none of it is built. `reactflow` is not even a
  dependency yet.
- **No live events.** Every screen polls or refetches on demand. RAGdoll's SSE
  bus is not ported.
- **No real login.** There is a sign-in screen and the token is held in
  `localStorage`, but it is the control plane's single bearer token rather than
  a session for a person. Every console user is therefore the same principal
  with the same access — see "separation of duties" below.

### The console's copy was written for me, not for an operator

Fixed once, and worth watching. Screens carried paragraphs justifying design
decisions — ADR citations, "on purpose", "the alternative would be" — which
is documentation aimed at whoever built it rather than whoever is looking at
it. It has been swept; the reasoning moved into the component doc comments,
where it belongs and where it is still there for the next maintainer.

The test that was applied: does this sentence help the reader decide or act? A
banner that says "some tenants have no bindings" fails it — the reader then
scans the table to work out which. It names them now, and the row carries the
mark.

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

The catalog cache key design is settled (`(entitlement_set, toolset,
snapshot_version)`) and the snapshot marks which tools require an idempotency key
(`requires_idempotency_key`, derived from effect class at build time). Neither
cache is implemented.

### A tenant admin cannot see the registry, and the console does not explain it

Registry, snapshot, and gateway operations require their permission at **global**
scope, because the registry is one org-wide document: it lists every backend and
every tenant's binding addresses, and a tenant admin reading another tenant's
hostnames is a leak.

The consequence is that a tenant admin's console is mostly 403s. They can do
their whole job — users, grants, keys, and a tenant list filtered to what they
hold — and every other screen refuses. That is *correct* and it is a bad
experience, because nothing on those screens says why.

The fix is a scoped view of the registry rather than a scoped permission on the
whole document: a tenant admin should see the backends bound to their tenant and
the toolsets those tools land in, and nothing else. That is a new operation
rather than a permission change, which is why it is here and not done.

### Separation of duties is enforced; the console does not yet render from it

The control plane resolves every request to a principal and checks every
operation against their grants (ADR 0022). `snapshot:build` is separate from
`snapshot:publish`, a tenant admin's scope stops at their tenant, and
`putGrants` refuses a grant the caller does not themselves hold — so
"the person who prepares a change is not the person who ships it" is enforced
rather than merely expressible.

What is missing is the console reading `getSession` to decide what to *show*.
It has the data — `permissions` is on the session — and every screen still
renders every control. A button that 403s is worse than a button that is not
there, and a tenant admin currently sees the signing-key screen and finds out
by clicking.

### Real identity provider

API keys are the working mechanism for agents (ADR 0021). For people there are
local passwords, and `HeaderIdentityResolver`, which trusts client-supplied
headers and **refuses to be constructed** when the environment is production —
a header-trusting resolver reaching production would be a total authorization
bypass, so that is a constructor error rather than a documented caveat. It is
also chained strictly behind the API key resolver outside production, so a valid
key always wins over a claimed subject.

People sign in with a local password and the control plane enforces their
grants (ADR 0022). OIDC/JWT validation, SAML, SCIM, and the gRPC SPI of ADR 0020
are not built — and OIDC in particular is now an addition rather than a
prerequisite, because it produces a `User` and joins the same path.

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
- **Schedules are intervals, not cron.** No calendar cadences: "every hour at
  :05" and "daily at 03:00 UTC" cannot be expressed. Nothing MCPDoll schedules
  today wants one — the three jobs are maintenance loops, and the shortest is a
  30-second heartbeat cron cannot express at all. The `schedules.kind` column is
  a discriminator so an evaluator is additive when a job needs one, without
  rewriting the table. See ADR 0026.
- **A schedule keeps only its last outcome, not a history.** `last_run_at`,
  `last_error`, and `last_duration_ms` are overwritten on each run, so "has this
  been flapping?" has no answer here. A runs table would answer it and would
  grow forever to schedule three things; the logs carry the history until
  something needs it structured (ADR 0026).
