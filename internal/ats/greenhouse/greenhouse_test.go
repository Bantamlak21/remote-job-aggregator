package greenhouse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *url.URL) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}

	cfg := httpclient.DefaultConfig()
	cfg.Timeout = 5 * time.Second
	return New(httpclient.New(cfg)), base
}

func withTestEndpoint(t *testing.T, base *url.URL) {
	t.Helper()
	original := endpointBase
	endpointBase = base.String() + "/v1/boards"
	t.Cleanup(func() { endpointBase = original })
}

// realGitLabResponseSample is a trimmed, byte-faithful excerpt of a real
// response from https://boards-api.greenhouse.io/v1/boards/gitlab/jobs?content=true
// fetched live during Phase 3 research (2026-09-23) — including the
// genuinely double-entity-encoded content field, so this test proves
// the whole ListJobs -> ats.HTMLToText pipeline against real Greenhouse
// output, not a synthetic guess at its shape.
const realGitLabResponseSample = `{
	"jobs": [
		{
			"absolute_url": "https://job-boards.greenhouse.io/gitlab/jobs/8556658002",
			"internal_job_id": 6417799002,
			"location": {"name": "Remote, Bangalore"},
			"id": 8556658002,
			"updated_at": "2026-09-14T16:01:39-04:00",
			"requisition_id": "6401",
			"title": "AI Engineer",
			"company_name": "GitLab",
			"first_published": "2026-05-22T09:16:29-04:00",
			"language": "en",
			"application_deadline": null,
			"content": "&lt;div class=&quot;content-intro&quot;&gt;&lt;p&gt;GitLab is the intelligent orchestration platform for DevSecOps.&lt;/p&gt;&lt;/div&gt;"
		}
	],
	"meta": {"total": 1}
}`

func TestListJobs_ParsesRealGreenhouseResponseShape(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realGitLabResponseSample))
	})
	withTestEndpoint(t, base)

	jobs, err := c.ListJobs(context.Background(), "gitlab")
	if err != nil {
		t.Fatalf("ListJobs() failed: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}

	j := jobs[0]
	if j.ExternalID != "8556658002" {
		t.Errorf("ExternalID = %q, want %q", j.ExternalID, "8556658002")
	}
	if j.Title != "AI Engineer" {
		t.Errorf("Title = %q, want %q", j.Title, "AI Engineer")
	}
	if j.URL != "https://job-boards.greenhouse.io/gitlab/jobs/8556658002" {
		t.Errorf("URL = %q, want the absolute_url value", j.URL)
	}
	if j.LocationRaw != "Remote, Bangalore" {
		t.Errorf("LocationRaw = %q, want %q", j.LocationRaw, "Remote, Bangalore")
	}
	if strings.Contains(j.Description, "&lt;") || strings.Contains(j.Description, "<div") {
		t.Errorf("Description = %q, still contains raw HTML/entities", j.Description)
	}
	if !strings.Contains(j.Description, "GitLab is the intelligent orchestration platform for DevSecOps.") {
		t.Errorf("Description = %q, missing the real decoded text", j.Description)
	}
	wantPublished, _ := time.Parse(time.RFC3339, "2026-05-22T09:16:29-04:00")
	if !j.PublishedAt.Equal(wantPublished) {
		t.Errorf("PublishedAt = %v, want %v (first_published, preferred over updated_at)", j.PublishedAt, wantPublished)
	}
}

func TestListJobs_EmptyJobsArrayReturnsEmptySliceNotError(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs": [], "meta": {"total": 0}}`))
	})
	withTestEndpoint(t, base)

	jobs, err := c.ListJobs(context.Background(), "empty-board")
	if err != nil {
		t.Fatalf("ListJobs() failed: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("got %d jobs, want 0", len(jobs))
	}
}

// Regression test pinning the real, live-confirmed 404 shape:
// {"status":404,"error":"Job not found"} — verified via curl against
// google.com/boards-api.greenhouse.io for a nonexistent board token
// during Phase 3 research.
func TestListJobs_UnknownBoardReturnsErrBoardNotFound(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":404,"error":"Job not found"}`))
	})
	withTestEndpoint(t, base)

	_, err := c.ListJobs(context.Background(), "this-board-does-not-exist")
	if !errors.Is(err, ats.ErrBoardNotFound) {
		t.Errorf("err = %v, want it to match ats.ErrBoardNotFound", err)
	}
}

func TestListJobs_UnexpectedStatusWithNoParseableBody(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	})
	withTestEndpoint(t, base)

	cfg := httpclient.DefaultConfig()
	cfg.MaxRetries = 0
	c = New(httpclient.New(cfg))

	_, err := c.ListJobs(context.Background(), "some-board")
	if err == nil {
		t.Fatal("expected an error for a 500 with an unparseable body, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention the status code", err.Error())
	}
	if errors.Is(err, ats.ErrBoardNotFound) {
		t.Error("a 500 must not match ats.ErrBoardNotFound")
	}
}

func TestListJobs_MalformedJSONIsReportedNotPanicked(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs": [{"id": "not-a-number-but-should-be"`)) // truncated/invalid
	})
	withTestEndpoint(t, base)

	_, err := c.ListJobs(context.Background(), "some-board")
	if err == nil {
		t.Fatal("expected a parse error for malformed JSON, got nil")
	}
}

