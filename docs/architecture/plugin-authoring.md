# Writing an MCPDoll plugin

A plugin is one function. Everything else — the ABI, the JSON, the buffer
management — is the SDK's problem.

```go
func handle(inv *sdk.Invocation) *sdk.Verdict {
    if inv.Tool != nil && inv.Tool.EffectClass == "destructive" {
        return sdk.DenyVerdict("destructive tools are disabled on this gateway")
    }
    return sdk.AllowVerdict()
}
```

This guide covers what you need to write a real one.

---

## Before you start: is it a WASM plugin?

**Yes**, if it computes over the data it is given. Pattern matching,
authorization from claims, argument validation, result shaping.

**No**, if it needs the network — a model, an external policy service, a
database lookup. That is a gRPC plugin, which gives up the containment guarantee
below and is correspondingly harder to justify. (The gRPC host is not implemented
in this build; see `docs/deferred.md`.)

A WASM plugin **cannot reach the network**. Not because a policy forbids it, but
because the functions do not exist in its import namespace. That is the reason
WASM is the default, and it is worth staying inside if you can.

---

## Repository layout

A plugin is an ordinary Go module. Nothing about it is special except the build
command and the one import.

```
my-plugin/
├── go.mod
├── main.go          # init() registers the handler; main() is empty
├── main_test.go     # ordinary Go tests, no WASM needed
└── Makefile
```

```
// go.mod
module example.com/my-plugin

go 1.24

require github.com/mcpdoll/mcpdoll v0.1.0
```

Only `github.com/mcpdoll/mcpdoll/plugins/sdk` is imported. It has no dependencies
of its own beyond the standard library, so it compiles for `wasip1` cleanly.

---

## The handler

```go
package main

import "github.com/mcpdoll/mcpdoll/plugins/sdk"

// Registration goes in init, NOT main. See "The one thing that will bite you".
func init() { sdk.Handle(handle) }

func main() {}

func handle(inv *sdk.Invocation) *sdk.Verdict {
    switch inv.Hook {
    case "on_tool_call":
        return onCall(inv)
    case "on_tool_result":
        return onResult(inv)
    default:
        return sdk.AllowVerdict()
    }
}
```

### What you get

| Field | When it is set | What it is |
|---|---|---|
| `Hook` | always | which of the seven hooks |
| `Shadow` | always | your verdict will be recorded, not acted on |
| `Principal` | always | subject, groups, claims |
| `Audience` | always | the endpoint slug |
| `Tool` | call and result hooks | name, effect class, backend, digest |
| `Arguments` | `on_tool_call` | the call's arguments |
| `Result` | `on_tool_result` | the backend's result |
| `Catalog` | `on_catalog` | the tool list |
| `Config` | always | your manifest's `config` block |
| `InputResponses`, `PluginState` | an MRTR retry | what the human answered |

Everything is restricted to your manifest's declared `reads` before it reaches
you. A plugin that did not declare `principal` sees an empty one.

### What you return

Five decisions, and no more:

```go
sdk.AllowVerdict()                       // proceed — also how you abstain
sdk.DenyVerdict("reason the model sees") // refuse
sdk.MutateVerdict("what you did", patch) // proceed with an RFC 6902 patch
sdk.AnnotateVerdict(map[string]any{...}) // proceed, attach audit metadata
&sdk.Verdict{Decision: sdk.Defer, ...}   // ask the human first
```

**Allow is how you abstain.** "I have no opinion" and "I approve" are
deliberately the same answer, so a plugin that cannot decide cannot block.

**A denial's reason reaches the model** and the audit trail. Write it for the
person who will have to explain the refusal, not for yourself.

---

## Mutation

A patch is RFC 6902, and it must stay inside your manifest's `writes`:

```go
return sdk.MutateVerdict("redacted a card number", []sdk.PatchOp{
    sdk.Replace("/result/content/0/text", cleaned),
})
```

Rules the host enforces, so you do not have to remember them:

- A patch touching anything outside `writes` is **rejected whole** — not
  partially applied. Both `path` and `from` are checked.
- Array indices are not scope boundaries: `result.content` covers
  `/result/content/0/text`.
- `move` and `copy` are not supported. Emit a `remove` plus an `add`.
- The whole document cannot be replaced.
- At most 256 operations.
- The result is re-canonicalized, so you cannot change key ordering.

Patch narrowly. `/result/content/0/text` reads well in the console's diff;
replacing `/result` does not, and would let you drop `isError` by accident.

