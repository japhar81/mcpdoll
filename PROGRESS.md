# MCPDoll — build progress

> Recovery point. If context is lost, read this first, then `docs/deferred.md`,
> then `docs/adr/`.

**Current state:** phase 1 complete and demonstrable. A real MCP client connects
to the real edge over HTTP, discovers an aggregated catalog assembled from five
live fixture backends, and calls tools across them — through the real pipeline,
against a real signed snapshot.

**What is not built:** the control plane's durable side, the console, the plugin
hosts, and the prober. See `docs/deferred.md`, which is honest about the size of
those gaps rather than optimistic.

## Ground truth established (verified, not assumed)

Checked against the real SDK source at
`$GOPATH/pkg/mod/github.com/modelcontextprotocol/go-sdk@v1.7.0`, not from
memory:

| Claim | Verdict | Where |
|---|---|---|
| SDK v1.7.0 supports 2026-07-28 | ✅ `latestProtocolVersion = protocolVersion20260728` | `mcp/shared.go:50` |
| …and 2025-11-25 / 2025-06-18 / 2025-03-26 / 2024-11-05 | ✅ all five in `supportedProtocolVersions` | `mcp/shared.go:58` |
| `StreamableHTTPOptions.Stateless` exists | ✅ | `mcp/streamable.go:146` |
| 2026-07-28 over HTTP *requires* `Stateless = true` | ✅ non-stateless returns 400 with an explicit message | `mcp/streamable.go:1516` |
| `server/discover` handled by the SDK | ✅ `methodDiscover` → `(*Server).discover` | `mcp/server.go:1783` |
| `Mcp-Method` / `Mcp-Name` validated, `-32020` on mismatch | ✅ `CodeHeaderMismatch = -32020` | `mcp/shared.go:388`, `mcp/streamable.go:1580` |
| List results carry `ttlMs` / `cacheScope` | ✅ embedded `Cacheable`; **defaults to `public`, `ttlMs: 0`** | `mcp/protocol.go:1168` |
| Stateless mode forbids server→client requests | ✅ documented on the `Stateless` field | `mcp/streamable.go:134` |
| MRTR is `resultType: "input_required"` | ✅ server returns `InputRequests` + `RequestState` | `mcp/protocol.go:28`, `mcp/mrtr.go` |

Facts discovered *while building*, which the docs do not state:

- **The SDK client caches list results per `ttlMs`** (`mcp/cache.go`, SEP-2549).
  So the `ttlMs` we emit has real behavioural effect, and a test that republishes
  a snapshot must open a fresh session or it is testing the client's cache.
- **`_meta` protocol keys are namespaced**:
  `io.modelcontextprotocol/protocolVersion`, not `protocolVersion`. A
  hand-written request with the bare name silently drops out of the modern
  protocol branch. `server/discover` needs the full triple (version, clientInfo,
  clientCapabilities).
- **A non-stateless streamable server is a genuine legacy backend.** Its
  `SupportsProtocolVersion` reports no 2026-07-28 support, so a modern client
  negotiates down through the SDK's real path. The legacy fixture is therefore
  not simulated.

## Bugs found by tests that a mock would have hidden

Recorded because each one argues for the no-mocks approach.

1. **`[]byte` schemas base64-encode.** `encoding/json` renders a plain `[]byte`
   as a base64 string, so the SDK rejected every tool with "can't marshal input
   schema to a JSON object". Fixed by using `json.RawMessage`. Invisible until a
   schema actually crosses the wire.

2. **`defer cancel()` on a dial context kills the session.** The SDK retains the
   connect context for the connection's lifetime (`jsonrpc2.NewConnection(ctx,…)`),
   so cancelling a per-call dial timeout tore down the session immediately after
   connecting. Harmless for a stateless backend (each call is an independent POST),
   fatal for a stateful one. Fixed: the pool holds a lifetime context, and a
   watchdog bounds only the handshake.

3. **The SDK's protocol-version context key leaks across the gateway.** The
   streamable *server* stores the inbound negotiated version under an unexported
   context key; the streamable *client* reads that same key to set
   `Mcp-Protocol-Version`. Passing an inbound handler context to an outbound call
   therefore told a 2025-11-25 backend the request was 2026-07-28, and it replied
   400. The key is unexported so it cannot be deleted — fixed with
   `backends.detachContext`, which builds a fresh outbound context inheriting only
   cancellation and deadline. See ADR 0003.

4. **A plugin's input responses were forwarded to the backend.** Response keys
   are chosen by whoever issued the requests, so a plugin's keys mean nothing to
   a backend. The backend saw a non-empty response map, took its second-round
   branch, found none of its own keys, and returned "confirmation response
   missing". Fixed by recording the deferral source in the signed envelope and
   routing responses to whoever asked. See ADR 0012.

## Completed slices

### Slice 1 — canonicalization + content-addressed digests
`internal/platform/canonical` · surfaces: library

RFC 8785 JCS, `$defs` resolution, and the dual digest scheme drift
classification is built on.

- ECMAScript `Number::toString` implemented from the spec — Go's `%g` disagrees
  with JCS at the exponent-notation thresholds.
- UTF-16 code-unit key ordering, which differs from Go's byte ordering above the
  BMP.
- Duplicate keys and trailing content rejected: identity must not be
  parser-dependent.
