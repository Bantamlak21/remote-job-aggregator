package job

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

// The helpers below are deliberately duplicated from internal/company's
// and internal/database's own test helpers, not shared: each
// DB-touching package resets the same schema in its own tests, and
// `go test -p 1 ./...` (see Makefile) is what serializes them safely —
// see CLAUDE.md/README's documented reasoning for that requirement.

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
			"against a database whose name ends in \"_test\"", dbName)
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

func newDB(t *testing.T) *database.DB {
	t.Helper()
	url := testDatabaseURL(t)
	resetSchema(t, url)

	db, err := database.New(context.Background(), dbConfig(url))
	if err != nil {
		t.Fatalf("database.New() failed: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// seedTarget creates a company and one target board for it, returning
// the target's id and the company's id — everything UpsertFromATS needs
// to attribute a job to.
func seedTarget(t *testing.T, ctx context.Context, db *database.DB, companyName, boardID string) (targetID, companyID int64) {
	t.Helper()
	c, err := company.NewStore(db.Pool).Upsert(ctx, company.UpsertParams{Name: companyName})
	if err != nil {
		t.Fatalf("seeding company %q: %v", companyName, err)
	}
	tgt, err := company.NewTargetStore(db.Pool).Upsert(ctx, company.TargetUpsertParams{
		CompanyID: c.ID, ATSProvider: "greenhouse", ExternalBoardID: boardID,
	})
	if err != nil {
		t.Fatalf("seeding target %q: %v", boardID, err)
	}
	return tgt.ID, c.ID
}

func baseRecord(targetID, companyID int64, sourceJobID string) Record {
	return Record{
		CompanyID: companyID, TargetCompanyID: targetID,
		Source: "greenhouse", SourceJobID: sourceJobID,
		Title: "Backend Engineer", Description: "Build things.",
		ApplicationURL: "https://job-boards.greenhouse.io/acme/jobs/" + sourceJobID,
		LocationRaw:    "Remote",
		PublishedAt:    time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestStoreUpsertFromATS_InsertsNewJob(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)

	outcome, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, "1001"))
	if err != nil {
		t.Fatalf("UpsertFromATS() failed: %v", err)
	}
	if !outcome.Inserted || !outcome.Changed {
		t.Errorf("outcome = %+v, want Inserted=true Changed=true for a brand-new job", outcome)
	}

	result, err := store.List(ctx, Filter{PageSize: 10})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if result.Total != 1 || len(result.Jobs) != 1 {
		t.Fatalf("List() = %+v, want exactly one job", result)
	}
	if result.Jobs[0].Title != "Backend Engineer" || result.Jobs[0].CompanyName != "Acme" {
		t.Errorf("Jobs[0] = %+v, unexpected fields", result.Jobs[0])
	}
	if result.Jobs[0].RemoteType != RemoteTypeUnknown || result.Jobs[0].EmploymentType != EmploymentTypeUnknown {
		t.Errorf("Jobs[0] remote/employment type = %q/%q, want unknown/unknown (Phase 3 never classifies)",
			result.Jobs[0].RemoteType, result.Jobs[0].EmploymentType)
	}
}

func TestStoreUpsertFromATS_ReUpsertWithUnchangedContentOnlyAdvancesLastSeen(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	rec := baseRecord(targetID, companyID, "1001")

	first, err := store.UpsertFromATS(ctx, rec)
	if err != nil {
		t.Fatalf("first UpsertFromATS() failed: %v", err)
	}
	if !first.Inserted || !first.Changed {
		t.Fatalf("first outcome = %+v, want Inserted=true Changed=true", first)
	}
	var firstLastChangedAt time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT last_changed_at FROM jobs WHERE source_job_id = '1001'`).Scan(&firstLastChangedAt); err != nil {
		t.Fatalf("reading last_changed_at: %v", err)
	}

	time.Sleep(10 * time.Millisecond) // ensure now() strictly advances between calls
	second, err := store.UpsertFromATS(ctx, rec)
	if err != nil {
		t.Fatalf("second UpsertFromATS() failed: %v", err)
	}
	if second.Inserted {
		t.Error("second call: Inserted = true, want false (already exists)")
	}
	if second.Changed {
		t.Error("second call: Changed = true, want false (identical content)")
	}

	var lastSeenAt, lastChangedAt time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT last_seen_at, last_changed_at FROM jobs WHERE source_job_id = '1001'`).
		Scan(&lastSeenAt, &lastChangedAt); err != nil {
		t.Fatalf("reading timestamps: %v", err)
	}
	if !lastChangedAt.Equal(firstLastChangedAt) {
		t.Errorf("last_changed_at moved (%s -> %s) for unchanged content, want it to stay put", firstLastChangedAt, lastChangedAt)
	}
	if lastSeenAt.Before(firstLastChangedAt) {
		t.Errorf("last_seen_at = %s, want it to have advanced past the first upsert's time", lastSeenAt)
	}
}

func TestStoreUpsertFromATS_ChangedContentUpdatesFieldsAndLastChangedAt(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	rec := baseRecord(targetID, companyID, "1001")

	if _, err := store.UpsertFromATS(ctx, rec); err != nil {
		t.Fatalf("first UpsertFromATS() failed: %v", err)
	}

	rec.Title = "Senior Backend Engineer"
	outcome, err := store.UpsertFromATS(ctx, rec)
	if err != nil {
		t.Fatalf("second UpsertFromATS() failed: %v", err)
	}
	if outcome.Inserted {
		t.Error("Inserted = true on a re-upsert, want false")
	}
	if !outcome.Changed {
		t.Error("Changed = false after a real title change, want true")
	}

	got, err := store.Get(ctx, mustJobID(t, ctx, db, "1001"))
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if got.Title != "Senior Backend Engineer" {
		t.Errorf("Title = %q, want the updated title", got.Title)
	}
}

func TestStoreUpsertFromATS_RejectsReassigningToADifferentTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetA, companyA := seedTarget(t, ctx, db, "Acme", "acme")
	targetB, companyB := seedTarget(t, ctx, db, "Globex", "globex")
	store := NewStore(db.Pool)

	if _, err := store.UpsertFromATS(ctx, baseRecord(targetA, companyA, "shared-id")); err != nil {
		t.Fatalf("seeding under target A failed: %v", err)
	}

	_, err := store.UpsertFromATS(ctx, baseRecord(targetB, companyB, "shared-id"))
	if !errors.Is(err, ErrJobTargetMismatch) {
		t.Fatalf("err = %v, want it to wrap ErrJobTargetMismatch", err)
	}

	// The original row must be provably untouched.
	var gotCompanyID int64
	if err := db.Pool.QueryRow(ctx, `SELECT company_id FROM jobs WHERE source_job_id = 'shared-id'`).Scan(&gotCompanyID); err != nil {
		t.Fatalf("reading job: %v", err)
	}
	if gotCompanyID != companyA {
		t.Errorf("company_id = %d after the rejected reassignment, want it to stay %d (company A)", gotCompanyID, companyA)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE source_job_id = 'shared-id'`).Scan(&count); err != nil {
		t.Fatalf("counting jobs: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want exactly 1 row (no duplicate created)", count)
	}
}

func TestStoreUpsertFromATS_ReopensAPreviouslyRemovedJob(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	rec := baseRecord(targetID, companyID, "1001")

	if _, err := store.UpsertFromATS(ctx, rec); err != nil {
		t.Fatalf("UpsertFromATS() failed: %v", err)
	}
	if _, err := store.MarkMissingAsRemoved(ctx, targetID, nil); err != nil {
		t.Fatalf("MarkMissingAsRemoved() failed: %v", err)
	}

	result, err := store.List(ctx, Filter{PageSize: 10})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if result.Total != 0 {
		t.Fatalf("List() after removal = %+v, want 0 (removed jobs are never public)", result)
	}

	if _, err := store.UpsertFromATS(ctx, rec); err != nil {
		t.Fatalf("re-UpsertFromATS() failed: %v", err)
	}
	result, err = store.List(ctx, Filter{PageSize: 10})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if result.Total != 1 {
		t.Fatalf("List() after re-appearing = %+v, want 1 (reopened)", result)
	}
}

func TestStoreMarkMissingAsRemoved_ClosesOnlyJobsNotInSeenList(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)

	if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, "keep")); err != nil {
		t.Fatalf("seeding 'keep' failed: %v", err)
	}
	if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, "gone")); err != nil {
		t.Fatalf("seeding 'gone' failed: %v", err)
	}

	removed, err := store.MarkMissingAsRemoved(ctx, targetID, []string{"keep"})
	if err != nil {
		t.Fatalf("MarkMissingAsRemoved() failed: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	var keepStatus, goneStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE source_job_id = 'keep'`).Scan(&keepStatus); err != nil {
		t.Fatalf("reading 'keep' status: %v", err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE source_job_id = 'gone'`).Scan(&goneStatus); err != nil {
		t.Fatalf("reading 'gone' status: %v", err)
	}
	if keepStatus != "open" {
		t.Errorf(`"keep" status = %q, want "open"`, keepStatus)
	}
	if goneStatus != "removed" {
		t.Errorf(`"gone" status = %q, want "removed"`, goneStatus)
	}
}

func TestStoreMarkMissingAsRemoved_NeverAffectsAnotherTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetA, companyA := seedTarget(t, ctx, db, "Acme", "acme")
	targetB, companyB := seedTarget(t, ctx, db, "Globex", "globex")
	store := NewStore(db.Pool)

	if _, err := store.UpsertFromATS(ctx, baseRecord(targetA, companyA, "a-job")); err != nil {
		t.Fatalf("seeding target A's job failed: %v", err)
	}
	if _, err := store.UpsertFromATS(ctx, baseRecord(targetB, companyB, "b-job")); err != nil {
		t.Fatalf("seeding target B's job failed: %v", err)
	}

	// Mark everything missing for target A (empty seen list) -- target
	// B's job, under a completely different target_company_id, must
	// survive untouched.
	if _, err := store.MarkMissingAsRemoved(ctx, targetA, nil); err != nil {
		t.Fatalf("MarkMissingAsRemoved() failed: %v", err)
	}

	var bStatus string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE source_job_id = 'b-job'`).Scan(&bStatus); err != nil {
		t.Fatalf("reading target B's job status: %v", err)
	}
	if bStatus != "open" {
		t.Errorf("target B's job status = %q after marking target A's missing jobs removed, want unaffected \"open\"", bStatus)
	}
}

func TestStoreList_FiltersAndOnlyReturnsOpenJobs(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)

	if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, "open-1")); err != nil {
		t.Fatalf("seeding failed: %v", err)
	}
	if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, "removed-1")); err != nil {
		t.Fatalf("seeding failed: %v", err)
	}
	if _, err := store.MarkMissingAsRemoved(ctx, targetID, []string{"open-1"}); err != nil {
		t.Fatalf("MarkMissingAsRemoved() failed: %v", err)
	}

	result, err := store.List(ctx, Filter{Company: "acme", PageSize: 10})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if result.Total != 1 {
		t.Fatalf("List() = %+v, want exactly the 1 open job (removed-1 excluded)", result)
	}
}

