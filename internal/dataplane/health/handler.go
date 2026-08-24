// Copyright 2026 Henry Zektser.

package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Report is what the admin surface serves.
type Report struct {
	Summary  Summary   `json:"summary"`
	Backends []Backend `json:"backends"`
}

// Report snapshots every backend's condition.
func (r *Registry) Report() Report {
	backends := r.All()
	if backends == nil {
		backends = []Backend{}
	}
	return Report{Summary: r.Summary(), Backends: backends}
}

// Handler serves the report as JSON.
//
// It belongs on the **admin** listener, never the public MCP one. This response
// names every backend, its endpoint, and its condition — an inventory of the
// systems behind the gateway. Publishing that to whoever can reach the tool
// endpoint would hand an attacker the map, and it is the same reason the
// readiness endpoint reports an audience *count* rather than a list.
func (r *Registry) Handler(log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(r.Report()); err != nil && log != nil {
			log.Error("encoding the backend health report failed",
				slog.String("error", err.Error()))
		}
	})
}
