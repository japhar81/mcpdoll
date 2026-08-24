// Copyright 2026 Henry Zektser.

package snapshot_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// The revocation list is the second signed artifact, and every property worth
// testing is about what it *cannot* do: it cannot authorize anything, it cannot
// be forged with a snapshot's signature, and it cannot silently drop denials
// the serving snapshot has not absorbed.

// signerAndVerifier builds a matched pair under a caller-chosen key id.
//
// The id is a parameter rather than a constant because two pairs sharing one
// would turn an untrusted-key test into a bad-signature test: the verifier
// would find the id, load the wrong public key, and fail at the signature. The
// distinction matters — one says "I do not know this signer" and the other says
// "these bytes were altered", and they send an operator to different places.
func signerAndVerifier(t *testing.T, keyID string) (*snapshot.Signer, *snapshot.Verifier) {
	t.Helper()
	pub, priv, err := snapshot.GenerateKey()
	require.NoError(t, err)
	signer, err := snapshot.NewSigner(keyID, priv)
	require.NoError(t, err)
	v, err := snapshot.NewVerifier([]string{snapshot.TrustedKeyEntry(keyID, pub)})
	require.NoError(t, err)
	return signer, v
}

func listOf(version int64, ids ...string) *snapshotpb.RevocationList {
	return &snapshotpb.RevocationList{
		Version: version, IssuedAt: timestamppb.Now(), PrincipalIds: ids,
	}
}

func TestARevocationListRoundTrips(t *testing.T) {
	t.Parallel()
	signer, verifier := signerAndVerifier(t, "test")

	signed, err := signer.SignRevocations(listOf(7, "key_1", "key_2"))
	require.NoError(t, err)

	list, err := verifier.VerifyRevocations(signed)
	require.NoError(t, err)
	require.Equal(t, int64(7), list.Version)
	require.Equal(t, 2, list.Count())
	require.True(t, list.Refuses("key_1"))
	require.False(t, list.Refuses("key_3"))
}

func TestASnapshotSignatureDoesNotVerifyAsARevocationList(t *testing.T) {
	t.Parallel()
	signer, verifier := signerAndVerifier(t, "test")

	// The whole reason the signing context differs. Without domain separation,
	// anyone who could get a snapshot signed could present its bytes as a
	// revocation list — and a list is a deny list, so that is a way to revoke
	// every principal in the deployment.
	list := listOf(7, "key_1")
	signedList, err := signer.SignRevocations(list)
	require.NoError(t, err)

	forged := &snapshotpb.SignedSnapshot{
		SnapshotBytes: signedList.ListBytes,
		KeyId:         signedList.KeyId,
		Algorithm:     signedList.Algorithm,
		Signature:     signedList.Signature,
	}
	_, err = verifier.Verify(forged)
	require.ErrorIs(t, err, snapshot.ErrBadSignature,
		"a revocation signature verified as a snapshot")
}

func TestATamperedListIsRefused(t *testing.T) {
	t.Parallel()
	signer, verifier := signerAndVerifier(t, "test")

	signed, err := signer.SignRevocations(listOf(7, "key_1"))
	require.NoError(t, err)
	signed.ListBytes[len(signed.ListBytes)-1] ^= 0xff

	_, err = verifier.VerifyRevocations(signed)
	require.ErrorIs(t, err, snapshot.ErrBadSignature)
}

func TestAnUntrustedKeyIsRefused(t *testing.T) {
	t.Parallel()
	signer, _ := signerAndVerifier(t, "ours")
	_, other := signerAndVerifier(t, "theirs")

	signed, err := signer.SignRevocations(listOf(7, "key_1"))
	require.NoError(t, err)

	_, err = other.VerifyRevocations(signed)
	var untrusted *snapshot.ErrUntrustedKey
	require.ErrorAs(t, err, &untrusted)
}

func TestAnEmptyPrincipalIDIsRefused(t *testing.T) {
	t.Parallel()
	signer, verifier := signerAndVerifier(t, "test")

	// It would match no principal, so it is harmless — and it is a producer bug
	// worth surfacing rather than a silent no-op in a security artifact.
	signed, err := signer.SignRevocations(listOf(7, "key_1", ""))
	require.NoError(t, err)

	_, err = verifier.VerifyRevocations(signed)
	require.ErrorContains(t, err, "empty principal id")
}

func TestAgeIsTheExposureWindow(t *testing.T) {
	t.Parallel()
	signer, verifier := signerAndVerifier(t, "test")

	old := listOf(7, "key_1")
	old.IssuedAt = timestamppb.New(time.Now().Add(-90 * time.Second))
	signed, err := signer.SignRevocations(old)
	require.NoError(t, err)

	list, err := verifier.VerifyRevocations(signed)
	require.NoError(t, err)
	// The number an operator alerts on: a revoked credential keeps working for
	// exactly this long (ADR 0023).
	require.InDelta(t, 90.0, list.Age().Seconds(), 5.0)
}

func TestEmptyRevocationsRefuseNobody(t *testing.T) {
	t.Parallel()

	// A gateway that has never loaded a list and one that loaded an empty one
	// behave identically, and they should — neither has anything to refuse.
	empty := snapshot.EmptyRevocations()
	require.False(t, empty.Refuses("key_1"))
	require.Zero(t, empty.Count())

	var absent *snapshot.Revocations
	require.False(t, absent.Refuses("key_1"), "a nil list must not panic or refuse")
}

func TestAKnownKeyIDWithTheWrongKeyIsABadSignature(t *testing.T) {
	t.Parallel()

	// The other half of the pair above, and a distinct failure: the verifier
	// knows this key id and holds a different public key for it. That is a
	// rotation gone wrong, or an attempt to pass off somebody else's bytes, and
	// it must not read as "untrusted signer".
	signer, _ := signerAndVerifier(t, "dev")
	_, other := signerAndVerifier(t, "dev")

	signed, err := signer.SignRevocations(listOf(7, "key_1"))
	require.NoError(t, err)

	_, err = other.VerifyRevocations(signed)
	require.ErrorIs(t, err, snapshot.ErrBadSignature)
}
