// Copyright 2026 Henry Zektser.

package snapshot

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// ErrStaleVersion reports a snapshot no newer than the one being served.
type ErrStaleVersion struct {
	Offered int64
	Serving int64
}

func (e *ErrStaleVersion) Error() string {
	return fmt.Sprintf("snapshot: refusing version %d while serving %d", e.Offered, e.Serving)
}

// ErrNotFound reports a rollback target the instance no longer retains.
type ErrNotFound struct {
	Version int64
}

func (e *ErrNotFound) Error() string {
	return fmt.Sprintf("snapshot: version %d is not in local history", e.Version)
}

// Store holds the snapshot the data plane is serving and swaps in new ones.
//
// Reads go through an atomic.Pointer, so the serving path never takes a lock and
// an in-flight request keeps the exact view it started with even while a newer
// snapshot activates underneath it. Writes are serialized by a mutex because
// activation is rare and has to be ordered.
type Store struct {
	current atomic.Pointer[View]

	// revocations is the second signed artifact (ADR 0023). Held beside the
	// snapshot rather than inside it, because it changes on a different clock:
	// a snapshot is republished when configuration changes, a revocation list
	// the moment somebody revokes something.
	//
	// It only ever subtracts. Nothing read from here can authorize anything, so
	// an *allowed* action is still explained by the snapshot alone.
	revocations atomic.Pointer[Revocations]

	mu      sync.Mutex
	history []*historyEntry
	// maxHistory bounds retained snapshots. History exists so an operator can
	// roll back without the control plane, which is precisely the situation
	// where the control plane is the thing that broke.
	maxHistory int

	// observers are notified after a successful activation, so the edge can
	// rebuild its per-audience MCP servers exactly once per swap rather than
	// checking for a new snapshot on every request.
	observers []func(*View)

	// onReject is notified when a snapshot is refused, for whatever the caller
	// wants to do about it.
	onReject func(version int64, reason string)

	// preparer runs after a snapshot is verified and indexed but *before* it is
	// swapped in, and may refuse it.
	//
	// This exists because indexing a snapshot is not the same as being able to
	// serve it. The edge has to construct an MCP server per audience, and that
	// construction can fail on a definition the view builder accepted. Without a
	// fallible pre-swap step those failures happen after the commit — or, worse,
	// panic — and a bad publish takes down every instance simultaneously instead
	// of being refused.
	preparer func(*View) error

	lastReject struct {
		version int64
		reason  string
	}
}

type historyEntry struct {
	version int64
	signed  *snapshotpb.SignedSnapshot
	view    *View
}

// NewStore builds an empty store. The data plane is not ready to serve until a
// snapshot has been activated.
func NewStore(maxHistory int) *Store {
	if maxHistory < 1 {
		maxHistory = 1
	}
	return &Store{maxHistory: maxHistory}
}

// Current returns the serving view, or nil before the first activation.
//
// Callers must tolerate nil: a data plane that has started but not yet received
// a snapshot is a real state, and it must fail requests with a clear "not ready"
// rather than panic.
func (s *Store) Current() *View { return s.current.Load() }

// Revocations returns the list in effect. Never nil: a gateway that has never
// loaded one and one that loaded an empty one behave identically, and they
// should — neither has anything to refuse.
func (s *Store) Revocations() *Revocations {
	if r := s.revocations.Load(); r != nil {
		return r
	}
	return EmptyRevocations()
}

// ApplyRevocations swaps in a newer list, or refuses it and says why.
//
// Two refusals, and both keep strictly more denials than accepting would:
//
//   - A list no newer than the one held. Same monotonicity rule as the
//     snapshot, and for the same reason: a replayed older artifact must not
//     roll policy backwards.
//   - A list pruned against a snapshot newer than the one being served.
//     Accepting it would silently drop denials this snapshot has not absorbed.
//     It self-corrects the moment that snapshot lands.
func (s *Store) ApplyRevocations(next *Revocations) error {
	if next == nil {
		return errors.New("snapshot: nothing to apply")
	}
	if held := s.revocations.Load(); held != nil && next.Version <= held.Version {
		return ErrStaleRevocations
	}
	if serving := s.Version(); next.PrunedThroughVersion > serving {
		return ErrRevocationsAheadOfSnapshot
	}
	s.revocations.Store(next)
	return nil
}

// Version returns the serving version, or 0 if nothing is active.
func (s *Store) Version() int64 {
	if v := s.current.Load(); v != nil {
		return v.Version
	}
	return 0
}

// Observe registers a callback invoked after each activation, with the store's
// write lock released so an observer may call back into the store.
func (s *Store) Observe(fn func(*View)) {
	s.mu.Lock()
	s.observers = append(s.observers, fn)
	s.mu.Unlock()
}

// SetPreparer registers the single function that must succeed before a snapshot
// is swapped in.
//
// Returning an error refuses the activation and leaves the current snapshot
// serving, exactly as a signature or validation failure does. There is one
// preparer rather than a list because "prepared" is not a partial state: if two
// preparers ran and the second failed, the first would already have committed.
func (s *Store) SetPreparer(fn func(*View) error) {
	s.mu.Lock()
	s.preparer = fn
	s.mu.Unlock()
}

