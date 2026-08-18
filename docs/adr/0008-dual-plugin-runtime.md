# ADR 0008: WASM + gRPC Plugin Runtimes, and Why Not `buildmode=plugin`

## Status

Accepted

## Context

MCPDoll runs third-party code inside the request path of every tool call in the
organization. A plugin can deny a request, rewrite arguments before they reach a
backend, and read results before a model does. The runtime choice is therefore a
security decision first and an ergonomics decision second.

Three properties matter, in this order:

1. **Containment.** A plugin must not be able to reach the network, the
   filesystem, or another plugin's state.
2. **Termination.** A plugin that hangs must not hang the gateway.
3. **Failure isolation.** A plugin that crashes must not crash the gateway.

## Decision

**Two runtimes: WASM via `wazero` as the default, gRPC for the plugins that
genuinely cannot be pure. Never Go's `plugin` package.**

### WASM is the default, and the reason is structural

A WASM module instantiated with no host imports beyond WASI **cannot** open a
socket, read a file, or see a clock — not because a policy forbids it, but
because those functions do not exist in its import namespace. wazero's
`wasi_snapshot_preview1` has no sockets at all, and the module config grants no
filesystem, no environment, and no stdio.

That is a categorically different guarantee from "the sandbox is configured to
deny it". There is no misconfiguration that opens it, and no bug in our policy
code that leaks it, because there is no policy code.

wazero specifically: pure Go, no cgo, so the gateway remains a static binary and
cross-compiles.

**What WASM does not give us:** wazero has no instruction metering. The manifest
carries a `fuel_limit` field and this host does not enforce it; termination is
enforced by the engine's per-plugin *deadline*, via
`WithCloseOnContextDone`. That is a real limitation and it is load-dependent —
the same plugin may complete on a quiet host and be cut off on a busy one. The
host logs a warning when a manifest sets `fuel_limit`, rather than letting an
operator believe a limit is in force that is not. Recorded in
`docs/deferred.md`.

### gRPC exists for the plugins that cannot be pure

The LLM guard has to reach a model. No amount of sandboxing changes that, so
pretending otherwise would just mean a WASM plugin with a network escape hatch —
which is strictly worse than an honest out-of-process one.

A gRPC plugin gives up the structural guarantee and gains process isolation: it
crashes on its own, and it can carry a runtime the WASM host cannot host. The
channel is bidirectional streaming, because the hot path cannot afford a
connection setup per hook and because a guard benefits from keeping model
connections warm.

**The distinction is recorded in the manifest, not hidden.** A reviewer looking
at a plugin list can see immediately which plugins have network access, because
`runtime: grpc` is exactly that statement.

### Never `buildmode=plugin`

Go's `plugin` package fails all three properties:

- **No containment whatsoever.** A `.so` loaded this way runs with the host's
  full privileges, in the host's address space, with access to every package the
  host imports. It can call `os.Exit`, dial the network, or reach into unexported
  state via linkname. "Sandbox" is not a word that applies.
- **No termination.** A plugin goroutine that spins cannot be stopped.
- **No failure isolation.** A panic or a segfault takes the gateway with it.

And beyond safety: it requires an exact toolchain and dependency-version match
between host and plugin, does not work on all platforms, and cannot be unloaded —
so a plugin update requires a restart, which defeats the point of a hot-swappable
snapshot.

## Alternatives considered

- **WASM only.** Rejected because the guard is a real requirement and it needs a
  model. Forcing it into WASM would mean granting the WASM host a network import,
  which would remove the structural guarantee for *every* plugin to accommodate
  one.
- **gRPC only.** Rejected: it makes every plugin a deployment unit with a
  process, a port, and a lifecycle. For a plugin that is fundamentally a
  regular-expression pass over a string, that is a great deal of operational
  weight for no benefit, and it gives up containment for plugins that did not
  need to give it up.
- **Wasmtime or Wasmer via cgo.** Both have instruction metering, which wazero
  lacks and which this ADR concedes as a real gap. Rejected because cgo costs the
  static binary and complicates cross-compilation for a data plane meant to
  deploy anywhere — and because the deadline does terminate a runaway plugin, just
  less reproducibly. Worth revisiting if fuel becomes a practical problem rather
  than a theoretical one.
- **Lua or Starlark embedding.** Lighter than WASM and genuinely sandboxed.
  Rejected because it forces plugin authors into a second language and gives up
  the ecosystem — a WASM plugin can be written in Go, Rust, or anything else that
  targets wasip1, using its normal libraries.
- **Sidecar processes over HTTP.** Effectively gRPC with a worse contract: no
  streaming, no schema, per-request connection overhead.

## Consequences

- Two hosts to maintain, and a `pipeline.Host` interface that must stay honest
  about the difference. The interface is deliberately minimal — `Invoke` and
  `Close` — so the engine cannot accidentally depend on a runtime detail.
- A WASM plugin cannot log. It returns annotations instead, which end up in the
  audit trail — arguably better than a log line, since it is attached to the
  request it describes.
- Go-compiled WASM modules are large (~2–4 MB) because they carry the Go runtime.
  Compiled modules are cached on disk, so the cost is paid at deploy rather than
  per restart. A plugin written in Rust or TinyGo would be far smaller; the ABI
  does not care which language produced the module.
- The WASM ABI (`mcpdoll_alloc` / `mcpdoll_free` / `mcpdoll_invoke`) is a
  contract MCPDoll now owns. The SDK implements it so no plugin author writes it,
  which matters more than it sounds: getting it wrong produces silent memory
  corruption rather than an error. See ADR 0013.
