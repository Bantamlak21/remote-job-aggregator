package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

// recordingRepo returns canned jobs and remembers the Filter it was given,
// so a test can assert what the handler parsed from the query string.
type recordingRepo struct {
	jobs   []job.Job
	filter job.Filter
}

func (r *recordingRepo) List(_ context.Context, f job.Filter) (job.ListResult, error) {
	r.filter = f
	return job.ListResult{Jobs: r.jobs, Total: len(r.jobs)}, nil
}

func (r *recordingRepo) Get(_ context.Context, id string) (job.Job, error) {
	for _, j := range r.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return job.Job{}, job.ErrNotFound
}

func priorityFixture() *recordingRepo {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return &recordingRepo{jobs: []job.Job{
		{ID: "1", Title: "Backend Engineer", CompanyName: "Kifiya", IsPriority: true, PostedAt: at, Tags: []string{}},
		{ID: "2", Title: "Designer", CompanyName: "Acme", PostedAt: at, Tags: []string{}},
	}}
}

func TestListJobs_ReportsIsPriorityPerJob(t *testing.T) {
	h := testHandler(t, priorityFixture())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Decode into a raw map: the contract is the JSON key, not our DTO field.
	var resp struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got := resp.Jobs[0]["is_priority"]; got != true {
		t.Errorf(`jobs[0]["is_priority"] = %v, want true`, got)
	}
	if got := resp.Jobs[1]["is_priority"]; got != false {
		t.Errorf(`jobs[1]["is_priority"] = %v, want false (always present, never omitted)`, got)
	}
}

func TestGetJob_ReportsIsPriority(t *testing.T) {
	h := testHandler(t, priorityFixture())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body["is_priority"] != true {
		t.Errorf(`detail["is_priority"] = %v, want true`, body["is_priority"])
	}
}

func TestListJobs_PriorityQueryParam(t *testing.T) {
	cases := []struct {
		query    string
		wantCode int
		wantOnly bool
	}{
		{"", http.StatusOK, false},
		{"?priority=true", http.StatusOK, true},
		{"?priority=false", http.StatusOK, false},
		{"?priority=1", http.StatusBadRequest, false},
		{"?priority=TRUE", http.StatusBadRequest, false},
		{"?priority=yes", http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			repo := priorityFixture()
			h := testHandler(t, repo)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs"+tc.query, nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK && repo.filter.PriorityOnly != tc.wantOnly {
				t.Errorf("Filter.PriorityOnly = %t, want %t", repo.filter.PriorityOnly, tc.wantOnly)
			}
		})
	}
}
