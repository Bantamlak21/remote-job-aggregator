package job

import (
	"context"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
)

// seedPriorityJobs creates a priority company ("Kifiya") and a regular one
// ("Acme") whose jobs INTERLEAVE in time, newest first:
//
//	a-new-1 (Acme, Sep 22) > k-new-1 (Kifiya, Sep 20) > a-mid-1 (Acme, Sep 10) > k-old-1 (Kifiya, Jan 1)
//
// so a list sorted by recency alternates companies, and any grouping by
// priority is visible as a break in that order.
func seedPriorityJobs(t *testing.T, ctx context.Context, db *database.DB) *Store {
	t.Helper()
	store := NewStore(db.Pool)

	kifiyaTarget, kifiyaCompany := seedTarget(t, ctx, db, "Kifiya", "kifiya")
	if err := company.NewStore(db.Pool).SetPriority(ctx, kifiyaCompany, true); err != nil {
		t.Fatalf("SetPriority() failed: %v", err)
	}
	acmeTarget, acmeCompany := seedTarget(t, ctx, db, "Acme", "acme")

	for _, r := range []Record{
		withPublished(baseRecord(kifiyaTarget, kifiyaCompany, "k-old-1"), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		withPublished(baseRecord(kifiyaTarget, kifiyaCompany, "k-new-1"), time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)),
		withPublished(baseRecord(acmeTarget, acmeCompany, "a-mid-1"), time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)),
		withPublished(baseRecord(acmeTarget, acmeCompany, "a-new-1"), time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s failed: %v", r.SourceJobID, err)
		}
	}
	return store
}

func withPublished(r Record, at time.Time) Record {
	r.PublishedAt = at
	return r
}

// Strictly recency: a priority company's job gets no position of its own.
// The newest job overall is Acme's (regular), and the priority company's
// OLD job comes last, below a regular job that is newer than it.
func TestStoreList_SortsByRecencyRegardlessOfPriority(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := seedPriorityJobs(t, ctx, db)

	res, err := store.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if res.Total != 4 || len(res.Jobs) != 4 {
		t.Fatalf("List() total=%d len=%d, want 4/4", res.Total, len(res.Jobs))
	}
	wantCompanies := []string{"Acme", "Kifiya", "Acme", "Kifiya"}
	wantPriority := []bool{false, true, false, true}
	for i, j := range res.Jobs {
		if j.CompanyName != wantCompanies[i] || j.IsPriority != wantPriority[i] {
			t.Errorf("job[%d] = %s priority=%t, want %s priority=%t (pure newest-first)",
				i, j.CompanyName, j.IsPriority, wantCompanies[i], wantPriority[i])
		}
	}
	for i := 1; i < len(res.Jobs); i++ {
		if res.Jobs[i-1].PostedAt.Before(res.Jobs[i].PostedAt) {
			t.Errorf("job[%d] (%v) is older than job[%d] (%v); list is not newest-first",
				i-1, res.Jobs[i-1].PostedAt, i, res.Jobs[i].PostedAt)
		}
	}
}

// Equal publish dates are ordered by id descending (newest row first), so
// the order is deterministic and pagination cannot skip or repeat a job.
// Jobs from a priority and a regular company tie here on purpose.
func TestStoreList_EqualDatesAreOrderedByIDDescending(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := NewStore(db.Pool)

	kifiyaTarget, kifiyaCompany := seedTarget(t, ctx, db, "Kifiya", "kifiya")
	if err := company.NewStore(db.Pool).SetPriority(ctx, kifiyaCompany, true); err != nil {
		t.Fatalf("SetPriority() failed: %v", err)
	}
	acmeTarget, acmeCompany := seedTarget(t, ctx, db, "Acme", "acme")

	same := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	// Insert order (= id order): first, second, third. The priority job is in the middle.
	for _, r := range []Record{
		withPublished(baseRecord(acmeTarget, acmeCompany, "first"), same),
		withPublished(baseRecord(kifiyaTarget, kifiyaCompany, "second"), same),
		withPublished(baseRecord(acmeTarget, acmeCompany, "third"), same),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s failed: %v", r.SourceJobID, err)
		}
	}

	var got []string
	for page := 1; page <= 3; page++ { // one per page: also proves pagination is stable
		res, err := store.List(ctx, Filter{Page: page, PageSize: 1})
		if err != nil {
			t.Fatalf("List() page %d failed: %v", page, err)
		}
		if len(res.Jobs) != 1 {
			t.Fatalf("page %d has %d jobs, want 1", page, len(res.Jobs))
		}
		got = append(got, res.Jobs[0].ID)
	}
	want := []string{
		mustJobID(t, ctx, db, "third"), mustJobID(t, ctx, db, "second"), mustJobID(t, ctx, db, "first"),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids by page = %v, want %v (highest id first; priority gets no position)", got, want)
		}
	}
}

