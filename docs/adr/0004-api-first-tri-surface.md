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

**One contract, three surfaces held against it, and a build gate that fails on
divergence.**

### `api/openapi.yaml` is the source of truth

Every operation is defined there once.

**What was built diverges from what this ADR first proposed, and the divergence
is deliberate.** The original plan was to generate the Go server types, a Go
client for the CLI, and a TypeScript client for the console with `oapi-codegen`
and `openapi-typescript`. What shipped instead:

| Artifact | How | Why |
|---|---|---|
| Go wire types (`internal/api`) | hand-written, single-sourced | see below |
| Go server handlers | hand-written over those types | |
| CLI output types | **the same structs** | |
| TypeScript types (`web/src/lib/types.ts`) | hand-written | generation deferred |
| `web/src/routes.gen.ts` | generated from the router | |

The Go change is an improvement on the plan, not a shortcut. Generating a
*server* type and a separate *client* type produces two definitions that a
generator keeps in agreement — but only as long as both are regenerated
together. Instead the HTTP server and the CLI marshal **the same struct**, so
`mcpdoll registry show --output json` and `GET /api/v1/registry` return
identical bytes by construction rather than by discipline.
`internal/cli/parity_test.go` asserts exactly that, with `require.JSONEq`,
for five operations.

The TypeScript half is the honest weak point: `web/src/lib/types.ts` is a second
definition of the same shapes and can drift. Generating it is recorded in
`docs/deferred.md`.

To keep the spec from becoming decorative in the meantime,
`internal/api/schema_test.go` holds it against the Go structs: every schema must
name a Go type and every Go type a schema; JSON field names must match exactly;
a field marked `required` may not be `omitempty`; and every `$ref` must resolve.
When that check was first written it found sixteen drifted schemas, which is
roughly what one would expect of a spec written before the implementation
settled — and precisely the drift that would otherwise have surfaced as
`undefined` in a browser.

**The CLI does not proxy through the API for local operations.** Anything that
reads a file — validating a registry, inspecting or verifying a snapshot,
building one, minting a key — the CLI does directly. This diverges from
"the CLI is an API client, always", and the reason is that
`mcpdoll registry validate` has to work in a pull-request check where no
control plane is running, and `mcpdoll snapshot build` has to run where the
signing key lives, which is deliberately not on a server reachable over HTTP.
The property that mattered — no privileged path an integrator cannot use — is
preserved: every one of those operations is also an API operation, and the
shared types make the two answers identical.

### Every operation declares its surfaces

```yaml
x-mcpdoll-surfaces:
  cli: "mcpdoll registry server publish"
  ui:  "/registry/servers/:id/publish"
```

### `make parity` is the gate

`tools/paritycheck` parses the spec, enumerates the cobra command tree via a
hidden `mcpdoll __commands --json` **run against the built binary**, reads the
route manifest **generated from the console's router**, and fails with a
specific list if:

- an operation has no CLI command, or
- an operation has no UI route, or
- a CLI or UI binding names an operation that does not exist.

The third case matters as much as the first two: it catches the renamed
operation whose CLI command still points at the old id, which is otherwise a
runtime 404 nobody notices until a user hits it.

All three inputs are real artifacts rather than hand-maintained lists. A list of
"commands we have" would drift from the commands users actually get, which is
the drift this exists to catch.

**The check is built early, not at the end.** Retrofitting it against fifty
operations produces a list of fifty violations, which gets suppressed.

In practice it caught three things on the commit that introduced it, none of
which any existing test would have found: `mcpdoll snapshot inspect` claimed an
`operationId` the spec did not define (the trace a rename leaves and nothing
else); `walkCommands` silently dropped any command that had subcommands, which
cost `mcpdoll registry servers` its binding; and an empty
`~/.mcpdoll/config.yaml` failed every command with "EOF".

## Alternatives considered

- **Hand-write the three surfaces and review carefully.** Rejected. This is the
  status quo everywhere it has been tried, and the failure is not a lack of
  care — it is that the cost of divergence is paid later, by someone else.
- **Generate the CLI and UI entirely from the spec.** Rejected. Generated CRUD
  is fine for CRUD, and MCPDoll's important screens are not CRUD: the request
  trace waterfall, the bundle composer with a live token meter, the drift diff.
  A generator would produce a console that works and that nobody wants to use.
  So: hand-write the *experience* and enforce *coverage* mechanically, rather
  than generating an implementation.
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
- `make generate` needs `protoc` and the two protoc-gen-go plugins on PATH, plus
  Node for the route manifest. The OpenAPI generators are not required, because
  nothing is generated from the spec — the spec is *checked against* the code
  instead. That is a weaker guarantee than generation for the TypeScript client
  and a stronger one for Go, and both halves are stated plainly above rather
  than implied.
- The schema check compares field names, not types or formats. A field whose
  Go type changed from `int` to `string` passes. This is a real limit; the
  failure it does catch — a renamed or dropped field — is the common one.
