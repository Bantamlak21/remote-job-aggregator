package database_test

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

// testDatabaseURL returns the DATABASE_URL to run integration tests
// against, skipping the test entirely if it isn't set. These tests hit a
// real PostgreSQL instance (docker compose up -d db) rather than a mock,
// per CLAUDE.md's "Integration tests: PostgreSQL, migrations, repositories,
// upserts, transactions, constraints."
//
// Several of these tests call MigrateDown, which drops every table. To
// make it structurally impossible to point this at a real dev/prod
// database by copy-pasting the wrong URL, the database name must end in
// "_test".
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; run `docker compose up -d db` and export TEST_DATABASE_URL to run this test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL does not parse as a URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.HasSuffix(dbName, "_test") {
		t.Fatalf("TEST_DATABASE_URL points at database %q; these tests drop tables and must run "+
			"against a database whose name ends in \"_test\" (see docker-compose.yml's aggregator_test)", dbName)
	}
	return raw
}

func dbConfig(url string) config.DatabaseConfig {
	return config.DatabaseConfig{
		URL:             url,
		MaxConns:        5,
		MinConns:        0,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
	}
}

// resetSchema drops and re-applies migrations so each test starts from a
// clean, known schema regardless of what earlier test runs left behind.
// MigrateDown() already treats "nothing to roll back" as success, so a
// real failure here (e.g. a dirty schema_migrations row left by a crashed
// previous run) is not something tests should silently paper over.
func resetSchema(t *testing.T, url string) {
	t.Helper()
	if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("resetSchema: MigrateDown() failed: %v", err)
	}
	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("resetSchema: MigrateUp() failed: %v", err)
	}
	t.Cleanup(func() {
		if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
			t.Errorf("cleanup MigrateDown() failed: %v", err)
		}
	})
}

// unreachableAddr returns a "host:port" that is guaranteed to have
// nothing listening on it: bind an ephemeral port, then release it. A
// connection attempt gets an immediate ECONNREFUSED instead of depending
// on a public IP's black-hole behavior, which varies by network and made
// the equivalent test slow (a multi-second dial timeout) and
// environment-dependent.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unreachableAddr: failed to bind a probe port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("unreachableAddr: failed to release probe port: %v", err)
	}
	return addr
}

func TestNew_ConnectsAndHealthChecks(t *testing.T) {
	url := testDatabaseURL(t)
	ctx := context.Background()

	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	if err := db.Health(ctx); err != nil {
		t.Errorf("Health() failed on a freshly opened pool: %v", err)
	}
}

func TestNew_RejectsUnreachableHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	addr := unreachableAddr(t)
	_, err := database.New(ctx, dbConfig("postgres://user:pass@"+addr+"/jobs"))
	if err == nil {
		t.Fatal("New() succeeded against an unreachable host, want an error")
	}
}

func TestMigrateUp_CreatesExpectedTablesAndIsIdempotent(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	// Running Up again with nothing pending must be a no-op, not an error.
	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("second MigrateUp() call failed: %v", err)
	}

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	for _, table := range []string{"companies", "target_companies", "jobs"} {
		var exists bool
		err := db.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
			table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("checking table %q existence: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q does not exist after MigrateUp()", table)
		}
	}
}

func TestMigrateDown_RemovesTables(t *testing.T) {
	url := testDatabaseURL(t)
	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("MigrateUp() failed: %v", err)
	}
	if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("MigrateDown() failed: %v", err)
	}
	t.Cleanup(func() {
		if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
			t.Errorf("cleanup MigrateUp() failed: %v", err)
		}
	})

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	var exists bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jobs')`,
	).Scan(&exists); err != nil {
		t.Fatalf("checking table existence: %v", err)
	}
	if exists {
		t.Error("table \"jobs\" still exists after MigrateDown()")
	}
}