// The recency order holds across page boundaries.
func TestStoreList_RecencyOrderHoldsAcrossPages(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := seedPriorityJobs(t, ctx, db)

	page1, err := store.List(ctx, Filter{Page: 1, PageSize: 3})
	if err != nil {
		t.Fatalf("List() page 1 failed: %v", err)
	}
	page2, err := store.List(ctx, Filter{Page: 2, PageSize: 3})
	if err != nil {
		t.Fatalf("List() page 2 failed: %v", err)
	}
	if page1.Total != 4 || page2.Total != 4 {
		t.Errorf("totals = %d, %d, want 4, 4", page1.Total, page2.Total)
	}
	if len(page1.Jobs) != 3 || len(page2.Jobs) != 1 {
		t.Fatalf("page sizes = %d, %d, want 3, 1", len(page1.Jobs), len(page2.Jobs))
	}
	if page1.Jobs[0].CompanyName != "Acme" || page1.Jobs[1].CompanyName != "Kifiya" || page1.Jobs[2].CompanyName != "Acme" {
		t.Errorf("page 1 companies = %s %s %s, want Acme Kifiya Acme",
			page1.Jobs[0].CompanyName, page1.Jobs[1].CompanyName, page1.Jobs[2].CompanyName)
	}
	if page2.Jobs[0].CompanyName != "Kifiya" || !page2.Jobs[0].IsPriority {
		t.Errorf("page 2 job = %s priority=%t, want Kifiya's old job (still badged, just old)",
			page2.Jobs[0].CompanyName, page2.Jobs[0].IsPriority)
	}
}

func TestStoreList_PriorityOnlyFilter(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := seedPriorityJobs(t, ctx, db)

	res, err := store.List(ctx, Filter{PriorityOnly: true})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if res.Total != 2 || len(res.Jobs) != 2 {
		t.Fatalf("List(PriorityOnly) total=%d len=%d, want 2/2", res.Total, len(res.Jobs))
	}
	for _, j := range res.Jobs {
		if j.CompanyName != "Kifiya" || !j.IsPriority {
			t.Errorf("PriorityOnly returned %s priority=%t", j.CompanyName, j.IsPriority)
		}
	}

	// The out-of-range-page total fallback must respect the filter too.
	past, err := store.List(ctx, Filter{PriorityOnly: true, Page: 5, PageSize: 10})
	if err != nil {
		t.Fatalf("List() past the end failed: %v", err)
	}
	if past.Total != 2 || len(past.Jobs) != 0 {
		t.Errorf("past-the-end PriorityOnly total=%d len=%d, want 2/0", past.Total, len(past.Jobs))
	}
}

func TestStoreGet_ReportsPriority(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := seedPriorityJobs(t, ctx, db)

	priority, err := store.Get(ctx, mustJobID(t, ctx, db, "k-old-1"))
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if !priority.IsPriority {
		t.Errorf("Get() of a priority company's job: IsPriority = false")
	}
	regular, err := store.Get(ctx, mustJobID(t, ctx, db, "a-new-1"))
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if regular.IsPriority {
		t.Errorf("Get() of a regular company's job: IsPriority = true")
	}
}

func TestStoreCloseStale_ClosesOnlyOldOpenJobsOfThatTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	otherTarget, otherCompany := seedTarget(t, ctx, db, "Other", "other")
	store := NewStore(db.Pool)

	for _, r := range []Record{
		baseRecord(targetID, companyID, "fresh"),
		baseRecord(targetID, companyID, "stale"),
		baseRecord(otherTarget, otherCompany, "other-stale"),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s failed: %v", r.SourceJobID, err)
		}
	}
	// Age two jobs by rewriting first_seen_at and last_seen_at together
	// (the CHECK forbids last_seen_at < first_seen_at).
	if _, err := db.Pool.Exec(ctx,
		`UPDATE jobs SET first_seen_at = now() - interval '40 days', last_seen_at = now() - interval '30 days'
		 WHERE source_job_id IN ('stale', 'other-stale')`); err != nil {
		t.Fatalf("aging jobs: %v", err)
	}

	closed, err := store.CloseStale(ctx, targetID, 21*24*time.Hour)
	if err != nil {
		t.Fatalf("CloseStale() failed: %v", err)
	}
	if closed != 1 {
		t.Errorf("CloseStale() closed %d, want 1 (only this target's stale job)", closed)
	}

	status := func(id string) string {
		t.Helper()
		var s string
		if err := db.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE source_job_id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("reading status of %s: %v", id, err)
		}
		return s
	}
	if got := status("fresh"); got != "open" {
		t.Errorf("fresh job status = %q, want open", got)
	}
	if got := status("stale"); got != "removed" {
		t.Errorf("stale job status = %q, want removed", got)
	}
	if got := status("other-stale"); got != "open" {
		t.Errorf("another target's stale job status = %q, want open (untouched)", got)
	}

	// A stale job that is seen again reopens through the normal upsert.
	if _, err := store.UpsertFromATS(ctx, baseRecord(targetID, companyID, "stale")); err != nil {
		t.Fatalf("re-upsert failed: %v", err)
	}
	if got := status("stale"); got != "open" {
		t.Errorf("re-seen job status = %q, want open", got)
	}
}

