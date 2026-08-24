// Copyright 2026 The MCPDoll Authors.

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/store/dbgen"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// Revocation: the one thing that must not wait for a snapshot.
//
// Everything else in this system takes effect at snapshot latency, and ADR 0018
// argued that is right. A leaked credential is the exception — the reason
// somebody revokes a key is that it is being used *right now* — so revocations
// travel in their own signed artifact that can only subtract (ADR 0023).

// Revocation is one refused principal.
type Revocation struct {
	ID          int64      `json:"id"`
	PrincipalID uuid.UUID  `json:"principal_id"`
	Kind        string     `json:"kind"`
	UserID      *uuid.UUID `json:"user_id,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	RevokedAt   time.Time  `json:"revoked_at"`
}

// RevocationState is the published list's version and its pruning watermark.
type RevocationState struct {
	Version int64 `json:"version"`
	// PrunedThrough is the snapshot version that already reflects everything
	// removed from the list. A data plane serving older than this refuses the
	// list rather than losing denials it still needs.
	PrunedThrough int64     `json:"pruned_through"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Revoke records one principal as refused and bumps the list version.
//
// The bump is what makes the new list acceptable to a data plane that already
// holds one: it refuses anything not newer, for the same reason it refuses a
// stale snapshot.
func (s *Store) Revoke(
	ctx context.Context, principalID uuid.UUID, kind string, userID *uuid.UUID, reason string,
) (RevocationState, error) {
	if _, err := s.q.AddRevocation(ctx, dbgen.AddRevocationParams{
		PrincipalID: principalID, Kind: kind, UserID: userID, Reason: nilIfEmpty(reason),
	}); err != nil {
		return RevocationState{}, wrap(err, "recording the revocation")
	}
	row, err := s.q.BumpRevocationVersion(ctx)
	if err != nil {
		return RevocationState{}, wrap(err, "bumping the revocation version")
	}
	return revocationStateFrom(row), nil
}

// RevokeUser refuses every credential a user holds: their API keys and their
// sessions.
//
// Both, because they fail differently and only one of them is loud. Revoking
// only their sessions leaves the person gone from the console with their
// automation still running — the failure hardest to notice, because everything
// an offboarding checklist *looks* at says they are gone. Revoking only their
// keys is the reverse and far more obvious: their agents stop and they can
// still sign in.
//
// The keys are what reach the data plane, so they are what the signed
// revocation list is for. The sessions are control-plane credentials and are
// already refused by [Store.ResolveSession], which re-reads the owner's status
// on every call — marking the rows and listing them is defence in depth and
// makes the state visible, not the mechanism.
func (s *Store) RevokeUser(ctx context.Context, userID uuid.UUID, reason string) (RevocationState, error) {
	keyIDs, err := s.q.ListAPIKeyIDsByUser(ctx, userID)
	if err != nil {
		return RevocationState{}, wrap(err, "listing the user's keys")
	}
	for _, id := range keyIDs {
		if _, err := s.q.AddRevocation(ctx, dbgen.AddRevocationParams{
			PrincipalID: id, Kind: "api_key", UserID: &userID, Reason: nilIfEmpty(reason),
		}); err != nil {
			return RevocationState{}, wrap(err, "revoking key %s", id)
		}
	}

	sessionIDs, err := s.q.RevokeSessionsByUser(ctx, userID)
	if err != nil {
		return RevocationState{}, wrap(err, "revoking the user's sessions")
	}
	for _, id := range sessionIDs {
		if _, err := s.q.AddRevocation(ctx, dbgen.AddRevocationParams{
			PrincipalID: id, Kind: "session", UserID: &userID, Reason: nilIfEmpty(reason),
		}); err != nil {
			return RevocationState{}, wrap(err, "revoking session %s", id)
		}
	}

	row, err := s.q.BumpRevocationVersion(ctx)
	if err != nil {
		return RevocationState{}, wrap(err, "bumping the revocation version")
	}
	return revocationStateFrom(row), nil
}

// BumpRevocationVersion advances the published version without changing the
// set.
//
// Used by the heartbeat: republishing an unchanged list is what keeps its age
// bounded, and the data plane refuses anything not newer — so a heartbeat that
// did not bump would publish a file nobody applies.
func (s *Store) BumpRevocationVersion(ctx context.Context) (RevocationState, error) {
	row, err := s.q.BumpRevocationVersion(ctx)
	if err != nil {
		return RevocationState{}, wrap(err, "bumping the revocation version")
	}
	return revocationStateFrom(row), nil
}

// ListRevocations returns everything still in effect.
func (s *Store) ListRevocations(ctx context.Context) ([]Revocation, error) {
	rows, err := s.q.ListActiveRevocations(ctx)
	if err != nil {
		return nil, wrap(err, "listing revocations")
	}
	out := make([]Revocation, 0, len(rows))
	for _, row := range rows {
		out = append(out, revocationFrom(row))
	}
	return out, nil
}

// RevocationState reads the published list's version and watermark.
func (s *Store) RevocationState(ctx context.Context) (RevocationState, error) {
	row, err := s.q.GetRevocationState(ctx)
	if err != nil {
		return RevocationState{}, wrap(err, "reading the revocation state")
	}
	return revocationStateFrom(row), nil
}

// RevocationList builds the artifact to sign.
func (s *Store) RevocationList(ctx context.Context) (*snapshotpb.RevocationList, error) {
	state, err := s.RevocationState(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := s.ListRevocations(ctx)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.PrincipalID.String())
	}
	// Version zero would be refused by the data plane as not newer than its
	// starting state, so a deployment that has never revoked anything publishes
	// version 1 with an empty set rather than nothing at all. A list that
	// exists and is empty is a different, better state than no list: it proves
	// the pipeline works before anybody needs it to.
	version := state.Version
	if version <= 0 {
		version = 1
	}
	return &snapshotpb.RevocationList{
		Version:                      version,
		IssuedAt:                     timestamppb.New(time.Now()),
		PrincipalIds:                 ids,
		PrunedThroughSnapshotVersion: state.PrunedThrough,
	}, nil
}

// PruneRevocations drops entries a published snapshot already reflects.
//
// `builtAt` must be when the build *read the database*, not when it finished:
// anything committed after that read is not in the snapshot, and pruning it
// would silently un-revoke a credential.
func (s *Store) PruneRevocations(
	ctx context.Context, snapshotVersion int64, builtAt time.Time,
) (RevocationState, error) {
	if err := s.q.SupersedeRevocationsBefore(ctx, dbgen.SupersedeRevocationsBeforeParams{
		SupersededBy: &snapshotVersion, RevokedAt: timestamptz(builtAt),
	}); err != nil {
		return RevocationState{}, wrap(err, "pruning revocations")
	}
	row, err := s.q.SetRevocationPrunedThrough(ctx, snapshotVersion)
	if err != nil {
		return RevocationState{}, wrap(err, "recording the pruning watermark")
	}
	return revocationStateFrom(row), nil
}

func revocationFrom(row dbgen.Revocation) Revocation {
	out := Revocation{
		ID: row.ID, PrincipalID: row.PrincipalID, Kind: row.Kind,
		UserID: row.UserID, RevokedAt: row.RevokedAt.Time,
	}
	if row.Reason != nil {
		out.Reason = *row.Reason
	}
	return out
}

func revocationStateFrom(row dbgen.RevocationState) RevocationState {
	return RevocationState{
		Version: row.Version, PrunedThrough: row.PrunedThrough,
		UpdatedAt: row.UpdatedAt.Time,
	}
}
