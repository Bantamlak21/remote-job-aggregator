package job

import (
	"context"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
)

// Five jobs whose dates deliberately contradict the ranked order, so a test
// that merely happened to agree with recency (a real risk: seedEligibilityJobs
// elsewhere puts the eligible job newest already) cannot pass by accident.
// Expected ranked order (eligibility tier, then role relevance, then priority
// company, then recency — see internal/ranking):
//  1. eligible-relevant   (eligible, relevant,     not priority, OLDEST : day 1)
//  2. eligible-plain      (eligible, not relevant, not priority, day 30, NEWEST)
//  3. unclassified-prio   (no verdict,             priority,     day 15)
//  4. uncertain-plain     (uncertain,               not priority, day 20)
//  5. ineligible-prio     (ineligible,             priority,     day 29)
//
// Expected recency (default) order is the reverse of that by date:
// eligible-plain(30), ineligible-prio(29), uncertain-plain(20),
// unclassified-prio(15), eligible-relevant(1).
func seedRankingJobs(t *testing.T, ctx context.Context) (*Store, map[string]string) {
	t.Helper()
	db := newDB(t)
	store := NewStore(db.Pool)

	plainTarget, plainCompany := seedTarget(t, ctx, db, "Plain Co", "plain")
	prioTarget, prioCompany := seedTarget(t, ctx, db, "Priority Co", "prio")
	if err := company.NewStore(db.Pool).SetPriority(ctx, prioCompany, true); err != nil {
		t.Fatalf("SetPriority() failed: %v", err)
	}

	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	for _, r := range []Record{
		withPublished(baseRecord(plainTarget, plainCompany, "eligible-relevant"), day(1)),
		withPublished(baseRecord(plainTarget, plainCompany, "eligible-plain"), day(30)),
		withPublished(baseRecord(prioTarget, prioCompany, "unclassified-prio"), day(15)),
		withPublished(baseRecord(plainTarget, plainCompany, "uncertain-plain"), day(20)),
		withPublished(baseRecord(prioTarget, prioCompany, "ineligible-prio"), day(29)),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s: %v", r.SourceJobID, err)
		}
	}

	ids := map[string]string{}
	for _, k := range []string{"eligible-relevant", "eligible-plain", "unclassified-prio", "uncertain-plain", "ineligible-prio"} {
		ids[k] = mustJobID(t, ctx, db, k)
	}
	insertVerdict := func(key, status string, relevant bool) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO job_eligibility (job_id, target_country, status, confidence, basis,
			reasons, evidence, detected_locations, restrictions, role_family, relevant, classifier_version, job_content_hash)
			VALUES ($1, 'ET', $2, 0.9, 'worldwide', '["x"]', '[]', '[]', '[]'::jsonb, 'software_engineering', $3, 1, 'h')`,
			ids[key], status, relevant); err != nil {
			t.Fatalf("inserting a verdict for %s: %v", key, err)
		}
	}
	insertVerdict("eligible-relevant", "eligible", true)
	insertVerdict("eligible-plain", "eligible", false)
	insertVerdict("uncertain-plain", "uncertain", false)
	insertVerdict("ineligible-prio", "ineligible", false)
	// unclassified-prio gets no job_eligibility row at all.

	return store, ids
}

func idsOf(jobs []Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}

func TestStoreList_SortRankedOrdersByEligibilityThenRelevanceThenPriorityThenRecency(t *testing.T) {
	ctx := context.Background()
	store, ids := seedRankingJobs(t, ctx)

	res, err := store.List(ctx, Filter{Sort: SortRanked})
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	want := []string{
		ids["eligible-relevant"], ids["eligible-plain"], ids["unclassified-prio"],
		ids["uncertain-plain"], ids["ineligible-prio"],
	}
	if got := idsOf(res.Jobs); !equalStrings(got, want) {
		t.Errorf("ranked order = %v, want %v", got, want)
	}
	if res.Total != 5 {
		t.Errorf("Total = %d, want 5", res.Total)
	}
}

func TestStoreList_DefaultSortIsStillRecencyRegardlessOfEligibility(t *testing.T) {
	ctx := context.Background()
	store, ids := seedRankingJobs(t, ctx)

	// The zero value (Filter{}, no Sort set) and the explicit "newest" form
	// (already normalized by internal/api before reaching Filter) both mean
	// the same thing.
	for _, f := range []Filter{{}, {Sort: SortNewest}} {
		res, err := store.List(ctx, f)
		if err != nil {
			t.Fatalf("List(%+v) failed: %v", f, err)
		}
		want := []string{
			ids["eligible-plain"], ids["ineligible-prio"], ids["uncertain-plain"],
			ids["unclassified-prio"], ids["eligible-relevant"],
		}
		if got := idsOf(res.Jobs); !equalStrings(got, want) {
			t.Errorf("Filter{Sort:%q} recency order = %v, want %v", f.Sort, got, want)
		}
	}
}

// The ranked tiers are exactly internal/ranking's, not a coincidence: this
// paginates a small page size to confirm the ordering holds across pages too
// (a bug that only sorts within one page but not across OFFSET/LIMIT would
// slip past a single-page test).
func TestStoreList_SortRankedHoldsAcrossPages(t *testing.T) {
	ctx := context.Background()
	store, ids := seedRankingJobs(t, ctx)

	page1, err := store.List(ctx, Filter{Sort: SortRanked, Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	page2, err := store.List(ctx, Filter{Sort: SortRanked, Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	page3, err := store.List(ctx, Filter{Sort: SortRanked, Page: 3, PageSize: 2})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	got := append(append(idsOf(page1.Jobs), idsOf(page2.Jobs)...), idsOf(page3.Jobs)...)
	want := []string{
		ids["eligible-relevant"], ids["eligible-plain"], ids["unclassified-prio"],
		ids["uncertain-plain"], ids["ineligible-prio"],
	}
	if !equalStrings(got, want) {
		t.Errorf("paginated ranked order = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
