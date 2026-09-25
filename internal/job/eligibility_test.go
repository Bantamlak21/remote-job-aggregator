package job

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
)

// Three jobs: an eligible relevant one, an ineligible non-relevant one, and one
// with no verdict yet.
func seedEligibilityJobs(t *testing.T, ctx context.Context) (*Store, *database.DB, map[string]int64) {
	t.Helper()
	db := newDB(t)
	store := NewStore(db.Pool)
	targetID, companyID := seedTarget(t, ctx, db, "Acme", "acme")
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	for _, r := range []Record{
		withPublished(baseRecord(targetID, companyID, "eligible-dev"), day(22)),
		withPublished(baseRecord(targetID, companyID, "ineligible-sales"), day(21)),
		withPublished(baseRecord(targetID, companyID, "unclassified"), day(20)),
	} {
		if _, err := store.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("seeding %s: %v", r.SourceJobID, err)
		}
	}
	ids := map[string]int64{}
	for _, k := range []string{"eligible-dev", "ineligible-sales", "unclassified"} {
		id, _ := strconv.ParseInt(mustJobID(t, ctx, db, k), 10, 64)
		ids[k] = id
	}
	insert := func(jobID int64, status, family string, relevant bool, restrictions, hours string) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO job_eligibility (job_id, target_country, status, confidence, basis, reasons, evidence,
			detected_locations, restrictions, hours_constraint, role_family, relevant, classifier_version, job_content_hash)
			VALUES ($1, 'ET', $2, 0.9, 'worldwide', '["open worldwide"]', '[{"field":"location","text":"Worldwide"}]', '["Worldwide"]',
			$3::jsonb, NULLIF($4, ''), $5, $6, 1, 'h')`, jobID, status, restrictions, hours, family, relevant); err != nil {
			t.Fatalf("inserting a verdict: %v", err)
		}
	}
	insert(ids["eligible-dev"], "eligible", "software_engineering", true, `[]`, "works US hours")
	insert(ids["ineligible-sales"], "ineligible", "non_tech", false, `["United States only"]`, "")
	return store, db, ids
}

func listTitles(t *testing.T, store *Store, f Filter) (jobs []Job, total int) {
	t.Helper()
	res, err := store.List(context.Background(), f)
	if err != nil {
		t.Fatalf("List(%+v) failed: %v", f, err)
	}
	return res.Jobs, res.Total
}

func TestStoreList_ReportsTheVerdictAndTheRoleOrNilWhenUnclassified(t *testing.T) {
	ctx := context.Background()
	store, _, _ := seedEligibilityJobs(t, ctx)
	jobs, total := listTitles(t, store, Filter{})
	if total != 3 || len(jobs) != 3 {
		t.Fatalf("total %d, jobs %d, want 3 (a LEFT JOIN must not drop unclassified jobs)", total, len(jobs))
	}
	el := jobs[0].Eligibility // newest first: eligible-dev
	if el == nil || el.Status != "eligible" || el.Basis != "worldwide" || el.Confidence != 0.9 || el.HoursConstraint != "works US hours" || len(el.Restrictions) != 0 {
		t.Errorf("eligible job: %+v", el)
	}
	if r := jobs[0].Role; r == nil || r.Family != "software_engineering" || !r.Relevant {
		t.Errorf("role: %+v", r)
	}
	if el := jobs[1].Eligibility; el == nil || el.Status != "ineligible" || !slices.Equal(el.Restrictions, []string{"United States only"}) || el.HoursConstraint != "" {
		t.Errorf("ineligible job: %+v", el)
	}
	if jobs[2].Eligibility != nil || jobs[2].Role != nil {
		t.Errorf("an unclassified job has eligibility %+v role %+v, want nil", jobs[2].Eligibility, jobs[2].Role)
	}
}

func TestStoreList_FiltersByEligibilityRelevanceAndRoleFamily(t *testing.T) {
	ctx := context.Background()
	store, _, _ := seedEligibilityJobs(t, ctx)
	sourceIDs := func(f Filter) []string {
		jobs, total := listTitles(t, store, f)
		if total != len(jobs) {
			t.Errorf("%+v: total %d but %d jobs", f, total, len(jobs))
		}
		var out []string
		for _, j := range jobs {
			if j.Eligibility == nil {
				out = append(out, "none")
			} else {
				out = append(out, j.Eligibility.Status)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name string
		f    Filter
		want []string
	}{
		{"eligible", Filter{Eligibility: []string{"eligible"}}, []string{"eligible"}},
		{"eligible or ineligible", Filter{Eligibility: []string{"eligible", "ineligible"}}, []string{"eligible", "ineligible"}},
		{"unclassified matches jobs with no verdict", Filter{Eligibility: []string{"unclassified"}}, []string{"none"}},
		{"uncertain matches nothing here", Filter{Eligibility: []string{"uncertain"}}, nil},
		{"relevant only", Filter{RelevantOnly: true}, []string{"eligible"}},
		{"role family", Filter{RoleFamily: "non_tech"}, []string{"ineligible"}},
		{"eligible and relevant", Filter{Eligibility: []string{"eligible"}, RelevantOnly: true}, []string{"eligible"}},
		{"eligible and non_tech", Filter{Eligibility: []string{"eligible"}, RoleFamily: "non_tech"}, nil},
		{"no filter", Filter{}, []string{"eligible", "ineligible", "none"}},
	} {
		if got := sourceIDs(tc.f); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// Totals and pages agree under a filter.
	jobs, total := listTitles(t, store, Filter{Eligibility: []string{"eligible", "ineligible"}, Page: 2, PageSize: 1})
	if total != 2 || len(jobs) != 1 || jobs[0].Eligibility.Status != "ineligible" {
		t.Errorf("page 2 of the filtered list: %d jobs, total %d", len(jobs), total)
	}
	if jobs, total = listTitles(t, store, Filter{Eligibility: []string{"eligible"}, Page: 5}); len(jobs) != 0 || total != 1 {
		t.Errorf("a page past the end: %d jobs, total %d, want 0 and the real total 1", len(jobs), total)
	}
}

func TestStoreGet_CarriesTheReasonsEvidenceAndLocations(t *testing.T) {
	ctx := context.Background()
	store, db, ids := seedEligibilityJobs(t, ctx)
	j, err := store.Get(ctx, strconv.FormatInt(ids["eligible-dev"], 10))
	if err != nil {
		t.Fatal(err)
	}
	el := j.Eligibility
	if el == nil || !slices.Equal(el.Reasons, []string{"open worldwide"}) || !slices.Equal(el.Locations, []string{"Worldwide"}) ||
		len(el.Evidence) != 1 || el.Evidence[0].Field != "location" || el.Evidence[0].Text != "Worldwide" || el.ClassifierVersion != 1 {
		t.Errorf("detail eligibility = %+v", el)
	}
	if j.Role == nil || j.Role.Family != "software_engineering" {
		t.Errorf("detail role = %+v", j.Role)
	}
	j, err = store.Get(ctx, strconv.FormatInt(ids["unclassified"], 10))
	if err != nil || j.Eligibility != nil || j.Role != nil {
		t.Errorf("unclassified detail: %+v %+v %v", j.Eligibility, j.Role, err)
	}
	// Malformed stored JSON degrades to empty lists, never an error.
	if _, err := db.Pool.Exec(ctx, `UPDATE job_eligibility SET restrictions = '{"a":1}'::jsonb, reasons = '"x"'::jsonb`); err != nil {
		t.Fatal(err)
	}
	if j, err = store.Get(ctx, strconv.FormatInt(ids["eligible-dev"], 10)); err != nil || j.Eligibility == nil || len(j.Eligibility.Reasons) != 0 || len(j.Eligibility.Restrictions) != 0 {
		t.Errorf("malformed JSON: %+v %v", j.Eligibility, err)
	}
}

func TestValidEligibilityFilter(t *testing.T) {
	for s, want := range map[string]bool{"eligible": true, "ineligible": true, "uncertain": true, "unclassified": true, "": false, "Eligible": false, "maybe": false} {
		if got := ValidEligibilityFilter(s); got != want {
			t.Errorf("ValidEligibilityFilter(%q) = %t, want %t", s, got, want)
		}
	}
}
