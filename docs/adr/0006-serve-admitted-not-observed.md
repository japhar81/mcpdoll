# ADR 0006: Serve Admitted Definitions, Never Observed Ones

## Status

Accepted

## Context

A gateway fronting 20+ independently-published MCP backends has a choice about
what to put in `tools/list`: what the backends currently report, or what was
reviewed and approved.

Proxying live output is the obvious implementation and it is wrong. The tool
description is a prompt. It is read by a model that will act on it, and it is
authored by whoever controls the backend — which, for an internal service
maintained by another team, is a large and shifting set of people. A backend
that redeploys with an extra sentence in a description has changed the
instructions given to every agent in the organization, with no review, no
audit trail, and no way to notice.

That is the entire attack in cross-server prompt injection: the payload does not
have to arrive in a tool *result*. It can arrive in the catalog.

## Decision

**The gateway serves definitions from the signed snapshot. Live backend output
is used only to detect drift.**

Concretely:

- `tools/list` is rendered from the snapshot. No request path calls a backend to
  build a catalog.
- A backend that changes a description produces a **drift event**, not a
  changed catalog.
- A backend that starts serving a tool that was never admitted does not have it
  appear. Ever.
- A backend that withdraws a tool keeps it listed for the grace window, with
  calls failing fast (ADR 0014).
- `tools/call` targets are resolved against the snapshot, so a call can only
  reach an admitted tool.

Per-backend `serving_mode` chooses what happens *to the backend* when it
diverges — `strict` quarantines, `advisory` records — but never what clients
see. There is no mode in which observed output is served.

The conformance suite pins this: `TestConformanceServesAdmittedNotObserved`
mutates a live backend's description and adds an un-admitted tool, then asserts
the client sees neither.

## Alternatives considered

- **Proxy live output.** Rejected: it hands catalog-shaped prompt injection to
  anyone who can deploy a backend, and makes the catalog unreviewable in
  principle.
- **Proxy live output, but scan it for injection on the way through.** Rejected
  as a hot-path design. Scanning a full catalog per `tools/list` is expensive and
  unbounded; worse, a scanner is a probabilistic filter standing where an
  authorization decision belongs. Description review belongs in admission, once,
  where a human can see the diff — which is why the LLM guard's `ON_ANALYZE`
  placement is an admission concern and not a serving one (ADR 0015).
- **Serve live output with a short cache.** Rejected: it is the same design with
  a delay, and the delay makes the injection window harder to reason about
  rather than smaller.
- **Auto-admit changes that look cosmetic.** Rejected, and this is the tempting
  one. "Only the description changed" is precisely the case that matters:
  description-only changes are exactly what an injection looks like. The
  cosmetic/structural distinction (ADR 0005) exists to *route* the review, not
  to skip it.

## Consequences

- A publisher's change is not live until it is admitted. That is a deliberate
  friction, and it is the product: the gateway's value is that the catalog is
  reviewed.
- The registry is authoritative, so it must be complete. A tool that exists on a
  backend but not in the registry is invisible, which makes the drift
  "appearance" class an operational signal rather than a curiosity — it usually
  means someone shipped without publishing.
- Drift detection becomes load-bearing rather than a nice-to-have: without it, a
  divergence between admitted and served definitions is silent. Hence the
  digest pair in ADR 0005 and the prober in phase 4.
- The gateway can serve a catalog for a backend that is completely unreachable,
  which is what makes the grace window possible at all.
