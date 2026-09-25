package job

import (
	"context"
	"errors"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
)

// A job moves to another target of a collector's provider, and the move is
// refused for a target of another provider or a company that does not own it.
func TestMoveJob_RehomesUnderARenamedEmployerAndKeepsTheForeignKeysHonest(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	store := NewStore(db.Pool)
	companies, targets := company.NewStore(db.Pool), company.NewTargetStore(db.Pool)

	// Two employers on one board provider ("remotive"): the job starts under "Old Name".
	mk := func(name, provider string) (int64, int64) {
		c, err := companies.Upsert(ctx, company.UpsertParams{Name: name})
		if err != nil {
			t.Fatalf("company %s: %v", name, err)
		}
		tg, err := targets.Upsert(ctx, company.TargetUpsertParams{CompanyID: c.ID, ATSProvider: provider, ExternalBoardID: name})
		if err != nil {
			t.Fatalf("target %s: %v", name, err)
		}
		return tg.ID, c.ID
	}
	oldTarget, oldCompany := mk("Old Name", "remotive")
	newTarget, newCompany := mk("New Name", "remotive")
	otherProviderTarget, otherProviderCompany := mk("Elsewhere", "greenhouse")

	rec := baseRecord(oldTarget, oldCompany, "77")
	rec.Source = "remotive"
	if _, err := store.UpsertFromATS(ctx, rec); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	// Without a move the renamed employer's copy is refused.
	rec.TargetCompanyID, rec.CompanyID = newTarget, newCompany
	if _, err := store.UpsertFromATS(ctx, rec); !errors.Is(err, ErrJobTargetMismatch) {
		t.Fatalf("upsert under another target: err = %v, want ErrJobTargetMismatch", err)
	}

	if err := store.MoveJob(ctx, "remotive", "77", newTarget, newCompany); err != nil {
		t.Fatalf("MoveJob() failed: %v", err)
	}
	if _, err := store.UpsertFromATS(ctx, rec); err != nil {
		t.Fatalf("upsert after the move: %v", err)
	}
	res, err := store.List(ctx, Filter{})
	if err != nil || res.Total != 1 || res.Jobs[0].CompanyName != "New Name" {
		t.Errorf("List() = %+v, %v; want the one job under New Name", res, err)
	}

	// A target of another provider, or one that does not belong to the company, is refused.
	if err := store.MoveJob(ctx, "remotive", "77", otherProviderTarget, otherProviderCompany); err == nil {
		t.Errorf("moved a remotive job to a greenhouse target")
	}
	if err := store.MoveJob(ctx, "remotive", "77", newTarget, oldCompany); err == nil {
		t.Errorf("moved a job to a target with a company that does not own it")
	}
	if err := store.MoveJob(ctx, "remotive", "missing", newTarget, newCompany); !errors.Is(err, ErrNotFound) {
		t.Errorf("moving an unknown job: err = %v, want ErrNotFound", err)
	}
}
