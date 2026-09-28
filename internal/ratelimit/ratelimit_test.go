package ratelimit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestAllow_LimitsPerKeyWithinWindow(t *testing.T) {
	c := newClock()
	l := New(3, 10*time.Minute, 100).WithClock(c.now)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("event %d denied, want allowed", i+1)
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("4th event allowed, want denied")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("a different key was denied by a's limit")
	}
}

func TestAllow_SlidingWindowAndRetryAfter(t *testing.T) {
	c := newClock()
	l := New(3, 10*time.Minute, 100).WithClock(c.now)
	l.Allow("k") // t0
	c.advance(time.Minute)
	l.Allow("k") // t0+1m
	c.advance(time.Minute)
	l.Allow("k") // t0+2m
	c.advance(time.Minute)

	ok, retry := l.Allow("k") // t0+3m: oldest (t0) leaves the window at t0+10m
	if ok {
		t.Fatal("allowed inside a full window")
	}
	if retry != 7*time.Minute {
		t.Errorf("retryAfter = %v, want 7m (until the oldest counted event leaves)", retry)
	}

	c.advance(7*time.Minute - time.Second) // t0+9m59s: still full
	if ok, _ := l.Allow("k"); ok {
		t.Error("allowed one second before the oldest event expires")
	}
	c.advance(2 * time.Second) // t0+10m01s: only the first event has left
	if ok, _ := l.Allow("k"); !ok {
		t.Error("denied after the oldest event left the window (a sliding window frees one slot at a time)")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Error("a second event allowed: only one slot should have freed")
	}
}

func TestAllow_DeniedEventsDoNotExtendTheWindow(t *testing.T) {
	c := newClock()
	l := New(1, 10*time.Minute, 100).WithClock(c.now)
	l.Allow("k")
	for i := 0; i < 5; i++ {
		c.advance(time.Minute)
		if ok, _ := l.Allow("k"); ok {
			t.Fatal("allowed inside the window")
		}
	}
	c.advance(5*time.Minute + time.Second) // 10m01s after the only recorded event
	if ok, _ := l.Allow("k"); !ok {
		t.Error("denied although the only recorded event is older than the window: denied attempts must not count")
	}
}

func TestAllow_ExpiredKeysAreSweptWhenTheTableIsFull(t *testing.T) {
	c := newClock()
	l := New(1, 10*time.Minute, 2).WithClock(c.now)
	l.Allow("a")
	l.Allow("b")
	c.advance(11 * time.Minute) // every key has expired
	if ok, _ := l.Allow("c"); !ok {
		t.Error("new key refused after the old ones expired")
	}
	if n := len(l.hits); n != 1 {
		t.Errorf("tracked keys after sweep = %d, want 1 (the expired ones are gone)", n)
	}
}

func TestAllow_AFloodOfNewKeysCannotLockOutAFreshOneAndMemoryStaysBounded(t *testing.T) {
	c := newClock()
	const maxKeys = 100
	l := New(1, time.Hour, maxKeys).WithClock(c.now)
	for i := 0; i < 5*maxKeys; i++ { // a flood of live, distinct keys
		if ok, _ := l.Allow(fmt.Sprintf("attacker-%d", i)); !ok {
			t.Fatalf("flood key %d refused: a new key must always be admitted", i)
		}
		if n := len(l.hits); n > maxKeys {
			t.Fatalf("tracked keys = %d, want at most %d", n, maxKeys)
		}
	}
	if ok, _ := l.Allow("a-real-visitor"); !ok {
		t.Fatal("a fresh key was refused after a flood: the table must not lock out newcomers")
	}
	// A tracked key is still limited.
	if ok, _ := l.Allow("a-real-visitor"); ok {
		t.Error("a tracked key exceeded its limit")
	}
}

func TestAllow_ConcurrentCallsNeverExceedTheLimit(t *testing.T) {
	l := New(10, time.Hour, 100)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("k"); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 10 {
		t.Errorf("allowed = %d, want exactly 10", allowed.Load())
	}
}
