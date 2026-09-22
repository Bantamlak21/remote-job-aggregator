package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRun_UsageErrorOnNoArgs(t *testing.T) {
	err := run(context.Background(), []string{})
	if err == nil {
		t.Fatal("run([]) = nil, want a usage error")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("run([]) error = %q, want it to mention usage", err.Error())
	}
}

// Regression test: an unknown command must be rejected before config is
// loaded, so a typo'd command reports "unknown command", not a confusing
// "DATABASE_URL is required" — even with no environment configured at all.
func TestRun_UnknownCommandRejectedBeforeConfigLoad(t *testing.T) {
	clearRequiredEnv(t)

	err := run(context.Background(), []string{"bogus"})
	if err == nil {
		t.Fatal(`run(["bogus"]) = nil, want an unknown-command error`)
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q, want it to mention unknown command", err.Error())
	}
	if strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error = %q, want it to not even mention DATABASE_URL", err.Error())
	}
}

func TestRun_PropagatesConfigError(t *testing.T) {
	clearRequiredEnv(t)

	err := run(context.Background(), []string{"migrate-up"})
	if err == nil {
		t.Fatal(`run(["migrate-up"]) = nil, want a config error for missing DATABASE_URL`)
	}
	if !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Errorf("error = %q, want it to mention DATABASE_URL is required", err.Error())
	}
}

// migrate-down always requires --yes — even for the "single step" default
// — because this repo currently has exactly one migration, so a single
// step back drops every table just like --all would.
func TestRunMigrateDown_RejectsBareCommandWithoutYes(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	err := run(context.Background(), []string{"migrate-down"})
	if err == nil {
		t.Fatal(`migrate-down (no flags) = nil, want it rejected`)
	}
	if !strings.Contains(err.Error(), "requires --yes") {
		t.Errorf("error = %q, want it to explain --yes is required", err.Error())
	}
}

func TestRunMigrateDown_RejectsAllWithoutYes(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	err := run(context.Background(), []string{"migrate-down", "--all"})
	if err == nil {
		t.Fatal(`migrate-down --all (without --yes) = nil, want it rejected`)
	}
	if !strings.Contains(err.Error(), "requires --yes") {
		t.Errorf("error = %q, want it to explain --yes is required", err.Error())
	}
}

func TestRunMigrateDown_RejectsUnknownFlag(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	err := run(context.Background(), []string{"migrate-down", "--bogus"})
	if err == nil {
		t.Fatal(`migrate-down --bogus = nil, want it rejected`)
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error = %q, want it to mention the unknown flag", err.Error())
	}
}

func TestRunMigrateForce_RequiresExactlyOneVersionArg(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	for _, args := range [][]string{
		{"migrate-force"},
		{"migrate-force", "1", "2"},
		{"migrate-force", "not-a-number"},
	} {
		err := run(context.Background(), args)
		if err == nil {
			t.Errorf("run(%v) = nil, want an error", args)
		}
	}
}

func TestRunDiscover_RejectsTooManyArgs(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	err := run(context.Background(), []string{"discover", "seed.json", "extra"})
	if err == nil {
		t.Fatal(`discover with two positional args = nil, want an error`)
	}
}

func TestRunDiscover_MissingSeedFileFailsBeforeTouchingTheDatabase(t *testing.T) {
	clearRequiredEnv(t)
	// A bad DATABASE_URL that would fail to connect if this ever got that
	// far — it must not, since the seed file load happens first.
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:1/does-not-exist")

	err := run(context.Background(), []string{"discover", "/nonexistent/seed.json"})
	if err == nil {
		t.Fatal("discover with a nonexistent seed file = nil, want an error")
	}
	if !strings.Contains(err.Error(), "opening seed file") {
		t.Errorf("error = %q, want it to mention opening the seed file", err.Error())
	}
}

func TestRunDiscover_EmptySeedFileSucceedsWithoutTouchingTheDatabase(t *testing.T) {
	clearRequiredEnv(t)
	// Same bad DATABASE_URL as above: if an empty candidate list still
	// tried to connect, this would fail with a connection error instead.
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:1/does-not-exist")

	dir := t.TempDir()
	path := dir + "/empty.json"
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatalf("writing empty seed file: %v", err)
	}

	if err := run(context.Background(), []string{"discover", path}); err != nil {
		t.Errorf("discover with an empty seed file = %v, want nil", err)
	}
}

func TestRunSearchDiscover_RejectsTooManyArgs(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	err := run(context.Background(), []string{"search-discover", "names.txt", "extra"})
	if err == nil {
		t.Fatal(`search-discover with two positional args = nil, want an error`)
	}
}

func TestRunSearchDiscover_RequiresSearchCredentials(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("SERPER_API_KEY", "")

	err := run(context.Background(), []string{"search-discover"})
	if err == nil {
		t.Fatal("search-discover without search credentials = nil, want an error")
	}
	if !strings.Contains(err.Error(), "SERPER_API_KEY") {
		t.Errorf("error = %q, want it to mention the missing credentials", err.Error())
	}
}

