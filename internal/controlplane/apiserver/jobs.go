// Copyright 2026 Henry Zektser.

package apiserver

import (
	"context"
	"errors"
	"time"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/scheduler"
)

// The platform's timed work, as jobs rather than tickers (ADR 0026).
//
// Each of these used to be a goroutine with a `time.NewTicker` and its cadence
// in a Go constant. The behaviour is unchanged; what changed is that the
// cadence now lives in a row an operator can read and retune, and that a job
// which has been failing since Tuesday says so somewhere other than a log.

// ScheduledJobs is everything this control plane does on its own.
func (s *Server) ScheduledJobs() []scheduler.Job {
	return []scheduler.Job{
		{
			Type:  "revocations.publish",
			Name:  "Republish the revocation list",
			Every: 30 * time.Second,
			// Thirty seconds because this cadence *is* the exposure window for
			// a leaked credential (ADR 0023), and because republishing an
			// unchanged list is what keeps its age meaningful — an age that
			// only moved on change could not distinguish a quiet deployment
			// from a broken distribution path.
			Run: func(ctx context.Context) error {
				if problem := s.publishRevocations(ctx); problem != "" {
					return errors.New(problem)
				}
				return nil
			},
		},
		{
			Type:  "principals.publish",
			Name:  "Republish the principal set",
			Every: 30 * time.Second,
			Run: func(ctx context.Context) error {
				if problem := s.publishPrincipals(ctx); problem != "" {
					return errors.New(problem)
				}
				return nil
			},
		},
		{
			Type: "catalog.rebuild",
			Name: "Rebuild the catalog from the backends",
			// The configured value seeds the row the first time it is created;
			// after that the row is what runs.
			Every: s.rebuildInterval(),
			Run: func(ctx context.Context) error {
				report, fail := s.buildAndPublish(ctx, BuildSnapshotRequest{})
				if fail != nil {
					s.rebuilds.note(time.Now(), false, fail)
					return fail
				}
				s.noteRebuild(report, nil)
				if !report.Unchanged {
					s.log.Info("catalog rebuilt",
						"snapshot_version", report.Version,
						"tools", report.Tools, "servers", report.Servers)
				}
				return nil
			},
		},
	}
}