- External `$ref` rejected as a security control (SSRF at admission time +
  unsigned meaning).
- Depth (64) and node (20 000) budgets; the node budget catches the exponential
  `$defs` expansion bomb the depth budget misses.
- Golden test pins the exact canonical bytes. 87.6% coverage.

ADR: [0005](docs/adr/0005-content-addressed-tool-definitions.md)

### Slice 2 — platform foundations
`internal/platform/{ids,logging,config}` · surfaces: library

- **ids**: prefixed k-sortable ULIDs. `Is()` is the check that stops a
  well-formed id for the wrong entity reaching a query.
- **logging**: field vocabulary, trace correlation, and mechanical secret
  redaction — attribute names normalized separator-free (one rule covers
  `api_key`, `apiKey`, `x-api-key`) plus value scanning by credential *shape*, so
  a token buried in a struct or an error string still cannot reach a log.
  Includes the required "no token-shaped strings in log output" test, which logs
  credentials through eight different careless routes.
- **config**: defaults → YAML → `MCPDOLL_*`, unknown YAML keys rejected, every
  cross-field invariant checked at startup. An unpinned guard model alias is a
  startup error, because it would make both the verdict cache key and the audit
  record untrue.

### Slice 3 — observability
`internal/observability` · surfaces: library

OTEL from the first commit, not retrofitted. Full metric instrument set,
`_meta` trace-context propagation (propagated, never regenerated), carrier
whitelisted to the three W3C keys in both directions.

### Slice 4 — snapshot: schema, signing, view, atomic swap
`internal/dataplane/snapshot`, `proto/` · surfaces: library

- Protobuf schema for the whole serving configuration, plus the distribution
  service and the plugin host contract.
- Ed25519 signing over the **transmitted octets**, verified **before parsing**,
  domain-separated. Multi-key verifier for overlapping rotation.
- Indexed view built once per activation: the serving path does map lookups and
  reads a pre-sorted slice.
- Stable catalog order `(bundle priority, namespace prefix, tool name)`, tested
  to not perturb existing entries when a tool or a namespace is added.
- `cacheScope` computed at build time; exactly one expression in the codebase can
  return `public`.
- Monotonic, fail-safe activation with local rollback history. 89.4% coverage.

ADRs: [0009](docs/adr/0009-snapshot-signing-and-distribution.md),
[0010](docs/adr/0010-stable-catalog-ordering.md),
[0011](docs/adr/0011-cachescope-correctness.md)

### Slice 5 — the edge: real MCP over HTTP
`internal/dataplane/{edge,backends}`, `internal/mcp`, `fixtures/` · surfaces: library + HTTP endpoint

- Five real fixture MCP backends: modern (2026-07-28), legacy (2025-11-25 by
  construction, not simulation), misbehaving (slow / flapping / down / drifting
  on command), hostile (poisoned descriptions, injected results), and confirming
  (real MRTR).
- Backend pool: lifetime-scoped sessions, per-backend negotiation, consecutive-
  failure circuit breaker with a half-open probe, RFC 8693 token-exchange seam
  with no passthrough path.
- Edge: stateless streamable HTTP, per-audience MCP server rebuilt on snapshot
  swap, namespace-prefixed catalog, dispatch with grace-window behaviour, catalog
  middleware owning `ttlMs`/`cacheScope`.
- MRTR: signed `requestState` envelope binding tool, principal, audience, and
  argument digest, with source-routed input responses.

**44 tests**, all over real HTTP against live backends: conformance (16),
security (6), MRTR (7), chaos (8), plus unit coverage.

ADRs: [0003](docs/adr/0003-protocol-version-strategy.md),
[0006](docs/adr/0006-serve-admitted-not-observed.md),
[0012](docs/adr/0012-mrtr-requeststate-wrapping.md)

## Blockers

- **`mcp-gateway-architecture.md` was never supplied.** The brief names it as the
  design of record; it is not in the repo, the home directory, or Downloads.
  Proceeding from the build brief, which is detailed enough to build from. Every
  decision the missing document might have settled is recorded as an ADR, so the
  divergence is reviewable when it appears.

## Next, in priority order

1. **The console and `tools/paritycheck`.** The tri-surface law is the brief's
   first law and is currently unsatisfied. Needs `api/openapi.yaml` first, since
   everything generates from it.
2. **Control plane persistence + registry API.** Postgres schema, `sqlc`, the
   registry handlers, then the admission stages.
3. **Pipeline engine + `wazero` host.** The contract is settled; the engine is
   not written.
4. **Prober + drift + health state machine.** Inputs exist
   (`mcp.Discover`, `DigestTools`); the loop does not.
5. **`make dev`**: compose, LGTM, dashboards.

## Decisions of record

- No Tailwind — RAGdoll does not use it, and adding it would make the console
  look *less* like the family it is joining. ADR 0001.
- Protobuf on the wire, signature over transmitted octets — which makes
  protobuf's lack of canonical serialization irrelevant. ADR 0009.
- HMAC (not Ed25519) for `requestState`: the gateway is issuer and verifier both.
  Signed but **not** encrypted — a deliberate, documented divergence from the
  spec's wording, since nothing in the envelope is secret. ADR 0012.
- Go module `github.com/mcpdoll/mcpdoll`, Go 1.24 language level (toolchain
  1.26.4).
