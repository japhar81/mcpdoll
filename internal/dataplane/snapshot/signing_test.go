// Copyright 2026 The MCPDoll Authors.

package snapshot

import (
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

func testSigner(t *testing.T, keyID string) (*Signer, *Verifier) {
	t.Helper()
	pub, priv, err := GenerateKey()
	require.NoError(t, err)
	s, err := NewSigner(keyID, priv)
	require.NoError(t, err)
	v, err := NewVerifierFromKeys(map[string]ed25519.PublicKey{keyID: pub})
	require.NoError(t, err)
	return s, v
}

func minimalSnapshot(version int64) *snapshotpb.Snapshot {
	return &snapshotpb.Snapshot{Version: version, Id: "snap_test"}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s, v := testSigner(t, "k1")
	signed, err := s.Sign(minimalSnapshot(7))
	require.NoError(t, err)
	require.Equal(t, "k1", signed.KeyId)
	require.Equal(t, AlgorithmEd25519, signed.Algorithm)
	require.Len(t, signed.Signature, ed25519.SignatureSize)

	got, err := v.Verify(signed)
	require.NoError(t, err)
	require.Equal(t, int64(7), got.Version)
}

func TestNewSignerRejectsBadInput(t *testing.T) {
	_, priv, err := GenerateKey()
	require.NoError(t, err)

	_, err = NewSigner("", priv)
	require.ErrorContains(t, err, "key id")

	_, err = NewSigner("k1", ed25519.PrivateKey("too short"))
	require.ErrorContains(t, err, "want 64")
}

func TestSignRejectsBadSnapshot(t *testing.T) {
	s, _ := testSigner(t, "k1")
	_, err := s.Sign(nil)
	require.ErrorContains(t, err, "nothing to sign")
	_, err = s.Sign(minimalSnapshot(0))
	require.ErrorContains(t, err, "version must be positive")
}

// TestVerifyRejectsTamperedBytes is the whole point of signing: a snapshot
// modified in transit must not activate, whatever the modification.
func TestVerifyRejectsTamperedBytes(t *testing.T) {
	s, v := testSigner(t, "k1")
	signed, err := s.Sign(minimalSnapshot(7))
	require.NoError(t, err)

	t.Run("body flipped", func(t *testing.T) {
		bad := proto.Clone(signed).(*snapshotpb.SignedSnapshot)
		bad.SnapshotBytes[0] ^= 0xFF
		_, err := v.Verify(bad)
		require.ErrorIs(t, err, ErrBadSignature)
	})

	t.Run("body replaced with a different snapshot", func(t *testing.T) {
		bad := proto.Clone(signed).(*snapshotpb.SignedSnapshot)
		other, err := proto.Marshal(minimalSnapshot(9999))
		require.NoError(t, err)
		bad.SnapshotBytes = other
		_, err = v.Verify(bad)
		require.ErrorIs(t, err, ErrBadSignature)
	})

	t.Run("signature flipped", func(t *testing.T) {
		bad := proto.Clone(signed).(*snapshotpb.SignedSnapshot)
		bad.Signature[0] ^= 0xFF
		_, err := v.Verify(bad)
		require.ErrorIs(t, err, ErrBadSignature)
	})

	t.Run("signature truncated", func(t *testing.T) {
		bad := proto.Clone(signed).(*snapshotpb.SignedSnapshot)
		bad.Signature = bad.Signature[:10]
		_, err := v.Verify(bad)
		require.ErrorIs(t, err, ErrBadSignature)
	})

	t.Run("signature removed", func(t *testing.T) {
		bad := proto.Clone(signed).(*snapshotpb.SignedSnapshot)
		bad.Signature = nil
		_, err := v.Verify(bad)
		require.ErrorIs(t, err, ErrBadSignature)
	})
}

func TestVerifyRejectsUntrustedKey(t *testing.T) {
	s, _ := testSigner(t, "attacker-key")
	_, trusted := testSigner(t, "k1")

	signed, err := s.Sign(minimalSnapshot(7))
	require.NoError(t, err)

	_, err = trusted.Verify(signed)
	var untrusted *ErrUntrustedKey
	require.ErrorAs(t, err, &untrusted)
	require.Equal(t, "attacker-key", untrusted.KeyID)
}

// TestVerifyRejectsKeyIDSubstitution: relabelling a signature with a trusted
// key's id must not make it verify. The key id selects which public key to use;
// it is not itself a credential.
func TestVerifyRejectsKeyIDSubstitution(t *testing.T) {
	attacker, _ := testSigner(t, "attacker")
	_, trusted := testSigner(t, "k1")

	signed, err := attacker.Sign(minimalSnapshot(7))
	require.NoError(t, err)
	signed.KeyId = "k1" // claim to be the trusted key

	_, err = trusted.Verify(signed)
	require.ErrorIs(t, err, ErrBadSignature)
}

func TestVerifyRejectsUnsupportedAlgorithm(t *testing.T) {
	s, v := testSigner(t, "k1")
	signed, err := s.Sign(minimalSnapshot(7))
	require.NoError(t, err)

	for _, alg := range []string{"", "none", "ES256", "ED25519"} {
		bad := proto.Clone(signed).(*snapshotpb.SignedSnapshot)
		bad.Algorithm = alg
		_, err := v.Verify(bad)
		var unsupported *ErrUnsupportedAlgorithm
		require.ErrorAs(t, err, &unsupported, "algorithm %q must be rejected", alg)
	}
}

// TestSignatureIsDomainSeparated: a signature over the same bytes without the
// context prefix must not verify. Domain separation is what stops a signature
// minted for another MCPDoll artifact being replayed as a snapshot signature.
func TestSignatureIsDomainSeparated(t *testing.T) {
	pub, priv, err := GenerateKey()
	require.NoError(t, err)
	v, err := NewVerifierFromKeys(map[string]ed25519.PublicKey{"k1": pub})
	require.NoError(t, err)

	body, err := proto.Marshal(minimalSnapshot(7))
	require.NoError(t, err)

	// A signature over the raw body, with no context prefix.
	forged := &snapshotpb.SignedSnapshot{
		SnapshotBytes: body,
		KeyId:         "k1",
		Signature:     ed25519.Sign(priv, body),
		Algorithm:     AlgorithmEd25519,
	}
	_, err = v.Verify(forged)
	require.ErrorIs(t, err, ErrBadSignature,
		"a signature without the domain-separation prefix must not verify")
}

func TestVerifyRejectsNil(t *testing.T) {
	_, v := testSigner(t, "k1")
	_, err := v.Verify(nil)
	require.ErrorContains(t, err, "nothing to verify")
}

func TestNewVerifierParsesConfigEntries(t *testing.T) {
	pub, _, err := GenerateKey()
	require.NoError(t, err)
	entry := TrustedKeyEntry("k1", pub)

	v, err := NewVerifier([]string{entry})
	require.NoError(t, err)
	require.Equal(t, []string{"k1"}, v.TrustedKeyIDs())
}

func TestNewVerifierRejectsBadConfig(t *testing.T) {
	pub, _, err := GenerateKey()
	require.NoError(t, err)
	good := TrustedKeyEntry("k1", pub)

	tests := []struct {
		name, wantErr string
		entries       []string
	}{
		{name: "no keys at all", wantErr: "at least one trusted signing key", entries: nil},
		{name: "missing separator", wantErr: "keyID:base64", entries: []string{"nokeyid"}},
		{name: "empty key id", wantErr: "keyID:base64", entries: []string{":" + base64.StdEncoding.EncodeToString(pub)}},
		{name: "empty key body", wantErr: "keyID:base64", entries: []string{"k1:"}},
		{name: "not base64", wantErr: "illegal base64", entries: []string{"k1:!!!!"}},
		{name: "wrong key length", wantErr: "want 32", entries: []string{"k1:" + base64.StdEncoding.EncodeToString([]byte("short"))}},
		{name: "duplicate key id", wantErr: "appears twice", entries: []string{good, good}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewVerifier(tc.entries)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestVerifierSupportsKeyRotation: during a rotation both keys must verify, or
// the rollout requires a lockstep restart of every instance.
func TestVerifierSupportsKeyRotation(t *testing.T) {
	oldPub, oldPriv, err := GenerateKey()
	require.NoError(t, err)
	newPub, newPriv, err := GenerateKey()
	require.NoError(t, err)

	v, err := NewVerifierFromKeys(map[string]ed25519.PublicKey{
		"old": oldPub, "new": newPub,
	})
	require.NoError(t, err)

	for keyID, priv := range map[string]ed25519.PrivateKey{"old": oldPriv, "new": newPriv} {
		s, err := NewSigner(keyID, priv)
		require.NoError(t, err)
		signed, err := s.Sign(minimalSnapshot(1))
		require.NoError(t, err)
		_, err = v.Verify(signed)
		require.NoError(t, err, "key %q should verify during rotation", keyID)
	}
}

func TestKeyFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pub, priv, err := GenerateKey()
	require.NoError(t, err)
	require.NoError(t, WriteKeyPair(dir, "devkey", pub, priv))

	loaded, err := LoadPrivateKey(filepath.Join(dir, "devkey.key"))
	require.NoError(t, err)
	require.Equal(t, priv, loaded)

	s, err := NewSigner("devkey", loaded)
	require.NoError(t, err)
	require.Equal(t, pub, s.PublicKey())
	require.Equal(t, "devkey", s.KeyID())
}

func TestLoadPrivateKeyErrors(t *testing.T) {
	dir := t.TempDir()

	_, err := LoadPrivateKey(filepath.Join(dir, "absent.key"))
	require.ErrorContains(t, err, "reading signing key")

	bad := filepath.Join(dir, "bad.key")
	require.NoError(t, writeFile(bad, "!!!not base64!!!"))
	_, err = LoadPrivateKey(bad)
	require.ErrorContains(t, err, "decoding signing key")

	short := filepath.Join(dir, "short.key")
	require.NoError(t, writeFile(short, base64.StdEncoding.EncodeToString([]byte("short"))))
	_, err = LoadPrivateKey(short)
	require.ErrorContains(t, err, "want 64")
}