// Activate verifies, builds, and swaps in a snapshot.
//
// Failure at any stage leaves the current snapshot serving, untouched. That is
// the central availability property of the data plane: a bad publish degrades to
// "still serving the last good configuration", never to an outage. The reason is
// recorded so the console can show *why* an instance is behind rather than
// merely that it is.
func (s *Store) Activate(signed *snapshotpb.SignedSnapshot, v *Verifier) (*View, error) {
	if signed == nil {
		return nil, errors.New("snapshot: nothing to activate")
	}
	if v == nil {
		return nil, errors.New("snapshot: a verifier is required; an unverified snapshot is never activated")
	}

	snap, err := v.Verify(signed)
	if err != nil {
		s.recordReject(0, err.Error())
		return nil, err
	}

	s.mu.Lock()
	serving := s.current.Load()
	if serving != nil && snap.Version <= serving.Version {
		s.mu.Unlock()
		err := &ErrStaleVersion{Offered: snap.Version, Serving: serving.Version}
		s.recordReject(snap.Version, err.Error())
		return nil, err
	}
	s.mu.Unlock()

	// Build outside the lock: indexing is the expensive part and does not need
	// exclusion, and a slow build must not block a concurrent rollback.
	view, err := Build(snap)
	if err != nil {
		s.recordReject(snap.Version, err.Error())
		return nil, err
	}

	s.mu.Lock()
	// Re-check under the lock: two concurrent activations must not race, and
	// the second must lose rather than roll the first one back.
	if serving := s.current.Load(); serving != nil && view.Version <= serving.Version {
		s.mu.Unlock()
		err := &ErrStaleVersion{Offered: view.Version, Serving: serving.Version}
		s.recordReject(view.Version, err.Error())
		return nil, err
	}
	preparer := s.preparer
	s.mu.Unlock()

	// Prepare outside the lock — building an MCP server per audience is real
	// work — and refuse the snapshot if it fails.
	if preparer != nil {
		if err := preparer(view); err != nil {
			s.recordReject(view.Version, err.Error())
			return nil, fmt.Errorf("snapshot: version %d cannot be served: %w", view.Version, err)
		}
	}

	s.mu.Lock()
	if serving := s.current.Load(); serving != nil && view.Version <= serving.Version {
		s.mu.Unlock()
		err := &ErrStaleVersion{Offered: view.Version, Serving: serving.Version}
		s.recordReject(view.Version, err.Error())
		return nil, err
	}
	s.history = append(s.history, &historyEntry{version: view.Version, signed: signed, view: view})
	if len(s.history) > s.maxHistory {
		s.history = s.history[len(s.history)-s.maxHistory:]
	}
	s.current.Store(view)
	observers := slices.Clone(s.observers)
	s.lastReject.version = 0
	s.lastReject.reason = ""
	s.mu.Unlock()

	for _, fn := range observers {
		fn(view)
	}
	return view, nil
}

// Rollback re-activates a retained earlier version.
//
// This is the only path that moves the serving version backwards, and it is
// deliberately explicit: an operator asking for it has decided the newer
// snapshot is worse than the older one. Automatic rollback is not offered
// because the gateway cannot tell a bad policy change from a correct one that
// happens to deny more traffic.
func (s *Store) Rollback(version int64) (*View, error) {
	s.mu.Lock()
	var target *historyEntry
	for _, e := range s.history {
		if e.version == version {
			target = e
			break
		}
	}
	if target == nil {
		s.mu.Unlock()
		return nil, &ErrNotFound{Version: version}
	}
	// Re-index rather than reusing the stored view, so LoadedAt reflects when
	// this instance actually started serving it — which is what the age metric
	// and the console's per-instance display should show.
	view, err := Build(target.view.Proto())
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("snapshot: rebuilding version %d for rollback: %w", version, err)
	}
	preparer := s.preparer
	s.mu.Unlock()

	// A rollback target was servable once, but prepare it again rather than
	// assuming: the binary may have changed since it was last activated.
	if preparer != nil {
		if err := preparer(view); err != nil {
			return nil, fmt.Errorf("snapshot: version %d can no longer be served: %w", version, err)
		}
	}

	s.mu.Lock()
	s.current.Store(view)
	observers := slices.Clone(s.observers)
	s.mu.Unlock()

	for _, fn := range observers {
		fn(view)
	}
	return view, nil
}

// History lists the retained versions, oldest first.
func (s *Store) History() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.history))
	for _, e := range s.history {
		out = append(out, e.version)
	}
	return out
}

// Signed returns the signed bytes of a retained version, so an instance can
// re-serve or re-verify what it was given without asking the control plane.
func (s *Store) Signed(version int64) (*snapshotpb.SignedSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.history {
		if e.version == version {
			return e.signed, nil
		}
	}
	return nil, &ErrNotFound{Version: version}
}

// LastReject reports the most recent refused activation, for the console's
// per-instance status. Zero version means nothing has been refused since the
// last successful activation.
func (s *Store) LastReject() (int64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReject.version, s.lastReject.reason
}

// SetRejectObserver installs a hook called whenever a snapshot is refused.
//
// A callback rather than a metrics dependency: this package is below
// observability, and a refusal is a fact about the store that several callers
// might want — a counter, a log line, an alert — rather than a metric the
// store should be opinionated about.
func (s *Store) SetRejectObserver(fn func(version int64, reason string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onReject = fn
}

func (s *Store) recordReject(version int64, reason string) {
	s.mu.Lock()
	s.lastReject.version = version
	s.lastReject.reason = reason
	observer := s.onReject
	s.mu.Unlock()

	// Outside the lock. A hook that recorded a metric or wrote a log while
	// holding the store's mutex would serialise every activation behind it.
	if observer != nil {
		observer(version, reason)
	}
}
