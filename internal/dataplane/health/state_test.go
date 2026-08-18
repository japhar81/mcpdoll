// Copyright 2026 The MCPDoll Authors.

package health

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClassifyGivesAFailingBackendItsGraceWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	boom := errors.New("connection refused")

	// Failing for one minute of a ten-minute window: degraded. Tools stay
	// listed, because dropping them invalidates every client's prompt cache
	// over what is usually a restart.
	require.Equal(t, StateDegraded,
		classify(boom, nil, now.Add(-1*time.Minute), now, 10*time.Minute))

	// Past the window: the failure is durable, and saying so lets a model stop
	// retrying.
	require.Equal(t, StateUnavailable,
		classify(boom, nil, now.Add(-11*time.Minute), now, 10*time.Minute))
}

func TestABackendThatNeverAnsweredGetsNoGrace(t *testing.T) {
	t.Parallel()
	now := time.Now()

	// A zero LastSuccess means it has never worked. There is nothing to be
	// temporarily degraded *from*, and treating it as degraded would report a
	// misconfigured endpoint as a transient blip for ten minutes.
	require.Equal(t, StateUnavailable,
		classify(errors.New("no such host"), nil, time.Time{}, now, 10*time.Minute))
}

func TestOnlyBlockingDriftMakesABackendDrifted(t *testing.T) {
	t.Parallel()
	now := time.Now()

	cosmetic := []ToolDrift{{Kind: DriftCosmetic}}
	require.Equal(t, StateHealthy, classify(nil, cosmetic, now, now, time.Minute))

	semantic := []ToolDrift{{Kind: DriftSemantic}}
	require.Equal(t, StateDrifted, classify(nil, semantic, now, now, time.Minute))
}

func TestStrictBlocksAndAdvisoryDoesNot(t *testing.T) {
	t.Parallel()

	drift := []ToolDrift{
		{Name: "a", QualifiedName: "whs.a", Kind: DriftSemantic, Detail: "input schema changed"},
		{Name: "b", QualifiedName: "whs.b", Kind: DriftCosmetic},
		{Name: "c", Kind: DriftAdded},
	}

	strict := Backend{ServingMode: "strict", Drift: drift}
	require.Equal(t, []string{"whs.a"}, strict.BlockedTools())

	// Advisory means serve and record. The same drift, a different decision —
	// which is the entire content of the setting.
	advisory := Backend{ServingMode: "advisory", Drift: drift}
	require.Empty(t, advisory.BlockedTools())
}

func TestRegistryBlocksWithAReasonAModelCanActOn(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Set(Backend{
		ServerID: "srv_1", ServerName: "warehouse", ServingMode: "strict",
		State: StateDrifted,
		Drift: []ToolDrift{{
			Name: "check_stock", QualifiedName: "whs.check_stock",
			Kind: DriftSemantic, Detail: "input schema changed",
		}},
	})

	reason, blocked := r.Blocked("whs.check_stock")
	require.True(t, blocked)
	require.Contains(t, reason, "input schema changed")

	_, blocked = r.Blocked("whs.reserve_stock")
	require.False(t, blocked)
}

func TestForgetUnblocksABackendRemovedFromTheSnapshot(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Set(Backend{
		ServerID: "srv_1", ServerName: "warehouse", ServingMode: "strict",
		Drift: []ToolDrift{{
			QualifiedName: "whs.check_stock", Kind: DriftSemantic, Detail: "x",
		}},
	})
	_, blocked := r.Blocked("whs.check_stock")
	require.True(t, blocked)

	// A stale block would refuse calls to a tool a later publish legitimately
	// reinstated, and nothing would ever clear it.
	r.Forget(map[string]bool{})
	_, blocked = r.Blocked("whs.check_stock")
	require.False(t, blocked)
	require.Empty(t, r.All())
}

func TestSummaryCountsEveryState(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Set(Backend{ServerID: "a", ServerName: "a", State: StateHealthy})
	r.Set(Backend{ServerID: "b", ServerName: "b", State: StateDegraded})
	r.Set(Backend{ServerID: "c", ServerName: "c", State: StateUnavailable})
	r.Set(Backend{
		ServerID: "d", ServerName: "d", State: StateDrifted, ServingMode: "strict",
		Drift: []ToolDrift{{QualifiedName: "x.y", Kind: DriftRemoved}},
	})
	r.Set(Backend{ServerID: "e", ServerName: "e", State: StateUnknown})

	s := r.Summary()
	require.Equal(t, Summary{
		Total: 5, Healthy: 1, Degraded: 1, Unavailable: 1, Drifted: 1, Unknown: 1,
		BlockedTools: 1,
	}, s)
}

func TestEWMAStartsAtTheFirstSampleRatherThanZero(t *testing.T) {
	t.Parallel()

	// Folding the first sample into a zero baseline would report a fifth of the
	// real latency and take several intervals to converge — during which the
	// number is wrong in the reassuring direction.
	require.Equal(t, 100*time.Millisecond, ewma(0, 100*time.Millisecond, 0.2))
	require.Equal(t, 100*time.Millisecond, ewma(100*time.Millisecond, 100*time.Millisecond, 0.2))

	got := ewma(100*time.Millisecond, 200*time.Millisecond, 0.5)
	require.Equal(t, 150*time.Millisecond, got)
}

func TestRegistryAllIsOrderedByName(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Set(Backend{ServerID: "3", ServerName: "zeta"})
	r.Set(Backend{ServerID: "1", ServerName: "alpha"})
	r.Set(Backend{ServerID: "2", ServerName: "mu"})

	names := []string{}
	for _, b := range r.All() {
		names = append(names, b.ServerName)
	}
	// Map order is randomised; a console table that reshuffles on every poll is
	// unreadable.
	require.Equal(t, []string{"alpha", "mu", "zeta"}, names)
}
