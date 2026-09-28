package job

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestStoreRecentlyOpened is the scheduler's "what's new this tick" query
// (internal/notification). It must return only jobs first seen at or after
// the cutoff, and only open ones -- a job that closed the moment it was
// found is not something to notify anyone about.
func TestStoreRecentlyOpened(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	store := NewStore(db.Pool)
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")

	for _, r := range []Record{
		baseRecord(targetID, companyID, "fresh-open"),
		baseRecord(targetID, companyID, "old-open"),
		baseRecord(targetID, companyID, "fresh-closed"),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s failed: %v", r.SourceJobID, err)
		}
	}
	if _, err := db.Pool.Exec(ctx,
		`UPDATE jobs SET first_seen_at = now() - interval '2 hours', last_seen_at = now() - interval '2 hours'
		 WHERE source_job_id = 'old-open'`); err != nil {
		t.Fatalf("aging old-open: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE jobs SET status = 'removed', closed_at = now() WHERE source_job_id = 'fresh-closed'`); err != nil {
		t.Fatalf("closing fresh-closed: %v", err)
	}

	got, err := store.RecentlyOpened(ctx, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("RecentlyOpened: %v", err)
	}
	freshOpenID := mustJobID(t, ctx, db, "fresh-open")
	if len(got) != 1 {
		t.Fatalf("RecentlyOpened = %+v, want exactly one candidate (fresh-open)", got)
	}
	gotID, err := strconv.ParseInt(freshOpenID, 10, 64)
	if err != nil {
		t.Fatalf("parsing expected job id: %v", err)
	}
	if got[0].JobID != gotID || got[0].Title != "Backend Engineer" {
		t.Errorf("RecentlyOpened()[0] = %+v, want JobID=%d Title=%q", got[0], gotID, "Backend Engineer")
	}
}
