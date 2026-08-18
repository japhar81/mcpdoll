# MCPDoll — build progress

> Recovery point. If context is lost, read this first, then `docs/deferred.md`,
> then `docs/adr/`.

**Current phase:** 1 — skeleton that serves real MCP

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
| `Mcp-Method` / `Mcp-Name` validated, `-32020` on mismatch | ✅ `CodeHeaderMismatch = -32020`, enforced in `validateMcpHeaders` | `mcp/shared.go:388`, `mcp/streamable.go:1580` |
| List results carry `ttlMs` / `cacheScope` | ✅ embedded `Cacheable` struct; defaults to `"public"` | `mcp/protocol.go:1168` |
| Stateless mode forbids server→client requests | ✅ documented on the `Stateless` field | `mcp/streamable.go:134` |
| MRTR is `resultType: "input_required"` | ✅ `resultTypeInputRequired`; server returns `InputRequests` + `RequestState` | `mcp/protocol.go:28`, `mcp/mrtr.go` |

Consequences for the design, already decided:

- The edge does **not** reimplement header validation or version negotiation —
  the SDK does both. The conformance suite asserts the behaviour end to end
  rather than trusting it.
- Dynamic, snapshot-driven tools use `(*Server).AddTool(t *Tool, h ToolHandler)`
  — the untyped variant. The generic `AddTool[In, Out]` requires compile-time
  Go types and is unusable for definitions that arrive at runtime.
- `NewStreamableHTTPHandler(getServer func(*http.Request) *Server, …)` is the
  seam where audience resolution happens.

## Completed slices

### Slice 1 — canonicalization + content-addressed digests
`internal/platform/canonical` · surfaces: none (library)

RFC 8785 JSON Canonicalization Scheme, `$defs` resolution, and the dual
digest scheme (full + semantic) that drift classification is built on.

- ECMAScript `Number::toString` formatting — Go's `%g` disagrees with JCS at
  the exponent-notation thresholds, so it is implemented from the spec.
- UTF-16 code-unit key ordering, which differs from Go's byte ordering for
  anything above the BMP.
- Duplicate keys and trailing content rejected (identity cannot be
  parser-dependent).
- External `$ref` rejected as a security control (SSRF + unsigned meaning).
- Depth (64) and node (20 000) budgets; the node budget catches the
  exponential `$defs` expansion bomb that the depth budget misses.
- 87.6% statement coverage; golden test pins the exact canonical bytes.

ADR: [0005](docs/adr/0005-content-addressed-tool-definitions.md)

## In flight

Phase 1 remainder: config/logging/IDs, OTEL wiring, the MCP edge, fixture
backends, conformance suite, OpenAPI + tri-surface + parity check.

## Blockers

- **`mcp-gateway-architecture.md` was never supplied.** The brief names it as
  the design of record; it is not in the repo, the home directory, or
  Downloads. Proceeding from the build brief itself, which is detailed enough
  to build from. Every design decision that the missing document might have
  settled is instead recorded as an ADR, so the divergence is reviewable when
  the document appears.

## Decisions made

- No Tailwind — RAGdoll does not use it, and adding it would make the console
  look *less* like the family it is meant to join. See ADR 0001.
- Go module path `github.com/mcpdoll/mcpdoll`, Go 1.24 language level
  (toolchain present is 1.26.4).