// Regression test: MigrateDownStep must not report success when
// schema_migrations points at a version this binary has no migration
// file for — a real failure, not the harmless "already at nil version"
// case that also happens to surface as os.ErrNotExist. Found by a
// round-3 adversarial review after the initial fix for this function's
// idempotency swallowed both cases identically.
func TestMigrateDownStep_FailsOnUnknownVersion_DoesNotReportFalseSuccess(t *testing.T) {
	url := testDatabaseURL(t)
	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("MigrateUp() failed: %v", err)
	}
	if err := database.MigrateForce(url, migrations.FS, 99); err != nil {
		t.Fatalf("MigrateForce(99) failed: %v", err)
	}
	t.Cleanup(func() {
		_ = database.MigrateForce(url, migrations.FS, 1)
		if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
			t.Errorf("cleanup MigrateDown() failed: %v", err)
		}
	})

	err := database.MigrateDownStep(context.Background(), url, migrations.FS)
	if err == nil {
		t.Fatal("MigrateDownStep() = nil for a schema_migrations version with no matching migration file, want an error")
	}
}

func TestMigrateDownStep_RollsBackOneVersion(t *testing.T) {
	url := testDatabaseURL(t)
	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("MigrateUp() failed: %v", err)
	}
	t.Cleanup(func() {
		if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
			t.Errorf("cleanup MigrateUp() failed: %v", err)
		}
	})

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	tableExists := func() bool {
		t.Helper()
		var exists bool
		if err := db.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jobs')`,
		).Scan(&exists); err != nil {
			t.Fatalf("checking table existence: %v", err)
		}
		return exists
	}
	priorityColumnExists := func() bool {
		t.Helper()
		var exists bool
		if err := db.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'companies' AND column_name = 'is_priority')`,
		).Scan(&exists); err != nil {
			t.Fatalf("checking column existence: %v", err)
		}
		return exists
	}

	expiresColumnExists := func() bool {
		t.Helper()
		var exists bool
		if err := db.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = 'expires_at')`,
		).Scan(&exists); err != nil {
			t.Fatalf("checking column existence: %v", err)
		}
		return exists
	}

	marketColumnExists := func() bool {
		t.Helper()
		var exists bool
		if err := db.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'target_companies' AND column_name = 'market')`,
		).Scan(&exists); err != nil {
			t.Fatalf("checking column existence: %v", err)
		}
		return exists
	}

	// Step 1 undoes only the latest migration (000004's market column); the
	// earlier schema must survive it.
	if err := database.MigrateDownStep(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("MigrateDownStep() failed: %v", err)
	}
	if marketColumnExists() {
		t.Error("target_companies.market still exists after the first MigrateDownStep()")
	}
	if !expiresColumnExists() || !priorityColumnExists() || !tableExists() {
		t.Error("an earlier migration's schema vanished after one MigrateDownStep(); only the latest should be undone")
	}

	// Step 2 undoes 000003's expires_at column.
	if err := database.MigrateDownStep(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("second MigrateDownStep() failed: %v", err)
	}
	if expiresColumnExists() {
		t.Error("jobs.expires_at still exists after the second MigrateDownStep()")
	}
	if !priorityColumnExists() || !tableExists() {
		t.Error("an earlier migration's schema vanished after two MigrateDownStep() calls")
	}

	// Step 3 undoes 000002's is_priority column.
	if err := database.MigrateDownStep(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("third MigrateDownStep() failed: %v", err)
	}
	if priorityColumnExists() {
		t.Error("companies.is_priority still exists after the third MigrateDownStep()")
	}
	if !tableExists() {
		t.Error("table \"jobs\" vanished after three MigrateDownStep() calls; 000001 must still be applied")
	}

	// Step 4 undoes 000001, which removes everything it created.
	if err := database.MigrateDownStep(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("fourth MigrateDownStep() failed: %v", err)
	}
	if tableExists() {
		t.Error("table \"jobs\" still exists after rolling back the initial migration")
	}

	// Idempotent at the bottom: stepping back again with nothing left
	// must be a no-op, not an error — mirrors MigrateDown/MigrateUp's
	// ErrNoChange handling.
	if err := database.MigrateDownStep(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("fifth MigrateDownStep() call (nothing left to roll back) failed: %v", err)
	}
}

// Regression tests for a false-success bug found live during round-4
// review: golang-migrate's runMigrations returns nil on a graceful stop
// exactly as it does on genuine completion, so watchGracefulStop's own
// mechanism (wired to ctx cancellation) could make MigrateUp/MigrateDown/
// MigrateDownStep report success for a signal that arrived before the
// work finished — reproduced live by holding a lock on schema_migrations
// and sending one SIGTERM to a blocked migrate-down --yes --all: it
// logged "all migrations rolled back" and exited 0 with all 4 tables
// still present.
//
// These tests use an already-canceled context, which deterministically
// exercises checkInterrupted's own ctx.Err() check — the guarantee the
// fix actually provides — rather than the graceful-stop race itself,
// which is not deterministic for a single-migration repo (whether the
// watcher goroutine wins the race to push into m.GracefulStop before
// runMigrations' loop checks m.stop() for the one and only migration
// item depends on Go scheduler timing, not on this code). What must be
// deterministic, and is: an already-canceled ctx always yields a
// non-nil error, and a subsequent call with a fresh context always
// recovers regardless of what the canceled-context call actually did
// underneath — Up/Down/Steps are idempotent either way.

func TestMigrateUp_ReturnsErrorOnAlreadyCanceledContext(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)
	if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("MigrateDown() failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := database.MigrateUp(ctx, url, migrations.FS)
	if err == nil {
		t.Fatal("MigrateUp(already-canceled ctx) = nil, want an error rather than silently claiming success")
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error = %q, want it to mention the migration being interrupted", err.Error())
	}

	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Errorf("MigrateUp() with a fresh context failed to recover after the canceled-context call: %v", err)
	}
}

func TestMigrateDown_ReturnsErrorOnAlreadyCanceledContext(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := database.MigrateDown(ctx, url, migrations.FS)
	if err == nil {
		t.Fatal("MigrateDown(already-canceled ctx) = nil, want an error rather than silently claiming success")
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error = %q, want it to mention the migration being interrupted", err.Error())
	}

	if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
		t.Errorf("MigrateDown() with a fresh context failed to recover after the canceled-context call: %v", err)
	}
}

func TestMigrateDownStep_ReturnsErrorOnAlreadyCanceledContext(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := database.MigrateDownStep(ctx, url, migrations.FS)
	if err == nil {
		t.Fatal("MigrateDownStep(already-canceled ctx) = nil, want an error rather than silently claiming success")
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error = %q, want it to mention the migration being interrupted", err.Error())
	}

	if err := database.MigrateDownStep(context.Background(), url, migrations.FS); err != nil {
		t.Errorf("MigrateDownStep() with a fresh context failed to recover after the canceled-context call: %v", err)
	}
}

// latestMigrationVersion is the highest version in migrations/. Tests that
// force the schema_migrations row use it so they leave the schema state
// consistent with the tables that actually exist; bump it with every new
// migration.
const latestMigrationVersion = 4

func TestMigrateForce_ClearsDirtyState(t *testing.T) {
	url := testDatabaseURL(t)
	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Fatalf("MigrateUp() failed: %v", err)
	}
	t.Cleanup(func() {
		_ = database.MigrateForce(url, migrations.FS, latestMigrationVersion)
		if err := database.MigrateDown(context.Background(), url, migrations.FS); err != nil {
			t.Errorf("cleanup MigrateDown() failed: %v", err)
		}
	})

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	// Simulate what a process killed mid-migration leaves behind:
	// golang-migrate marks the row dirty before running the migration's
	// statements and only clears it on success.
	if _, err := db.Pool.Exec(ctx, `UPDATE schema_migrations SET dirty = true`); err != nil {
		t.Fatalf("simulating a dirty migration state: %v", err)
	}

	if err := database.MigrateUp(context.Background(), url, migrations.FS); err == nil {
		t.Fatal("MigrateUp() succeeded against a dirty schema_migrations row, want an error")
	}

	if err := database.MigrateForce(url, migrations.FS, latestMigrationVersion); err != nil {
		t.Fatalf("MigrateForce() failed to clear the dirty state: %v", err)
	}

	var dirty bool
	if err := db.Pool.QueryRow(ctx, `SELECT dirty FROM schema_migrations`).Scan(&dirty); err != nil {
		t.Fatalf("checking dirty state: %v", err)
	}
	if dirty {
		t.Error("schema_migrations still dirty after MigrateForce()")
	}

	// The recovery is real, not just cosmetic: migrate-up must work again.
	if err := database.MigrateUp(context.Background(), url, migrations.FS); err != nil {
		t.Errorf("MigrateUp() still fails after MigrateForce(): %v", err)
	}
}

// seedCompanyAndTarget inserts one company and one greenhouse target for
// it, for tests that need a valid parent row to hang a jobs insert off of.
func seedCompanyAndTarget(t *testing.T, ctx context.Context, db *database.DB, companyName, boardID string) (companyID, targetID int64) {
	t.Helper()
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO companies (name) VALUES ($1) RETURNING id`, companyName,
	).Scan(&companyID); err != nil {
		t.Fatalf("inserting company: %v", err)
	}
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO target_companies (company_id, ats_provider, external_board_id) VALUES ($1, 'greenhouse', $2) RETURNING id`,
		companyID, boardID,
	).Scan(&targetID); err != nil {
		t.Fatalf("inserting target company: %v", err)
	}
	return companyID, targetID
}

func TestSchema_EnforcesJobIdentityUniqueConstraint(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	companyID, targetID := seedCompanyAndTarget(t, ctx, db, "Acme", "acme")

	insertJob := `
		INSERT INTO jobs (company_id, target_company_id, source, source_job_id, title, application_url, content_hash)
		VALUES ($1, $2, 'greenhouse', 'job-123', 'Backend Engineer', 'https://example.com/jobs/123', 'hash1')`

	if _, err := db.Pool.Exec(ctx, insertJob, companyID, targetID); err != nil {
		t.Fatalf("first insert of job-123 failed: %v", err)
	}

	// Same (source, source_job_id) again must be rejected by the unique
	// constraint per CLAUDE.md §9 — job identity, not merely title+company.
	_, err = db.Pool.Exec(ctx, insertJob, companyID, targetID)
	if err == nil {
		t.Fatal("duplicate (source, source_job_id) insert succeeded, want a unique constraint violation")
	}
}

// A job's company_id must actually be the company that owns its target
// board — otherwise a transposed variable in ingestion code silently
// mis-attributes a posting with no constraint ever complaining.
func TestSchema_RejectsCompanyNotOwningTarget(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	_, acmeTargetID := seedCompanyAndTarget(t, ctx, db, "Acme", "acme")
	globexID, _ := seedCompanyAndTarget(t, ctx, db, "Globex", "globex")

	_, err = db.Pool.Exec(ctx, `
		INSERT INTO jobs (company_id, target_company_id, source, source_job_id, title, application_url, content_hash)
		VALUES ($1, $2, 'greenhouse', 'job-1', 'Backend Engineer', 'https://example.com/jobs/1', 'hash1')`,
		globexID, acmeTargetID, // Globex's company_id paired with Acme's target.
	)
	if err == nil {
		t.Fatal("insert with mismatched company_id/target_company_id succeeded, want a foreign key violation")
	}
}

// A job's source must match the ats_provider of the target board it
// belongs to; otherwise it can collide with, or split away from, the
// wrong (source, source_job_id) identity.
func TestSchema_RejectsSourceNotMatchingTargetProvider(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	companyID, targetID := seedCompanyAndTarget(t, ctx, db, "Acme", "acme") // target is greenhouse

	_, err = db.Pool.Exec(ctx, `
		INSERT INTO jobs (company_id, target_company_id, source, source_job_id, title, application_url, content_hash)
		VALUES ($1, $2, 'lever', 'job-1', 'Backend Engineer', 'https://example.com/jobs/1', 'hash1')`,
		companyID, targetID,
	)
	if err == nil {
		t.Fatal("insert with source != target's ats_provider succeeded, want a foreign key violation")
	}
}

func TestSchema_RejectsBlankIdentityFields(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	if _, err := db.Pool.Exec(ctx, `INSERT INTO companies (name) VALUES ('')`); err == nil {
		t.Error("blank company name inserted successfully, want a CHECK violation")
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO companies (name) VALUES ('   ')`); err == nil {
		t.Error("whitespace-only company name inserted successfully, want a CHECK violation")
	}
	// website and canonical_url are nullable, but a blank (non-null)
	// string must still be rejected — otherwise a second "no website"
	// company using '' instead of NULL would collide under the unique
	// index on normalize_website(website), since '' normalizes to itself.
	if _, err := db.Pool.Exec(ctx, `INSERT INTO companies (name, website) VALUES ('Blank Website Co', '')`); err == nil {
		t.Error("blank (non-null) company website inserted successfully, want a CHECK violation")
	}

	companyID, targetID := seedCompanyAndTarget(t, ctx, db, "Acme", "acme")

	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO jobs (company_id, target_company_id, source, source_job_id, title, application_url, content_hash, canonical_url)
		VALUES ($1, $2, 'greenhouse', 'job-canonical-blank', 'Backend Engineer', 'https://example.com/jobs/1', 'hash1', '')`,
		companyID, targetID,
	); err == nil {
		t.Error("blank (non-null) canonical_url inserted successfully, want a CHECK violation")
	}

	cases := map[string]string{
		"source":          "",
		"source_job_id":   "",
		"title":           "",
		"application_url": "",
		"content_hash":    "",
	}
	for col := range cases {
		values := map[string]string{
			"source":          "greenhouse",
			"source_job_id":   "job-1",
			"title":           "Backend Engineer",
			"application_url": "https://example.com/jobs/1",
			"content_hash":    "hash1",
		}
		values[col] = ""
		_, err := db.Pool.Exec(ctx, `
			INSERT INTO jobs (company_id, target_company_id, source, source_job_id, title, application_url, content_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			companyID, targetID, values["source"], values["source_job_id"], values["title"], values["application_url"], values["content_hash"],
		)
		if err == nil {
			t.Errorf("blank %s inserted successfully, want a CHECK violation", col)
		}
	}
}

