package company_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
)

func TestStoreSetPriority_FlipsAndReadsBack(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	created, err := store.Upsert(ctx, company.UpsertParams{Name: "Kifiya"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}
	if created.IsPriority {
		t.Fatalf("new company IsPriority = true, want false by default")
	}

	// Twice: setting the value a row already has must succeed, not report
	// "not found" because no row changed.
	for i := range 2 {
		if err := store.SetPriority(ctx, created.ID, true); err != nil {
			t.Fatalf("SetPriority(true) call %d failed: %v", i+1, err)
		}
	}
	got, err := store.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if !got.IsPriority {
		t.Errorf("IsPriority = false after SetPriority(true)")
	}

	if err := store.SetPriority(ctx, created.ID, false); err != nil {
		t.Fatalf("SetPriority(false) failed: %v", err)
	}
	got, err = store.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if got.IsPriority {
		t.Errorf("IsPriority = true after SetPriority(false)")
	}
}

func TestStoreSetPriority_UnknownIDReturnsErrNotFound(t *testing.T) {
	db := newDB(t)
	store := company.NewStore(db.Pool)

	err := store.SetPriority(context.Background(), 424242, true)
	if !errors.Is(err, company.ErrNotFound) {
		t.Fatalf("SetPriority() error = %v, want it to wrap company.ErrNotFound", err)
	}
}

// Discovery re-finding a company by name goes through Upsert; it must
// never be able to reset the curated priority flag.
func TestStoreUpsert_DoesNotTouchPriority(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewStore(db.Pool)

	created, err := store.Upsert(ctx, company.UpsertParams{Name: "Kifiya"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}
	if err := store.SetPriority(ctx, created.ID, true); err != nil {
		t.Fatalf("SetPriority() failed: %v", err)
	}

	again, err := store.Upsert(ctx, company.UpsertParams{Name: "  KIFIYA ", Website: "https://kifiya.com"})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}
	if !again.IsPriority {
		t.Errorf("Upsert() cleared IsPriority; it must leave the flag alone")
	}
}
