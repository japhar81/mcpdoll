// Copyright 2026 Henry Zektser.

package snapshot

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// Who exists and what they hold.
//
// This used to live inside the snapshot, which meant minting an API key
// required re-probing every backend — and made a grant, which names a *scope*
// and is deliberately independent of what that scope currently admits, depend
// on the catalog's publish cycle. ADR 0024 separates them.
//
// Nothing about the authorization model changed: the same grants, the same
// scopes, the same compiled decider. Only where the data arrives from.

// principalSigningContext is prefixed to the signed bytes.
//
// Distinct from the snapshot's and the revocation list's, so a signature minted
// over one artifact cannot verify against another.
var principalSigningContext = []byte("mcpdoll.principals.v1\x00")

// ErrStalePrincipals reports a set no newer than the one already held.
var ErrStalePrincipals = errors.New("snapshot: principal set is not newer than the one in effect")

// Principals is a verified principal set, indexed for lookup.
type Principals struct {
	Version  int64
	IssuedAt time.Time

	catalog     authz.Catalog
	byID        map[string]*snapshotpb.Principal
	byKeyPrefix map[string]*snapshotpb.Principal
	engine      authz.Engine
}

// EmptyPrincipals is the starting state: nobody, version zero.
//
// A real value rather than a nil check at every call site. A gateway that has
// never loaded a set serves nobody, which is correct — not an error.
func EmptyPrincipals() *Principals {
	return &Principals{
		catalog:     authz.Catalog{},
		byID:        map[string]*snapshotpb.Principal{},
		byKeyPrefix: map[string]*snapshotpb.Principal{},
		engine:      authz.BuiltinEngine{},
	}
}

// Count is how many principals this set carries.
func (p *Principals) Count() int {
	if p == nil {
		return 0
	}
	return len(p.byID)
}

// Age is how long ago this set was issued.
//
// The bound on how out of date the gateway's idea of who exists can be — a key
// minted more recently than this is not yet usable.
func (p *Principals) Age() time.Duration {
	if p == nil || p.IssuedAt.IsZero() {
		return 0
	}
	return time.Since(p.IssuedAt)
}

// ByID returns one principal.
func (p *Principals) ByID(id string) (*snapshotpb.Principal, bool) {
	if p == nil {
		return nil, false
	}
	v, ok := p.byID[id]
	return v, ok
}

// ByKeyPrefix returns the principal a credential prefix addresses.
//
// The prefix proves nothing on its own — the caller still verifies the secret
// against KeySecretSha256. Separating them is what lets the lookup be indexed
// while the comparison stays constant-time (ADR 0021).
func (p *Principals) ByKeyPrefix(prefix string) (*snapshotpb.Principal, bool) {
	if p == nil {
		return nil, false
	}
	v, ok := p.byKeyPrefix[prefix]
	return v, ok
}

// IDs lists every principal, sorted.
func (p *Principals) IDs() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.byID))
	for id := range p.byID {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// decider compiles one principal's grants.
func (p *Principals) decider(ctx context.Context, principal *snapshotpb.Principal) (authz.Decider, error) {
	grants := make([]authz.Grant, 0, len(principal.Grants))
	for _, g := range principal.Grants {
		grants = append(grants, authz.Grant{Role: g.Role, Scope: g.Scope})
	}
	return p.engine.Prepare(ctx, grants, p.catalog)
}

// SignPrincipals signs a principal set.
func (s *Signer) SignPrincipals(set *snapshotpb.PrincipalSet) (*snapshotpb.SignedPrincipalSet, error) {
	if set == nil {
		return nil, errors.New("snapshot: cannot sign a nil principal set")
	}
	if set.Version <= 0 {
		return nil, errors.New("snapshot: a principal set needs a positive version")
	}
	if set.IssuedAt == nil {
		set.IssuedAt = timestamppb.Now()
	}

	body, err := proto.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("snapshot: marshalling the principal set: %w", err)
	}
	return &snapshotpb.SignedPrincipalSet{
		SetBytes:  body,
		KeyId:     s.keyID,
		Algorithm: AlgorithmEd25519,
		Signature: ed25519.Sign(s.priv, principalSignedPayload(body)),
	}, nil
}

// VerifyPrincipals checks a signed set against the trusted keys and indexes it.
func (v *Verifier) VerifyPrincipals(signed *snapshotpb.SignedPrincipalSet) (*Principals, error) {
	return v.VerifyPrincipalsWithEngine(signed, authz.BuiltinEngine{})
}

