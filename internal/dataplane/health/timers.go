// Copyright 2026 Henry Zektser.

package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// The data plane's own timed work, reported so it can be *seen* from the
// console without being *owned* there (ADR 0026).
//
// These deliberately are not schedules. A probe cadence living in the control
// plane's database would mean a control-plane outage stops the data plane
// noticing an unhealthy backend — during precisely the outage the two-plane
// split exists to survive. So they stay in this process's config file, and this
// endpoint exists only so somebody looking at the platform's timed work is not
// told a half-truth.

// Timer is one thing the data plane does on its own.
type Timer struct {
	Name string `json:"name"`
	// Every is a Go duration. A string rather than a number because it is read
	// by a person, and "30s" survives the trip through JSON without anybody
	// having to remember which unit was meant.
	Every       string `json:"every"`
	Description string `json:"description"`
}

// TimerReport is every timer, with the reason they are not editable.
type TimerReport struct {
	Timers []Timer `json:"timers"`
	// Source is where these are configured, so the console can tell somebody
	// where to go rather than offering them a control that does nothing.
	Source string `json:"source"`
}

// TimersHandler serves the data plane's cadences.
//
// On the admin listener, with the backend report, for the same reason: this
// describes the machinery behind the gateway rather than anything a tool caller
// is entitled to.
func TimersHandler(
	log *slog.Logger, probeInterval, driftScanInterval, graceWindow time.Duration,
) http.Handler {
	report := TimerReport{
		Source: "the data plane's config file (health:)",
		Timers: []Timer{
			{
				Name:  "Backend health probing",
				Every: probeInterval.String(),
				Description: "Probes every backend behind the gateway and scores it. " +
					"Ejection and recovery both follow from this.",
			},
			{
				Name:  "Drift scanning",
				Every: driftScanInterval.String(),
				Description: "Re-reads what each backend publishes and compares it to " +
					"what was admitted. How a backend quietly changing a tool is noticed.",
			},
			{
				Name:  "Unreachable-backend grace window",
				Every: graceWindow.String(),
				Description: "How long a tool from an unreachable backend stays listed, " +
					"failing fast, before removal. Dropping it invalidates every " +
					"client's prompt cache, so this trades staleness for cache stability.",
			},
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(report); err != nil && log != nil {
			log.Error("encoding the timer report failed", slog.String("error", err.Error()))
		}
	})
}
