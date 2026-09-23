package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

func testHandler(t *testing.T, repo job.Repository) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	return NewHandler(repo, Config{Addr: ":0", CORSAllowedOrigin: "http://localhost:5173"}, logger)
}

func TestListJobs_ReturnsAllFixturesByDefault(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp listJobsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v; body: %s", err, rec.Body.String())
	}
	if resp.Total != 12 {
		t.Errorf("Total = %d, want 12", resp.Total)
	}
	if resp.Page != 1 || resp.PageSize != 20 {
		t.Errorf("Page/PageSize = %d/%d, want default 1/20", resp.Page, resp.PageSize)
	}
	if len(resp.Jobs) != 12 {
		t.Errorf("len(Jobs) = %d, want 12", len(resp.Jobs))
	}
}

func TestListJobs_FiltersByQueryParams(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?remote_type=hybrid", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp listJobsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	for _, j := range resp.Jobs {
		if j.RemoteType != "hybrid" {
			t.Errorf("job %s remote_type = %q, want hybrid", j.ID, j.RemoteType)
		}
	}
}

func TestListJobs_InvalidRemoteTypeIs400(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?remote_type=bogus", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if resp.Error == "" {
		t.Error("error message is empty")
	}
}

func TestListJobs_InvalidPageSizeIs400(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	cases := []string{"0", "-1", "101", "not-a-number"}
	for _, ps := range cases {
		t.Run(ps, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?page_size="+ps, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("page_size=%s: status = %d, want 400", ps, rec.Code)
			}
		})
	}
}

func TestListJobs_PaginationParamsRespected(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?page=2&page_size=5", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp listJobsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Page != 2 || resp.PageSize != 5 {
		t.Errorf("Page/PageSize = %d/%d, want 2/5", resp.Page, resp.PageSize)
	}
	if len(resp.Jobs) != 5 {
		t.Errorf("len(Jobs) = %d, want 5", len(resp.Jobs))
	}
	if resp.Total != 12 {
		t.Errorf("Total = %d, want 12 regardless of pagination", resp.Total)
	}
}

func TestListJobs_NullCompanyLogoURLSerializesAsJSONNull(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?page_size=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding raw response: %v", err)
	}
	jobs, ok := raw["jobs"].([]any)
	if !ok || len(jobs) == 0 {
		t.Fatalf("raw jobs = %v, want at least one job", raw["jobs"])
	}
	first, ok := jobs[0].(map[string]any)
	if !ok {
		t.Fatalf("jobs[0] = %v, want an object", jobs[0])
	}
	val, present := first["company_logo_url"]
	if !present {
		t.Fatal(`"company_logo_url" key is missing entirely, want it present with value null`)
	}
	if val != nil {
		t.Errorf("company_logo_url = %v, want JSON null (all fixtures have no logo)", val)
	}
}

func TestGetJob_ReturnsDetailIncludingDescription(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/job_spotify_backend_eng", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var detail jobDetailDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if detail.CompanyName != "Spotify" {
		t.Errorf("CompanyName = %q, want %q", detail.CompanyName, "Spotify")
	}
	if detail.Description == "" {
		t.Error("Description is empty")
	}
}

func TestGetJob_UnknownIDIs404(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/does-not-exist", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if resp.Error == "" {
		t.Error("error message is empty")
	}
}

func TestJobsEndpoint_CORSHeadersPresent(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "http://localhost:5173")
	}
}

func TestJobsEndpoint_OPTIONSPreflightReturnsNoContentWithoutHittingTheRepository(t *testing.T) {
	h := testHandler(t, &panicIfCalledRepository{t: t})

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

// panicIfCalledRepository fails the test loudly if List/Get is ever
// invoked — used to prove withCORS answers OPTIONS itself and never
// forwards the request to the mux/handlers/repository.
type panicIfCalledRepository struct{ t *testing.T }

func (p *panicIfCalledRepository) List(_ context.Context, _ job.Filter) (job.ListResult, error) {
	p.t.Fatal("List() called for an OPTIONS preflight request")
	return job.ListResult{}, nil
}

func (p *panicIfCalledRepository) Get(_ context.Context, _ string) (job.Job, error) {
	p.t.Fatal("Get() called for an OPTIONS preflight request")
	return job.Job{}, nil
}

func TestJobsResponse_PostedAtIsRFC3339(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?page_size=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp listJobsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Jobs) == 0 {
		t.Fatal("no jobs returned")
	}
	if _, err := time.Parse(time.RFC3339, resp.Jobs[0].PostedAt); err != nil {
		t.Errorf("posted_at %q is not RFC3339: %v", resp.Jobs[0].PostedAt, err)
	}
}
