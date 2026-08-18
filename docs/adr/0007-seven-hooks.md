# ADR 0007: Seven Hooks, and Why Not More

## Status

Accepted

## Context

The pipeline needs extension points. The question is how many, and the pressure
runs entirely in one direction: every proposed feature suggests a new hook, each
one individually reasonable, and nobody ever proposes removing one.

The cost of an extra hook is not the code that dispatches it. It is:

- **A place plugin authors must reason about.** Every hook multiplies the
  "where should this run?" question, and a plugin registered at the wrong one is
  a subtle bug — it works in testing and misses the case that mattered.
- **A division of the request budget.** The total plugin budget is fixed by the
  latency a client will tolerate. More hooks means thinner slices, or a budget
  that only holds if most hooks are empty.
- **A new interaction surface.** N hooks with mutating plugins is N places two
  plugins can disagree about ordering.

## Decision

**Exactly seven hooks. Adding an eighth requires an ADR.**

| Hook | When | What it is for |
|---|---|---|
| `ON_REQUEST` | A request arrives, before anything is known about it | Rate limiting, request-shape validation, correlation |
| `ON_IDENTITY` | The principal is resolved | Claim enrichment, group expansion, tenant resolution |
| `ON_CATALOG` | A `tools/list` result is assembled | Entitlement filtering, progressive disclosure |
| `ON_TOOL_CALL` | Before dispatch to a backend | Authorization, argument validation, confirmation, header mapping |
| `ON_TOOL_RESULT` | After a backend responds | Redaction, injection detection, result shaping |
| `ON_RESPONSE` | Before the response leaves the gateway | Final shaping, response-level annotation |
| `ON_AUDIT` | After the response, off the hot path | Enrichment for the audit record, export |

The set is not arbitrary. It is the *lifecycle*: a request arrives, acquires an
identity, discovers or invokes, gets a result, becomes a response, and is
recorded. Each hook is a point where the request has meaningfully changed shape,
which is the only defensible criterion for "somewhere a plugin might need to
act".

### The two that carry the weight

`ON_TOOL_CALL` and `ON_TOOL_RESULT` are where nearly every real plugin lives,
and the asymmetry between them is worth stating. `ON_TOOL_CALL` sees what the
*model* decided to do — authorization belongs there. `ON_TOOL_RESULT` sees what
the *world* said back — and that is where content nobody reviewed enters the
context window, which is why cross-server injection is a result-hook problem
first and a call-hook problem second.

### Why not fewer

Three hooks (before, after, audit) was considered. It fails on `ON_CATALOG`:
filtering a catalog is a different operation from authorizing a call, with a
different payload shape and different caching consequences (ADR 0011), and
folding them together would mean every catalog plugin branching on "is this a
list or a call".

### Enforcement

The set is closed in three places, so it cannot expand by accident:

- `snapshotpb.Hook` — an unlisted value does not exist.
- `registry.ParseHook` — an unrecognised name in a document is a hard error, not
  a pass-through.
- `registry.HookNames` — the canonical execution order, asserted by a test that
  counts seven.

## Alternatives considered

- **A generic middleware chain**, where a plugin declares an arbitrary insertion
  point. Rejected: it makes ordering a plugin-author concern instead of an
  operator one, and it makes the request budget impossible to divide sensibly.
  It also makes the trace unreadable — the console's waterfall depends on a
  fixed set of phases to render against.
- **Per-method hooks** (`before_tools_list`, `before_tools_call`, …). Rejected:
  it grows with the protocol, so every MCP method addition becomes a pipeline
  change, and most plugins would register at all of them anyway.
- **Two hooks — before and after — with the payload carrying which phase.**
  Rejected: it moves the dispatch into every plugin. Every plugin author writes
  the same switch, and gets it subtly wrong in different ways.
- **Leaving the set open, with a documented convention.** Rejected: a convention
  is not a constraint. The point of this ADR is that the eighth hook has to be
  *argued for*, in writing, against the costs above.

## Consequences

- A plugin that wants to act somewhere else has to fit one of the seven, or make
  the case for an eighth in an ADR. That friction is the intended design.
- `ON_REQUEST` and `ON_RESPONSE` currently have no first-party plugins. They are
  kept because they complete the lifecycle and because rate limiting and
  response shaping are obvious near-term needs — but a reviewer is entitled to
  note that they are unproven.
- The seven names appear in the registry document, the audit trail, the metrics,
  and the console. Renaming one is a breaking change to all four, so the names
  were chosen to be boring and durable.
