// Copyright 2026 Henry Zektser.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/scheduler"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/store/dbgen"
)

// Schedule is one piece of timed work (ADR 0026).
type Schedule struct {
	ID      uuid.UUID `json:"id"`
	JobType string    `json:"job_type"`
	Name    string    `json:"name"`
	Kind    string    `json:"kind"`
	Spec    string    `json:"spec"`
	Enabled bool      `json:"enabled"`
	// System schedules cannot be deleted. Their cadence is editable and they
	// can be disabled — the platform does not get to decide that an operator
	// must rebuild hourly — but nothing would ever recreate a deleted one.
	System         bool       `json:"system"`
	NextRunAt      *time.Time `json:"next_run_at,omitempty"`
	LastRunAt      *time.Time `json:"last_run_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	LastDurationMs int32      `json:"last_duration_ms,omitempty"`
}

// ListSchedules returns every piece of timed work, whether or not it is on.
func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.q.ListSchedules(ctx)
	if err != nil {
		return nil, wrap(err, "listing schedules")
	}
	out := make([]Schedule, 0, len(rows))
	for _, row := range rows {
		out = append(out, scheduleFrom(row))
	}
	return out, nil
}

// GetSchedule reads one by its job type.
func (s *Store) GetSchedule(ctx context.Context, jobType string) (Schedule, error) {
	row, err := s.q.GetScheduleByJobType(ctx, jobType)
	if err != nil {
		return Schedule{}, wrap(err, "reading schedule %s", jobType)
	}
	return scheduleFrom(row), nil
}

// RegisterSchedule creates the row for a job the binary knows how to run.
//
// Idempotent, and deliberately does not overwrite the cadence: a schedule an
// operator retuned or disabled has to survive a restart, or every deploy
// silently reverts their decision.
func (s *Store) RegisterSchedule(ctx context.Context, jobType, name, kind, spec string) error {
	_, err := s.q.UpsertSystemSchedule(ctx, dbgen.UpsertSystemScheduleParams{
		JobType: jobType, Name: name, Kind: kind, Spec: spec,
	})
	return wrap(err, "registering schedule %s", jobType)
}

// ClaimDue takes the schedule if it is due and pushes its next run out.
func (s *Store) ClaimDue(ctx context.Context, jobType string, every time.Duration) (bool, error) {
	_, err := s.q.ClaimDueSchedule(ctx, dbgen.ClaimDueScheduleParams{
		JobType: jobType, Column2: intervalOf(every),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Not due, or another replica got it first. Both are "nothing to do".
		return false, nil
	}
	if err != nil {
		return false, wrap(err, "claiming schedule %s", jobType)
	}
	return true, nil
}

// RecordOutcome stamps how the last run went.
func (s *Store) RecordOutcome(
	ctx context.Context, jobType string, runErr error, took time.Duration,
) error {
	row, err := s.q.GetScheduleByJobType(ctx, jobType)
	if err != nil {
		return wrap(err, "reading schedule %s", jobType)
	}
	var message *string
	if runErr != nil {
		text := runErr.Error()
		message = &text
	}
	ms := int32(took.Milliseconds())
	return wrap(s.q.RecordScheduleOutcome(ctx, dbgen.RecordScheduleOutcomeParams{
		ID: row.ID, LastError: message, LastDurationMs: &ms,
	}), "recording the outcome of %s", jobType)
}

// Cadence reads a schedule's current interval.
//
// A malformed spec disables the job rather than defaulting it. Falling back to
// a default would run the job at a cadence nobody chose while the row on screen
// said something else — and the row is the whole point.
func (s *Store) Cadence(ctx context.Context, jobType string) (time.Duration, bool, error) {
	row, err := s.q.GetScheduleByJobType(ctx, jobType)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, wrap(err, "reading schedule %s", jobType)
	}
	if !row.Enabled {
		return 0, false, nil
	}
	every, err := scheduler.ParseCadence(row.Kind, row.Spec)
	if err != nil {
		return 0, false, err
	}
	return every, true, nil
}

// UpdateSchedule retunes or switches off one piece of timed work.
func (s *Store) UpdateSchedule(
	ctx context.Context, jobType string, spec *string, enabled *bool,
) (Schedule, error) {
	if spec != nil {
		if _, err := scheduler.ParseCadence(scheduler.KindInterval, *spec); err != nil {
			return Schedule{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	row, err := s.q.UpdateScheduleCadence(ctx, dbgen.UpdateScheduleCadenceParams{
		JobType: jobType, Spec: spec, Enabled: enabled,
	})
	if err != nil {
		return Schedule{}, wrap(err, "updating schedule %s", jobType)
	}
	return scheduleFrom(row), nil
}

// RunScheduleNow brings a schedule forward so the next tick takes it.
//
// Not a second execution path. Running the job inline here would mean two ways
// for it to happen — one that claims the row and records an outcome, and one
// that does not — and they would drift. This one moves a timestamp.
func (s *Store) RunScheduleNow(ctx context.Context, jobType string) (Schedule, error) {
	if err := s.q.DueNow(ctx, jobType); err != nil {
		return Schedule{}, wrap(err, "scheduling %s to run now", jobType)
	}
	return s.GetSchedule(ctx, jobType)
}

func scheduleFrom(row dbgen.Schedule) Schedule {
	out := Schedule{
		ID: row.ID, JobType: row.JobType, Name: row.Name,
		Kind: row.Kind, Spec: row.Spec,
		Enabled: row.Enabled, System: row.System,
	}
	if row.NextRunAt.Valid {
		t := row.NextRunAt.Time
		out.NextRunAt = &t
	}
	if row.LastRunAt.Valid {
		t := row.LastRunAt.Time
		out.LastRunAt = &t
	}
	if row.LastError != nil {
		out.LastError = *row.LastError
	}
	if row.LastDurationMs != nil {
		out.LastDurationMs = *row.LastDurationMs
	}
	return out
}

// intervalOf turns a duration into the interval the claim query adds.
//
// Microseconds rather than a parsed string: pgtype builds the value directly,
// so there is no formatting step that could round a cadence into a different
// one than the row says.
func intervalOf(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
