// Copyright 2026 Henry Zektser.

package snapshot

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

func signedFixture(t *testing.T, s *Signer, version int64) *snapshotpb.SignedSnapshot {
	t.Helper()
	snap, err := defaultFixture(version).b.Build()
	require.NoError(t, err)
	signed, err := s.Sign(snap)
	require.NoError(t, err)
	return signed
}

func TestStoreStartsEmpty(t *testing.T) {
	s := NewStore(3)
	require.Nil(t, s.Current(), "a data plane with no snapshot must report nil, not panic")
	require.Zero(t, s.Version())
	require.Empty(t, s.History())
}

func TestActivate(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(3)

	view, err := store.Activate(signedFixture(t, signer, 1), verifier)
	require.NoError(t, err)
	require.Equal(t, int64(1), view.Version)
	require.Same(t, view, store.Current())
	require.Equal(t, int64(1), store.Version())
	require.Equal(t, []int64{1}, store.History())
}

func TestActivateRequiresVerifier(t *testing.T) {
	signer, _ := testSigner(t, "k1")
	store := NewStore(3)
	_, err := store.Activate(signedFixture(t, signer, 1), nil)
	require.ErrorContains(t, err, "verifier is required")

	_, err = store.Activate(nil, nil)
	require.ErrorContains(t, err, "nothing to activate")
}

// TestActivateRefusesStaleVersion: version monotonicity is what stops a
// replayed or misrouted older snapshot silently rolling policy backwards.
func TestActivateRefusesStaleVersion(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(3)

	_, err := store.Activate(signedFixture(t, signer, 5), verifier)
	require.NoError(t, err)

	for _, version := range []int64{1, 4, 5} {
		_, err := store.Activate(signedFixture(t, signer, version), verifier)
		var stale *ErrStaleVersion
		require.ErrorAs(t, err, &stale, "version %d should be refused", version)
		require.Equal(t, int64(5), store.Version(), "the serving version must not change")
	}

	// A genuinely newer version activates.
	_, err = store.Activate(signedFixture(t, signer, 6), verifier)
	require.NoError(t, err)
	require.Equal(t, int64(6), store.Version())
}

// TestFailedActivationKeepsServing is the central availability property: a bad
// publish degrades to "still serving the last good configuration", never to an
// outage.
func TestFailedActivationKeepsServing(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	attacker, _ := testSigner(t, "attacker")
	store := NewStore(3)

	good, err := store.Activate(signedFixture(t, signer, 1), verifier)
	require.NoError(t, err)

	t.Run("untrusted signature", func(t *testing.T) {
		_, err := store.Activate(signedFixture(t, attacker, 2), verifier)
		require.Error(t, err)
		require.Same(t, good, store.Current())
	})

	t.Run("tampered bytes", func(t *testing.T) {
		bad := signedFixture(t, signer, 2)
		bad.SnapshotBytes[0] ^= 0xFF
		_, err := store.Activate(bad, verifier)
		require.ErrorIs(t, err, ErrBadSignature)
		require.Same(t, good, store.Current())
	})

	t.Run("structurally invalid but correctly signed", func(t *testing.T) {
		snap, err := defaultFixture(2).b.Build()
		require.NoError(t, err)
		// A dangling bundle reference — correctly signed, but unservable.
		snap.Tools[0].ToolsetId = "ts_gone"
		signed, err := signer.Sign(snap)
		require.NoError(t, err)

		_, err = store.Activate(signed, verifier)
		require.ErrorContains(t, err, "unknown toolset")
		require.Same(t, good, store.Current(),
			"a signed-but-broken snapshot must not displace a working one")
	})

	require.Equal(t, int64(1), store.Version())
}

// TestLastRejectIsRecorded: an operator needs to know *why* an instance is
// behind, not merely that it is.
func TestLastRejectIsRecorded(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	attacker, _ := testSigner(t, "attacker")
	store := NewStore(3)

	_, err := store.Activate(signedFixture(t, signer, 1), verifier)
	require.NoError(t, err)
	version, reason := store.LastReject()
	require.Zero(t, version)
	require.Empty(t, reason)

	_, err = store.Activate(signedFixture(t, attacker, 2), verifier)
	require.Error(t, err)
	_, reason = store.LastReject()
	require.Contains(t, reason, "untrusted key")

	// A subsequent success clears it, so the console does not show a stale
	// complaint about a problem that has since been fixed.
	_, err = store.Activate(signedFixture(t, signer, 2), verifier)
	require.NoError(t, err)
	version, reason = store.LastReject()
	require.Zero(t, version)
	require.Empty(t, reason)
}

func TestHistoryIsBounded(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(3)

	for v := int64(1); v <= 6; v++ {
		_, err := store.Activate(signedFixture(t, signer, v), verifier)
		require.NoError(t, err)
	}
	require.Equal(t, []int64{4, 5, 6}, store.History(), "only the newest N are retained")
}

