// Package ratelimit is a small in-memory sliding-window limiter, for a single
// server process. It bounds abuse of the sign-in endpoints (email bombing,
// token guessing); limits that must hold across restarts or several servers are
// also checked in the database (see internal/account).
package ratelimit

import (
	"sync"
	"time"
)

// Limiter allows at most limit events per key in any window.
type Limiter struct {
	limit   int
	window  time.Duration
	maxKeys int
	now     func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

// New returns a Limiter allowing limit events per key per window. It tracks at
// most maxKeys keys: past that, expired keys are swept and, if it is still full,
// an arbitrary tracked key is forgotten to make room. The map cannot be used to
// exhaust memory, and a flood of new keys cannot lock out a fresh one.
func New(limit int, window time.Duration, maxKeys int) *Limiter {
	return &Limiter{limit: limit, window: window, maxKeys: maxKeys, now: time.Now, hits: map[string][]time.Time{}}
}

// WithClock replaces the clock (tests).
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.now = now
	return l
}

// Allow records an event for key and reports whether it is within the limit. When it is
// not, retryAfter is how long until the oldest counted event leaves the window.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)

	events := l.hits[key]
	if events == nil && len(l.hits) >= l.maxKeys {
		l.sweep(cutoff)
		if len(l.hits) >= l.maxKeys {
			// Still full of live keys. Refusing the newcomer would let a flood of
			// throwaway keys lock every real visitor out, so instead one tracked key is
			// forgotten (a map range picks an arbitrary one in O(1)): memory stays
			// bounded and the worst a flood does is reset some other key's counter.
			for k := range l.hits {
				delete(l.hits, k)
				break
			}
		}
	}
	kept := events[:0]
	for _, t := range events {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false, kept[0].Add(l.window).Sub(now)
	}
	l.hits[key] = append(kept, now)
	return true, 0
}

func (l *Limiter) sweep(cutoff time.Time) {
	for k, events := range l.hits {
		if len(events) == 0 || !events[len(events)-1].After(cutoff) {
			delete(l.hits, k)
		}
	}
}
