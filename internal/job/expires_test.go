package job

import (
	"context"
	"testing"
	"time"
)

func TestStoreExpiry_PublicReadsHideJobsPastTheirDeadline(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := NewStore(db.Pool)
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")

	now := time.Now()
	for _, tc := range []struct {
		id      string
		expires time.Time
	}{
		{"no-deadline", time.Time{}},
		{"future", now.Add(5 * 24 * time.Hour)},
		{"expired", now.Add(-time.Hour)},
		{"expired-long-ago", now.Add(-30 * 24 * time.Hour)},
	} {
		r := baseRecord(targetID, companyID, tc.id)
		r.PublishedAt = now.Add(-2 * 24 * time.Hour)
		r.ExpiresAt = tc.expires
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s failed: %v", tc.id, err)
		}
	}

	res, err := store.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	got := sourceIDs(t, ctx, db, res.Jobs)
	if res.Total != 2 || !got["no-deadline"] || !got["future"] {
		t.Errorf("visible = %v total=%d, want exactly no-deadline and future", got, res.Total)
	}

	for id, visible := range map[string]bool{"no-deadline": true, "future": true, "expired": false, "expired-long-ago": false} {
		_, err := store.Get(ctx, mustJobID(t, ctx, db, id))
		if visible && err != nil {
			t.Errorf("Get(%s) = %v, want it visible", id, err)
		}
		if !visible && !isNotFound(err) {
			t.Errorf("Get(%s) error = %v, want ErrNotFound", id, err)
		}
	}
}

// The deadline applies even with no age limit and with the age limit set:
// the two rules are independent.
func TestStoreExpiry_AppliesWithAndWithoutTheAgeLimit(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	base := NewStore(db.Pool)

	r := baseRecord(targetID, companyID, "expired-but-recent")
	r.PublishedAt = time.Now().Add(-24 * time.Hour)
	r.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := base.UpsertFromATS(ctx, r); err != nil {
		t.Fatalf("seeding failed: %v", err)
	}
	for name, s := range map[string]*Store{"no limit": base, "15 days": base.WithMaxAge(15 * 24 * time.Hour)} {
		res, err := s.List(ctx, Filter{})
		if err != nil || res.Total != 0 {
			t.Errorf("%s: total = %d, %v; want 0 (deadline passed)", name, res.Total, err)
		}
	}
}

func TestStoreExpiry_UpsertUpdatesADeadlineButAnAbsentOneKeepsTheStoredOne(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	store := NewStore(db.Pool)

	expires := func() *time.Time {
		t.Helper()
		var e *time.Time
		if err := db.Pool.QueryRow(ctx, `SELECT expires_at FROM jobs WHERE source_job_id = 'j'`).Scan(&e); err != nil {
			t.Fatalf("reading expires_at: %v", err)
		}
		return e
	}

	r := baseRecord(targetID, companyID, "j")
	if _, err := store.UpsertFromATS(ctx, r); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if e := expires(); e != nil {
		t.Errorf("expires_at = %v with no deadline supplied, want NULL", e)
	}

	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	extended := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	r.ExpiresAt = first
	if _, err := store.UpsertFromATS(ctx, r); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	r.ExpiresAt = extended // the employer extended the deadline
	if _, err := store.UpsertFromATS(ctx, r); err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	if e := expires(); e == nil || !e.Equal(extended) {
		t.Errorf("expires_at = %v, want the extended %v", e, extended)
	}

	r.ExpiresAt = time.Time{}
	if _, err := store.UpsertFromATS(ctx, r); err != nil {
		t.Fatalf("fourth upsert: %v", err)
	}
	if e := expires(); e == nil || !e.Equal(extended) {
		t.Errorf("expires_at = %v after an upsert with no deadline, want %v kept", e, extended)
	}
}
