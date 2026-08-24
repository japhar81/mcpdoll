// Copyright 2026 The MCPDoll Authors.

package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Two hash functions live in this file, and the difference is deliberate.
//
//   - Passwords are Argon2id. A human password is drawn from a distribution an
//     attacker can enumerate, so the defence has to be per-guess cost.
//   - API key secrets are SHA-256. A key secret is 192 bits from a CSPRNG.
//     There is nothing to enumerate, so a memory-hard KDF defends against
//     nothing — and it would have to run on the data plane's request path, at
//     64 MiB a time, which is a denial-of-service primitive pointed at
//     ourselves. See ADR 0021.
//
// Anyone reading this and reaching for consistency should read that ADR first.
// Making both Argon2id would reintroduce exactly the problem it names.

// API key format: `mcpd.<prefix>.<secret>`.
//
// The prefix is a public, indexed handle; the secret is what proves possession.
// Splitting them is what lets authentication be one indexed lookup followed by
// one constant-time comparison, rather than a scan that hashes every stored key.
//
// The `mcpd.` marker is not decoration: it lets a secret scanner recognise a
// leaked MCPDoll credential in a repository or a log, which is the difference
// between a key that gets revoked and one that does not.
const (
	keyMarker   = "mcpd"
	prefixBytes = 8
	secretBytes = 24
	// A dot, not an underscore.
	//
	// The prefix and secret are base64url, whose alphabet is `A-Za-z0-9-_` —
	// so both `-` and `_` occur inside the fields and neither can separate
	// them. A key whose secret happened to contain an underscore simply failed
	// to parse, which is an intermittent authentication failure that looks like
	// anything but a formatting bug. `.` is outside the alphabet, and is what
	// JWTs use for the same reason.
	keySeparator = "."
)

// Argon2id parameters.
//
// RFC 9106's second recommended option (64 MiB, 3 passes), chosen over the
// first (2 GiB) because this runs on a request path in a container with a
// memory limit — a login that OOMs the control plane is a denial of service
// wearing a security control's clothes.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// ErrMalformedCredential marks a credential that is not in the expected shape.
// Deliberately indistinguishable, to a caller, from one that simply does not
// match: an attacker learns nothing from the difference.
var ErrMalformedCredential = errors.New("malformed credential")

// NewAPIKey mints a key, returning the plaintext to show the operator once, the
// prefix to store and index, and the hash to store.
//
// The plaintext is never persisted and cannot be recovered. That is the whole
// point, and it is why the console has to be explicit that this is the only
// time it will be shown.
func NewAPIKey() (plaintext, prefix, hash string, err error) { //nolint:nonamedreturns // four strings
	prefixRaw := make([]byte, prefixBytes)
	if _, err := rand.Read(prefixRaw); err != nil {
		return "", "", "", fmt.Errorf("store: generating a key prefix: %w", err)
	}
	secretRaw := make([]byte, secretBytes)
	if _, err := rand.Read(secretRaw); err != nil {
		return "", "", "", fmt.Errorf("store: generating a key secret: %w", err)
	}

	enc := base64.RawURLEncoding
	prefix = enc.EncodeToString(prefixRaw)
	secret := enc.EncodeToString(secretRaw)

	return strings.Join([]string{keyMarker, prefix, secret}, keySeparator),
		prefix, HashKeySecret(secret), nil
}

// SplitAPIKey separates a presented key into its prefix and secret.
func SplitAPIKey(presented string) (prefix, secret string, err error) {
	parts := strings.Split(presented, keySeparator)
	if len(parts) != 3 || parts[0] != keyMarker || parts[1] == "" || parts[2] == "" {
		return "", "", ErrMalformedCredential
	}
	return parts[1], parts[2], nil
}

// HashKeySecret digests an API key secret.
//
// Unsalted and cheap, on purpose. A salt defends against a precomputed table
// over a guessable input space, and there is no table over 192 random bits;
// what it would cost instead is the ability to look a key up by its digest,
// which is exactly what lets the data plane verify offline from a signed
// snapshot rather than asking the control plane per request (ADR 0021).
func HashKeySecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// VerifyKeySecret reports whether a presented secret matches a stored digest.
//
// Constant-time comparison, because the digest is a fixed-length value an
// attacker can submit guesses against one byte at a time if the comparison
// short-circuits.
func VerifyKeySecret(secret, stored string) bool {
	computed := HashKeySecret(secret)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(stored)) == 1
}

// HashSecret derives a storable hash for a *password*, salted per secret.
//
// The encoded form carries its own parameters, so a future change to the cost
// settings does not invalidate existing hashes — they verify with the
// parameters they were created with, and can be upgraded on next use.
func HashSecret(secret string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("store: generating a salt: %w", err)
	}
	digest := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(digest)), nil
}

// VerifySecret reports whether a password matches a stored Argon2id hash.
//
// Constant-time, and it does the full derivation even for a hash it cannot
// parse — see below.
func VerifySecret(secret, encoded string) bool {
	parsed, err := parseArgonHash(encoded)
	if err != nil {
		// A malformed stored hash must still cost what a real verification
		// costs. Returning early here would make "no such user" measurably
		// faster than "wrong password", which is a user-enumeration oracle.
		dummy := make([]byte, argonSaltLen)
		argon2.IDKey([]byte(secret), dummy, argonTime, argonMemory, argonThreads, argonKeyLen)
		return false
	}

	computed := argon2.IDKey([]byte(secret), parsed.salt,
		parsed.time, parsed.memory, parsed.threads, uint32(len(parsed.digest)))
	return subtle.ConstantTimeCompare(computed, parsed.digest) == 1
}

type argonHash struct {
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	digest  []byte
}

func parseArgonHash(encoded string) (argonHash, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, digest
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonHash{}, ErrMalformedCredential
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argonHash{}, ErrMalformedCredential
	}
	if version != argon2.Version {
		// A hash from a different Argon2 version would verify against the
		// wrong algorithm and silently never match.
		return argonHash{}, ErrMalformedCredential
	}

	var out argonHash
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&out.memory, &out.time, &out.threads); err != nil {
		return argonHash{}, ErrMalformedCredential
	}

	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return argonHash{}, ErrMalformedCredential
	}
	digest, err := enc.DecodeString(parts[5])
	if err != nil || len(digest) == 0 {
		return argonHash{}, ErrMalformedCredential
	}
	out.salt, out.digest = salt, digest
	return out, nil
}
