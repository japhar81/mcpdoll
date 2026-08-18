// Copyright 2026 The MCPDoll Authors.

// Package snapshot verifies, loads, indexes, and hot-swaps the signed serving
// configuration the data plane runs from.
//
// The data plane trusts the snapshot's *signature*, not its transport. That is
// what lets it keep serving with the control plane unreachable, run in a
// different trust domain from the control plane, and refuse a snapshot that
// arrived over a compromised channel.
package snapshot

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/proto"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// AlgorithmEd25519 is the only signature algorithm accepted.
const AlgorithmEd25519 = "ed25519"

// signingContext is prefixed to the signed bytes for domain separation.
//
// Without it, a signature produced over some other MCPDoll artifact with the
// same key — a plugin artifact manifest, say — could be replayed as a snapshot
// signature. The prefix binds a signature to its purpose.
var signingContext = []byte("mcpdoll.snapshot.v1\x00")

// ErrUntrustedKey reports a snapshot signed by a key this instance does not
// trust.
type ErrUntrustedKey struct {
	KeyID string
}

func (e *ErrUntrustedKey) Error() string {
	return fmt.Sprintf("snapshot: signed by untrusted key %q", e.KeyID)
}

// ErrBadSignature reports a snapshot whose signature does not verify.
var ErrBadSignature = errors.New("snapshot: signature does not verify")

// ErrUnsupportedAlgorithm reports an algorithm this build cannot check.
type ErrUnsupportedAlgorithm struct {
	Algorithm string
}

func (e *ErrUnsupportedAlgorithm) Error() string {
	return fmt.Sprintf("snapshot: unsupported signature algorithm %q (only %q is accepted)",
		e.Algorithm, AlgorithmEd25519)
}

// Signer produces signed snapshots. Control plane only.
type Signer struct {
	keyID string
	priv  ed25519.PrivateKey
}

// NewSigner builds a signer from a raw Ed25519 private key.
func NewSigner(keyID string, priv ed25519.PrivateKey) (*Signer, error) {
	if keyID == "" {
		return nil, errors.New("snapshot: signer needs a key id so verifiers can select the right public key")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("snapshot: private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	return &Signer{keyID: keyID, priv: priv}, nil
}

// GenerateKey mints a new keypair. Used by `make dev` and by tests; production
// keys come from the operator's key management.
func GenerateKey() (pub ed25519.PublicKey, priv ed25519.PrivateKey, err error) {
	return ed25519.GenerateKey(rand.Reader)
}

// KeyID is the identifier verifiers match against their trusted set.
func (s *Signer) KeyID() string { return s.keyID }

// PublicKey returns the verification key for this signer.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// Sign serializes and signs a snapshot.
//
// The returned SignedSnapshot carries the exact octets that were signed. The
// caller must transmit those bytes unchanged: verification is over the received
// octets, never over a re-serialization, because protobuf does not promise
// byte-identical output across library versions.
func (s *Signer) Sign(snap *snapshotpb.Snapshot) (*snapshotpb.SignedSnapshot, error) {
	if snap == nil {
		return nil, errors.New("snapshot: nothing to sign")
	}
	if snap.Version <= 0 {
		return nil, fmt.Errorf("snapshot: version must be positive, got %d", snap.Version)
	}
	body, err := proto.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("snapshot: serializing: %w", err)
	}
	return &snapshotpb.SignedSnapshot{
		SnapshotBytes: body,
		KeyId:         s.keyID,
		Signature:     ed25519.Sign(s.priv, signedPayload(body)),
		Algorithm:     AlgorithmEd25519,
	}, nil
}

// Verifier checks snapshot signatures against a set of trusted public keys.
//
// More than one key is supported so a rotation can overlap: the new key is added
// to every data plane, then the control plane starts signing with it, then the
// old key is removed. A single-key verifier would require a lockstep restart.
type Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewVerifier builds a verifier from `keyID:base64PublicKey` entries, which is
// how trusted keys appear in configuration and Helm values.
func NewVerifier(entries []string) (*Verifier, error) {
	if len(entries) == 0 {
		return nil, errors.New("snapshot: at least one trusted signing key is required; " +
			"a data plane with no trusted key can never activate a snapshot")
	}
	keys := make(map[string]ed25519.PublicKey, len(entries))
	for _, entry := range entries {
		keyID, encoded, ok := strings.Cut(entry, ":")
		if !ok || keyID == "" || encoded == "" {
			return nil, fmt.Errorf("snapshot: trusted key %q is not in \"keyID:base64\" form", entry)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("snapshot: trusted key %q: %w", keyID, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("snapshot: trusted key %q is %d bytes, want %d",
				keyID, len(raw), ed25519.PublicKeySize)
		}
		if _, dup := keys[keyID]; dup {
			return nil, fmt.Errorf("snapshot: trusted key id %q appears twice", keyID)
		}
		keys[keyID] = raw
	}
	return &Verifier{keys: keys}, nil
}

// NewVerifierFromKeys builds a verifier directly, for tests and in-process wiring.
func NewVerifierFromKeys(keys map[string]ed25519.PublicKey) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, errors.New("snapshot: at least one trusted signing key is required")
	}
	copied := make(map[string]ed25519.PublicKey, len(keys))
	for id, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("snapshot: key %q is %d bytes, want %d", id, len(k), ed25519.PublicKeySize)
		}
		copied[id] = k
	}
	return &Verifier{keys: copied}, nil
}

