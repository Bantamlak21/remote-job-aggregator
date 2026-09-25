package main

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

func classifyTestDB(t *testing.T) *database.DB {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(u.Path, "/"), "_test") {
		t.Fatalf("TEST_DATABASE_URL must name a database ending in _test")
	}
	ctx := context.Background()
	if err := database.MigrateDown(ctx, raw, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if err := database.MigrateUp(ctx, raw, migrations.FS); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.MigrateDown(ctx, raw, migrations.FS) })
	db, err := database.New(ctx, config.DatabaseConfig{URL: raw, MaxConns: 3, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// classifyJobs is what `aggregator classify` and the end of `ingest` run: it
// must give every stored job a verdict, and a broken role profile must be an
// error, not a silent skip.
func TestClassifyJobs_GivesStoredJobsAVerdictAndFailsOnABrokenProfile(t *testing.T) {
	db := classifyTestDB(t)
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	c, err := company.NewStore(db.Pool).Upsert(ctx, company.UpsertParams{Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	tg, err := company.NewTargetStore(db.Pool).Upsert(ctx, company.TargetUpsertParams{CompanyID: c.ID, ATSProvider: "greenhouse", ExternalBoardID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.NewStore(db.Pool).UpsertFromATS(ctx, job.Record{CompanyID: c.ID, TargetCompanyID: tg.ID, Source: "greenhouse", SourceJobID: "1",
		Title: "Backend Engineer", Description: "x", ApplicationURL: "https://x.example/1", LocationRaw: "Worldwide", RemoteType: "remote"}); err != nil {
		t.Fatal(err)
	}

	st, err := classifyJobs(ctx, &config.Config{}, quiet, db, false)
	if err != nil || st.Classified != 1 || st.Eligible != 1 {
		t.Fatalf("classifyJobs() = %+v, %v; want the one job classified eligible", st, err)
	}
	if st, err = classifyJobs(ctx, &config.Config{}, quiet, db, false); err != nil || st.Classified != 0 {
		t.Errorf("second run = %+v, %v; want nothing to do", st, err)
	}
	if st, err = classifyJobs(ctx, &config.Config{}, quiet, db, true); err != nil || st.Classified != 1 {
		t.Errorf("forced run = %+v, %v; want the job again", st, err)
	}

	broken := &config.Config{}
	broken.Filtering.RelevanceProfile = "/nonexistent/profile.json"
	if _, err := classifyJobs(ctx, broken, quiet, db, false); err == nil {
		t.Errorf("classifyJobs with an unreadable role profile returned no error")
	}
}
