// Copyright 2026 Henry Zektser.

package backends

import (
	"sync"
	"time"
)

// State is a circuit breaker's state.
type State int

const (
	// StateClosed passes traffic normally.
	StateClosed State = iota
	// StateOpen rejects immediately.
	StateOpen
	// StateHalfOpen lets a single probe through to test recovery.
	StateHalfOpen
)

// String renders the state for metrics and the console.
func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// breaker is a consecutive-failure circuit breaker.
//
// Consecutive failures rather than a failure *rate*: a rate needs a window, and
// a window needs a decision about what to do with a backend that has served
// nothing recently. Consecutive failures answer the question the gateway
// actually has — "is this backend broken right now" — with no window at all, and
// a single success is enough to clear it.
//
// The half-open state exists so recovery does not depend on a probe: the next
// real request after the cooldown is the probe. A breaker that only reopened on
// a scheduled health check would stay open for up to a full probe interval after
// the backend recovered.
type breaker struct {
	threshold int
	cooldown  time.Duration

	mu            sync.Mutex
	consecutive   int
	state         State
	openedAt      time.Time
	probeInFlight bool

	// now is injectable so tests can advance time without sleeping.
	now func() time.Time
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	if threshold < 1 {
		threshold = 1
	}
	return &breaker{
		threshold: threshold,
		cooldown:  cooldown,
		state:     StateClosed,
		now:       time.Now,
	}
}

// Allow reports whether a request may proceed, and reserves the half-open probe
// slot if it is granting one.
func (b *breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		// Cooldown elapsed: promote to half-open and let exactly one request
		// through. Letting the whole backlog through at once would hammer a
		// backend that is still recovering.
		b.state = StateHalfOpen
		b.probeInFlight = true
		return true
	case StateHalfOpen:
		if b.probeInFlight {
			return false
		}
		b.probeInFlight = true
		return true
	default:
		return true
	}
}

// Success records a healthy response, closing the breaker.
func (b *breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.state = StateClosed
	b.probeInFlight = false
}

// Failure records a failure, opening the breaker at the threshold.
func (b *breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// A failed half-open probe reopens immediately and restarts the cooldown,
	// without waiting to accumulate `threshold` failures again — the backend has
	// already demonstrated it is still broken.
	if b.state == StateHalfOpen {
		b.state = StateOpen
		b.openedAt = b.now()
		b.probeInFlight = false
		return
	}

	b.consecutive++
	if b.consecutive >= b.threshold {
		b.state = StateOpen
		b.openedAt = b.now()
		b.probeInFlight = false
	}
}

// State reports the current state, promoting an expired open breaker to
// half-open so a reader sees the state a request would encounter.
func (b *breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.cooldown {
		return StateHalfOpen
	}
	return b.state
}

// OpenUntil reports when the breaker will next admit a probe. It is surfaced to
// the client in the structured unavailability error, so a model is told when to
// try again rather than left to guess.
func (b *breaker) OpenUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != StateOpen {
		return time.Time{}
	}
	return b.openedAt.Add(b.cooldown)
}

// Consecutive reports the current consecutive-failure count, for the health
// board.
func (b *breaker) Consecutive() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consecutive
}