func TestSchema_ClosedAtMustMatchStatus(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	companyID, targetID := seedCompanyAndTarget(t, ctx, db, "Acme", "acme")
	insert := `
		INSERT INTO jobs (company_id, target_company_id, source, source_job_id, title, application_url, content_hash, status, closed_at)
		VALUES ($1, $2, 'greenhouse', $3, 'Backend Engineer', 'https://example.com/jobs/1', 'hash1', $4, $5)`

	if _, err := db.Pool.Exec(ctx, insert, companyID, targetID, "job-open-with-closed-at", "open", time.Now()); err == nil {
		t.Error("status='open' with a non-null closed_at inserted successfully, want a CHECK violation")
	}
	if _, err := db.Pool.Exec(ctx, insert, companyID, targetID, "job-closed-without-closed-at", "closed", nil); err == nil {
		t.Error("status='closed' with a null closed_at inserted successfully, want a CHECK violation")
	}
}

func TestSchema_LastSeenCannotPrecedeFirstSeen(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	companyID, targetID := seedCompanyAndTarget(t, ctx, db, "Acme", "acme")
	now := time.Now()

	_, err = db.Pool.Exec(ctx, `
		INSERT INTO jobs (company_id, target_company_id, source, source_job_id, title, application_url, content_hash, first_seen_at, last_seen_at)
		VALUES ($1, $2, 'greenhouse', 'job-1', 'Backend Engineer', 'https://example.com/jobs/1', 'hash1', $3, $4)`,
		companyID, targetID, now, now.Add(-10*24*time.Hour),
	)
	if err == nil {
		t.Error("last_seen_at 10 days before first_seen_at inserted successfully, want a CHECK violation")
	}
}

