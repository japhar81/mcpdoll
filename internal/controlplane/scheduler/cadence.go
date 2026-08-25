// Copyright 2026 Henry Zektser.

// Package scheduler runs the platform's timed work from rows rather than from
// hardcoded tickers (ADR 0026).
package scheduler

import (
	"fmt"
	"time"
)

// KindInterval is the only cadence kind today: a fixed gap between runs.
//
// Cron is the obvious alternative and it does not fit what this system
// actually schedules. Its finest granularity is one minute, and the revocation
// heartbeat — the job whose timing matters most, because its cadence *is* the
// exposure window for a leaked credential — runs every thirty seconds. A
// scheduler that could not express the most important schedule it has would be
// a worse abstraction than the tickers it replaced.
//
// The column is a discriminator so calendar cadences can be added later
// without rewriting the table. See docs/deferred.md.
const KindInterval = "interval"

// MinInterval is the floor on how often a job may run.
//
// Not a safety rail against a typo so much as against a plausible number: a
// rebuild is a discovery sweep of every backend, and somebody setting it to
// "1s" to see a change faster would point the gateway's whole traffic budget
// at its own upstreams.
const MinInterval = 5 * time.Second

// ParseCadence turns a stored kind and spec into a duration.
func ParseCadence(kind, spec string) (time.Duration, error) {
	if kind != KindInterval {
		return 0, fmt.Errorf("scheduler: unknown cadence kind %q", kind)
	}
	d, err := time.ParseDuration(spec)
	if err != nil {
		return 0, fmt.Errorf("scheduler: %q is not a duration: %w", spec, err)
	}
	if d < MinInterval {
		return 0, fmt.Errorf(
			"scheduler: %s is faster than the %s floor", d, MinInterval)
	}
	return d, nil
}