func TestStoreList_TagFilterAlwaysReturnsEmpty(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, "1001")); err != nil {
		t.Fatalf("seeding failed: %v", err)
	}

	// No tags column exists on jobs yet -- a tag filter must match
	// nothing, deterministically, rather than silently ignoring the
	// filter and returning every job.
	result, err := store.List(ctx, Filter{Tag: "go", PageSize: 10})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if result.Total != 0 || len(result.Jobs) != 0 {
		t.Errorf("List() with a tag filter = %+v, want zero results", result)
	}
}

func TestStoreList_Paginates(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	for i := 0; i < 5; i++ {
		rec := baseRecord(targetID, companyID, string(rune('a'+i)))
		if _, err := store.UpsertFromATS(ctx, rec); err != nil {
			t.Fatalf("seeding job %d failed: %v", i, err)
		}
	}

	page1, err := store.List(ctx, Filter{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("List() page 1 failed: %v", err)
	}
	if len(page1.Jobs) != 2 || page1.Total != 5 {
		t.Errorf("page 1 = %d jobs, total %d; want 2 jobs, total 5", len(page1.Jobs), page1.Total)
	}

	page3, err := store.List(ctx, Filter{Page: 3, PageSize: 2})
	if err != nil {
		t.Fatalf("List() page 3 failed: %v", err)
	}
	if len(page3.Jobs) != 1 {
		t.Errorf("page 3 (job 5 of 5) = %d jobs, want 1", len(page3.Jobs))
	}
}

func TestStoreGet_ReturnsErrNotFoundForNonNumericID(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := NewStore(db.Pool)

	_, err := store.Get(ctx, "not-a-number")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestStoreGet_ReturnsErrNotFoundForARemovedJob(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, "1001")); err != nil {
		t.Fatalf("seeding failed: %v", err)
	}
	id := mustJobID(t, ctx, db, "1001")

	if _, err := store.MarkMissingAsRemoved(ctx, targetID, nil); err != nil {
		t.Fatalf("MarkMissingAsRemoved() failed: %v", err)
	}

	_, err := store.Get(ctx, id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() on a removed job: err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestStoreGet_ReturnsFullDetailIncludingDescription(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, "1001")); err != nil {
		t.Fatalf("seeding failed: %v", err)
	}

	got, err := store.Get(ctx, mustJobID(t, ctx, db, "1001"))
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if got.Description != "Build things." {
		t.Errorf("Description = %q, want the seeded description", got.Description)
	}
	if got.Tags == nil || len(got.Tags) != 0 {
		t.Errorf("Tags = %v, want a non-nil empty slice (no tags column exists)", got.Tags)
	}
}

