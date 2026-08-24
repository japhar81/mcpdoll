// Copyright 2026 Henry Zektser.

package snapshot

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// The revocation list: the second signed artifact, which can only subtract.
//
// A principal named here is refused whatever the snapshot says. It carries no
// grants, no scopes, and no roles — so an *allowed* action is still explained
// by the snapshot alone, which is what answers ADR 0018's objection to a side
// channel and lets ADR 0023 build one.

// revocationSigningContext is prefixed to the signed bytes.
//
// Deliberately not the snapshot's. Without a distinct context a signature
// minted over one artifact would verify against the other, so anyone who could
// get a snapshot signed could present its bytes as a revocation list.
var revocationSigningContext = []byte("mcpdoll.revocations.v1\x00")

// ErrStaleRevocations reports a list no newer than the one already held.
var ErrStaleRevocations = errors.New("snapshot: revocation list is not newer than the one in effect")

// ErrRevocationsAheadOfSnapshot reports a list pruned past what is being served.
//
// Accepting it would silently drop denials the serving snapshot has not yet
// absorbed. Refusing keeps strictly more denials, which is the safe direction,
// and it corrects itself the moment the newer snapshot lands.
var ErrRevocationsAheadOfSnapshot = errors.New(
	"snapshot: revocation list was pruned against a newer snapshot than the one being served")

// Revocations is a verified list, indexed for lookup.
type Revocations struct {
	Version              int64
	IssuedAt             time.Time
	PrunedThroughVersion int64

	refused map[string]struct{}
}

// EmptyRevocations is the starting state: nothing revoked, version zero.
//
// A real value rather than a nil check at every call site. A gateway that has
// never loaded a list and one that loaded an empty one behave identically, and
// they should — neither has anything to refuse.
func EmptyRevocations() *Revocations {
	return &Revocations{refused: map[string]struct{}{}}
}

// Refuses reports whether this principal has been revoked.
func (r *Revocations) Refuses(principalID string) bool {
	if r == nil {
		return false
	}
	_, ok := r.refused[principalID]
	return ok
}

// Count is how many principals the list names.
func (r *Revocations) Count() int {
	if r == nil {
		return 0
	}
	return len(r.refused)
}

// Age is how long ago this list was issued.
//
// The number that bounds the exposure ADR 0023 does not eliminate: a leaked key
// keeps working for as long as the data plane's list is out of date, so this is
// the thing to put on a dashboard and alert on.
func (r *Revocations) Age() time.Duration {
	if r == nil || r.IssuedAt.IsZero() {
		return 0
	}
	return time.Since(r.IssuedAt)
}

// SignRevocations signs a list.
func (s *Signer) SignRevocations(list *snapshotpb.RevocationList) (*snapshotpb.SignedRevocationList, error) {
	if list == nil {
		return nil, errors.New("snapshot: cannot sign a nil revocation list")
	}
	if list.Version <= 0 {
		return nil, errors.New("snapshot: a revocation list needs a positive version")
	}
	if list.IssuedAt == nil {
		list.IssuedAt = timestamppb.Now()
	}

	body, err := proto.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("snapshot: marshalling the revocation list: %w", err)
	}
	return &snapshotpb.SignedRevocationList{
		ListBytes: body,
		KeyId:     s.keyID,
		Algorithm: AlgorithmEd25519,
		Signature: ed25519.Sign(s.priv, revocationSignedPayload(body)),
	}, nil
}

// VerifyRevocations checks a signed list against the trusted keys and indexes it.
func (v *Verifier) VerifyRevocations(signed *snapshotpb.SignedRevocationList) (*Revocations, error) {
	if signed == nil {
		return nil, errors.New("snapshot: no revocation list")
	}
	if signed.Algorithm != "" && signed.Algorithm != AlgorithmEd25519 {
		return nil, &ErrUnsupportedAlgorithm{Algorithm: signed.Algorithm}
	}
	pub, ok := v.keys[signed.KeyId]
	if !ok {
		return nil, &ErrUntrustedKey{KeyID: signed.KeyId}
	}
	if !ed25519.Verify(pub, revocationSignedPayload(signed.ListBytes), signed.Signature) {
		return nil, ErrBadSignature
	}

	var list snapshotpb.RevocationList
	if err := proto.Unmarshal(signed.ListBytes, &list); err != nil {
		return nil, fmt.Errorf("snapshot: parsing verified revocation bytes: %w", err)
	}

	out := &Revocations{
		Version:              list.Version,
		PrunedThroughVersion: list.PrunedThroughSnapshotVersion,
		refused:              make(map[string]struct{}, len(list.PrincipalIds)),
	}
	if list.IssuedAt != nil {
		out.IssuedAt = list.IssuedAt.AsTime()
	}
	for _, id := range list.PrincipalIds {
		if id == "" {
			// An empty id would match no principal, and a list carrying one is
			// a producer bug worth surfacing rather than a harmless no-op.
			return nil, errors.New("snapshot: revocation list contains an empty principal id")
		}
		out.refused[id] = struct{}{}
	}
	return out, nil
}

func revocationSignedPayload(body []byte) []byte {
	out := make([]byte, 0, len(revocationSigningContext)+len(body))
	out = append(out, revocationSigningContext...)
	return append(out, body...)
}

// WriteSignedRevocations persists a list atomically.
func WriteSignedRevocations(path string, signed *snapshotpb.SignedRevocationList) error {
	raw, err := proto.Marshal(signed)
	if err != nil {
		return fmt.Errorf("snapshot: marshalling the signed revocation list: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("snapshot: creating the revocation directory: %w", err)
	}

	// Same write-then-rename as the snapshot. A data plane watching this file
	// must never observe a partial write — it would read as a signature failure
	// and look like an attack.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".revocations-*")
	if err != nil {
		return fmt.Errorf("snapshot: creating a temporary file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: writing the revocation list: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: syncing the revocation list: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("snapshot: closing the revocation list: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("snapshot: setting revocation permissions: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}

// ReadSignedRevocations reads a list from disk without verifying it.
func ReadSignedRevocations(path string) (*snapshotpb.SignedRevocationList, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var signed snapshotpb.SignedRevocationList
	if err := proto.Unmarshal(raw, &signed); err != nil {
		return nil, fmt.Errorf("snapshot: %s is not a signed revocation list: %w", path, err)
	}
	return &signed, nil
}