// Regression (adversarial review, B2): a 200 whose body has no "jobs" key
// (e.g. board metadata) must be an error, never "zero jobs" — ingestion
// would otherwise close out every real job on the board.
func TestListJobs_ResponseWithoutJobsKeyIsAnError(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 1, "name": "board metadata, no jobs key"}`))
	})
	withTestEndpoint(t, base)

	jobs, err := c.ListJobs(context.Background(), "acme")
	if err == nil {
		t.Fatalf("got %d jobs and no error, want an error for a body with no \"jobs\" field", len(jobs))
	}
}

// Regression (adversarial review, B3): a board token is placed in a URL
// path, so anything outside [A-Za-z0-9_-] is refused before any request
// is made.
func TestListJobs_RejectsUnsafeBoardTokensWithoutRequesting(t *testing.T) {
	requested := false
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requested = true
		_, _ = w.Write([]byte(`{"jobs": []}`))
	})
	withTestEndpoint(t, base)

	for _, token := range []string{"", "acme?x=1#", "../../evil", "a/b", "acme jobs", "acme.inc"} {
		_, err := c.ListJobs(context.Background(), token)
		if !errors.Is(err, ats.ErrInvalidBoardToken) {
			t.Errorf("token %q: err = %v, want ats.ErrInvalidBoardToken", token, err)
		}
	}
	if requested {
		t.Error("a request reached the server for an invalid token")
	}
}

// Regression (adversarial review, S1): text fields are sanitized so one
// bad byte in one job can never abort a whole board's ingestion later.
func TestListJobs_SanitizesTextFields(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":1,"title":"\tEng\u0000ineer  ","absolute_url":" https://x.test/1 ","location":{"name":" Remote\u0000 "},"content":""}]}`))
	})
	withTestEndpoint(t, base)

	jobs, err := c.ListJobs(context.Background(), "acme")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs() = %v, %v", jobs, err)
	}
	if jobs[0].Title != "Engineer" || jobs[0].URL != "https://x.test/1" || jobs[0].LocationRaw != "Remote" {
		t.Errorf("job = %+v, want title/url/location sanitized (no NUL, trimmed)", jobs[0])
	}
}

// updated_at moves whenever a recruiter edits a posting and job.Store
// freezes published_at on first write, so it must never stand in for
// first_published.
func TestListJobs_PublishedAtNeverFallsBackToUpdatedAt(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":1,"title":"T","absolute_url":"https://x.test/1","location":{"name":""},"updated_at":"2026-09-14T16:01:39-04:00","content":""}]}`))
	})
	withTestEndpoint(t, base)

	jobs, err := c.ListJobs(context.Background(), "acme")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs() = %v, %v", jobs, err)
	}
	if !jobs[0].PublishedAt.IsZero() {
		t.Errorf("PublishedAt = %v, want zero when first_published is absent", jobs[0].PublishedAt)
	}
}

func TestListJobs_MissingIDBecomesEmptyExternalIDNotZero(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"title":"T","absolute_url":"https://x.test/1","location":{"name":""}}]}`))
	})
	withTestEndpoint(t, base)

	jobs, err := c.ListJobs(context.Background(), "acme")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs() = %v, %v", jobs, err)
	}
	if jobs[0].ExternalID != "" {
		t.Errorf("ExternalID = %q, want empty (ingestion skips it) rather than the colliding identity \"0\"", jobs[0].ExternalID)
	}
}

// bigBoardBody builds a valid list-jobs response of roughly `size` bytes
// (many jobs with long descriptions), the shape of a real large board.
func bigBoardBody(size int) []byte {
	desc := strings.Repeat("word ", 2000) // ~10 KB per job
	var b strings.Builder
	b.WriteString(`{"jobs":[`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"title":"Job %d","absolute_url":"https://x.test/%d","location":{"name":"Remote"},"content":"%s"}`, i+1, i, i, desc)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// Regression (round-1 review B1): real boards exceed the shared 5 MiB
// response cap (databricks: 9.7 MB). A ~6 MiB board must fetch fine
// through a client sized like ingestion's, and must fail loudly (not
// silently truncate) through one at the shared default — pinning both
// why the ingestion client exists and that the cap is honored.
func TestListJobs_BoardOverTheSharedCapNeedsTheIngestionSizedClient(t *testing.T) {
	body := bigBoardBody(6 << 20)
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	withTestEndpoint(t, base)

	shared := httpclient.DefaultConfig() // 5 MiB default cap
	if _, err := New(httpclient.New(shared)).ListJobs(context.Background(), "big"); err == nil {
		t.Fatal("a >5 MiB board through the shared-default client = nil error, want a size-cap error")
	}

	ingestion := httpclient.DefaultConfig()
	ingestion.MaxResponseBytes = 32 << 20
	ingestion.Timeout = 30 * time.Second
	jobs, err := New(httpclient.New(ingestion)).ListJobs(context.Background(), "big")
	if err != nil {
		t.Fatalf("a ~6 MiB board through an ingestion-sized client failed: %v", err)
	}
	if len(jobs) < 500 {
		t.Errorf("got %d jobs from a ~6 MiB board, want hundreds (nothing truncated)", len(jobs))
	}
}

func TestListJobs_RequestsContentTrue(t *testing.T) {
	var gotQuery url.Values
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs": []}`))
	})
	withTestEndpoint(t, base)

	if _, err := c.ListJobs(context.Background(), "acme"); err != nil {
		t.Fatalf("ListJobs() failed: %v", err)
	}
	if gotQuery.Get("content") != "true" {
		t.Errorf(`content query param = %q, want "true" (needed for the description field)`, gotQuery.Get("content"))
	}
}

func TestListJobs_RespectsContextCancellation(t *testing.T) {
	c, base := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"jobs": []}`))
	})
	withTestEndpoint(t, base)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.ListJobs(ctx, "acme")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("ListJobs() with an already-canceled context = nil error, want one")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ListJobs() did not return promptly for a canceled context")
	}
}
