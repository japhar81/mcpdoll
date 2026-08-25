// Copyright 2026 Henry Zektser.

package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// Job is one thing the platform does on its own.
type Job struct {
	// Type is the dispatch key and the row's identity. Stable forever: it is
	// what an operator's retuned cadence is attached to.
	Type string
	// Name is what a person reads.
	Name string
	// Default cadence, used only when the row is first created. A later
	// restart must not overwrite what somebody has changed.
	Every time.Duration
	// Run does the work. An error is recorded on the row and logged; it never
	// stops the loop.
	Run func(context.Context) error
}

// Store is what the scheduler needs from the database.
//
// An interface rather than the concrete store so the loop can be tested
// without one, and so this package does not import the whole control plane.
type Store interface {
	RegisterSchedule(ctx context.Context, jobType, name, kind, spec string) error
	// ClaimDue atomically takes the schedule if it is due, pushing its next run
	// out by `every`, and reports whether it got it.
	ClaimDue(ctx context.Context, jobType string, every time.Duration) (bool, error)
	RecordOutcome(ctx context.Context, jobType string, runErr error, took time.Duration) error
	// Cadence reads the row's current interval, so an operator's change takes
	// effect without a restart.
	Cadence(ctx context.Context, jobType string) (time.Duration, bool, error)
}

// Scheduler runs registered jobs from their rows.
type Scheduler struct {
	store Store
	log   *slog.Logger
	jobs  []Job

	// tick is how often the loop asks the database whether anything is due.
	//
	// Deliberately faster than the shortest cadence: a job due every 30s that
	// is only *checked* for every 30s drifts to a 60s effective period half the
	// time. Checking is a single indexed query; running is what costs.
	tick time.Duration
}

// DefaultTick is how often the loop looks for due work.
const DefaultTick = 5 * time.Second

// New builds a scheduler over a set of jobs.
func New(store Store, log *slog.Logger, jobs ...Job) *Scheduler {
	return &Scheduler{store: store, log: log, jobs: jobs, tick: DefaultTick}
}

// Run registers every job and then dispatches due work until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for _, job := range s.jobs {
		if err := s.store.RegisterSchedule(
			ctx, job.Type, job.Name, KindInterval, job.Every.String()); err != nil {
			s.log.Error("registering a schedule failed",
				"job", job.Type, "error", err)
		}
	}

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.dispatch(ctx)
		}
	}
}

func (s *Scheduler) dispatch(ctx context.Context) {
	for _, job := range s.jobs {
		// The row's cadence, not the job's default. This is what makes a
		// retuned schedule take effect without a restart, and it is the whole
		// reason the cadence lives in the database rather than in a constant.
		every, ok, err := s.store.Cadence(ctx, job.Type)
		if err != nil {
			s.log.Warn("reading a schedule failed", "job", job.Type, "error", err)
			continue
		}
		if !ok {
			continue
		}

		claimed, err := s.store.ClaimDue(ctx, job.Type, every)
		if err != nil {
			s.log.Warn("claiming a schedule failed", "job", job.Type, "error", err)
			continue
		}
		if !claimed {
			continue
		}

		s.runOne(ctx, job)
	}
}

func (s *Scheduler) runOne(ctx context.Context, job Job) {
	started := time.Now()
	runErr := job.Run(ctx)
	took := time.Since(started)

	if err := s.store.RecordOutcome(ctx, job.Type, runErr, took); err != nil {
		s.log.Warn("recording a schedule outcome failed", "job", job.Type, "error", err)
	}
	if runErr != nil {
		// Logged as well as recorded. The row is what somebody looks at; the
		// log is what a collector alerts on.
		s.log.Warn("scheduled job failed",
			"job", job.Type, "error", runErr, "took", took)
	}
}