func TestSchema_WebsiteUniquenessIgnoresCaseAndTrailingSlash(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	if _, err := db.Pool.Exec(ctx, `INSERT INTO companies (name, website) VALUES ('Acme Inc', 'https://acme.com')`); err != nil {
		t.Fatalf("inserting first company: %v", err)
	}

	_, err = db.Pool.Exec(ctx, `INSERT INTO companies (name, website) VALUES ('ACME Holdings', 'HTTPS://ACME.COM/')`)
	if err == nil {
		t.Error("a case- and trailing-slash-variant duplicate website inserted successfully, want a unique violation")
	}
}

// Regression test: the name index must trim the same way the CHECK
// constraint does, or 'Globex' and 'Globex ' pass the non-blank CHECK
// individually but collide as two different rows in the unique index —
// found by round-4 adversarial review as an inconsistency with the
// website index, which already trims.
func TestSchema_NameUniquenessIgnoresCaseAndSurroundingWhitespace(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	if _, err := db.Pool.Exec(ctx, `INSERT INTO companies (name) VALUES ('Globex')`); err != nil {
		t.Fatalf("inserting first company: %v", err)
	}

	_, err = db.Pool.Exec(ctx, `INSERT INTO companies (name) VALUES ('  GLOBEX  ')`)
	if err == nil {
		t.Error("a case- and whitespace-variant duplicate name inserted successfully, want a unique violation")
	}
}