func TestNewStoreClampsHistory(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	// A zero or negative history would leave nothing to roll back to.
	store := NewStore(0)
	_, err := store.Activate(signedFixture(t, signer, 1), verifier)
	require.NoError(t, err)
	require.Equal(t, []int64{1}, store.History())
}

// TestRollback is the local-recovery path: it must work with the control plane
// unreachable, which is exactly when it is needed.
func TestRollback(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(3)

	for v := int64(1); v <= 3; v++ {
		_, err := store.Activate(signedFixture(t, signer, v), verifier)
		require.NoError(t, err)
	}
	require.Equal(t, int64(3), store.Version())

	view, err := store.Rollback(1)
	require.NoError(t, err)
	require.Equal(t, int64(1), view.Version)
	require.Equal(t, int64(1), store.Version())

	// Rolling forward again is a normal activation, since 2 and 3 are still
	// retained but the serving version is now 1.
	_, err = store.Activate(signedFixture(t, signer, 4), verifier)
	require.NoError(t, err)
	require.Equal(t, int64(4), store.Version())
}

func TestRollbackToUnretainedVersion(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(2)
	for v := int64(1); v <= 4; v++ {
		_, err := store.Activate(signedFixture(t, signer, v), verifier)
		require.NoError(t, err)
	}
	_, err := store.Rollback(1)
	var notFound *ErrNotFound
	require.ErrorAs(t, err, &notFound)
	require.Equal(t, int64(1), notFound.Version)
	require.Equal(t, int64(4), store.Version(), "a failed rollback must not change what is served")
}

func TestSignedRetrievesOriginalBytes(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(3)
	original := signedFixture(t, signer, 1)
	_, err := store.Activate(original, verifier)
	require.NoError(t, err)

	got, err := store.Signed(1)
	require.NoError(t, err)
	require.True(t, proto.Equal(original, got))
	// And the retrieved bytes still verify, so an instance can re-check what it
	// was given without asking the control plane.
	_, err = verifier.Verify(got)
	require.NoError(t, err)

	_, err = store.Signed(99)
	var notFound *ErrNotFound
	require.ErrorAs(t, err, &notFound)
}

// TestObserversFireOnActivationAndRollback: the edge rebuilds its per-audience
// MCP servers from this callback, so a missed notification means it keeps
// serving the previous catalog.
func TestObserversFireOnActivationAndRollback(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(3)

	var mu sync.Mutex
	var seen []int64
	store.Observe(func(v *View) {
		mu.Lock()
		seen = append(seen, v.Version)
		mu.Unlock()
	})

	for v := int64(1); v <= 3; v++ {
		_, err := store.Activate(signedFixture(t, signer, v), verifier)
		require.NoError(t, err)
	}
	_, err := store.Rollback(2)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int64{1, 2, 3, 2}, seen)
}

func TestObserversDoNotFireOnFailedActivation(t *testing.T) {
	_, verifier := testSigner(t, "k1")
	attacker, _ := testSigner(t, "attacker")
	store := NewStore(3)

	var calls atomic.Int64
	store.Observe(func(*View) { calls.Add(1) })

	_, err := store.Activate(signedFixture(t, attacker, 1), verifier)
	require.Error(t, err)
	require.Zero(t, calls.Load(), "a refused snapshot must not notify observers")
}

// TestObserverMayCallBackIntoStore: observers run with the write lock released,
// so an observer that reads the store cannot deadlock it.
func TestObserverMayCallBackIntoStore(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(3)

	var observedHistory []int64
	store.Observe(func(*View) {
		observedHistory = store.History()
	})

	_, err := store.Activate(signedFixture(t, signer, 1), verifier)
	require.NoError(t, err)
	require.Equal(t, []int64{1}, observedHistory)
}

// TestConcurrentActivationAndReads is the property that justifies the
// atomic.Pointer: readers never block, never see a torn view, and an in-flight
// reader keeps the exact view it started with.
func TestConcurrentActivationAndReads(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(5)
	_, err := store.Activate(signedFixture(t, signer, 1), verifier)
	require.NoError(t, err)

	const versions = 20
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := int64(2); v <= versions; v++ {
			_, _ = store.Activate(signedFixture(t, signer, v), verifier)
		}
	}()

	// Readers assert the invariant a request depends on: whatever view they get
	// is complete and internally consistent.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				view := store.Current()
				if view == nil {
					continue
				}
				av := view.principalView(t)
				require.NotNil(t, av)
				require.Len(t, av.Tools, 4)
				require.NotNil(t, av.Tool("crm.lookup_customer"))
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(versions), store.Version())
}

// TestConcurrentActivationsOrderCorrectly: two activations racing must leave the
// higher version serving, never the lower.
func TestConcurrentActivationsOrderCorrectly(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	store := NewStore(5)

	lower := signedFixture(t, signer, 1)
	higher := signedFixture(t, signer, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = store.Activate(higher, verifier) }()
	go func() { defer wg.Done(); _, _ = store.Activate(lower, verifier) }()
	wg.Wait()

	require.Equal(t, int64(2), store.Version(),
		"the newer snapshot must win regardless of which activation ran first")
}
