// Copyright 2026 The MCPDoll Authors.

package health

import (
	"sort"
	"sync"
	"time"
)

// State is a backend's serving fitness.
type State string

const (
	// StateUnknown is a backend that has not been probed yet.
	//
	// Distinct from healthy on purpose. A gateway that has just started knows
	// nothing about its backends, and reporting that as health would make the
	// first sixty seconds after a deploy look better than they are.
	StateUnknown State = "unknown"

	// StateHealthy means the last probe succeeded.
	StateHealthy State = "healthy"

	// StateDegraded means probes are failing but the grace window has not
	// elapsed. Tools stay listed and their calls fail fast with a legible
	// error.
	//
	// This state exists because dropping tools from the catalog invalidates
	// every connected client's prompt cache. For a backend that is restarting,
	// that costs far more than the failed calls do.
	StateDegraded State = "degraded"

	// StateUnavailable means the grace window elapsed. The tools are still
	// listed — the snapshot is what defines the catalog, and only a publish
	// changes it — but the failure is now reported as durable rather than
	// transient, so a model stops retrying.
	StateUnavailable State = "unavailable"

	// StateDrifted means the backend answers, but publishes something other
	// than what was admitted. Serving continues or not depending on the
	// backend's serving mode.
	StateDrifted State = "drifted"
)

// Backend is one backend's observed condition.
type Backend struct {
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	Endpoint   string `json:"endpoint"`
	State      State  `json:"state"`

	// ServingMode is carried alongside the drift because the same drift means
	// different things under each: strict refuses, advisory serves and records.
	ServingMode string `json:"serving_mode"`

	LastProbe time.Time `json:"last_probe"`
	// LastSuccess is when a probe last succeeded. The gap between this and now
	// is what the grace window is measured against.
	LastSuccess time.Time `json:"last_success,omitempty"`

	ConsecutiveFailures int `json:"consecutive_failures"`
	// LatencyEWMAMs smooths probe latency. A single slow probe is noise; a
	// rising average is a backend going bad before it starts failing.
	LatencyEWMAMs int64 `json:"latency_ewma_ms"`

	NegotiatedVersion string `json:"negotiated_version,omitempty"`
	// Error is the last probe failure, empty when the last probe succeeded.
	Error string `json:"error,omitempty"`

	ToolsAdmitted int         `json:"tools_admitted"`
	ToolsObserved int         `json:"tools_observed"`
	Drift         []ToolDrift `json:"drift,omitempty"`
}

// BlockedTools returns the qualified names this backend must not serve.
//
// Empty for an advisory backend whatever it has done: advisory means serve and
// record. The decision belongs here rather than at the call site so that
// "which tools are refused?" has exactly one answer.
func (b Backend) BlockedTools() []string {
	if b.ServingMode != "strict" {
		return nil
	}
	var out []string
	for _, d := range b.Drift {
		if d.Kind.Blocking() && d.QualifiedName != "" {
			out = append(out, d.QualifiedName)
		}
	}
	sort.Strings(out)
	return out
}

// Registry holds the current condition of every backend.
//
// Reads are frequent (every dispatch may consult it) and writes are rare (once
// per probe interval), so it is a copy-on-write map behind an RWMutex rather
// than anything cleverer.
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Backend
	// blocked is derived from backends on every write, so the dispatch path
	// does a single map lookup instead of walking drift lists per call.
	blocked map[string]blockReason
}

type blockReason struct {
	server string
	kind   DriftKind
	detail string
}

// NewRegistry returns an empty registry. Every backend reads as unknown until
// probed.
func NewRegistry() *Registry {
	return &Registry{
		backends: map[string]Backend{},
		blocked:  map[string]blockReason{},
	}
}

// Set records one backend's condition.
func (r *Registry) Set(b Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.backends[b.ServerID] = b
	r.rebuildBlockedLocked()
}

// Forget drops backends that are no longer in the snapshot.
//
// Without this, a backend removed from the registry would keep its last
// condition forever, and a stale block would refuse calls to a tool that a
// later publish legitimately reinstated.
func (r *Registry) Forget(keep map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id := range r.backends {
		if !keep[id] {
			delete(r.backends, id)
		}
	}
	r.rebuildBlockedLocked()
}

func (r *Registry) rebuildBlockedLocked() {
	blocked := make(map[string]blockReason)
	for _, b := range r.backends {
		for _, name := range b.BlockedTools() {
			for _, d := range b.Drift {
				if d.QualifiedName == name {
					blocked[name] = blockReason{
						server: b.ServerName, kind: d.Kind, detail: d.Detail,
					}
					break
				}
			}
		}
	}
	r.blocked = blocked
}

// Blocked reports whether a qualified tool must be refused, and why.
//
// The reason travels with the answer because it is going into an error a model
// will read, and "this tool is unavailable" without a cause produces a retry
// loop.
func (r *Registry) Blocked(qualifiedName string) (reason string, blocked bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, found := r.blocked[qualifiedName]
	if !found {
		return "", false
	}
	switch entry.kind {
	case DriftRemoved:
		return "the backend serving this tool no longer publishes it, and this " +
			"gateway is configured to serve only admitted definitions", true
	default:
		return "this tool's definition has changed at the backend since it was " +
			"admitted (" + entry.detail + "), and this gateway is configured to " +
			"serve only admitted definitions", true
	}
}

// Backend returns one backend's condition.
func (r *Registry) Backend(serverID string) (Backend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.backends[serverID]
	return b, ok
}

// All returns every backend's condition, ordered by name.
func (r *Registry) All() []Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Backend, 0, len(r.backends))
	for _, b := range r.backends {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerName < out[j].ServerName })
	return out
}

// Summary counts backends by state, for a status line.
type Summary struct {
	Total       int `json:"total"`
	Healthy     int `json:"healthy"`
	Degraded    int `json:"degraded"`
	Unavailable int `json:"unavailable"`
	Drifted     int `json:"drifted"`
	Unknown     int `json:"unknown"`
	// BlockedTools is how many tools are refused because of drift. Zero in an
	// all-advisory deployment however much has drifted.
	BlockedTools int `json:"blocked_tools"`
}

// Summary counts the current conditions.
func (r *Registry) Summary() Summary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	s := Summary{Total: len(r.backends), BlockedTools: len(r.blocked)}
	for _, b := range r.backends {
		switch b.State {
		case StateHealthy:
			s.Healthy++
		case StateDegraded:
			s.Degraded++
		case StateUnavailable:
			s.Unavailable++
		case StateDrifted:
			s.Drifted++
		default:
			s.Unknown++
		}
	}
	return s
}

// classify decides a backend's state from one probe's outcome.
//
// Pure, so the transitions are testable without a clock or a network.
func classify(
	probeErr error,
	drifts []ToolDrift,
	lastSuccess time.Time,
	now time.Time,
	graceWindow time.Duration,
) State {
	if probeErr != nil {
		if lastSuccess.IsZero() {
			// Never succeeded. There is no grace to extend: a backend that has
			// never answered is not "temporarily" anything.
			return StateUnavailable
		}
		if now.Sub(lastSuccess) < graceWindow {
			return StateDegraded
		}
		return StateUnavailable
	}
	if len(Blocking(drifts)) > 0 {
		return StateDrifted
	}
	return StateHealthy
}

// ewma folds a new sample into a smoothed average.
func ewma(previous, sample time.Duration, alpha float64) time.Duration {
	if previous <= 0 {
		return sample
	}
	return time.Duration(alpha*float64(sample) + (1-alpha)*float64(previous))
}