// TrustedKeyIDs lists the key ids this verifier accepts, for diagnostics.
func (v *Verifier) TrustedKeyIDs() []string {
	out := make([]string, 0, len(v.keys))
	for id := range v.keys {
		out = append(out, id)
	}
	return out
}

// Verify checks the signature and returns the parsed snapshot.
//
// Parsing happens only after the signature verifies. Order matters: parsing
// attacker-controlled protobuf is the larger attack surface, and there is no
// reason to expose it to bytes that failed authentication.
func (v *Verifier) Verify(signed *snapshotpb.SignedSnapshot) (*snapshotpb.Snapshot, error) {
	if signed == nil {
		return nil, errors.New("snapshot: nothing to verify")
	}
	if signed.Algorithm != AlgorithmEd25519 {
		// An empty algorithm is treated the same as an unknown one rather than
		// defaulted: a snapshot that forgot to say how it was signed has not
		// earned the benefit of the doubt.
		return nil, &ErrUnsupportedAlgorithm{Algorithm: signed.Algorithm}
	}
	pub, ok := v.keys[signed.KeyId]
	if !ok {
		return nil, &ErrUntrustedKey{KeyID: signed.KeyId}
	}
	if len(signed.Signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature is %d bytes, want %d",
			ErrBadSignature, len(signed.Signature), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, signedPayload(signed.SnapshotBytes), signed.Signature) {
		return nil, ErrBadSignature
	}

	var snap snapshotpb.Snapshot
	if err := proto.Unmarshal(signed.SnapshotBytes, &snap); err != nil {
		return nil, fmt.Errorf("snapshot: parsing verified bytes: %w", err)
	}
	return &snap, nil
}

// signedPayload is the byte string the signature covers.
func signedPayload(body []byte) []byte {
	out := make([]byte, 0, len(signingContext)+len(body))
	out = append(out, signingContext...)
	return append(out, body...)
}

// ---------------------------------------------------------------- key files --

// WriteKeyPair persists a keypair for local development.
//
// The private key is written 0600. It is deliberately not encrypted: this
// function exists for `make dev`, and a passphrase prompt in a dev-up script is
// theatre rather than security. Production keys must come from the operator's
// key management, never from this function.
func WriteKeyPair(dir, keyID string, pub ed25519.PublicKey, priv ed25519.PrivateKey) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("snapshot: creating key directory: %w", err)
	}
	privPath := filepath.Join(dir, keyID+".key")
	pubPath := filepath.Join(dir, keyID+".pub")
	if err := os.WriteFile(privPath, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return fmt.Errorf("snapshot: writing private key: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return fmt.Errorf("snapshot: writing public key: %w", err)
	}
	return nil
}

// LoadPrivateKey reads a base64 Ed25519 private key from a file.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot: reading signing key: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("snapshot: decoding signing key %s: %w", path, err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("snapshot: signing key %s is %d bytes, want %d",
			path, len(decoded), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(decoded), nil
}

// TrustedKeyEntry formats a public key as configuration expects it.
func TrustedKeyEntry(keyID string, pub ed25519.PublicKey) string {
	return keyID + ":" + base64.StdEncoding.EncodeToString(pub)
}

// ParseUnverified parses a snapshot's bytes *without* checking the signature.
//
// It exists for one purpose: an operator diagnosing "why will this snapshot not
// activate" needs to see the contents, and they may not hold the key it was
// signed with. Nothing here is trusted for anything — the caller is inspecting,
// not serving.
//
// The name is deliberately alarming. Nothing on a serving path may call this;
// [Verifier.Verify] is the only way a snapshot becomes servable.
func ParseUnverified(signed *snapshotpb.SignedSnapshot) (*snapshotpb.Snapshot, error) {
	if signed == nil {
		return nil, errors.New("snapshot: nothing to parse")
	}
	var snap snapshotpb.Snapshot
	if err := proto.Unmarshal(signed.SnapshotBytes, &snap); err != nil {
		return nil, fmt.Errorf("snapshot: parsing unverified bytes: %w", err)
	}
	return &snap, nil
}
