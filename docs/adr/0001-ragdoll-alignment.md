# ADR 0001: Alignment with RAGdoll

## Status

Accepted

## Context

MCPDoll is a sibling product to [RAGdoll](../../../ragdoll), an enterprise
multi-tenant RAG pipeline platform. The two will be sold, deployed, and
operated by the same teams, often side by side. A user who has learned
RAGdoll's console should recognise MCPDoll's on sight, and an operator who
has automated against RAGdoll's CLI should find MCPDoll's CLI familiar.

RAGdoll is pure TypeScript (Node + Fastify + React). MCPDoll's data plane sits
on the hot path of every tool call for every agent in the enterprise, needs
tight latency control, a WASM host, and a long-lived in-process snapshot with
lock-free reads. That points at Go for the backend. So the alignment cannot be
"share the code" — it has to be "share the design, port the patterns".

This ADR records exactly what was taken, what was deliberately changed, and
why, so that later divergence is a decision rather than a drift.

## Decision

### Taken directly (UI)

The console reuses RAGdoll's design language verbatim rather than inventing a
new one:

| Element | Value carried over |
|---|---|
| Shell | `.app-shell` 220px sidebar + content grid, dark `#0f172a` sidebar on `#f8fafc` body |
| Type | Inter, 13px body, 11px uppercase-tracked group labels |
| Accent | `#2563eb` primary, `#1d4ed8` hover, `#93c5fd` on-dark links |
| Neutrals | `#111827` text, `#64748b` muted, `#e5e7eb`/`#cbd5e1` borders, `#f1f5f9` table head |
| Status | `.status-*` and `.badge-*` pills — green `#dcfce7`/`#166534`, red `#fee2e2`/`#991b1b`, blue `#dbeafe`/`#1e40af`, amber `#fef3c7`/`#92400e` |
| Components | `Screen`, `Table`/`table.grid`, `.panel`, `.card`, `.modal`, `.empty-state`, `.metric`, `.field`, `.chip`, `.link-btn`, `.toolbar` |
| Nav | Grouped sidebar (`Build` / `Operate` / `Govern` / `Settings`), inline 14px stroked SVG icons, no icon-font dependency |
| Patterns | Cmd-K command palette, help drawer, tooltip provider, live-status badge, `screen-body--fill` for tall grids |
| Graph | React Flow with `.rf-node` / `.rf-title` / `.rf-sub` / `.rf-handle-port` node chrome |
| Data | TanStack Query, `retry: false`, `refetchOnWindowFocus: false` |

MCPDoll's pipeline visualiser and bundle composer are ports of RAGdoll's
pipeline builder: the same React Flow node card, port-handle, and
palette-drag idiom.

### Taken directly (repo conventions)

- Monorepo with the app/package split; `web/` is a Vite workspace with its own
  `tsconfig`, excluded from the root typecheck.
- ADRs as `docs/adr/NNNN-kebab-title.md` with Status / Context / Decision /
  Consequences. MCPDoll adds an **Alternatives considered** section.