func TestStoreCloseBySourceID_ClosesOnlyThoseOpenJobsOfThatTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	otherTarget, otherCompany := seedTarget(t, ctx, db, "Other", "other")
	store := NewStore(db.Pool)

	for _, r := range []Record{
		baseRecord(targetID, companyID, "expired"),
		baseRecord(targetID, companyID, "live"),
		baseRecord(otherTarget, otherCompany, "expired-elsewhere"),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s failed: %v", r.SourceJobID, err)
		}
	}

	// "never-stored" is an id the target never had: it must be ignored, not an error.
	n, err := store.CloseBySourceID(ctx, targetID, []string{"expired", "never-stored", "expired-elsewhere"})
	if err != nil {
		t.Fatalf("CloseBySourceID() failed: %v", err)
	}
	if n != 1 {
		t.Errorf("closed %d, want 1 (only this target's 'expired')", n)
	}

	status := func(id string) string {
		t.Helper()
		var s string
		if err := db.Pool.QueryRow(ctx, `SELECT status FROM jobs WHERE source_job_id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("reading status of %s: %v", id, err)
		}
		return s
	}
	if status("expired") != "removed" || status("live") != "open" || status("expired-elsewhere") != "open" {
		t.Errorf("statuses: expired=%s live=%s expired-elsewhere=%s", status("expired"), status("live"), status("expired-elsewhere"))
	}

	// Closing an already-closed job is a no-op, and an empty list touches nothing.
	if n, err := store.CloseBySourceID(ctx, targetID, []string{"expired"}); err != nil || n != 0 {
		t.Errorf("second close = %d, %v; want 0, nil", n, err)
	}
	if n, err := store.CloseBySourceID(ctx, targetID, nil); err != nil || n != 0 {
		t.Errorf("empty close = %d, %v; want 0, nil", n, err)
	}
}

// A corrected publish date must replace a wrong stored one (LinkedIn jobs
// were first stored with Google's crawl date), while an absent date must
// never erase a known one.
func TestStoreUpsert_NewPublishDateReplacesTheStoredOneButAbsentKeepsIt(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)

	published := func() time.Time {
		t.Helper()
		var p time.Time
		if err := db.Pool.QueryRow(ctx, `SELECT published_at FROM jobs WHERE source_job_id = 'j'`).Scan(&p); err != nil {
			t.Fatalf("reading published_at: %v", err)
		}
		return p
	}

	wrong := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	right := time.Date(2025, 3, 2, 0, 0, 0, 0, time.UTC)

	r := baseRecord(targetID, companyID, "j")
	r.PublishedAt = wrong
	if _, err := store.UpsertFromATS(ctx, r); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	r.PublishedAt = right
	if _, err := store.UpsertFromATS(ctx, r); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got := published(); !got.Equal(right) {
		t.Errorf("published_at = %v, want the newly supplied %v", got, right)
	}

	r.PublishedAt = time.Time{}
	if _, err := store.UpsertFromATS(ctx, r); err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	if got := published(); !got.Equal(right) {
		t.Errorf("published_at = %v after an upsert with no date, want %v kept", got, right)
	}
}

func TestStoreCloseStale_RejectsNonPositiveWindow(t *testing.T) {
	db := newDB(t)
	store := NewStore(db.Pool)
	for _, d := range []time.Duration{0, -time.Hour} {
		if _, err := store.CloseStale(context.Background(), 1, d); err == nil {
			t.Errorf("CloseStale(window=%s) succeeded, want an error (a zero window would close every job)", d)
		}
	}
}

func TestMockRepository_SortsByRecencyAndFiltersPriority(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	m := &MockRepository{jobs: []Job{
		{ID: "1", Title: "New regular", CompanyName: "Acme", PostedAt: base.Add(48 * time.Hour)},
		{ID: "2", Title: "Old priority", CompanyName: "Kifiya", IsPriority: true, PostedAt: base},
		{ID: "3", Title: "Mid regular", CompanyName: "Acme", PostedAt: base.Add(24 * time.Hour)},
	}}

	all, err := m.List(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if all.Jobs[0].ID != "1" || all.Jobs[1].ID != "3" || all.Jobs[2].ID != "2" {
		t.Errorf("order = %s, %s, %s, want 1, 3, 2 (pure newest-first: the old priority job is last)",
			all.Jobs[0].ID, all.Jobs[1].ID, all.Jobs[2].ID)
	}

	only, err := m.List(context.Background(), Filter{PriorityOnly: true})
	if err != nil {
		t.Fatalf("List(PriorityOnly) failed: %v", err)
	}
	if only.Total != 1 || only.Jobs[0].ID != "2" {
		t.Errorf("PriorityOnly = %+v, want just job 2", only)
	}
}
