package job

import (
	"context"
	"errors"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
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

// OpenOpenings sees a company's open jobs in one market from other sources
// only: not its own source's, not another market's, not closed ones, not
// another company's.
func TestOpenOpenings_ListsOtherSourcesOpenJobsInTheMarket(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	store := NewStore(db.Pool)
	companies, targets := company.NewStore(db.Pool), company.NewTargetStore(db.Pool)

	acme, _ := companies.Upsert(ctx, company.UpsertParams{Name: "Acme"})
	other, _ := companies.Upsert(ctx, company.UpsertParams{Name: "Other Co"})
	mk := func(c *company.Company, provider, mkt string) int64 {
		t.Helper()
		tg, err := targets.Upsert(ctx, company.TargetUpsertParams{CompanyID: c.ID, ATSProvider: provider, ExternalBoardID: c.Name + provider + mkt, Market: market.Market(mkt)})
		if err != nil {
			t.Fatalf("target: %v", err)
		}
		return tg.ID
	}
	himalayas := mk(acme, "himalayas", "worldwide")
	jobicy := mk(acme, "jobicy", "worldwide")
	ethiojobs := mk(acme, "ethiojobs", "ethiopia")
	otherCo := mk(other, "himalayas", "worldwide")

	put := func(tgt int64, c *company.Company, source, id, title string) {
		t.Helper()
		r := baseRecord(tgt, c.ID, id)
		r.Source, r.Title, r.LocationRaw = source, title, "USA"
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	put(himalayas, acme, "himalayas", "h1", "Backend Engineer")
	put(himalayas, acme, "himalayas", "h2", "Closed Role")
	put(jobicy, acme, "jobicy", "j1", "Data Analyst")
	put(ethiojobs, acme, "ethiojobs", "e1", "Cashier")
	put(otherCo, other, "himalayas", "o1", "Someone Elses Role")
	if _, err := store.CloseBySourceID(ctx, himalayas, []string{"h2"}); err != nil {
		t.Fatalf("closing: %v", err)
	}

	got, err := store.OpenOpenings(ctx, acme.ID, "worldwide", "jobicy")
	if err != nil || len(got) != 1 || got[0].Title != "Backend Engineer" || got[0].Location != "USA" {
		t.Errorf("worldwide openings of Acme excluding jobicy = %+v, %v; want only Himalayas' open Backend Engineer", got, err)
	}
	got, _ = store.OpenOpenings(ctx, acme.ID, "ethiopia", "himalayas")
	if len(got) != 1 || got[0].Title != "Cashier" {
		t.Errorf("ethiopian openings = %+v, want the Ethiojobs cashier", got)
	}
	if got, _ = store.OpenOpenings(ctx, acme.ID, "worldwide", "himalayas"); len(got) != 1 || got[0].Title != "Data Analyst" {
		t.Errorf("worldwide openings excluding himalayas = %+v, want Jobicy's analyst only", got)
	}
}
