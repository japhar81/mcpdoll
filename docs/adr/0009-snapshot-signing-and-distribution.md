# ADR 0009: Snapshot Signing and Distribution

## Status

Accepted

## Context

The snapshot is the only channel from the control plane to the data plane
(ADR 0002). Everything the data plane enforces — which tools exist, who may
call them, what the policies are, which plugins run — comes from it. That makes
the snapshot the single most security-critical artifact in the system: an
attacker who can substitute one can add a tool, disable a plugin, or widen a
policy across every data-plane instance at once.

The data plane is also deliberately able to run where the control plane cannot
be reached or fully trusted: a DMZ, a customer VPC, an air-gapped network fed
by a file on disk. So "the connection is TLS" is not a sufficient answer —
there may be no connection.

## Decision

**Ed25519 over the transmitted bytes, with domain separation, verified before
parsing. Protobuf on the wire, gRPC stream for distribution.**

### Sign the octets you transmit; verify before you parse

`SignedSnapshot.snapshot_bytes` holds a serialized `Snapshot`, and the signature
covers exactly those bytes. The data plane verifies the bytes it received and
*only then* parses them.

That ordering is the whole trick, and it dissolves an objection worth stating:
protobuf does not guarantee byte-identical serialization across languages or
library versions, so a scheme that signed a *re-serialization* would break on a
library upgrade. Signing the transmitted octets makes serialization determinism
irrelevant. It also means untrusted protobuf never reaches the parser, which is
the larger attack surface of the two.

### Domain separation

The signature covers `"mcpdoll.snapshot.v1\x00" || snapshot_bytes`. Without the
prefix, a signature produced over some other MCPDoll artifact with the same key
could be replayed as a snapshot signature.
`TestSignatureIsDomainSeparated` asserts a signature over the raw body does not
verify.

### Multiple trusted keys

A verifier holds a set of `keyID → publicKey`. Rotation is: add the new key
everywhere, switch the control plane to sign with it, remove the old key. A
single-key verifier would require a lockstep restart of every instance.

The key id selects which public key to check against — it is **not** itself a
credential. `TestVerifyRejectsKeyIDSubstitution` asserts that relabelling an
attacker's signature with a trusted key's id does not make it verify.

### Failure always means "keep serving"

Verification failure, a stale version, or a structural problem all leave the
current snapshot serving untouched, with the reason recorded for the console.
A bad publish degrades to "still serving the last good configuration", never to
an outage.

Version monotonicity is enforced on activation: a snapshot no newer than the one
being served is refused. That is what stops a replayed or misrouted older
snapshot silently rolling policy backwards.

### Distribution

A gRPC stream (`SnapshotDistribution.Subscribe`) with heartbeats. The subscriber
sends the version it already has, so a reconnect does not force a redundant
swap. Reconnection is backoff-with-jitter, and the data plane serves throughout.
`Fetch` supports local rollback and CLI inspection; `ReportStatus` lets the
console show version and age per instance, including *why* an instance is behind.

There is deliberately no push endpoint on the data plane: an inbound
configuration path would be a second, weaker way in.

## Alternatives considered

- **Sign a canonical JSON form instead of protobuf.** Genuinely tempting: MCPDoll
  already has a byte-exact RFC 8785 canonicalizer, and JSON is inspectable.
  Rejected because verifying the transmitted octets removes the determinism
  problem entirely, and protobuf is materially more compact for 20+ backends ×
  tens of tools each — the artifact ships on every publish to every instance.
- **No signature; rely on mTLS between the planes.** Rejected: it makes the
  channel the security boundary, so the air-gapped and different-trust-domain
  deployments become impossible, and a compromised control-plane host can
  push anything.
- **X.509 certificate chains instead of raw keys.** Rejected as unnecessary
  machinery: there is no third party who needs to validate the chain, and PKI
  brings expiry, revocation, and chain-building failure modes for no gain over a
  small trusted-key set.
- **HMAC instead of a signature.** Rejected: a symmetric key would have to live
  on every data-plane instance, so any one compromised instance could mint
  snapshots for all of them. Asymmetric means an instance can verify but not
  forge. (HMAC *is* used for the MRTR `requestState` envelope, where the gateway
  is both issuer and verifier — see ADR 0012 — which is exactly the case
  asymmetric buys nothing.)
- **Automatic rollback when error rates rise after a publish.** Rejected: the
  gateway cannot distinguish a bad policy change from a correct one that
  legitimately denies more traffic, and automatically reverting a *tightening*
  is the wrong default for a security control. Rollback is explicit and
  operator-initiated.

## Consequences

- The control plane holds a private key. Compromising it is equivalent to
  compromising every data plane, so its handling is documented in
  `docs/operations/key-rotation.md` and it never appears in a snapshot, a log,
  or an audit record.
- `snapshot.WriteKeyPair` exists for `make dev` and writes an unencrypted 0600
  private key. Its doc comment says plainly that production keys must come from
  the operator's key management, because a passphrase prompt in a dev-up script
  is theatre rather than security.
- Retained history is bounded (`snapshot_history`, default 5). Rolling back
  further than that requires the control plane — acceptable, since rolling back
  five versions is a different kind of incident.
- Snapshot size grows with the registry. At 20 backends × 64 tools with full
  schemas this is a few hundred kilobytes, sent once per publish. If it ever
  becomes a problem the fix is delta distribution, which the version field
  already makes expressible; recorded in `docs/deferred.md`.
