# ADR 0012: MRTR requestState Wrapping

## Status

Accepted. **Amended by [ADR 0019](./0019-single-mcp-endpoint.md):** the
envelope binds `(tool, principal, tenant, argument digest)`. The `audience` slug
it previously bound no longer exists; the tenant replaces it and is bound
explicitly rather than inferred from the principal, so a confirmation cannot be
replayed across a tenant boundary and so the envelope is self-describing in an
audit trail.

## Context

In stateless mode the MCP server cannot make client requests. Elicitation and
sampling — anything that needs a human or a model in the loop — must therefore go
through MRTR: the server returns `resultType: "input_required"` with a set of
input requests and an opaque `requestState`; the client fulfils the requests and
retries the call, echoing the state back.

That state round-trips through an untrusted client. The spec is explicit that an
unauthenticated server must encrypt, sign, and verify it, and the reason is
immediate: the state is what tells the server "this action was approved". For a
destructive tool, a forgeable state means a client can self-authorize the
destruction.

A gateway has a second problem the spec does not address, because the spec does
not assume a middlebox. There are *two* possible sources of an input request —
the backend, and a gateway plugin — and each produces its own state. Passing a
backend's state through unchanged would expose it to the client and, worse, would
give the gateway no way to bind an approval to the call it approved.

## Decision

**The gateway wraps every `requestState` in its own signed envelope, binding the
approval to the specific call, with a TTL. The inner state is opaque to
everyone but its author.**

### The envelope

HMAC-SHA256 over a base64url payload, prefixed `mcpd1.`:

| Field | Purpose |
|---|---|
| `tool` | binds the approval to a specific tool |
| `sub` | binds it to a specific principal |
| `aud` | binds it to a specific audience |
| `args` | SHA-256 of the canonical arguments |
| `src` | who asked: `backend` or `plugin` |
| `bs` / `ps` | the backend's or plugin's own opaque state |
| `iat`, `n` | issue time and nonce |

The bindings are the point. Without `args`, an approval for
"void invoice INV-1" could be replayed against INV-2 — a confused-deputy bug
where the human approved one thing and a different thing happened.
`TestMRTRStateCannotBeReplayedAgainstDifferentArguments` asserts this directly.

HMAC rather than Ed25519 because the gateway is both issuer and verifier: no
third party needs to check the signature, and a symmetric MAC is materially
cheaper on a path that runs per interactive call. Domain-separated
(`"mcpdoll.requeststate.v1\x00"`) and compared with `hmac.Equal`, since a
timing-variable MAC comparison is a forgery oracle.

Verification happens **before** the payload is decoded, so unauthenticated bytes
never reach the JSON parser. The envelope expires after `StateTTL` (10 minutes),
because an approval valid forever is a standing authorization nobody remembers
granting. A future-dated envelope beyond a minute of tolerance is refused too:
that is either clock skew past what we will accept or a forgery attempt.

### `src` routes the input responses — and this is not optional

The keys in an `InputResponseMap` are chosen by whoever issued the requests, so
the backend's key namespace and a plugin's key namespace are unrelated.

Forwarding a plugin's responses to the backend makes the backend believe it was
answered: it sees a non-empty response map, takes its second-round branch, looks
for *its own* keys, and finds none. The user gets a baffling "confirmation
response missing" from a backend that was never actually asked twice.

So the envelope records the source, and responses are routed only to whoever
requested them. If both a plugin and the backend need input, that is two
sequential round trips — correct, and much easier to reason about than merged
namespaces.

This was found by a failing test, not by design review, and is called out here
because the symptom points nowhere near the cause.

### No signer configured means refuse, not degrade

If a backend or plugin asks for input and no `StateSigner` is configured, the
call fails with an explanatory error. Emitting an unsigned state would be worse
than refusing: it would look like it worked.

## Alternatives considered

- **Pass the backend's `requestState` through unchanged.** Rejected: the gateway
  then has no binding between an approval and the call it approved, cannot bind
  the principal, and exposes the backend's internal state to the client.
- **Store state server-side and hand the client an opaque id.** Rejected: it
  reintroduces per-request server state, which is precisely what stateless mode
  removes and what makes the data plane horizontally scalable. It would also need
  a shared store, so a multi-instance deployment gains a hard dependency on Redis
  for the interactive path.
- **Encrypt rather than sign.** The spec says "encrypt, sign and verify".
  MCPDoll signs and does not encrypt, because nothing in the envelope is secret:
  the tool, principal, and audience are all things the client already knows, and
  the inner state is chosen by its author with the client's visibility in mind.
  Signing is what provides the property that matters — unforgeability. Encryption
  would add key management for confidentiality nobody needs. **This is a
  deliberate divergence from the spec's wording and is recorded as such**; if a
  future backend puts genuinely secret material in its state, the envelope has a
  version prefix and can gain an encrypted variant without breaking existing
  tokens.
- **Per-instance random keys.** Rejected: a retry could land on a different
  instance and fail to verify. The key must be a shared deployment secret, which
  is stated in `docs/operations/` rather than papered over — silently accepting
  a state we cannot verify would defeat the entire mechanism.
- **Bind to a nonce stored server-side to prevent reuse within the TTL.**
  Deferred, not rejected. Single-use redemption needs shared state, and the
  argument binding already prevents redirecting an approval to a different
  target. Recorded in `docs/deferred.md`: within its 10-minute window an approval
  can be redeemed more than once for the *same* call.

## Consequences

- Interactive flows work in stateless mode with no server-side session, so they
  scale and survive an instance restart mid-approval.
- The `requestState` key is a second key to manage alongside the snapshot signing
  key. Both are covered in `docs/operations/key-rotation.md`.
- A plugin author never has to think about forgery: they return their own state
  and the edge wraps it. `ToolCallDecision.RequestState` documents that the value
  is the plugin's own and will be wrapped.
- Argument binding means an MRTR retry must present byte-identical arguments. A
  client that "helpfully" reorders JSON keys still works, because the digest is
  over the canonical form — but one that changes a value does not, which is the
  intent.
