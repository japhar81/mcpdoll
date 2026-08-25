// Copyright 2026 Henry Zektser.

package apiserver

import (
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// The snapshot rebuilds itself (ADR 0025).
//
// It used to be something a person published. That was never a decision anybody
// made — the registry says what should be served and the backends say what they
// have, so the correct catalog is a function of two things nobody needs a
// button to combine. What the button actually produced was a system with two
// states, "what is configured" and "what is serving", and an operator who had
// to remember they were different.

// DefaultRebuildInterval is how often the catalog is rebuilt from the backends.
//
// A minute rather than seconds: a rebuild is a discovery sweep of every backend
// behind the gateway, and the cost scales with how many there are. Seconds would
// make a large deployment spend its life discovering.
const DefaultRebuildInterval = 60 * time.Second

// RebuildState is what the surfaces report about the rebuild loop.
//
// LastBuiltAt is separate from the snapshot version on purpose, and it is the
// whole reason this type exists. ADR 0023 recorded the same lesson about
// revocations: if the only number reported is one that moves on *change*, then
// a deployment where nothing has changed and a deployment whose rebuild loop
// died look identical. That is worse than reporting nothing, because it looks
// like a control.
//
// So the version answers "what is serving" and LastBuiltAt answers "is this
// still being checked". A stale LastBuiltAt is the alert.
type RebuildState struct {
	LastBuiltAt   time.Time `json:"last_built_at,omitempty"`
	LastChangedAt time.Time `json:"last_changed_at,omitempty"`
	// Empty when the last rebuild succeeded. Reported rather than only logged:
	// a rebuild that has been failing for an hour is invisible in a log nobody
	// is tailing, and the catalog silently goes stale.
	LastError string `json:"last_error,omitempty"`
}

type rebuildTracker struct {
	mu    sync.RWMutex
	state RebuildState
}

func (t *rebuildTracker) note(builtAt time.Time, changed bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.LastBuiltAt = builtAt
	if changed {
		t.state.LastChangedAt = builtAt
	}
	if err != nil {
		t.state.LastError = err.Error()
		return
	}
	t.state.LastError = ""
}

func (t *rebuildTracker) snapshot() RebuildState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}

// noteRebuild records the outcome of a build, whoever triggered it.
//
// A request-triggered build counts: it proves the pipeline works just as well
// as a timer-triggered one, and treating only the timer's as evidence would
// report a stale catalog immediately after somebody rebuilt one by hand.
func (s *Server) noteRebuild(report api.BuildReport, err error) {
	if report.DryRun {
		// A dry run resolved nothing and published nothing. Counting it would
		// let `--dry-run` in a loop hold the freshness gauge green over a
		// catalog that has not been rebuilt in days.
		return
	}
	s.rebuilds.note(time.Now(), !report.Unchanged, err)
}

// RebuildState reports what the rebuild loop has been doing.
//
// Not how often it runs. That used to be here and became a lie the moment
// cadences moved into rows (ADR 0026): this would have gone on reporting the
// configured default while the schedule an operator retuned ran at something
// else. The cadence has one home now, and it is the schedule.
func (s *Server) RebuildState() RebuildState { return s.rebuilds.snapshot() }

// rebuildInterval is the cadence a *new* deployment's catalog schedule is
// created with. Once the row exists it wins, so this is a seed rather than a
// setting — see [Store.RegisterSchedule].
func (s *Server) rebuildInterval() time.Duration {
	if s.cfg.RebuildInterval > 0 {
		return s.cfg.RebuildInterval
	}
	return DefaultRebuildInterval
}

// publishedIsIdentical reports whether the snapshot already on disk carries the
// same content as the one just built.
//
// Build metadata is cleared from both before comparing: version, id, and
// built_at differ on every build by construction, so leaving them in would
// make every snapshot look changed and defeat the check entirely.
//
// This is a change detector, not an identity. It compares two messages this
// process built moments apart with the same code, so deterministic proto
// marshalling is enough — it is deliberately not the JCS canonicalization the
// signed digests use, which has to mean the same thing to another
// implementation on another machine.
func publishedIsIdentical(path string, built *snapshotpb.Snapshot) (bool, error) {
	signed, err := snapshot.ReadSignedSnapshot(path)
	if err != nil {
		// No published snapshot yet, or one that cannot be read. Either way the
		// answer is "publish this one".
		return false, nil
	}
	current, err := snapshot.ParseUnverified(signed)
	if err != nil {
		return false, nil
	}

	a, err := comparableBytes(current)
	if err != nil {
		return false, fmt.Errorf("canonicalizing the published snapshot: %w", err)
	}
	b, err := comparableBytes(built)
	if err != nil {
		return false, fmt.Errorf("canonicalizing the built snapshot: %w", err)
	}
	return string(a) == string(b), nil
}

func comparableBytes(in *snapshotpb.Snapshot) ([]byte, error) {
	stripped, ok := proto.Clone(in).(*snapshotpb.Snapshot)
	if !ok {
		return nil, fmt.Errorf("cloning the snapshot produced %T", stripped)
	}
	stripped.Version = 0
	stripped.Id = ""
	stripped.BuiltAt = nil
	return proto.MarshalOptions{Deterministic: true}.Marshal(stripped)
}