func TestRunSearchDiscover_MissingNamesFileFailsBeforeTouchingTheDatabase(t *testing.T) {
	clearRequiredEnv(t)
	// A bad DATABASE_URL that would fail to connect if this ever got that
	// far — it must not, since the names file load happens first.
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:1/does-not-exist")
	t.Setenv("SERPER_API_KEY", "test-key")

	err := run(context.Background(), []string{"search-discover", "/nonexistent/names.txt"})
	if err == nil {
		t.Fatal("search-discover with a nonexistent names file = nil, want an error")
	}
	if !strings.Contains(err.Error(), "opening company names file") {
		t.Errorf("error = %q, want it to mention opening the names file", err.Error())
	}
}

func TestCloseWithTimeout_ReturnsNilOnPromptClose(t *testing.T) {
	err := closeWithTimeout(time.Second, func() {})
	if err != nil {
		t.Errorf("closeWithTimeout() = %v, want nil for an instantly-closing resource", err)
	}
}

// Regression test: a shutdown that doesn't finish within the deadline
// must surface as an error, not silently report success (which is what
// let a caller believe a hung close was a clean exit).
func TestCloseWithTimeout_ReturnsErrorOnSlowClose(t *testing.T) {
	closed := make(chan struct{})
	start := time.Now()

	err := closeWithTimeout(50*time.Millisecond, func() {
		time.Sleep(500 * time.Millisecond)
		close(closed)
	})

	if err == nil {
		t.Fatal("closeWithTimeout() = nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to mention timing out", err.Error())
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("closeWithTimeout() took %s, want it to return promptly at the timeout, not wait for closeFn", elapsed)
	}

	// closeFn's goroutine must still be allowed to finish in the
	// background rather than being abandoned mid-write; give it time and
	// confirm it did.
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Error("closeFn's goroutine never completed after the timeout fired")
	}
}

func clearRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "")
}

func TestInstallSignalHandling_FirstSignalCancelsContext(t *testing.T) {
	ctx, stop := installSignalHandling(context.Background())
	defer stop()

	if ctx.Err() != nil {
		t.Fatal("context already canceled before any signal was sent")
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM to self: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not canceled within 2s of SIGTERM")
	}
}

// Regression test: stop() mimics signal.NotifyContext's stop signature,
// and that stop is safe to call more than once, so calling this one
// twice must not panic either — a bug found by round-3 review (the
// unwrapped close(done) would panic on a second call).
func TestInstallSignalHandling_StopIsIdempotent(t *testing.T) {
	_, stop := installSignalHandling(context.Background())
	stop()
	stop() // must not panic
}

func TestInstallSignalHandling_StopReleasesTheGoroutineWithoutAnySignal(t *testing.T) {
	// Regression check for a goroutine leak: stop() must let the internal
	// watcher goroutine return even when no signal ever arrives. There is
	// no direct way to assert a goroutine exited from outside it, so this
	// mainly documents the expectation and relies on the race detector /
	// -leak tooling to catch a regression; it does exercise the done-channel
	// path that TestInstallSignalHandling_FirstSignalCancelsContext does not.
	ctx, stop := installSignalHandling(context.Background())
	stop()
	if ctx.Err() == nil {
		t.Error("stop() should cancel ctx even when no signal was ever received")
	}
}

// Regression test for the double-signal force-exit path. os.Exit(1) can't
// be called from within the test process without killing the test
// runner, so this re-execs the test binary filtered to just this test,
// in a subprocess, and asserts on its exit code — the standard Go idiom
// for testing os.Exit behavior.
//
// This specifically guards against a bug found by live testing during
// development: signal.NotifyContext alone keeps re-delivering every
// signal as the same cancellation for as long as stop() hasn't run, so a
// process genuinely stuck inside an uninterruptible call (verified:
// golang-migrate's own advisory-lock acquisition ignores the context
// entirely) would never die from repeated SIGTERMs without this.
func TestInstallSignalHandling_SecondSignalForcesExit(t *testing.T) {
	if os.Getenv("AGGREGATOR_TEST_FORCE_EXIT_SUBPROCESS") == "1" {
		ctx, stop := installSignalHandling(context.Background())
		defer stop()

		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			panic(err)
		}
		<-ctx.Done()

		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			panic(err)
		}

		// Should never reach here: the second signal's handler calls
		// os.Exit(1) from its own goroutine before this sleep elapses.
		time.Sleep(5 * time.Second)
		os.Exit(0) // if we get here, the test below correctly fails
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestInstallSignalHandling_SecondSignalForcesExit")
	cmd.Env = append(os.Environ(), "AGGREGATOR_TEST_FORCE_EXIT_SUBPROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected the subprocess to exit with a non-zero status, got err=%v", err)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1 (from the second-signal force-exit path)", exitErr.ExitCode())
	}
	if !strings.Contains(stderr.String(), "forcing immediate exit") {
		t.Errorf("subprocess stderr = %q, want it to mention forcing immediate exit", stderr.String())
	}
}