- `docs/` split into `architecture/`, `operations/` (RAGdoll's `admin/`),
  `api/`, `cli/`.
- Makefile as the single entry point: `make dev`, `make test`, `make obs`.
- LGTM observability: the `grafana/otel-lgtm` all-in-one container, Grafana on
  `:3300`, provisioned datasources with Tempo↔Loki↔Prometheus correlation,
  dashboards as JSON under `deploy/observability/`.
- Postgres 16 + Redis 7 in compose with the same healthcheck shape.
- CI as a single `build-and-test` job with explicit per-suite steps.

### Ported, not copied (backend TS → Go)

| RAGdoll (TypeScript) | MCPDoll (Go) | Why the shape changed |
|---|---|---|
| BullMQ queues on Redis | `river` on Postgres | Control-plane jobs (admission runs, drift scans, probes) must be transactional with the registry writes that trigger them. A Redis queue cannot join that transaction, so a publish could commit while its admission job was lost. River enqueues in the same `pgx` transaction. Job handlers are plain Go workers taking `context.Context`; BullMQ's sandboxed-processor and job-events semantics are **not** reproduced. |
| Service classes + module registry | Packages with explicit constructor wiring | No DI container and no global singletons. Every dependency is a parameter to a `New…` constructor, which makes the data plane's startup order explicit and the test doubles trivial. |
| `casbin` policy engine | Compiled policy in the snapshot | Data-plane policy evaluation must be allocation-light and must not consult a database. Policies are authored in the control plane and compiled into the signed snapshot. |
| Tenant scoping via query helpers | Same model, `sqlc`-generated queries | The *model* ports directly: every tenant-scoped query takes the tenant as a bound parameter, never string interpolation, and the role check sits at the handler boundary. |
| `packages/observability/index.ts` lazy OTel init | `internal/observability` | Same "app code is telemetry-agnostic" rule (RAGdoll ADR 0007) — packages take an interface, not the OTel SDK. |
| Zod schemas at the API boundary | OpenAPI 3.1 + `oapi-codegen` | See ADR 0004: MCPDoll needs one contract to generate a Go server, a Go client for the CLI, and a TypeScript client for the console. |

### Deliberate divergences

1. **No Tailwind.** The build brief asked for "Tailwind/theme config". RAGdoll
   does not use Tailwind — it uses a single hand-written `styles.css` with
   literal hex values. Introducing Tailwind would have produced a console that
   *did not* look like it came from the same family, which is the opposite of
   the requirement. MCPDoll keeps the plain-CSS approach but lifts the literals
   into CSS custom properties (`--mcpd-*`) in a `:root` block, so the palette
   is nameable without changing the rendered result.

2. **No SVAR Grid.** RAGdoll uses `@svar-ui/react-grid` for its tall data
   grids. MCPDoll's heaviest tables (audit search, drift queue) ship with the
   plain `table.grid` component. This is a deferral, not a rejection — see
   `docs/deferred.md`.

3. **Ed25519 snapshot signing has no RAGdoll analogue.** RAGdoll ships pipeline
   specs unsigned because the control plane and runtime share a trust domain.
   MCPDoll's data plane is deliberately able to run where the control plane
   cannot be reached or trusted, so the snapshot is signed. See ADR 0009.

4. **RAGdoll's live-events bus (ADR 0015) is not ported.** MCPDoll's console
   polls. Server-sent events across a control plane that may be
   horizontally scaled needs a broker, and `ttlMs`-driven polling is adequate
   for v1. Recorded in `docs/deferred.md`.

## Alternatives considered

- **Write the backend in TypeScript to share code with RAGdoll.** Rejected:
  the data plane needs a WASM host with fuel metering (`wazero`), lock-free
  snapshot swaps, and predictable tail latency under a per-request plugin
  budget. Node gives none of those cheaply, and the official MCP Go SDK is the
  reference implementation for the 2026-07-28 stateless transport.
- **Extract RAGdoll's CSS into a shared npm package consumed by both.**
  Rejected for v1: it couples two products' release cycles for a file that
  changes rarely. Revisit if a third product appears.
- **Generate the console from the OpenAPI spec (e.g. a CRUD scaffolder).**
  Rejected: the screens that matter — the request-trace waterfall, the bundle
  composer, the drift diff — are not CRUD, and a scaffolder would produce a
  console that looks nothing like RAGdoll's.

## Consequences

- A RAGdoll user recognises MCPDoll's console immediately; the two can be
  demoed in one session without visual whiplash.
- The CSS is duplicated between the two repos. A palette change must be applied
  twice. Accepted, and cheap, because the palette is now tokenised in one
  `:root` block per repo.
- Go and TypeScript backends cannot share code, so the *patterns* are the only
  thing kept in sync. Each ported pattern is documented in the table above so a
  reviewer can check the correspondence rather than guess at it.
- River's Postgres queue means one fewer moving part than RAGdoll's
  Redis+NATS split, at the cost of putting queue load on the primary database.
  Acceptable: control-plane job volume is orders of magnitude below RAGdoll's
  ingestion volume.
