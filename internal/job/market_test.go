package job

import (
	"context"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

// One company ("Acme") with a worldwide Greenhouse target and an Ethiopian
// Ethiojobs target, plus an Ethiopian-only company: the market is a property
// of the target, so Acme appears in both lists.
func seedMarketJobs(t *testing.T, ctx context.Context) (*Store, func(id string) string) {
	t.Helper()
	db := newDB(t)
	store := NewStore(db.Pool)

	globalTarget, acme := seedTarget(t, ctx, db, "Acme", "acme")
	ethTarget, _ := seedTarget(t, ctx, db, "Acme", "acme-ethiopia")
	// seedTarget upserts the company by name, so both targets share Acme's row.
	if _, err := db.Pool.Exec(ctx, `UPDATE target_companies SET market = 'ethiopia' WHERE id = $1`, ethTarget); err != nil {
		t.Fatalf("setting market: %v", err)
	}
	localTarget, local := seedTarget(t, ctx, db, "Kifiya", "kifiya")
	if _, err := db.Pool.Exec(ctx, `UPDATE target_companies SET market = 'ethiopia' WHERE id = $1`, localTarget); err != nil {
		t.Fatalf("setting market: %v", err)
	}

	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	for _, r := range []Record{
		withPublished(baseRecord(globalTarget, acme, "g-remote"), day(20)),
		withPublished(baseRecord(ethTarget, acme, "e-acme-local"), day(21)),
		withPublished(baseRecord(localTarget, local, "e-kifiya"), day(22)),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s: %v", r.SourceJobID, err)
		}
	}
	return store, func(id string) string { return mustJobID(t, ctx, db, id) }
}

func TestStoreList_FiltersByMarket(t *testing.T) {
	ctx := context.Background()
	store, _ := seedMarketJobs(t, ctx)

	titles := func(f Filter) []string {
		t.Helper()
		res, err := store.List(ctx, f)
		if err != nil {
			t.Fatalf("List(%+v) failed: %v", f, err)
		}
		if res.Total != len(res.Jobs) {
			t.Errorf("List(%+v): total %d, jobs %d", f, res.Total, len(res.Jobs))
		}
		var out []string
		for _, j := range res.Jobs {
			out = append(out, string(j.Market)+":"+j.CompanyName)
		}
		return out
	}

	if got := titles(Filter{Market: market.Worldwide}); len(got) != 1 || got[0] != "worldwide:Acme" {
		t.Errorf("worldwide list = %v, want only Acme's worldwide job", got)
	}
	// Newest first: Kifiya (22), then Acme's Ethiopian posting (21).
	if got := titles(Filter{Market: market.Ethiopia}); len(got) != 2 || got[0] != "ethiopia:Kifiya" || got[1] != "ethiopia:Acme" {
		t.Errorf("ethiopia list = %v, want Kifiya then Acme, newest first", got)
	}
	if got := titles(Filter{}); len(got) != 3 {
		t.Errorf("unfiltered list = %v, want all 3", got)
	}
}

func TestStoreList_MarketCombinesWithOtherFiltersAndPaginates(t *testing.T) {
	ctx := context.Background()
	store, _ := seedMarketJobs(t, ctx)

	res, err := store.List(ctx, Filter{Market: market.Ethiopia, Page: 2, PageSize: 1})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if res.Total != 2 || len(res.Jobs) != 1 || res.Jobs[0].CompanyName != "Acme" {
		t.Errorf("page 2 of the Ethiopian list = %+v (total %d), want Acme with total 2", res.Jobs, res.Total)
	}
	res, err = store.List(ctx, Filter{Market: market.Ethiopia, Query: "kifiya"})
	if err != nil || res.Total != 1 || res.Jobs[0].CompanyName != "Kifiya" {
		t.Errorf("Ethiopian list with q=kifiya = %+v, %v; want only Kifiya", res, err)
	}
	res, err = store.List(ctx, Filter{Market: market.Worldwide, Query: "kifiya"})
	if err != nil || res.Total != 0 {
		t.Errorf("worldwide list with q=kifiya = %+v, %v; want nothing", res, err)
	}
}

func TestStoreGet_ReportsTheMarket(t *testing.T) {
	ctx := context.Background()
	store, id := seedMarketJobs(t, ctx)

	for jobID, want := range map[string]market.Market{"g-remote": market.Worldwide, "e-acme-local": market.Ethiopia, "e-kifiya": market.Ethiopia} {
		j, err := store.Get(ctx, id(jobID))
		if err != nil || j.Market != want {
			t.Errorf("Get(%s) market = %q, err %v; want %q", jobID, j.Market, err, want)
		}
	}
}

// An Ethiopian (priority) company's job belongs to the Ethiopian list even when
// a worldwide source found it: it never appears on the worldwide list, and
// reports market "ethiopia". A non-priority company keeps its target's market.
func TestStoreList_APriorityCompanysWorldwideSourceJobIsListedAsEthiopian(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	store := NewStore(db.Pool)

	target, kifiya := seedTarget(t, ctx, db, "Kifiya", "kifiya-himalayas") // a worldwide target
	otherTarget, acme := seedTarget(t, ctx, db, "Acme", "acme")
	if _, err := db.Pool.Exec(ctx, `UPDATE companies SET is_priority = true WHERE id = $1`, kifiya); err != nil {
		t.Fatalf("marking priority: %v", err)
	}
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	for _, r := range []Record{
		withPublished(baseRecord(target, kifiya, "k-worldwide-source"), day(22)),
		withPublished(baseRecord(otherTarget, acme, "a-worldwide"), day(21)),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s: %v", r.SourceJobID, err)
		}
	}

	list := func(m market.Market) (names []string) {
		t.Helper()
		res, err := store.List(ctx, Filter{Market: m})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != len(res.Jobs) {
			t.Errorf("%s: total %d, jobs %d", m, res.Total, len(res.Jobs))
		}
		for _, j := range res.Jobs {
			names = append(names, string(j.Market)+":"+j.CompanyName)
		}
		return names
	}
	if got := list(market.Worldwide); len(got) != 1 || got[0] != "worldwide:Acme" {
		t.Errorf("worldwide list = %v, want only Acme (Kifiya is Ethiopian)", got)
	}
	if got := list(market.Ethiopia); len(got) != 1 || got[0] != "ethiopia:Kifiya" {
		t.Errorf("ethiopia list = %v, want Kifiya, reported as ethiopia", got)
	}
	j, err := store.Get(ctx, mustJobID(t, ctx, db, "k-worldwide-source"))
	if err != nil || j.Market != market.Ethiopia {
		t.Errorf("Get market = %q, err %v; want ethiopia", j.Market, err)
	}
}
