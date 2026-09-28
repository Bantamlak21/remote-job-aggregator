// Package scheduler is Phase 6's "periodic, unattended execution": one
// in-process ticker running the ingest -> classify -> notify pipeline on an
// interval, so a deploy that just runs `aggregator serve` needs no external
// cron entry. No distributed scheduling infra (CLAUDE.md): one instance,
// one ticker, nothing to coordinate across processes.
package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// ticker is the subset of *time.Ticker Scheduler needs, so tests can swap
// in one they control instead of waiting on a real clock.
type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

func newRealTicker(d time.Duration) ticker { return realTicker{time.NewTicker(d)} }

// Scheduler runs Tick once immediately, then every Interval, until its
// context is done. Mirrors account.Service.Run's purge loop: running once
// at startup matters because a server restarted more often than Interval
// must not skip a cycle.
type Scheduler struct {
	Interval time.Duration
	Tick     func(ctx context.Context) error
	Logger   *slog.Logger

	// newTicker is swapped in tests; nil means the real clock.
	newTicker func(d time.Duration) ticker
}

// Run blocks until ctx is done. Every goroutine that calls Run has a clear
// shutdown path: ctx cancellation is checked both before and during the
// wait for the next tick.
func (s *Scheduler) Run(ctx context.Context) {
	newTicker := s.newTicker
	if newTicker == nil {
		newTicker = newRealTicker
	}
	s.runTick(ctx)
	if ctx.Err() != nil {
		return
	}
	t := newTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			s.runTick(ctx)
		}
	}
}

// runTick logs a failing tick and moves on: a bad tick (a target board
// down, a classify failure) must not stop future ticks any more than one
// bad `aggregator ingest` run should need a human to restart cron.
func (s *Scheduler) runTick(ctx context.Context) {
	if err := s.Tick(ctx); err != nil {
		s.Logger.Error("scheduler tick failed", "error", err)
	}
}
