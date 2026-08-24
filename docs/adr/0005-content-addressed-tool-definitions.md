# ADR 0005: Content-Addressed Tool Definitions

## Status

Accepted. **Amended by [ADR 0017](./0017-per-tenant-backends-and-pools.md):**
a digest now identifies a `(tenant, tool)` pair rather than a tool. The same
vendor tool published by two tenants' separately-deployed backends legitimately
has two different digests, and drift is measured against the tenant's own
admitted definition.

## Context

MCPDoll fronts 20+ independently-published MCP backends. It must be able to
answer, cheaply and without ambiguity:

- Is this tool definition the same one an approver signed off on?
- Did the backend change what it serves since we admitted it?
- Have we already asked the LLM guard about this exact content?
- Is this snapshot byte-identical to the one the other replica is serving?

All four are the same question — *is this the same definition?* — so all four
should share one answer: a digest over a canonical serialization. The
definition's identity **is** its digest.

That makes the canonicalization algorithm load-bearing in a way that is easy to
under-estimate. If two processes disagree about the canonical bytes by a single
character, they disagree about every identity derived from them: version
immutability breaks, drift fires spuriously, the guard cache misses forever,
and two data-plane replicas serve catalogs they each believe are identical.

## Decision

**Tool definition identity is `sha256` over an RFC 8785 canonical JSON encoding
of a `$defs`-resolved definition.**

Three parts:

### 1. RFC 8785 (JSON Canonicalization Scheme) for the encoding

Rather than invent an encoding, use the IETF standard:

- object keys sorted by **UTF-16 code unit**, not by UTF-8 bytes and not by
  code point (these differ for anything above the BMP — a surrogate pair
  sorts below `U+E000`);
- no insignificant whitespace;
- numbers formatted by the ECMAScript `Number::toString` algorithm;
- minimal string escaping, non-ASCII emitted as literal UTF-8.

Two hardening additions over plain JCS:

- **Duplicate object keys are rejected.** A duplicate key makes a document's
  meaning parser-dependent, which is precisely the ambiguity a content address
  cannot tolerate — two conformant gateways could hash different values from
  the same bytes.
- **Trailing content is rejected**, so `1 2` cannot canonicalize to `1`.

### 2. `$defs` inlining before hashing

A schema that factors a repeated subschema into `$defs` describes exactly the
same data as one that inlines it. Hashing the raw document would make that
editorial refactor look like a breaking republish. So references are inlined
first, and the now-empty `$defs`/`definitions` containers are dropped.

Recursion is handled by leaving a `$ref` in place when its own expansion is
already in progress. 2020-12 sibling keywords alongside a `$ref` are preserved
and override the referenced schema.

Two bounds are enforced during resolution, both of them security controls
rather than tidiness:

- `MaxSchemaDepth` (64) — a deeply nested schema can blow the stack.
- `MaxSchemaNodes` (20 000) — *N* definitions each referenced twice expand as
  2^N while staying shallow, so the depth limit alone does not catch the
  expansion bomb.

**External `$ref`s are rejected outright**, not merely left unresolved.
Dereferencing one would let a registered backend make the gateway fetch an
attacker-chosen URL at admission time (SSRF against the control plane's network
position), and would untether a signed definition's meaning from a document
under our control. The rejection lives in canonicalization, which is upstream
of both admission and snapshot build, so no later stage can forget it.

### 3. Two digests: full and semantic

Every definition has two content addresses:

- **`Digest`** — over everything. This is the identity. Any change produces a
  new definition requiring re-admission.
- **`SemanticDigest`** — over the definition with prose keys (`description`,
  `title`, `examples`, `$comment`) stripped recursively at every level.

The pair is what makes drift classification a digest comparison rather than a
field-by-field diff:

| Full digest | Semantic digest | Drift class |
|---|---|---|
| same | same | no drift |
| **changed** | same | cosmetic — wording only |
| changed | **changed** | semantic or structural |

`deprecated` is deliberately treated as structural, not prose: it reads like an
annotation but changes client behaviour.

Nested prose matters as much as top-level prose, which is why stripping is
recursive — a property's `description` is exactly where a cross-server prompt
injection hides, and cosmetic-vs-structural is the distinction the LLM guard's
placement depends on.

## Alternatives considered

- **Hash `json.Marshal` output of a Go struct.** Rejected: Go's map iteration
  is randomised, struct field order is a source-code detail, and float
  formatting is Go-specific. None of it is portable to the TypeScript console
  or to a future non-Go implementation.
- **Roll a bespoke canonical form.** Rejected: RFC 8785 exists, is
  interoperable, and has published test vectors. A bespoke form would need the
  same care and convince nobody.
- **Preserve arbitrary-precision numbers instead of JCS's IEEE-754 doubles.**
  Rejected as a deviation from the standard for a case that does not arise:
  JSON Schema numeric keywords beyond 2^53 are not seen in practice. The
  limitation is real and documented below rather than papered over.
- **One digest, with drift classified by diffing fields.** Rejected: the diff
  logic would have to be reimplemented anywhere classification happens, and
  would drift from the hashing logic. Two digests put the classification in the
  same place as the identity.
- **Strip prose from the identity digest too**, so rewording is not a
  republish. Rejected: descriptions are what the model reads. A reworded
  description is a genuinely different tool from the model's point of view and
  must go through approval — that is the entire premise of admission review.

## Consequences

- Definition identity is portable: any RFC 8785 implementation reproduces it.
  A future TypeScript or Rust component can compute the same digest.
- The golden test in `internal/platform/canonical` pins exact canonical bytes.
  Changing them is a data migration touching every stored identity in every
  deployment, and the failing golden test makes that impossible to do by
  accident.
- **Numbers outside ±2^53 lose precision**, per JCS. Two schemas differing only
  in a numeric keyword beyond that range would collide. Accepted and documented;
  admission's schema linter is the place to reject such a keyword if it ever
  matters.
- `$anchor` (`"$ref": "#SomeName"`) is not resolved and is rejected rather than
  passed through, because silently hashing an unresolved reference would defeat
  the purpose. Schemas using anchors cannot be admitted. Recorded in
  `docs/deferred.md`.
- Inlining means a schema with heavy `$defs` reuse produces a larger canonical
  form than its source. The node budget bounds this at 20 000 nodes.