// Mirrors internal/company's TestStoreUpsert_ConcurrentCallersConvergeOnOneRow:
// several concurrent UpsertFromATS calls for the same identity must
// converge on exactly one row, proving ON CONFLICT is doing the work a
// check-then-insert could not (no unique-violation panics/errors from
// any caller).
func TestStoreUpsertFromATS_ConcurrentCallersConvergeOnOneRow(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	rec := baseRecord(targetID, companyID, "contested")

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.UpsertFromATS(ctx, rec)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: UpsertFromATS() failed: %v", i, err)
		}
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE source_job_id = 'contested'`).Scan(&count); err != nil {
		t.Fatalf("counting jobs: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want exactly 1 row after %d concurrent upserts", count, n)
	}
}

// Regression (adversarial review, S1): the database refuses some data
// (NUL bytes, blank titles); IsRejectedRecord must classify those as
// per-job rejections so ingestion can skip them, while a real
// infrastructure failure is not classified that way.
func TestIsRejectedRecord_ClassifiesDataErrorsButNotInfrastructureOnes(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)

	nul := baseRecord(targetID, companyID, "nul")
	nul.Title = "Eng\x00ineer"
	blank := baseRecord(targetID, companyID, "blank")
	blank.Title = "   "
	for name, rec := range map[string]Record{"NUL byte": nul, "blank title": blank} {
		_, err := store.UpsertFromATS(ctx, rec)
		if err == nil {
			t.Fatalf("%s: UpsertFromATS() = nil, want the database to refuse it", name)
		}
		if !IsRejectedRecord(err) {
			t.Errorf("%s: IsRejectedRecord(%v) = false, want true", name, err)
		}
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := store.UpsertFromATS(canceled, baseRecord(targetID, companyID, "ok"))
	if err == nil || IsRejectedRecord(err) {
		t.Errorf("canceled-context error %v classified as rejected record, want it treated as infrastructure", err)
	}
	if IsRejectedRecord(errors.New("connection reset by peer")) {
		t.Error("a plain non-database error must not be classified as a rejected record")
	}
}

// Regression (adversarial review, N4): a removed job that reappears with
// identical content is a real state change, not "unchanged".
func TestStoreUpsertFromATS_ReopenedJobCountsAsChanged(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	rec := baseRecord(targetID, companyID, "1001")

	if _, err := store.UpsertFromATS(ctx, rec); err != nil {
		t.Fatalf("UpsertFromATS() failed: %v", err)
	}
	if _, err := store.MarkMissingAsRemoved(ctx, targetID, nil); err != nil {
		t.Fatalf("MarkMissingAsRemoved() failed: %v", err)
	}
	outcome, err := store.UpsertFromATS(ctx, rec)
	if err != nil {
		t.Fatalf("re-UpsertFromATS() failed: %v", err)
	}
	if outcome.Inserted || !outcome.Changed {
		t.Errorf("outcome = %+v, want Inserted=false Changed=true for a reopened job", outcome)
	}
}

// Regression (adversarial review, N2): "%" and "_" in the search text
// match literally, not as LIKE wildcards.
func TestStoreList_QueryMatchesLiteralPercentAndUnderscore(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	for id, title := range map[string]string{"1": "100% Remote Engineer", "2": "Backend Engineer", "3": "Job_ID Analyst"} {
		rec := baseRecord(targetID, companyID, id)
		rec.Title = title
		if _, err := store.UpsertFromATS(ctx, rec); err != nil {
			t.Fatalf("seeding %q failed: %v", title, err)
		}
	}

	for q, want := range map[string]int{"%": 1, "_": 1, "100%": 1, "Job_ID": 1, `\`: 0, "engineer": 2} {
		result, err := store.List(ctx, Filter{Query: q, PageSize: 10})
		if err != nil {
			t.Fatalf("List(q=%q) failed: %v", q, err)
		}
		if result.Total != want {
			t.Errorf("List(q=%q) total = %d, want %d", q, result.Total, want)
		}
	}
}

// Regression (adversarial review, N3): a page past the last one must
// still report the real total (the API returns it, and the frontend
// self-corrects an out-of-range page from it).
func TestStoreList_OutOfRangePageStillReportsTotal(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, id)); err != nil {
			t.Fatalf("seeding failed: %v", err)
		}
	}

	result, err := store.List(ctx, Filter{Page: 9, PageSize: 2})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(result.Jobs) != 0 || result.Total != 3 {
		t.Errorf("List(page 9) = %d jobs, total %d; want 0 jobs, total 3", len(result.Jobs), result.Total)
	}
}

// Regression (round-2 review): an absurd page number overflowed
// (page-1)*pageSize into a negative OFFSET, which Postgres rejects — a
// 500 from a public endpoint. It must behave like any past-the-end page.
func TestStoreList_HugePageNumberIsPastTheEndNotAnError(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, id)); err != nil {
			t.Fatalf("seeding failed: %v", err)
		}
	}

	result, err := store.List(ctx, Filter{Page: 1 << 62, PageSize: 100, Company: "acme"})
	if err != nil {
		t.Fatalf("List(page=1<<62) failed: %v, want an empty past-the-end result", err)
	}
	if len(result.Jobs) != 0 || result.Total != 3 {
		t.Errorf("List(huge page) = %d jobs, total %d; want 0 jobs, total 3", len(result.Jobs), result.Total)
	}
}

// The total reported for a past-the-end page must respect filters too
// (the count query reuses the same WHERE and parameter numbering).
func TestStoreList_OutOfRangePageTotalRespectsFilters(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	acmeTarget, acmeCompany := seedTarget(t, ctx, db, "Acme", "acme")
	globexTarget, globexCompany := seedTarget(t, ctx, db, "Globex", "globex")
	store := NewStore(db.Pool)
	for _, id := range []string{"a1", "a2", "a3"} {
		if _, err := store.UpsertFromATS(ctx, baseRecord(acmeTarget, acmeCompany, id)); err != nil {
			t.Fatalf("seeding failed: %v", err)
		}
	}
	if _, err := store.UpsertFromATS(ctx, baseRecord(globexTarget, globexCompany, "g1")); err != nil {
		t.Fatalf("seeding failed: %v", err)
	}

	result, err := store.List(ctx, Filter{Company: "acme", Query: "engineer", RemoteType: RemoteTypeUnknown,
		EmploymentType: EmploymentTypeUnknown, Page: 5, PageSize: 2})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(result.Jobs) != 0 || result.Total != 3 {
		t.Errorf("filtered past-the-end = %d jobs, total %d; want 0 jobs, total 3 (Acme only)", len(result.Jobs), result.Total)
	}
}

// A foreign-key violation means the target/company linkage is wrong for
// the whole board, so it must abort the run, not be skipped job by job.
func TestIsRejectedRecord_ForeignKeyViolationIsNotASkippableRejection(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := NewStore(db.Pool)

	_, err := store.UpsertFromATS(ctx, baseRecord(999999, 999999, "orphan"))
	if err == nil {
		t.Fatal("UpsertFromATS() with a nonexistent target = nil, want a foreign-key error")
	}
	if IsRejectedRecord(err) {
		t.Errorf("IsRejectedRecord(%v) = true, want false for a foreign-key violation", err)
	}
}

func mustJobID(t *testing.T, ctx context.Context, db *database.DB, sourceJobID string) string {
	t.Helper()
	var id int64
	if err := db.Pool.QueryRow(ctx, `SELECT id FROM jobs WHERE source_job_id = $1`, sourceJobID).Scan(&id); err != nil {
		t.Fatalf("looking up job id for source_job_id=%q: %v", sourceJobID, err)
	}
	return strconv.FormatInt(id, 10)
}