---

## Configuration

Whatever your manifest's `config` block holds arrives as `inv.Config`:

```yaml
plugins:
  - id: plg_mine
    name: my-plugin
    config:
      threshold: 0.8
      allowed_groups: [platform, security]
```

```go
threshold := inv.ConfigFloat("threshold", 0.5)
groups := inv.ConfigStrings("allowed_groups")
```

**A bad configuration should allow, not deny.** Whether a broken check ought to
block traffic is the *engine's* decision, made per effect class in the manifest's
`failure_policy` — it is not yours to make. Return an allow with the error in an
annotation so it reaches the audit trail:

```go
if err != nil {
    return &sdk.Verdict{
        Decision:    sdk.Allow,
        Reason:      "my-plugin: " + err.Error(),
        Annotations: map[string]any{"config_error": err.Error()},
    }
}
```

---

## Testing

The SDK's types have no build tag, so your handler is ordinary Go:

```go
func TestDeniesDestructive(t *testing.T) {
    v := handle(&sdk.Invocation{
        Hook: "on_tool_call",
        Tool: &sdk.Tool{Name: "bil.void_invoice", EffectClass: "destructive"},
    })
    if v.Decision != sdk.Deny {
        t.Fatalf("got %s, want deny", v.Decision)
    }
}
```

Run it with `go test ./...` — no WASM, no host, no gateway.

Then test it *compiled*, because the ABI boundary is where the interesting bugs
live. MCPDoll's own plugin tests compile the real module and run it through the
real host for exactly this reason.

---

## Building and deploying

```
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o my-plugin.wasm .
```

`-buildmode=c-shared` produces a **reactor** module — one that initializes and
waits to be called. Without it you get a command module that runs `main` and
exits, which cannot be invoked afterwards.

Then register it:

```yaml
plugins:
  - id: plg_mine
    name: my-plugin
    version: 1.0.0
    runtime: wasm
    hooks: [on_tool_call]
    priority: 50
    budget: 50ms

    reads: [principal, tool, arguments]
    writes: [arguments]

    # Every plugin starts here. Promote only after reading its shadow diffs.
    rollout: shadow

    artifact_ref: file:///opt/mcpdoll/plugins/my-plugin.wasm
    artifact_digest: "sha256:..."   # from `shasum -a 256 my-plugin.wasm`

    failure_policy:
      read: open
      destructive: closed
```

The **digest is checked before load**. A mismatch refuses the plugin — which is
what makes a swapped artifact fail closed, and which means the digest has to be
updated whenever the plugin is.

### Rollout

`shadow` → `canary(%)` → `enforce`, and there is no reason to skip a step.

In shadow your plugin runs, its verdict is recorded, and nothing changes. That
is the only way to find out what it *would* have done to real traffic before it
does it:

```
grep "shadow verdict diverged" mcpdoll-dp.log
```

Canary membership is stable per request, so a request that your plugin denied at
one hook is in the sample at every hook. A half-enforced request would be
impossible to reason about.

---

## The one thing that will bite you

**Register in `init()`, not `main()`.**

A reactor module never runs `main`. A plugin that registers there compiles,
loads, passes the host's ABI check — and then allows every request, because the
handler was never installed.

And nothing looks wrong: allowing is correct behaviour for an abstaining plugin.
The only symptom is a security control that quietly does nothing.

If you suspect it, look for this in the audit trail:

```
"plugin registered no handler"
```

---

## Budgets and failure

Your plugin has a deadline (`budget_ms`, capped by the hook's remaining budget).
Exceeding it is a failure, and the engine applies your manifest's
`failure_policy` for the request's effect class.

Return *something* before the deadline rather than being cancelled. A
cancellation is recorded as a skip and loses whatever you had computed; a
low-confidence allow at least tells the audit trail you looked.

Consecutive failures open your plugin's circuit breaker and it is skipped until
a cooldown elapses. An invalid verdict — a mutate with no patch, an unknown
decision — counts as a failure, so a plugin that always returns garbage is
eventually taken out of the path rather than invoked forever.

---

## Reference

- The ABI and why buffer pinning matters: `docs/adr/0013-wasm-abi-buffer-ownership.md`
- Why WASM and gRPC and never `buildmode=plugin`: `docs/adr/0008-dual-plugin-runtime.md`
- Why exactly seven hooks: `docs/adr/0007-seven-hooks.md`
- Working examples: `plugins/redact` and `plugins/entitlements`
