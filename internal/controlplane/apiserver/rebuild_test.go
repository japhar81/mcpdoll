// Copyright 2026 Henry Zektser.

// An internal test, where the rest of this package's tests are external.
//
// What ADR 0025 turns on is unexported — whether a rebuild counts as evidence
// of freshness, and whether two builds of the same catalog compare equal. The
// alternative was exporting a `NoteRebuildForTest` shim, which widens the
// package's surface to describe something no caller does.
package apiserver

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// A rebuild that changed nothing still proves the loop is alive. It has to move
// freshness without moving the version, because a version that only moves on
// change cannot tell a quiet deployment from a dead rebuilder.
func TestAnUnchangedRebuildStillCountsAsFresh(t *testing.T) {
	t.Parallel()
	s := &Server{}

	require.True(t, s.RebuildState().LastBuiltAt.IsZero())
	s.noteRebuild(api.BuildReport{Version: 7, Unchanged: true}, nil)

	state := s.RebuildState()
	require.False(t, state.LastBuiltAt.IsZero(), "an unchanged rebuild is still a rebuild")
	require.True(t, state.LastChangedAt.IsZero(), "nothing changed, so nothing changed")
	require.Empty(t, state.LastError)
}

func TestAChangedRebuildMovesBoth(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.noteRebuild(api.BuildReport{Version: 8}, nil)

	state := s.RebuildState()
	require.False(t, state.LastBuiltAt.IsZero())
	require.False(t, state.LastChangedAt.IsZero())
}

// A dry run resolved nothing and published nothing. Counting it would let
// `--dry-run` on a timer hold the freshness gauge green over a catalog nobody
// has actually rebuilt.
func TestADryRunIsNotEvidenceOfFreshness(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.noteRebuild(api.BuildReport{Version: 9, DryRun: true}, nil)
	require.True(t, s.RebuildState().LastBuiltAt.IsZero())
}

func TestAFailedRebuildIsReportedAndThenCleared(t *testing.T) {
	t.Parallel()
	s := &Server{}

	s.noteRebuild(api.BuildReport{}, &buildFailure{message: "no backend"})
	require.Equal(t, "no backend", s.RebuildState().LastError)

	s.noteRebuild(api.BuildReport{Version: 10}, nil)
	require.Empty(t, s.RebuildState().LastError, "a good rebuild clears the last failure")
}

// The change detector. Every build stamps a fresh version, id, and timestamp,
// so comparing the whole message would report every rebuild as a change and
// republish the same catalog once a minute forever.
func TestRebuildingTheSameCatalogIsNotAChange(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.pb")
	writeSnapshot(t, path, catalogWith("crm.lookup_customer"), 1)

	// Same content, later build: new version, new id, new timestamp.
	rebuilt := catalogWith("crm.lookup_customer")
	rebuilt.Version = 2
	rebuilt.Id = "snap_second"
	rebuilt.BuiltAt = timestamppb.Now()

	same, err := publishedIsIdentical(path, rebuilt)
	require.NoError(t, err)
	require.True(t, same, "only the build metadata differs")
}

func TestANewToolIsAChange(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.pb")
	writeSnapshot(t, path, catalogWith("crm.lookup_customer"), 1)

	same, err := publishedIsIdentical(
		path, catalogWith("crm.lookup_customer", "crm.update_customer"))
	require.NoError(t, err)
	require.False(t, same)
}

// Nothing published yet is not "unchanged" — it is the first publish, and
// treating a missing file as a match would leave the gateway with no snapshot
// while every rebuild reported success.
func TestWithNothingPublishedEverythingIsAChange(t *testing.T) {
	t.Parallel()
	same, err := publishedIsIdentical(
		filepath.Join(t.TempDir(), "absent.pb"), catalogWith("crm.lookup_customer"))
	require.NoError(t, err)
	require.False(t, same)
}

func catalogWith(names ...string) *snapshotpb.Snapshot {
	snap := &snapshotpb.Snapshot{
		Version: 1, Id: "snap_first", BuiltAt: timestamppb.Now(),
		RegistryDigest: "sha256:test",
	}
	for _, name := range names {
		snap.Tools = append(snap.Tools, &snapshotpb.ToolDefinition{QualifiedName: name})
	}
	return snap
}

func writeSnapshot(t *testing.T, path string, snap *snapshotpb.Snapshot, version int64) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer, err := snapshot.NewSigner("test", priv)
	require.NoError(t, err)
	snap.Version = version
	signed, err := signer.Sign(snap)
	require.NoError(t, err)
	require.NoError(t, snapshot.WriteSignedSnapshot(path, signed))
}
