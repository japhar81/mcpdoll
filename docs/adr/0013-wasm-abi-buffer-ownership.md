# ADR 0013: The WASM ABI and Buffer Ownership

## Status

Accepted

## Context

A WASM plugin and its host share nothing but a block of linear memory and a few
exported functions. Everything else — who allocated a buffer, who may read it,
when it may be reused — has to be a convention, and the convention has to be
exactly right, because the failure mode when it is not is *silent memory
corruption* rather than an error.

This ADR exists because that failure mode was observed during development, and it
looked like this:

```
input:  "hello from the host"
output: {"echo":"       om the h @"}
```

No panic, no error, no log line. The host wrote the payload, the guest's garbage
collector reclaimed the buffer because nothing in Go referenced it any more, and
the guest read whatever landed in that memory afterwards. In production this
would be a redaction plugin scanning garbage and finding nothing to redact, and
reporting success.

## Decision

**Three exports, and the guest owns every buffer until the host frees it.**

```
mcpdoll_alloc(size i32) -> i32        allocate `size` bytes, PIN them, return the pointer
mcpdoll_free(ptr i32)                 unpin
mcpdoll_invoke(ptr i32, len i32) -> i64   run; returns (resultPtr << 32) | resultLen
```

The host's protocol:

1. `ptr = alloc(len(input))`
2. write the input at `ptr`
3. `packed = invoke(ptr, len(input))`
4. read `resultLen` bytes at `resultPtr`, **copying them out**
5. `free(ptr)` and `free(resultPtr)`

### Pinning is the load-bearing part

`alloc` must keep a live reference to the buffer it returns — in the SDK, an
entry in a package-level map keyed by the pointer. Without it, a Go-compiled
guest's collector is entitled to reclaim the memory the instant `alloc` returns,
because a raw pointer held by the host is not a reference the collector can see.

`free` exists solely to release that pin. It is not a memory allocator's free;
it is "the host is done looking at this".

### The host copies before returning

The slice the host reads from guest memory *aliases* that memory, which the next
invocation will reuse. Copying is one allocation on a path that has already done
several, and not copying is a data race with no diagnostic.

### The SDK owns the ABI so no plugin author does

`plugins/sdk` implements all three exports behind a build tag, and a plugin
author writes one function:

```go
func handle(inv *sdk.Invocation) *sdk.Verdict
```

The rest of the SDK has no build tag, so a plugin author can unit-test their
handler as ordinary Go.

### Registration happens in `init`, not `main`

`-buildmode=c-shared` for `wasip1` produces a *reactor* module: the host
instantiates it with `_initialize`, which runs package initialization and then
returns, leaving the module resident to be called. **`main` is never run.**

A plugin that registers its handler in `main` therefore compiles, loads, passes
the host's ABI check, and then allows every request — because the handler was
never installed. And "allow" is the correct behaviour for an abstaining plugin,
so nothing looks wrong: the only symptom is a security control that quietly does
nothing.

This was also observed during development, which is why `sdk.Handle` carries a
warning in its doc comment, why both first-party plugins register in `init`, and
why `dispatch` returns the distinctive reason `"plugin registered no handler"`
rather than a bare allow.

### Bounds

- `MaxPayloadBytes` (8 MiB) applies in **both** directions. A guest returning a
  gigabyte of "verdict" would exhaust the host on a path meant to be bounded.
- The host verifies all three exports at **load** time, so a malformed plugin
  fails a deploy rather than a user's tool call.
- `MemoryLimitPages` caps a guest's linear memory, so a runaway allocation gets a
  failed `memory.grow` rather than taking the host with it.

## Alternatives considered

- **Pass data through WASI stdin/stdout.** Rejected: it requires a command module
  (which exits after `main`), so every invocation would pay full instantiation,
  and it grants the guest stdio it otherwise has no need for.
- **Host-exported callbacks the guest calls to fetch its input.** Rejected: each
  host import is a capability the guest gains, and the whole argument for WASM is
  that the import namespace is empty. Zero imports is a property worth protecting.
- **A fixed static buffer at a known offset.** Simpler, and rejected: it caps the
  payload at whatever was chosen at build time, and makes concurrent invocations
  on one instance silently corrupt each other rather than merely being disallowed.
- **Let the host allocate inside guest memory directly.** Not possible without
  understanding the guest's allocator, which differs per source language and
  defeats the point of a language-neutral ABI.
- **Reference counting instead of explicit free.** More machinery for a protocol
  with exactly one owner and one borrow.

## Consequences

- The ABI is a contract MCPDoll owns and versions. Changing it breaks every
  compiled plugin, so it should change roughly never.
- A plugin in another language must implement the same three exports and the same
  pinning discipline. The rules are stated here and in
  `docs/architecture/plugin-authoring.md` rather than left to be inferred from
  the Go SDK.
- Module instances are pooled, because a Go-compiled guest costs far more to
  instantiate than to call. A guest that trapped or overran its deadline is
  discarded rather than returned to the pool: its internal state is undefined,
  and reusing it would carry one request's corruption into the next.
- The tests compile the real plugin and run it through the real host. That is
  slower than a stub, and it is the only way to catch this class of bug — a mock
  has no garbage collector to get this wrong.