// VerifyPrincipalsWithEngine is [Verifier.VerifyPrincipals] with a chosen
// authorization engine, so a deployment running Casbin gets it here too.
func (v *Verifier) VerifyPrincipalsWithEngine(
	signed *snapshotpb.SignedPrincipalSet, engine authz.Engine,
) (*Principals, error) {
	if signed == nil {
		return nil, errors.New("snapshot: no principal set")
	}
	if signed.Algorithm != "" && signed.Algorithm != AlgorithmEd25519 {
		return nil, &ErrUnsupportedAlgorithm{Algorithm: signed.Algorithm}
	}
	pub, ok := v.keys[signed.KeyId]
	if !ok {
		return nil, &ErrUntrustedKey{KeyID: signed.KeyId}
	}
	if !ed25519.Verify(pub, principalSignedPayload(signed.SetBytes), signed.Signature) {
		return nil, ErrBadSignature
	}

	var set snapshotpb.PrincipalSet
	if err := proto.Unmarshal(signed.SetBytes, &set); err != nil {
		return nil, fmt.Errorf("snapshot: parsing verified principal bytes: %w", err)
	}
	return IndexPrincipalsWithEngine(&set, engine)
}

// IndexPrincipals indexes a set that has NOT had its signature checked.
//
// The counterpart to [ParseUnverified], and it exists for the same two reasons:
// a tool that wants to *look* at a set signed by a key it does not hold, and a
// test that wants one without minting a keypair. Nothing that serves traffic
// should call it — [Verifier.VerifyPrincipals] is the path a data plane takes,
// and it calls this after checking the signature.
func IndexPrincipals(set *snapshotpb.PrincipalSet) (*Principals, error) {
	return IndexPrincipalsWithEngine(set, authz.BuiltinEngine{})
}

// IndexPrincipalsWithEngine is [IndexPrincipals] with a chosen engine.
func IndexPrincipalsWithEngine(set *snapshotpb.PrincipalSet, engine authz.Engine) (*Principals, error) {
	if set == nil {
		return nil, errors.New("snapshot: no principal set")
	}

	out := EmptyPrincipals()
	out.Version = set.Version
	out.engine = engine
	if set.IssuedAt != nil {
		out.IssuedAt = set.IssuedAt.AsTime()
	}

	for _, rp := range set.RolePermissions {
		if out.catalog[rp.Role] == nil {
			out.catalog[rp.Role] = map[authz.Permission]struct{}{}
		}
		out.catalog[rp.Role][authz.Permission(rp.Permission)] = struct{}{}
	}
	// A catalog granting tool:call without tool:list would produce capabilities
	// that never appear in any catalog. Refused here rather than served, the
	// same check the builder used to make (ADR 0015).
	if len(out.catalog) > 0 {
		if err := authz.ValidateCatalog(out.catalog); err != nil {
			return nil, fmt.Errorf("snapshot: principal set's role catalog: %w", err)
		}
	}

	for _, p := range set.Principals {
		if p.Id == "" {
			return nil, errors.New("snapshot: a principal has no id")
		}
		if _, dup := out.byID[p.Id]; dup {
			return nil, fmt.Errorf("snapshot: principal id %q appears twice", p.Id)
		}
		out.byID[p.Id] = p

		if p.KeyPrefix != "" {
			if _, dup := out.byKeyPrefix[p.KeyPrefix]; dup {
				// Not a duplicate id — an ambiguity about who is calling, whose
				// answer would depend on map iteration order.
				return nil, fmt.Errorf(
					"snapshot: key prefix %q is claimed by two principals", p.KeyPrefix)
			}
			if p.KeySecretSha256 == "" {
				return nil, fmt.Errorf(
					"snapshot: principal %q carries a key prefix with no digest, so "+
						"any secret would verify against it", p.Subject)
			}
			out.byKeyPrefix[p.KeyPrefix] = p
		}
	}
	return out, nil
}

func principalSignedPayload(body []byte) []byte {
	out := make([]byte, 0, len(principalSigningContext)+len(body))
	out = append(out, principalSigningContext...)
	return append(out, body...)
}

// WriteSignedPrincipals persists a set atomically.
func WriteSignedPrincipals(path string, signed *snapshotpb.SignedPrincipalSet) error {
	raw, err := proto.Marshal(signed)
	if err != nil {
		return fmt.Errorf("snapshot: marshalling the signed principal set: %w", err)
	}
	return writeAtomic(path, raw, ".principals-*")
}

// ReadSignedPrincipals reads a set from disk without verifying it.
func ReadSignedPrincipals(path string) (*snapshotpb.SignedPrincipalSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var signed snapshotpb.SignedPrincipalSet
	if err := proto.Unmarshal(raw, &signed); err != nil {
		return nil, fmt.Errorf("snapshot: %s is not a signed principal set: %w", path, err)
	}
	return &signed, nil
}

// writeAtomic writes bytes through a temporary file and a rename.
//
// A watcher must never observe a partial write — it would read as a signature
// failure and look like an attack.
func writeAtomic(path string, raw []byte, pattern string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("snapshot: creating %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return fmt.Errorf("snapshot: creating a temporary file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: writing %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: syncing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("snapshot: closing %s: %w", path, err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("snapshot: setting permissions on %s: %w", path, err)
	}
	return os.Rename(tmp.Name(), path)
}
