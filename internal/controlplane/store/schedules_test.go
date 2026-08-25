// Copyright 2026 Henry Zektser.

package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
)

// The property that lets more than one control-plane replica run a scheduler
// without leader election. Ragdoll's scheduler documented the opposite —
// listDue then markRun, unfenced, double-firing if two instances run — so this
// is the divergence worth holding down with a test.
func TestOnlyOneClaimerWinsADueSchedule(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	job := "test.claim." + uniqueSlug(t)

	require.NoError(t, s.RegisterSchedule(ctx, job, "Claim race", "interval", "1h"))

	const racers = 8
	var wg sync.WaitGroup
	won := make([]bool, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := s.ClaimDue(ctx, job, time.Hour)
			require.NoError(t, err)
			won[i] = claimed
		}()
	}
	wg.Wait()

	winners := 0
	for _, w := range won {
		if w {
			winners++
		}
	}
	require.Equal(t, 1, winners, "exactly one claimer may take a due schedule")
}

func TestAClaimedScheduleIsNotDueAgain(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	job := "test.once." + uniqueSlug(t)

	require.NoError(t, s.RegisterSchedule(ctx, job, "Once", "interval", "1h"))

	first, err := s.ClaimDue(ctx, job, time.Hour)
	require.NoError(t, err)
	require.True(t, first)

	second, err := s.ClaimDue(ctx, job, time.Hour)
	require.NoError(t, err)
	require.False(t, second, "the next run was pushed an hour out")
}

// Registration must not overwrite a cadence somebody changed, or every deploy
// silently reverts their decision.
func TestRegisteringAgainKeepsARetunedCadence(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	job := "test.retune." + uniqueSlug(t)

	require.NoError(t, s.RegisterSchedule(ctx, job, "Retune", "interval", "30s"))

	retuned := "10m"
	_, err := s.UpdateSchedule(ctx, job, &retuned, nil)
	require.NoError(t, err)

	// A restart re-registers every job it knows how to run.
	require.NoError(t, s.RegisterSchedule(ctx, job, "Retune", "interval", "30s"))

	got, err := s.GetSchedule(ctx, job)
	require.NoError(t, err)
	require.Equal(t, "10m", got.Spec, "a restart must not revert a retuned cadence")
}

func TestADisabledScheduleReportsNoCadence(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	job := "test.off." + uniqueSlug(t)

	require.NoError(t, s.RegisterSchedule(ctx, job, "Off", "interval", "30s"))
	off := false
	_, err := s.UpdateSchedule(ctx, job, nil, &off)
	require.NoError(t, err)

	_, ok, err := s.Cadence(ctx, job)
	require.NoError(t, err)
	require.False(t, ok, "a disabled schedule has nothing to run")
}

// The floor exists against a plausible number rather than a typo: a rebuild is
// a discovery sweep of every backend, and "1s" would aim the gateway's whole
// traffic budget at its own upstreams.
func TestACadenceBelowTheFloorIsRefused(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	job := "test.floor." + uniqueSlug(t)

	require.NoError(t, s.RegisterSchedule(ctx, job, "Floor", "interval", "30s"))

	tooFast := "1s"
	_, err := s.UpdateSchedule(ctx, job, &tooFast, nil)
	require.ErrorIs(t, err, store.ErrInvalid)

	notADuration := "every tuesday"
	_, err = s.UpdateSchedule(ctx, job, &notADuration, nil)
	require.ErrorIs(t, err, store.ErrInvalid)

	got, err := s.GetSchedule(ctx, job)
	require.NoError(t, err)
	require.Equal(t, "30s", got.Spec, "a refused cadence must not have been stored")
}

// Re-arming from now is what makes a change appear to take effect. Without it,
// lengthening an interval leaves the old next_run_at in place and the schedule
// fires once more on the old cadence.
func TestChangingACadenceReArmsFromNow(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ctx := context.Background()
	job := "test.rearm." + uniqueSlug(t)

	require.NoError(t, s.RegisterSchedule(ctx, job, "Rearm", "interval", "30s"))
	_, err := s.ClaimDue(ctx, job, 24*time.Hour)
	require.NoError(t, err)

	longer := "10m"
	updated, err := s.UpdateSchedule(ctx, job, &longer, nil)
	require.NoError(t, err)
	require.NotNil(t, updated.NextRunAt)
	require.WithinDuration(t, time.Now(), *updated.NextRunAt, time.Minute,
		"the next run should be re-armed from now, not left a day out")
}
