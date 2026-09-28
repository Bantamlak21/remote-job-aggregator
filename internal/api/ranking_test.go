package api

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ranking"
)

func TestListJobs_SortValidation(t *testing.T) {
	cases := []struct {
		query      string
		wantStatus int
		wantSort   string
	}{
		{"", 200, job.SortNewest},
		{"?sort=newest", 200, job.SortNewest},
		{"?sort=ranked", 200, job.SortRanked},
		{"?sort=Ranked", 400, ""}, // case-sensitive, like every other enum filter
		{"?sort=relevance", 400, ""},
		{"?sort=", 200, job.SortNewest}, // an empty value is the same as omitting it
	}
	for _, tc := range cases {
		repo := &recordingRepo{}
		code, _ := get(t, testHandler(t, repo), "/api/v1/jobs"+tc.query)
		if code != tc.wantStatus {
			t.Errorf("GET /jobs%s: status %d, want %d", tc.query, code, tc.wantStatus)
			continue
		}
		if tc.wantStatus != 200 {
			continue
		}
		if repo.filter.Sort != tc.wantSort {
			t.Errorf("GET /jobs%s: filter.Sort = %q, want %q", tc.query, repo.filter.Sort, tc.wantSort)
		}
	}
}

// sortingRepo is recordingRepo plus the same ordering internal/job.Store and
// MockRepository apply for Filter.Sort, so this package can prove the HTTP
// layer's ?sort=ranked reaches the repository and actually changes the
// response order, without duplicating job.Store's SQL or exporting a
// test-only MockRepository constructor across packages. The ordering
// algorithm itself (which status/relevance/priority/date wins) is unit-tested
// directly in internal/job; this is only a wiring check.
type sortingRepo struct{ jobs []job.Job }

func (r *sortingRepo) List(_ context.Context, f job.Filter) (job.ListResult, error) {
	jobs := append([]job.Job{}, r.jobs...)
	sort.SliceStable(jobs, func(i, k int) bool {
		a, b := jobs[i], jobs[k]
		if f.Sort == job.SortRanked {
			tier := func(j job.Job) ranking.Tier {
				if j.Eligibility == nil {
					return ranking.EligibilityTier("")
				}
				return ranking.EligibilityTier(j.Eligibility.Status)
			}
			relevant := func(j job.Job) bool { return j.Role != nil && j.Role.Relevant }
			if at, bt := tier(a), tier(b); at != bt {
				return at > bt
			}
			if ar, br := relevant(a), relevant(b); ar != br {
				return ar
			}
			if a.IsPriority != b.IsPriority {
				return a.IsPriority
			}
		}
		return a.PostedAt.After(b.PostedAt) // always the base/tiebreak order, ranked or not
	})
	return job.ListResult{Jobs: jobs, Total: len(jobs)}, nil
}

func (r *sortingRepo) Get(_ context.Context, id string) (job.Job, error) {
	for _, j := range r.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return job.Job{}, job.ErrNotFound
}

// End to end through the real handler: confirms ?sort=ranked actually reaches
// the repository and changes the response order, not just that parseFilter
// accepts the parameter (TestListJobs_SortValidation already checks that).
func TestListJobs_SortRankedOrdersRealResponse(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	repo := &sortingRepo{jobs: []job.Job{
		{ID: "1", Title: "A", Market: market.Worldwide, PostedAt: day(1), Tags: []string{},
			Eligibility: &job.Eligibility{Status: "eligible"}, Role: &job.Role{Relevant: true}},
		{ID: "2", Title: "B", Market: market.Worldwide, PostedAt: day(30), Tags: []string{},
			Eligibility: &job.Eligibility{Status: "eligible"}, Role: &job.Role{Relevant: false}},
		{ID: "3", Title: "C", Market: market.Worldwide, PostedAt: day(15), Tags: []string{}, IsPriority: true},
	}}

	code, body := get(t, testHandler(t, repo), "/api/v1/jobs?sort=ranked")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 3 {
		t.Fatalf("jobs = %d, want 3", len(jobs))
	}
	if first := jobs[0].(map[string]any)["id"]; first != "1" {
		t.Errorf("first job = %v, want %q (eligible and relevant beats a newer, less-relevant, or unclassified job)", first, "1")
	}
	if last := jobs[2].(map[string]any)["id"]; last != "3" {
		t.Errorf("last job = %v, want %q (unclassified ranks below any classified-eligible job)", last, "3")
	}

	// The default (no sort param) reaches the same repository unchanged: pure
	// recency, oldest last.
	code, body = get(t, testHandler(t, repo), "/api/v1/jobs")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	jobs, _ = body["jobs"].([]any)
	if first := jobs[0].(map[string]any)["id"]; first != "2" {
		t.Errorf("first job (default sort) = %v, want %q (newest, day 30)", first, "2")
	}
}
