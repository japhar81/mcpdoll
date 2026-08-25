// Copyright 2026 Henry Zektser.

package apiserver

import (
	"context"
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
	Interval  string `json:"interval,omitempty"`
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
func (s *Server) RebuildState() RebuildState {
	state := s.rebuilds.snapshot()
	state.Interval = s.rebuildInterval().String()
	return state
}

func (s *Server) rebuildInterval() time.Duration {
	if s.cfg.RebuildInterval > 0 {
		return s.cfg.RebuildInterval
	}
	return DefaultRebuildInterval
}

// RunSnapshotRebuild rebuilds the catalog until ctx is cancelled.
//
// Failures are logged and recorded, never fatal. A backend that cannot be
// reached must not stop the loop: the previously published snapshot keeps
// serving, which is the behaviour ADR 0002 exists to provide, and the next
// tick tries again.
func (s *Server) RunSnapshotRebuild(ctx context.Context) {
	if s.cfg.SigningKeyPath == "" || s.cfg.SnapshotPath == "" {
		// Nothing to publish or nothing to sign with. A control plane in this
		// shape is a read-only one — `mcpdoll snapshot build` is run where the
		// key lives — and a loop that failed every minute would say so once a
		// minute forever.
		return
	}

	s.rebuildOnce(ctx)

	ticker := time.NewTicker(s.rebuildInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.rebuildOnce(ctx)
		}
	}
}

func (s *Server) rebuildOnce(ctx context.Context) {
	report, fail := s.buildAndPublish(ctx, BuildSnapshotRequest{})
	if fail != nil {
		s.rebuilds.note(time.Now(), false, fail)
		s.log.Warn("rebuilding the catalog failed",
			"problem", fail.message, "detail", problemDetail(fail.problems))
		return
	}
	s.noteRebuild(report, nil)
	if report.Unchanged {
		return
	}
	// Only on change. A line a minute saying nothing happened is how a log
	// stops being read.
	// `snapshot_version`, not `version`: the logger already carries a base
	// field named `version` for the build, and a JSON object with the key twice
	// is one a collector resolves however it likes.
	s.log.Info("catalog rebuilt",
		"snapshot_version", report.Version,
		"tools", report.Tools, "servers", report.Servers)
}

func problemDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
