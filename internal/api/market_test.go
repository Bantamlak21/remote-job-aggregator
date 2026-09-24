package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

func marketFixture() *recordingRepo {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return &recordingRepo{jobs: []job.Job{
		{ID: "1", Title: "Cashier", CompanyName: "Local Co", Market: market.Ethiopia, PostedAt: at, Tags: []string{}},
		{ID: "2", Title: "Backend Engineer", CompanyName: "Global Co", Market: market.Worldwide, PostedAt: at, Tags: []string{}},
	}}
}

func TestListJobs_MarketParameterIsParsedAndValidated(t *testing.T) {
	cases := []struct {
		query      string
		wantStatus int
		wantMarket market.Market
	}{
		{"", http.StatusOK, ""},
		{"?market=ethiopia", http.StatusOK, market.Ethiopia},
		{"?market=worldwide", http.StatusOK, market.Worldwide},
		{"?market=Ethiopia", http.StatusBadRequest, ""},
		{"?market=mars", http.StatusBadRequest, ""},
		{"?market=all", http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		repo := marketFixture()
		h := testHandler(t, repo)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs"+tc.query, nil))
		if rec.Code != tc.wantStatus {
			t.Errorf("GET /jobs%s: status = %d, want %d; body: %s", tc.query, rec.Code, tc.wantStatus, rec.Body.String())
			continue
		}
		if tc.wantStatus == http.StatusOK && repo.filter.Market != tc.wantMarket {
			t.Errorf("GET /jobs%s: filter market = %q, want %q", tc.query, repo.filter.Market, tc.wantMarket)
		}
	}
}

func TestJobs_ReportMarketInListAndDetail(t *testing.T) {
	h := testHandler(t, marketFixture())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
	var list struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding list: %v", err)
	}
	if list.Jobs[0]["market"] != "ethiopia" || list.Jobs[1]["market"] != "worldwide" {
		t.Errorf("markets in list = %v, %v; want ethiopia, worldwide", list.Jobs[0]["market"], list.Jobs[1]["market"])
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/1", nil))
	var detail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decoding detail: %v", err)
	}
	if detail["market"] != "ethiopia" {
		t.Errorf(`detail["market"] = %v, want ethiopia`, detail["market"])
	}
}
