package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeTicker is a ticker test code drives by hand instead of a real clock.
type fakeTicker struct {
	c       chan time.Time
	stopped bool
	mu      sync.Mutex
}

func newFakeTicker() *fakeTicker { return &fakeTicker{c: make(chan time.Time, 1)} }

func (f *fakeTicker) C() <-chan time.Time { return f.c }
func (f *fakeTicker) Stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
}
func (f *fakeTicker) fire() { f.c <- time.Now() }
func (f *fakeTicker) wasStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestScheduler_RunsOnceImmediatelyBeforeAnyTick(t *testing.T) {
	var calls int
	var mu sync.Mutex
	ft := newFakeTicker()
	s := &Scheduler{
		Interval:  time.Hour,
		Logger:    testLogger(),
		newTicker: func(time.Duration) ticker { return ft },
		Tick: func(context.Context) error {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 1
	})
	cancel()
	<-done
}

func TestScheduler_RunsAgainOnEveryTick(t *testing.T) {
	var calls int
	var mu sync.Mutex
	ft := newFakeTicker()
	s := &Scheduler{
		Interval:  time.Hour,
		Logger:    testLogger(),
		newTicker: func(time.Duration) ticker { return ft },
		Tick: func(context.Context) error {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls == 1 })
	ft.fire()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls == 2 })
	ft.fire()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls == 3 })

	cancel()
	<-done
	if !ft.wasStopped() {
		t.Error("Run: ticker was never Stop()ed on shutdown")
	}
}

func TestScheduler_AFailingTickDoesNotStopFutureTicks(t *testing.T) {
	var calls int
	var mu sync.Mutex
	ft := newFakeTicker()
	s := &Scheduler{
		Interval:  time.Hour,
		Logger:    testLogger(),
		newTicker: func(time.Duration) ticker { return ft },
		Tick: func(context.Context) error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return errors.New("boom")
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls == 1 })
	ft.fire()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls == 2 })

	cancel()
	<-done
}

func TestScheduler_StopsImmediatelyIfContextIsAlreadyDoneBeforeTheFirstTick(t *testing.T) {
	var calls int
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Scheduler{
		Interval: time.Hour,
		Logger:   testLogger(),
		Tick: func(context.Context) error {
			calls++
			return nil
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly for an already-canceled context")
	}
	// The immediate first tick still runs (mirrors account.Service.Run's purge:
	// a server restarted more often than Interval must not skip a cycle) --
	// what must not happen is a ticker being started after ctx is already done.
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (the immediate tick runs even though ctx was already canceled)", calls)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("waitFor: condition never became true")
}
