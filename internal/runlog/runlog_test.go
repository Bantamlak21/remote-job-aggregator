package runlog_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/runlog"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

// Same guard as the other integration tests: these drop every table, so the
// database name must end in "_test". Run with `go test -p 1`.
func newStore(t *testing.T) *runlog.Store {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; run `docker compose up -d db` and export TEST_DATABASE_URL to run this test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL does not parse as a URL: %v", err)
	}
	if name := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(name, "_test") {
		t.Fatalf("TEST_DATABASE_URL points at database %q; these tests drop tables and must run against a database whose name ends in \"_test\"", name)
	}
	ctx := context.Background()
	if err := database.MigrateDown(ctx, raw, migrations.FS); err != nil {
		t.Fatalf("MigrateDown() failed: %v", err)
	}
	if err := database.MigrateUp(ctx, raw, migrations.FS); err != nil {
		t.Fatalf("MigrateUp() failed: %v", err)
	}
	t.Cleanup(func() { _ = database.MigrateDown(ctx, raw, migrations.FS) })

	db, err := database.New(ctx, config.DatabaseConfig{URL: raw, MaxConns: 3, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute})
	if err != nil {
		t.Fatalf("database.New() failed: %v", err)
	}
	t.Cleanup(db.Close)
	return runlog.NewStore(db.Pool)
}

func TestStore_RemembersTheLastRunPerCollector(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, ok, err := s.LastRun(ctx, "remotive"); ok || err != nil {
		t.Fatalf("LastRun() before any run = ok %v, err %v; want not found and no error", ok, err)
	}
	first := time.Date(2026, 9, 25, 6, 0, 0, 0, time.UTC)
	if err := s.RecordRun(ctx, "remotive", first); err != nil {
		t.Fatalf("RecordRun() failed: %v", err)
	}
	got, ok, err := s.LastRun(ctx, "remotive")
	if err != nil || !ok || !got.Equal(first) {
		t.Errorf("LastRun() = %v, %v, %v; want %v", got, ok, err, first)
	}
	if _, ok, _ := s.LastRun(ctx, "himalayas"); ok {
		t.Errorf("another collector's run leaked into himalayas")
	}
}

// Two overlapping ingest processes must not make the newest run look older.
func TestStore_RecordRunOnlyMovesForward(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	late := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	early := late.Add(-3 * time.Hour)

	for _, at := range []time.Time{late, early} {
		if err := s.RecordRun(ctx, "remotive", at); err != nil {
			t.Fatalf("RecordRun(%v) failed: %v", at, err)
		}
	}
	if got, _, _ := s.LastRun(ctx, "remotive"); !got.Equal(late) {
		t.Errorf("LastRun() = %v after recording an older run, want %v kept", got, late)
	}
	if err := s.RecordRun(ctx, "  ", late); err == nil {
		t.Errorf("a blank collector name was accepted")
	}
}
