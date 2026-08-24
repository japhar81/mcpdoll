# MCPDoll — build progress

> Recovery point. If context is lost, read this first, then `docs/deferred.md`,
> then `docs/adr/`.

**Current state:** the data plane is complete and runnable. `make dev` starts
five fixture backends, builds a signed snapshot by discovering them, and serves
MCP 2026-07-28 on `:8080` with WASM plugins in the request path.

Verified by hand as well as by tests — the full loop below was run end to end:

```
registry.yaml -> discover 5 live backends -> canonicalize -> sign
              -> data plane (hot reload, no restart)
              -> MCP client -> edge -> pipeline -> wazero -> WASM plugin
              -> RFC 6902 patch (scope-checked) -> backend -> client
```

Observed: a card number returned by a backend never reached the client; the
entitlements plugin in shadow recorded "would have hidden 2 tools" while changing
nothing; promoting it to `enforce` and republishing changed the catalog from 8
tools to 6 for an unentitled principal and refused the call to a hidden tool.

**What is not built:** the control plane's durable side (Postgres, admission,
approvals), the console, the parity check, the prober, and the gRPC plugin host.
See `docs/deferred.md`, which states the size of those gaps rather than
minimising them. **The tri-surface law is not currently satisfied** — there is a
CLI but no API and no UI.

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

5. **`View.Tenant` takes a slug; the credential resolver passed it an id.** The
   API-key resolver looked the tenant up by `principal.TenantId` against a map
   keyed by slug, got nil, and refused every valid credential — so the gateway
   authenticated nobody. Both are `string`, so the compiler had nothing to say.

6. **The dev header resolver's default subject would have made every failed
   authentication succeed.** Chained behind the API key resolver, a wrong key
   fell through to the header resolver, which had no subject header, fell back
   to its default, and returned a principal. A test that presented a *wrong*
   key found it; the default is empty now, and the chain's ordering is the
   security property rather than a convenience.

7. **`--grant role@scope` split on the last `@`.** Role names never contain
   one, but a tool name can, and a scope naming that tool was cut in half into a
   grant that validated and authorized something else.

8. **The bootstrap's "did every endpoint get rewritten" guard checked a key
   that no longer existed.** It grepped for `endpoint: http://localhost`; a
   binding names its host as `primary:`. The check passed vacuously on every
   unrewritten address.

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
- Stable catalog order `(toolset priority, namespace prefix, tool name)`, tested
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
- Edge: stateless streamable HTTP, per-principal MCP server rebuilt on snapshot
  swap, namespace-prefixed catalog, dispatch with grace-window behaviour, catalog
  middleware owning `ttlMs`/`cacheScope`.
- MRTR: signed `requestState` envelope binding tool, principal, tenant, and
  argument digest, with source-routed input responses.

**39 tests** in the edge package, all over real HTTP against live backends:
conformance, security, MRTR, and chaos.

ADRs: [0003](docs/adr/0003-protocol-version-strategy.md),
[0006](docs/adr/0006-serve-admitted-not-observed.md),
[0012](docs/adr/0012-mrtr-requeststate-wrapping.md)

### Slice 6 — registry, snapshotter, CLI, data-plane binary
`internal/controlplane/{registry,snapshotter}`, `internal/cli`, `cmd/` · surfaces: CLI + HTTP

- **registry**: a reviewable YAML document declaring backends, tool effect
  classes, toolsets, policies and plugins. Unknown keys rejected, every
  cross-reference validated, all problems reported at once. Enums that gate
  behaviour refuse to default; the two that do — `serving_mode` → strict,
  `rollout` → shadow — default in the safe direction.
- **snapshotter**: concurrent discovery, canonicalization, resolution, signing.
  Every problem is a build failure; a name collision names both culprits.
- **cli**: `snapshot build/inspect/verify`, `keys generate`, `registry validate`,
  and a gateway inspector that connects as a chosen identity over real MCP.
  Human tables by default, JSON as the contract, documented exit codes, and the
  hidden `__commands` dump the parity check will consume.
- **mcpdoll-dp**: the data-plane binary, with a hot-reloading file snapshot
  source.

### Slice 7 — pipeline engine, WASM host, first-party plugins
`internal/dataplane/{pipeline,plugins,wiring}`, `plugins/` · surfaces: library

- **pipeline**: the seven-hook engine. Five-verdict model, per-hook and
  per-request budgets, per-plugin circuit breakers, shadow/canary/enforce, and
  per-effect-class failure policy. Every plugin's outcome is recorded including
  the ones that did *not* run and why.
- **patch**: RFC 6902 with write-scope enforcement — rejected whole if any
  operation is out of scope, source and destination both checked, output
  re-canonicalized.
- **plugins**: wazero host with a zero-import guarantee, artifact digests
  verified before load, pooled instances, and a guest discarded rather than
  reused after a trap.
- **sdk + redact + entitlements**: the ABI in one tested place.
- **wiring**: the composition, tested end to end with a compiled plugin.

ADRs: [0007](docs/adr/0007-seven-hooks.md),
[0008](docs/adr/0008-dual-plugin-runtime.md),
[0013](docs/adr/0013-wasm-abi-buffer-ownership.md)

### Slice 8 — tenancy, RBAC, and the single endpoint

The restructure. A tenant owns its users; a user is granted toolsets at
hierarchical scopes; a toolset binds to a different backend deployment per
tenant. There are no audiences and no `/mcp/{audience}` — the tenant and the
toolset both come from the credential.

- **authz**: `*` ⊃ `t/<tenant>` ⊃ `t/<tenant>/ts/<toolset>` ⊃ `.../<tool>`, a
  closed permission set, and two engines (built-in and Casbin) pinned to
  identical decisions by a 484-case conformance test.
