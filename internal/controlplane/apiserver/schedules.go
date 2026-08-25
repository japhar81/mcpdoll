// Copyright 2026 Henry Zektser.

package apiserver

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mcpdoll/mcpdoll/internal/api"
	"github.com/mcpdoll/mcpdoll/internal/controlplane/store"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// UpdateScheduleRequest retunes or switches off one piece of timed work.
type UpdateScheduleRequest struct {
	// Spec is a Go duration: "30s", "5m". Omitted leaves the cadence alone.
	Spec *string `json:"spec,omitempty"`
	// Enabled omitted leaves it alone, so a caller can change one without
	// having to know the other.
	Enabled *bool `json:"enabled,omitempty"`
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	st := s.requireStore(w)
	if st == nil {
		return
	}
	rows, err := st.ListSchedules(r.Context())
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	out := api.ScheduleList{Schedules: []api.Schedule{}}
	for _, row := range rows {
		out.Schedules = append(out.Schedules, scheduleOf(row))
	}
	writeJSON(w, s.log, http.StatusOK, out)
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	st := s.requireStore(w)
	if st == nil {
		return
	}
	row, err := st.GetSchedule(r.Context(), chi.URLParam(r, "jobType"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, scheduleOf(row))
}

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	st := s.requireStore(w)
	if st == nil {
		return
	}
	var req UpdateScheduleRequest
	if !decodeBody(w, r, s.log, &req) {
		return
	}
	if req.Spec == nil && req.Enabled == nil {
		writeError(w, s.log, http.StatusBadRequest, CodeInvalidRequest,
			"nothing to change: pass spec, enabled, or both")
		return
	}
	row, err := st.UpdateSchedule(r.Context(), chi.URLParam(r, "jobType"), req.Spec, req.Enabled)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, scheduleOf(row))
}

// handleRunScheduleNow brings a schedule forward rather than running it here.
//
// The job runs on the scheduler's next tick, through the same claim and the
// same outcome recording as any other run. Executing it inline would be a
// second path for the same work — one that records an outcome and one that
// does not — and the two would drift.
func (s *Server) handleRunScheduleNow(w http.ResponseWriter, r *http.Request) {
	st := s.requireStore(w)
	if st == nil {
		return
	}
	row, err := st.RunScheduleNow(r.Context(), chi.URLParam(r, "jobType"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, s.log, http.StatusAccepted, scheduleOf(row))
}

func scheduleOf(in store.Schedule) api.Schedule {
	out := api.Schedule{
		JobType: in.JobType, Name: in.Name, Kind: in.Kind, Spec: in.Spec,
		Enabled: in.Enabled, System: in.System,
		LastError: in.LastError, LastDurationMs: in.LastDurationMs,
	}
	if in.NextRunAt != nil {
		out.NextRunAt = in.NextRunAt.UTC().Format(time.RFC3339)
	}
	if in.LastRunAt != nil {
		out.LastRunAt = in.LastRunAt.UTC().Format(time.RFC3339)
	}
	return out
}

// schedulePermission is what these operations require.
//
// Retuning the catalog rebuild changes how quickly every tenant's tools update,
// and disabling the revocation heartbeat widens the exposure window for every
// leaked credential in the deployment. That is platform surgery, so it takes a
// platform-wide permission rather than a tenant-scoped one.
const schedulePermission = authz.PermSnapshotPublish
