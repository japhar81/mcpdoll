# ADR 0021: The Data Plane Verifies Credentials Offline

## Status

**Accepted.** Completes [0014](./0014-tenancy-and-principals.md) and
[0018](./0018-grants-in-the-snapshot.md), which together left one question
unanswered.

## Context

ADR 0018 put grants in the signed snapshot so the data plane could compose a
catalog without asking the control plane. ADR 0014 put credentials in Postgres
as `prefix` + Argon2id hash. Between them is a gap nobody named:

> An agent presents `mcpd.<prefix>.<secret>`. The data plane has no database.
> How does it know the secret is right?

Three answers, and the first two are already ruled out by decisions we have
made:

1. **Ask the control plane per request.** Reverses ADR 0002 — a control-plane
   outage would stop tool calls. This is the property the whole architecture
   exists to provide.
2. **Trust the header.** `HeaderIdentityResolver` does this and refuses to start
   in production for exactly that reason. It is a development affordance.
3. **Put the verifier in the snapshot.** Preserves ADR 0002. It requires
   deciding what "the verifier" is, because Argon2id cannot be it.

### Why Argon2id cannot be it

RFC 9106's second recommended option — 64 MiB, 3 passes — is roughly 50-100ms
and 64 MiB of memory per verification. Per tool call, at any concurrency, that
is not a cost; it is a denial-of-service primitive pointed at ourselves. Ten
concurrent connections would allocate 640 MiB to decide whether ten agents may
say hello.

## Decision

**API key secrets are stored and verified as SHA-256, not Argon2id. Passwords
remain Argon2id. The snapshot carries each key's prefix and SHA-256 digest, and
the data plane verifies against it with one hash and a constant-time compare.**

### Why a plain hash is correct here, and not a shortcut

Argon2id exists to make *low-entropy* secrets expensive to guess. A human
password is drawn from a distribution an attacker can enumerate, so the defence
has to be per-guess cost.

An API key secret is 256 bits from a CSPRNG. There is no distribution to
enumerate — an attacker holding the digest faces 2²⁵⁵ expected work against
SHA-256, which is not a number that gets smaller with better hardware in any
sense that matters. Adding a memory-hard KDF on top defends against nothing and
costs everything, on the request path, forever.

The distinction is the standard one: **KDF for what a human chose, digest for
what a CSPRNG produced.** Applying the same function to both is a category
error that reads as caution.

### What this means for the snapshot

`Principal` gains `key_prefix` and `key_secret_sha256`. A snapshot's principals
are its API keys — one per key, not one per user:

- A user with no keys contributes no principal. Correct: a user cannot reach the
  data plane without a credential, and publishing an unreachable principal would
  only make the snapshot bigger.
- A user with three keys contributes three principals with three grant sets,
  each already intersected with the owner's (ADR 0014). The intersection is
  computed at build time here, because it must be — the data plane holds no
  owner to intersect against.

Publishing a digest of a 256-bit random value in a signed artifact is safe: it
is not invertible, and it is bound to the signature, so an attacker who can
modify it can already modify the grants it protects.

### Revoking a key is the one thing that must not wait for a snapshot

Everything else in this system takes effect at snapshot latency, and ADR 0018
argued that is right. A leaked credential is different: the reason somebody
revokes a key is that it is being used *right now* by someone who should not
have it, and "a few seconds" is the wrong answer to that.

So revocation is written to the database **and** the snapshot rebuild is
triggered immediately, and until the swap lands the key still works. This ADR
does not solve that, and it must not pretend to. It is recorded in
`docs/deferred.md` as the one place where snapshot latency is a real exposure,
with the fix named: a signed revocation list the data plane loads out of band,
which is the second mechanism ADR 0018 said should arrive explicitly rather than
as an optimization.

## Alternatives considered

- **HMAC-SHA256 with a server pepper instead of a bare digest.** Genuinely
  better if the digest could leak *without* the snapshot leaking, because a
  pepper the attacker lacks makes an offline dictionary attack impossible. It
  buys nothing here — there is no dictionary against 256 random bits — and it
  introduces a secret every data-plane instance must hold and rotate in step
  with the control plane. Rejected on operational cost for no security gain.
  Revisit if key secrets ever become user-chosen, at which point the whole
  reasoning above inverts.
- **Keeping Argon2id and caching verifications in the data plane.** A cache
  makes the steady state fast and the cold start a thundering herd, and it means
  the first request after every deploy pays 64 MiB. Rejected: it optimizes a
  cost that should not exist.
- **Bcrypt or scrypt at low cost parameters.** All the complexity of a KDF, none
  of the strength, and a parameter somebody will eventually "fix" upward without
  knowing why it was low. Rejected as a shape that invites a bad edit.

## Consequences

- **Two hash functions in `credential.go`**, and the file has to say which is
  for what and why. A future reader who sees SHA-256 next to Argon2id and
  "corrects" the inconsistency would reintroduce the DoS.
- **A key minted before this change cannot be verified after it.** Argon2id
  digests are not convertible. There is no migration and no dual-read path:
  existing keys are invalid and must be re-minted. That is acceptable only
  because nothing is deployed, and it is stated rather than papered over.
- **The data plane can now authenticate**, which is what makes the single `/mcp`
  endpoint of ADR 0019 usable at all.
- **Key revocation is the one exposure snapshot latency does not cover.**
  Recorded in `docs/deferred.md`, not hidden here.
