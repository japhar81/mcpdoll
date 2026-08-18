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

### Control plane: Postgres persistence, registry API, admission pipeline

**Not built.** The snapshot *builder* is complete and is the same code the
control plane would use (`snapshot.Builder`), and the test harness exercises the
whole publish path — discover a backend, canonicalize, digest, build, sign,
activate — which is the substance of what admission produces. What is missing is
the durable side: the Postgres schema, `sqlc` queries, the registry HTTP API,
the admission stages (`ON_SUBMIT` … `ON_PROMOTE`), human approval with
publisher ≠ approver, and the `river` job queue.

Consequence: a publish today is programmatic, not reviewed. The gateway serves
admitted definitions (ADR 0006) but "admitted" currently means "built into the
snapshot" rather than "approved by a human who is not the publisher".

### The console

**Not built.** ADR 0001 records the design language extracted from RAGdoll in
enough detail to build against — tokens, component classes, nav structure,
React Flow node chrome — but no `web/` code exists.

Consequence: the tri-surface law is **not** currently satisfied, and
`make parity` cannot pass because there is no route manifest to check. The parity
tool itself is not built either. This is the largest single gap against the brief
and is stated plainly rather than buried.

### Pipeline engine and plugin hosts

**Contract built, engine not.** The seven hooks are in the proto
(`snapshotpb.Hook`), the five-verdict model is in the plugin proto
(`pluginpb.Decision`), manifests carry declared `reads`/`writes`, budgets,
per-effect-class failure policy, and rollout state, and the edge's `Pipeline`
interface is real — the MRTR and cacheScope tests drive it with working
in-process implementations.

Missing: the hook engine that sequences plugins with budgets and circuit
breakers, the `wazero` WASM host, the gRPC plugin host, the four first-party
plugins, shadow/canary rollout evaluation, and the LLM guard.

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

### Deployment: `make dev`, compose, Helm, Grafana dashboards

`make dev` is wired to `deploy/dev-up.sh`, which does not exist. No compose file,
no LGTM stack, no dashboards-as-code. The observability *instrumentation* is
complete and wired from the first commit — spans, the full metric set, trace
correlation in logs — so there is data to display; there is nothing yet to
display it in.

### Playwright e2e and `testcontainers-go` integration tests

Neither exists. The e2e suite needs the console; the testcontainers suite needs
the Postgres schema. The in-process integration coverage that does exist is
substantial and real (the edge suite runs the full data plane over HTTP against
five live fixture backends) but it is not the same as a containerized
control-plane→snapshot→data-plane→backend path.

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
