// Copyright 2026 Henry Zektser.

package pipeline

import (
	"sync"
	"time"
)

// breaker is a per-plugin consecutive-failure circuit breaker.
//
// A plugin that is failing should be taken out of the path quickly and put back
// automatically, without an operator in the loop. Consecutive failures answer the
// question the engine actually has — "is this plugin broken right now" — with no
// window to configure, and a single success clears it.
//
// It is a separate type from the backend breaker despite the family resemblance.
// The two differ in what counts as a failure (a plugin's *invalid verdict*
// counts; a backend's *tool error* does not) and in what happens when they open
// (a plugin is skipped per its failure policy; a backend call fails). Sharing the
// implementation would mean sharing those decisions, which are not the same.
type breaker struct {
	threshold int
	cooldown  time.Duration

	mu            sync.Mutex
	consecutive   int
	open          bool
	openedAt      time.Time
	probeInFlight bool
	// justOpened is set for exactly one read, so the engine can log and count a
	// trip once rather than on every subsequent skip.
	justOpened bool

	now func() time.Time
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	if threshold < 1 {
		threshold = 1
	}
	return &breaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// Allow reports whether the plugin may run, reserving the half-open probe slot
// when it grants one.
func (b *breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.open {
		return true
	}
	if b.now().Sub(b.openedAt) < b.cooldown {
		return false
	}
	// Cooldown elapsed: let exactly one request through as a probe. Releasing
	// the whole backlog at once would hammer a plugin that is still recovering.
	if b.probeInFlight {
		return false
	}
	b.probeInFlight = true
	return true
}

// Success closes the breaker.
func (b *breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.open = false
	b.probeInFlight = false
}

// Failure records a failure, opening the breaker at the threshold.
func (b *breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// A failed probe reopens immediately: the plugin has already demonstrated it
	// is still broken, so waiting to re-accumulate `threshold` failures would
	// send `threshold` more requests through a plugin that will fail them.
	if b.probeInFlight {
		b.open = true
		b.openedAt = b.now()
		b.probeInFlight = false
		return
	}

	b.consecutive++
	if b.consecutive >= b.threshold && !b.open {
		b.open = true
		b.openedAt = b.now()
		b.justOpened = true
	}
}

// JustOpened reports (once) that the breaker has just tripped.
func (b *breaker) JustOpened() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.justOpened {
		b.justOpened = false
		return true
	}
	return false
}

// Consecutive is the current consecutive-failure count.
func (b *breaker) Consecutive() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consecutive
}

// State renders the breaker for metrics and the console.
func (b *breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return "closed"
	}
	if b.now().Sub(b.openedAt) >= b.cooldown {
		return "half_open"
	}
	return "open"
}
