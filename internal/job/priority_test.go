package job

import (
	"context"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
)

// seedPriorityJobs creates one priority company ("Kifiya") with two OLD
// jobs and one regular company ("Acme") with two NEW jobs, so any order
// that is not "priority first" is visibly a date order instead.
func seedPriorityJobs(t *testing.T, ctx context.Context, db *database.DB) *Store {
	t.Helper()
	store := NewStore(db.Pool)

	kifiyaTarget, kifiyaCompany := seedTarget(t, ctx, db, "Kifiya", "kifiya")
	if err := company.NewStore(db.Pool).SetPriority(ctx, kifiyaCompany, true); err != nil {
		t.Fatalf("SetPriority() failed: %v", err)
	}
	acmeTarget, acmeCompany := seedTarget(t, ctx, db, "Acme", "acme")

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range []Record{
		withPublished(baseRecord(kifiyaTarget, kifiyaCompany, "k-old-1"), old),
		withPublished(baseRecord(kifiyaTarget, kifiyaCompany, "k-old-2"), old.Add(time.Hour)),
		withPublished(baseRecord(acmeTarget, acmeCompany, "a-new-1"), recent),
		withPublished(baseRecord(acmeTarget, acmeCompany, "a-new-2"), recent.Add(time.Hour)),
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

func TestStoreList_PinsPriorityCompaniesFirst(t *testing.T) {
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
	wantCompanies := []string{"Kifiya", "Kifiya", "Acme", "Acme"}
	wantPriority := []bool{true, true, false, false}
	for i, j := range res.Jobs {
		if j.CompanyName != wantCompanies[i] || j.IsPriority != wantPriority[i] {
			t.Errorf("job[%d] = %s priority=%t, want %s priority=%t (priority pinned first, older or not)",
				i, j.CompanyName, j.IsPriority, wantCompanies[i], wantPriority[i])
		}
	}
	// Within the priority group: newest first.
	if !res.Jobs[0].PostedAt.After(res.Jobs[1].PostedAt) {
		t.Errorf("priority group not newest-first: %v then %v", res.Jobs[0].PostedAt, res.Jobs[1].PostedAt)
	}
	// Within the regular group: newest first.
	if !res.Jobs[2].PostedAt.After(res.Jobs[3].PostedAt) {
		t.Errorf("regular group not newest-first: %v then %v", res.Jobs[2].PostedAt, res.Jobs[3].PostedAt)
	}
}

// The pin must hold across page boundaries: page 1 of size 2 is exactly
// the priority group, page 2 the rest, with a stable total.
func TestStoreList_PriorityPinHoldsAcrossPages(t *testing.T) {
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
	if !page1.Jobs[0].IsPriority || !page1.Jobs[1].IsPriority || page1.Jobs[2].IsPriority {
		t.Errorf("page 1 priority flags = %t %t %t, want true true false",
			page1.Jobs[0].IsPriority, page1.Jobs[1].IsPriority, page1.Jobs[2].IsPriority)
	}
	if page2.Jobs[0].IsPriority {
		t.Errorf("page 2 job is priority; every priority job must already be on page 1")
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

func TestStoreCloseStale_RejectsNonPositiveWindow(t *testing.T) {
	db := newDB(t)
	store := NewStore(db.Pool)
	for _, d := range []time.Duration{0, -time.Hour} {
		if _, err := store.CloseStale(context.Background(), 1, d); err == nil {
			t.Errorf("CloseStale(window=%s) succeeded, want an error (a zero window would close every job)", d)
		}
	}
}

func TestMockRepository_PinsAndFiltersPriority(t *testing.T) {
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
	if got := all.Jobs[0].ID; got != "2" {
		t.Errorf("first job = %s, want the priority job 2 pinned above newer regular jobs", got)
	}
	if all.Jobs[1].ID != "1" || all.Jobs[2].ID != "3" {
		t.Errorf("regular jobs order = %s, %s, want 1, 3 (newest first)", all.Jobs[1].ID, all.Jobs[2].ID)
	}

	only, err := m.List(context.Background(), Filter{PriorityOnly: true})
	if err != nil {
		t.Fatalf("List(PriorityOnly) failed: %v", err)
	}
	if only.Total != 1 || only.Jobs[0].ID != "2" {
		t.Errorf("PriorityOnly = %+v, want just job 2", only)
	}
}
