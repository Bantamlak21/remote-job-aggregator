package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

func eligibilityFixture() *recordingRepo {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	hours := "works US hours (EST)"
	return &recordingRepo{jobs: []job.Job{
		{ID: "1", Title: "Backend Engineer", CompanyName: "A", Market: market.Worldwide, PostedAt: at, Tags: []string{},
			Eligibility: &job.Eligibility{Status: "eligible", Confidence: 0.9, Basis: "worldwide", HoursConstraint: hours,
				Reasons: []string{"open worldwide"}, Evidence: []job.EligibilityEvidence{{Field: "location", Text: "Worldwide"}}, Locations: []string{"Worldwide"}, ClassifierVersion: 1},
			Role: &job.Role{Family: "software_engineering", Relevant: true}},
		{ID: "2", Title: "Sales Rep", CompanyName: "B", Market: market.Worldwide, PostedAt: at, Tags: []string{},
			Eligibility: &job.Eligibility{Status: "ineligible", Confidence: 0.85, Basis: "restricted_places", Restrictions: []string{"United States only"}},
			Role:        &job.Role{Family: "non_tech"}},
		{ID: "3", Title: "New job", CompanyName: "C", Market: market.Worldwide, PostedAt: at, Tags: []string{}}, // not classified yet
	}}
}

func get(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestListJobs_EligibilityRelevanceAndRoleFilters(t *testing.T) {
	cases := []struct {
		query        string
		wantStatus   int
		wantElig     []string
		wantRelevant bool
		wantFamily   string
	}{
		{"", 200, nil, false, ""},
		{"?eligibility=eligible", 200, []string{"eligible"}, false, ""},
		{"?eligibility=eligible,uncertain", 200, []string{"eligible", "uncertain"}, false, ""},
		{"?eligibility=eligible,,uncertain", 400, nil, false, ""}, // an empty item is not a status
		{"?eligibility=uncertain,uncertain,eligible", 200, []string{"uncertain", "eligible"}, false, ""},
		{"?eligibility=unclassified", 200, []string{"unclassified"}, false, ""},
		{"?eligibility=maybe", 400, nil, false, ""},
		{"?eligibility=Eligible", 400, nil, false, ""},
		{"?relevant=true", 200, nil, true, ""},
		{"?relevant=false", 200, nil, false, ""}, // the same as omitting it
		{"?relevant=yes", 400, nil, false, ""},
		{"?role_family=data_ml", 200, nil, false, "data_ml"},
		{"?role_family=Data-ML", 400, nil, false, ""},
		{"?role_family=" + strings.Repeat("a", 41), 400, nil, false, ""},
		{"?eligibility=eligible&relevant=true&market=worldwide", 200, []string{"eligible"}, true, ""},
	}
	for _, tc := range cases {
		repo := eligibilityFixture()
		code, _ := get(t, testHandler(t, repo), "/api/v1/jobs"+tc.query)
		if code != tc.wantStatus {
			t.Errorf("GET /jobs%s: status %d, want %d", tc.query, code, tc.wantStatus)
			continue
		}
		if tc.wantStatus != 200 {
			continue
		}
		if !slices.Equal(repo.filter.Eligibility, tc.wantElig) || repo.filter.RelevantOnly != tc.wantRelevant || repo.filter.RoleFamily != tc.wantFamily {
			t.Errorf("GET /jobs%s: filter = %+v, want eligibility %v relevant %t family %q", tc.query, repo.filter, tc.wantElig, tc.wantRelevant, tc.wantFamily)
		}
	}
}

func TestJobs_ReportEligibilityInListAndDetail(t *testing.T) {
	h := testHandler(t, eligibilityFixture())

	_, list := get(t, h, "/api/v1/jobs")
	jobs := list["jobs"].([]any)
	byID := map[string]map[string]any{}
	for _, j := range jobs {
		m := j.(map[string]any)
		byID[m["id"].(string)] = m
	}

	el := byID["1"]["eligibility"].(map[string]any)
	if el["status"] != "eligible" || el["basis"] != "worldwide" || el["confidence"].(float64) != 0.9 || el["hours_constraint"] != "works US hours (EST)" {
		t.Errorf("job 1 eligibility = %v", el)
	}
	if _, has := el["reasons"]; has {
		t.Errorf("the list carries the detail-only reasons: %v", el)
	}
	if role := byID["1"]["role"].(map[string]any); role["family"] != "software_engineering" || role["relevant"] != true {
		t.Errorf("job 1 role = %v", role)
	}

	el2 := byID["2"]["eligibility"].(map[string]any)
	if el2["hours_constraint"] != nil {
		t.Errorf("no hours constraint should be JSON null, got %v", el2["hours_constraint"])
	}
	if r, ok := el2["restrictions"].([]any); !ok || len(r) != 1 || r[0] != "United States only" {
		t.Errorf("restrictions = %v", el2["restrictions"])
	}
	// A job with no restrictions has an empty array, never null.
	if r, ok := el["restrictions"].([]any); !ok || len(r) != 0 {
		t.Errorf("job 1 restrictions = %v, want []", el["restrictions"])
	}

	// Not classified yet: both are null, not "uncertain".
	if byID["3"]["eligibility"] != nil || byID["3"]["role"] != nil {
		t.Errorf("an unclassified job reports eligibility %v role %v, want null", byID["3"]["eligibility"], byID["3"]["role"])
	}

	// The detail adds the reasons, the evidence, the places and the version.
	code, detail := get(t, h, "/api/v1/jobs/1")
	if code != 200 {
		t.Fatalf("detail status %d", code)
	}
	d := detail["eligibility"].(map[string]any)
	ev := d["evidence"].([]any)
	if d["status"] != "eligible" || len(d["reasons"].([]any)) != 1 || len(ev) != 1 || ev[0].(map[string]any)["text"] != "Worldwide" ||
		d["locations"].([]any)[0] != "Worldwide" || d["classifier_version"].(float64) != 1 {
		t.Errorf("detail eligibility = %v", d)
	}
	_, detail3 := get(t, h, "/api/v1/jobs/3")
	if detail3["eligibility"] != nil {
		t.Errorf("an unclassified job's detail eligibility = %v, want null", detail3["eligibility"])
	}
}
