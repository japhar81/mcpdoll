# ADR 0004: API-First Tri-Surface Enforcement

## Status

Accepted

## Context

Every capability MCPDoll has must be reachable three ways: the HTTP API, the
CLI, and the console. This is not a nicety. An operator automating a rollout
needs the CLI; an approver reviewing a publish needs the UI; a platform team
integrating MCPDoll into their own tooling needs the API. A feature available
in only one of them is a feature half the users cannot use.

The failure mode is well known and gravitational: features land in the API
because that is where the work is, the CLI follows for the ones someone
scripted, and the UI accumulates whatever was demoed. Six months later nobody
can say what is reachable from where. Good intentions do not survive this;
"UI later" always wins unless something mechanical prevents it.

## Decision

**One contract, three generated surfaces, and a build gate that fails on
divergence.**

### `api/openapi.yaml` is the source of truth

Every operation is defined there once. From it we generate:

| Artifact | Generator | Consumer |
|---|---|---|
| Go server interfaces and types | `oapi-codegen` | control-plane handlers |
| Go API client | `oapi-codegen` | the CLI |
| TypeScript client and types | `openapi-typescript` | the console |
| `docs/api/` | from the spec | humans |

Generation runs in `make generate` and CI runs `make verify-generated`, which
fails if the committed output is stale.

**The CLI is an API client and never touches the database.** That is what makes
the CLI a real test of the API: anything the CLI can do, an external integrator
can do, because the CLI has no privileged path.

### Every operation declares its surfaces

```yaml
x-mcpdoll-surfaces:
  cli: "mcpdoll registry server publish"
  ui:  "/registry/servers/:id/publish"
```

### `make parity` is the gate

`tools/paritycheck` parses the spec, enumerates the cobra command tree via a
hidden `mcpdoll __commands --json`, reads the generated React route manifest,
and fails with a specific list if:

- an operation has no CLI command, or
- an operation has no UI route, or
- a CLI or UI binding names an operation that does not exist.

The third case matters as much as the first two: it catches the renamed
operation whose CLI command still points at the old id, which is otherwise a
runtime 404 nobody notices until a user hits it.

**The check is built in phase 1, when there are two operations, not at the end.**
Retrofitting it against fifty operations produces a list of fifty violations,
which gets suppressed. Introducing it against two produces a green check that
stays green, and every subsequent operation faces the gate on the commit that
adds it.

## Alternatives considered

- **Hand-write the three surfaces and review carefully.** Rejected. This is the
  status quo everywhere it has been tried, and the failure is not a lack of
  care — it is that the cost of divergence is paid later, by someone else.
- **Generate the CLI and UI entirely from the spec.** Rejected. Generated CRUD
  is fine for CRUD, and MCPDoll's important screens are not CRUD: the request
  trace waterfall, the bundle composer with a live token meter, the drift diff.
  A generator would produce a console that works and that nobody wants to use.
  So: generate the *client*, hand-write the *experience*, and enforce coverage
  rather than implementation.
- **Code-first with a generated spec** (annotate Go handlers, emit OpenAPI).
  Rejected: it makes the Go server the source of truth, so the TypeScript client
  and the docs always trail an implementation that has already shipped. It also
  makes the spec's shape an artifact of Go's type system rather than a designed
  contract.
- **gRPC with generated gateways for all three.** Rejected for the *external*
  API: the console and third-party integrators want plain HTTP and JSON, and
  gRPC-Web adds a proxy hop and a debugging burden for no benefit here. gRPC is
  used where it earns its keep — snapshot distribution and the plugin host —
  both of which are internal, high-frequency, and streaming.
- **A parity check that warns instead of failing.** Rejected: a warning is a
  thing people learn to scroll past. The whole value is that it cannot be
  ignored.

## Consequences

- Adding an operation is more work up front: spec, regenerate, handler, CLI
  command, UI route. That is the intended cost, and it is paid by the person
  adding the feature rather than by the user who cannot reach it.
- The parity check enforces *existence*, not *quality*. A CLI command that
  merely dumps JSON satisfies it. Coverage is mechanical; usefulness is a
  review concern. This is a real limit and is stated here rather than implied.
- A spec change that renames an `operationId` breaks parity until the CLI and UI
  bindings are updated — which is the point, but it does mean renames are not
  free.
- `make generate` needs `oapi-codegen`, `openapi-typescript`, `protoc`, and the
  two protoc-gen-go plugins on PATH. Documented in `docs/developer/`, and CI
  installs them explicitly so a missing tool is a clear failure rather than a
  silently stale artifact.
