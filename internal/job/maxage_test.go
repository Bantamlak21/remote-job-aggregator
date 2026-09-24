package job

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
)

// seedAgedJobs stores jobs with known ages relative to the database clock:
//
//	fresh   published 3 days ago
//	edge    published 14 days ago
//	just-over published 16 days ago (one day past the 15-day limit)
//	stale   published 20 days ago
//	ancient published 400 days ago
//	undated no published_at, first seen just now (so it counts as brand new)
//	old-seen no published_at, but first seen 30 days ago
func seedAgedJobs(t *testing.T, ctx context.Context, db *database.DB) *Store {
	t.Helper()
	store := NewStore(db.Pool)
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")

	now := time.Now()
	for _, r := range []Record{
		withPublished(baseRecord(targetID, companyID, "fresh"), now.Add(-3*24*time.Hour)),
		withPublished(baseRecord(targetID, companyID, "edge"), now.Add(-14*24*time.Hour)),
		withPublished(baseRecord(targetID, companyID, "just-over"), now.Add(-16*24*time.Hour)),
		withPublished(baseRecord(targetID, companyID, "stale"), now.Add(-20*24*time.Hour)),
		withPublished(baseRecord(targetID, companyID, "ancient"), now.Add(-400*24*time.Hour)),
		withPublished(baseRecord(targetID, companyID, "undated"), time.Time{}),
		withPublished(baseRecord(targetID, companyID, "old-seen"), time.Time{}),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s failed: %v", r.SourceJobID, err)
		}
	}
	// first_seen_at can only move earlier if last_seen_at does not precede it.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE jobs SET first_seen_at = now() - interval '30 days', last_seen_at = now() - interval '30 days'
		 WHERE source_job_id = 'old-seen'`); err != nil {
		t.Fatalf("aging old-seen: %v", err)
	}
	return store
}

func sourceIDs(t *testing.T, ctx context.Context, db *database.DB, jobs []Job) map[string]bool {
	t.Helper()
	byID := map[string]string{}
	rows, err := db.Pool.Query(ctx, `SELECT id::text, source_job_id FROM jobs`)
	if err != nil {
		t.Fatalf("reading ids: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, src string
		if err := rows.Scan(&id, &src); err != nil {
			t.Fatalf("scanning ids: %v", err)
		}
		byID[id] = src
	}
	out := map[string]bool{}
	for _, j := range jobs {
		out[byID[j.ID]] = true
	}
	return out
}

func TestStoreWithMaxAge_ListHidesJobsPostedTooLongAgo(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := seedAgedJobs(t, ctx, db).WithMaxAge(15 * 24 * time.Hour)

	res, err := store.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	got := sourceIDs(t, ctx, db, res.Jobs)
	want := map[string]bool{"fresh": true, "edge": true, "undated": true}
	if len(got) != len(want) || res.Total != len(want) {
		t.Fatalf("visible = %v total=%d, want exactly %v (just-over 16d, stale 20d, ancient 400d and old-seen 30d hidden)", got, res.Total, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("job %q should be visible; visible = %v", id, got)
		}
	}
}

func TestStoreWithMaxAge_ZeroMeansNoLimitAndNegativeIsTreatedAsNone(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	base := seedAgedJobs(t, ctx, db)

	for _, store := range []*Store{base, base.WithMaxAge(0), base.WithMaxAge(-time.Hour)} {
		res, err := store.List(ctx, Filter{})
		if err != nil {
			t.Fatalf("List() failed: %v", err)
		}
		if res.Total != 7 {
			t.Errorf("total = %d, want all 7 jobs when there is no age limit", res.Total)
		}
	}
}

// WithMaxAge returns a copy: the original keeps serving everything.
func TestStoreWithMaxAge_DoesNotMutateTheReceiver(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	base := seedAgedJobs(t, ctx, db)
	_ = base.WithMaxAge(15 * 24 * time.Hour)

	res, err := base.List(ctx, Filter{})
	if err != nil || res.Total != 7 {
		t.Errorf("original store total = %d, %v; want 7 (WithMaxAge must not change its receiver)", res.Total, err)
	}
}

func TestStoreWithMaxAge_TotalAndPaginationAgreeWithTheLimit(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := seedAgedJobs(t, ctx, db).WithMaxAge(15 * 24 * time.Hour)

	page1, err := store.List(ctx, Filter{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("List() page 1 failed: %v", err)
	}
	page2, err := store.List(ctx, Filter{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("List() page 2 failed: %v", err)
	}
	if page1.Total != 3 || page2.Total != 3 || len(page1.Jobs) != 2 || len(page2.Jobs) != 1 {
		t.Errorf("page1 total=%d len=%d, page2 total=%d len=%d; want 3/2 and 3/1", page1.Total, len(page1.Jobs), page2.Total, len(page2.Jobs))
	}
	// Past the end: the fallback count query must apply the same limit.
	past, err := store.List(ctx, Filter{Page: 9, PageSize: 2})
	if err != nil {
		t.Fatalf("List() past the end failed: %v", err)
	}
	if past.Total != 3 || len(past.Jobs) != 0 {
		t.Errorf("past-the-end total=%d len=%d, want 3/0 (the count must respect the age limit)", past.Total, len(past.Jobs))
	}
	huge, err := store.List(ctx, Filter{Page: 1 << 50, PageSize: 100})
	if err != nil {
		t.Fatalf("List() with a huge page failed: %v", err)
	}
	if huge.Total != 3 {
		t.Errorf("huge-page total = %d, want 3", huge.Total)
	}
}

func TestStoreWithMaxAge_CombinesWithOtherFilters(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := seedAgedJobs(t, ctx, db).WithMaxAge(15 * 24 * time.Hour)

	// A text query matching every job's title still only sees the young ones.
	res, err := store.List(ctx, Filter{Query: "Backend"})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if res.Total != 3 {
		t.Errorf("query total = %d, want 3", res.Total)
	}
	res, err = store.List(ctx, Filter{Company: "Acme", RemoteType: RemoteTypeUnknown})
	if err != nil || res.Total != 3 {
		t.Errorf("company+remote_type total = %d, %v; want 3", res.Total, err)
	}
}

func TestStoreWithMaxAge_GetHidesOldJobsToo(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := seedAgedJobs(t, ctx, db)
	limited := store.WithMaxAge(15 * 24 * time.Hour)

	for src, visible := range map[string]bool{"fresh": true, "edge": true, "undated": true, "just-over": false, "stale": false, "ancient": false, "old-seen": false} {
		id := mustJobID(t, ctx, db, src)
		_, err := limited.Get(ctx, id)
		if visible && err != nil {
			t.Errorf("Get(%s) = %v, want it visible", src, err)
		}
		if !visible && err == nil {
			t.Errorf("Get(%s) succeeded, want ErrNotFound for a job past the age limit", src)
		}
		if !visible && err != nil && !isNotFound(err) {
			t.Errorf("Get(%s) error = %v, want ErrNotFound", src, err)
		}
		// Without the limit everything is still there.
		if _, err := store.Get(ctx, id); err != nil {
			t.Errorf("unlimited Get(%s) = %v", src, err)
		}
	}
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