- **store**: tenants, users, grants, API keys, and the declarative `SetGrants`
  that makes "what should this person hold" the question rather than a sequence
  of deltas.
- **snapshot**: tenants, toolsets, per-tenant bindings, and compiled RBAC — all
  signed. `PrincipalView` composes lazily on first connect and is cached by
  snapshot version, so a swap drops every view rather than invalidating them one
  at a time.
- **edge**: one `/mcp`. API keys verified against the snapshot with one hash and
  no database, so a control-plane outage is still invisible to a tool call.
- **tri-surface**: twelve new operations for tenants, users, grants, keys, and
  the role catalog — API, CLI, and console each.

Verified against the live stack: four principals, one toolset name, four
different catalogs, and `globex/support` reaching a different container than
`acme/support` through identical tool names.

ADRs: [0014](docs/adr/0014-tenancy-and-principals.md),
[0015](docs/adr/0015-rbac-scopes-and-engines.md),
[0016](docs/adr/0016-toolsets-replace-audiences.md),
[0017](docs/adr/0017-per-tenant-backends-and-pools.md),
[0018](docs/adr/0018-grants-in-the-snapshot.md),
[0019](docs/adr/0019-single-mcp-endpoint.md),
[0020](docs/adr/0020-pluggable-identity-and-authz.md),
[0021](docs/adr/0021-offline-credential-verification.md)

### Slice 9 — the control plane authenticates, and revocation stops waiting

Two corrections to things I had recorded as done or deferred.

The control plane compared a bearer token against one value and ran everything
past it unchecked, so every console user was the same principal — the most
privileged one. I had recorded that as blocked on an identity provider, and it
was not: a local password *is* a principal, and grants compile to the same
decider the data plane uses. Sessions, three resolvable credentials, and a
permission plus a scope declared at every route, so reading the route table
answers "who can do this" without reading a handler.

The check that earns its own test: `putGrants` requires role:manage at the scope
of each grant being *issued*, not only at the target user's tenant. Without it a
tenant admin could grant themselves platform_admin at `*` and the permission set
would be decoration.

And a revoked key kept working until somebody republished. ADR 0018 named the
fix and deferred it; ADR 0023 builds it. A signed revocation list with its own
signing context, distributed the way the snapshot is, that can only subtract —
which is what answers 0018's own objection, since an allowed action is still
explained by the snapshot alone. It is republished on a timer whether or not
anything changed, because otherwise its age grows forever in a healthy system
and there is nothing to alert on.

Verified live: revoking a key stopped it in about a second, with the snapshot
version unchanged.

ADRs: [0022](docs/adr/0022-control-plane-sessions-and-permissions.md),
[0023](docs/adr/0023-out-of-band-revocation.md)

## Blockers

- **`mcp-gateway-architecture.md` was never supplied.** The brief names it as the
  design of record; it is not in the repo, the home directory, or Downloads.
  Proceeding from the build brief, which is detailed enough to build from. Every
  decision the missing document might have settled is recorded as an ADR, so the
  divergence is reviewable when it appears.

## Next, in priority order

1. **The console rendering from `getSession`.** The control plane enforces RBAC
   per operation now, and the console still shows every control to everyone —
   so a tenant admin finds out they cannot mint a signing key by clicking. The
   data is already on the session; nothing reads it.
2. **OIDC.** No longer a prerequisite for anything — it produces a `User` and
   joins the path local passwords already take (ADR 0022). It is the largest
   remaining gap for an enterprise deployment all the same.
3. **Admission + the job queue.** The snapshotter is already the piece admission
   would feed.
4. **The gRPC plugin host and the LLM guard.** The proto contract is defined and
   the host registry refuses a gRPC plugin loudly rather than ignoring it.
5. **Alert rules, starting with `mcpdoll.revocations.age`.** It is the exposure
   window for a revoked credential and the one number in this system that is
   meaningless without an alert on it. `mcpdoll.drift.events` and
   `mcpdoll.snapshot.rejects` are the next two.

## The tri-surface law

`make parity` is green: **33 operations, 33 CLI commands, 33 console routes.**

It checks three real artifacts, never a hand-maintained list — the spec, the
built binary's `__commands --json`, and a route manifest generated from the
console's router. On the commit that introduced it, it found a CLI command
bound to an `operationId` the spec did not define, and a walker bug that
silently cost `registry servers` its binding.

Two further checks stop the surfaces agreeing only nominally:

- `internal/cli/parity_test.go` asserts `require.JSONEq` between
  `mcpdoll <cmd> --output json` and the corresponding endpoint, for five
  operations. They marshal the same structs, so this is true by construction —
  the test exists to keep it that way.
- `internal/api/schema_test.go` holds every spec schema against the Go struct
  that produces it: field names must match exactly, every schema needs a type
  and every type a schema, a `required` field may not be `omitempty`, and every
  `$ref` must resolve. It found **sixteen** drifted schemas when first written.

The TypeScript half is closed too, in two directions:

- `web/src/lib/types.ts` is **generated** from the spec by `tools/gents`, so a
  renamed field cannot compile on the console side and read as `undefined` at
  runtime. The generator refuses on any construct it does not understand rather
  than emitting `unknown`, because a file that compiles and lies is worse than a
  build failure.
- `internal/api/console_test.go` holds the *paths* `web/src/lib/api.ts` fetches
  against the spec, both ways. It was written after `listTenants` moved
  server-side and the console kept calling the old URL: parity was green, the
  types compiled, and the screen 404'd.

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