// Regression test: same class of gap as the two tests above, found by
// round-5 review — target_companies' identity index must normalize
// external_board_id the same way companies.name/website do, or a
// whitespace/case variant of an existing board id registers as a
// second, distinct target for the same provider.
func TestSchema_TargetBoardUniquenessIgnoresCaseAndSurroundingWhitespace(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	companyID, _ := seedCompanyAndTarget(t, ctx, db, "Acme", "acme")

	_, err = db.Pool.Exec(ctx,
		`INSERT INTO target_companies (company_id, ats_provider, external_board_id) VALUES ($1, 'greenhouse', '  ACME  ')`,
		companyID,
	)
	if err == nil {
		t.Error("a case- and whitespace-variant duplicate external_board_id inserted successfully, want a unique violation")
	}
}

func TestSchema_UpdatedAtTriggerFiresOnlyOnRealChanges(t *testing.T) {
	url := testDatabaseURL(t)
	resetSchema(t, url)

	ctx := context.Background()
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	var companyID int64
	var updatedAt time.Time
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO companies (name) VALUES ('Globex') RETURNING id, updated_at`,
	).Scan(&companyID, &updatedAt); err != nil {
		t.Fatalf("inserting company: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	var afterRealChange time.Time
	if err := db.Pool.QueryRow(ctx,
		`UPDATE companies SET description = 'updated' WHERE id = $1 RETURNING updated_at`,
		companyID,
	).Scan(&afterRealChange); err != nil {
		t.Fatalf("updating company: %v", err)
	}
	if !afterRealChange.After(updatedAt) {
		t.Errorf("updated_at did not advance on a real change: before=%s after=%s", updatedAt, afterRealChange)
	}

	time.Sleep(10 * time.Millisecond)

	var afterNoop time.Time
	if err := db.Pool.QueryRow(ctx,
		`UPDATE companies SET description = 'updated' WHERE id = $1 RETURNING updated_at`,
		companyID,
	).Scan(&afterNoop); err != nil {
		t.Fatalf("no-op updating company: %v", err)
	}
	if !afterNoop.Equal(afterRealChange) {
		t.Errorf("updated_at advanced on a no-op update: before=%s after=%s", afterRealChange, afterNoop)
	}
}
