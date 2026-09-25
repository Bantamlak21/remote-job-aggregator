package ashby

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

func serve(t *testing.T, status int, body string) (*Client, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/acme" {
			t.Errorf("unexpected request %s", r.URL.RequestURI())
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	old := endpointBase
	endpointBase = srv.URL
	t.Cleanup(func() { endpointBase = old })
	return New(httpclient.New(httpclient.DefaultConfig())), &hits
}

// Live capture (2026-09-25): Linear's board, trimmed, plus one job marked unlisted.
func TestListJobs_MapsRealJobsAndSkipsUnlistedOnes(t *testing.T) {
	body, err := os.ReadFile("testdata/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := serve(t, 200, string(body))
	jobs, err := c.ListJobs(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListJobs() failed: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs, want 3 (the unlisted one is not on the public board)", len(jobs))
	}
	for _, j := range jobs {
		if j.ExternalID == "unlisted-1" {
			t.Errorf("an unlisted job was returned")
		}
	}
	j := jobs[0]
	if j.ExternalID != "d3bc1ced-3ce4-4086-a050-555055dbb1ff" || j.Title != "Senior / Staff Fullstack Engineer" ||
		j.RemoteType != "remote" || j.EmploymentType != "full_time" || j.LocationRaw != "Europe" {
		t.Errorf("job = %+v", j)
	}
	if j.URL != "https://jobs.ashbyhq.com/linear/d3bc1ced-3ce4-4086-a050-555055dbb1ff" || j.Description == "" {
		t.Errorf("URL = %q, description %q", j.URL, j.Description)
	}
	if want := time.Date(2021, 4, 27, 20, 13, 45, 158000000, time.UTC); !j.PublishedAt.Equal(want) {
		t.Errorf("PublishedAt = %v, want %v", j.PublishedAt, want)
	}
}

func TestListJobs_ABoardWithNoOpeningsIsAnEmptyListAnUnknownOneIsNotFound(t *testing.T) {
	c, _ := serve(t, 200, `{"jobs":[],"apiVersion":"1"}`)
	jobs, err := c.ListJobs(context.Background(), "acme")
	if err != nil || len(jobs) != 0 {
		t.Errorf("empty board: %d jobs, err = %v; want none and no error", len(jobs), err)
	}
	c, _ = serve(t, 404, `Not Found`)
	if _, err := c.ListJobs(context.Background(), "acme"); !errors.Is(err, ats.ErrBoardNotFound) {
		t.Errorf("unknown board: err = %v, want ErrBoardNotFound", err)
	}
}

func TestListJobs_ChangedFormatsAndFailuresAreErrorsNeverAnEmptyList(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"no jobs field":  {200, `{"apiVersion":"1"}`},
		"array":          {200, `[]`},
		"html":           {200, `<html>maintenance</html>`},
		"empty body":     {200, ``},
		"server error":   {500, `oops`},
		"rate limited":   {429, `slow down`},
		"jobs is a null": {200, `{"jobs":null}`},
	} {
		c, _ := serve(t, tc.status, tc.body)
		if jobs, err := c.ListJobs(context.Background(), "acme"); err == nil {
			t.Errorf("%s: %d jobs, nil; want an error", name, len(jobs))
		}
	}
}

func TestListJobs_WorkplaceFallbacksLocationsAndOddRecords(t *testing.T) {
	body := `{"jobs":[
	 {"id":"1","title":"Remote flag only","location":"Lisbon","secondaryLocations":[{"location":"Madrid"},{"location":" "}],"publishedAt":"2026-09-01T10:00:00.000+00:00","isRemote":true,"employmentType":"Intern","jobUrl":"https://jobs.ashbyhq.com/acme/1","descriptionHtml":"<p>Hello <b>you</b></p>"},
	 {"id":"2","title":42,"jobUrl":"https://jobs.ashbyhq.com/acme/2"},
	 {"id":"3","title":"Hybrid","workplaceType":"Hybrid","isRemote":true,"jobUrl":"https://jobs.ashbyhq.com/acme/3","isListed":true},
	 {"id":"4","title":"Off host","jobUrl":"https://evil.example/acme/4"},
	 {"id":"5","title":"Bad date","publishedAt":"soon","jobUrl":"https://jobs.ashbyhq.com/acme/5","workplaceType":"OnSite","employmentType":"Temporary"},
	 {"id":"6","title":"Plain http","jobUrl":"http://jobs.ashbyhq.com/acme/6"},
	 {"id":"7","title":"Lookalike","jobUrl":"https://jobs.ashbyhq.com.evil.example/acme/7"}
	]}`
	c, _ := serve(t, 200, body)
	jobs, err := c.ListJobs(context.Background(), "acme")
	if err != nil || len(jobs) != 6 {
		t.Fatalf("jobs = %d, err = %v; want 6 (the numeric title is one odd record)", len(jobs), err)
	}
	if j := jobs[0]; j.RemoteType != "remote" || j.EmploymentType != "internship" || j.LocationRaw != "Lisbon; Madrid" || j.Description != "Hello you" {
		t.Errorf("remote-flag job = %+v", j)
	}
	if jobs[1].RemoteType != "hybrid" {
		t.Errorf("workplaceType must win over isRemote: %+v", jobs[1])
	}
	if jobs[2].URL != "" {
		t.Errorf("off-host URL kept: %q", jobs[2].URL)
	}
	if jobs[4].URL != "" || jobs[5].URL != "" {
		t.Errorf("plain-http and lookalike-host URLs kept: %q, %q", jobs[4].URL, jobs[5].URL)
	}
	if j := jobs[3]; !j.PublishedAt.IsZero() || j.RemoteType != "onsite" || j.EmploymentType != "contract" {
		t.Errorf("bad-date job = %+v, want an unknown date, onsite and contract", j)
	}
}

func TestListJobs_RefusesAnUnsafeBoardTokenWithoutARequest(t *testing.T) {
	c, hits := serve(t, 200, `{"jobs":[]}`)
	for _, tok := range []string{"", "a/b", "../x", "a?b", "a b", "a#b"} {
		if _, err := c.ListJobs(context.Background(), tok); !errors.Is(err, ats.ErrInvalidBoardToken) {
			t.Errorf("token %q: err = %v, want ErrInvalidBoardToken", tok, err)
		}
	}
	if *hits != 0 {
		t.Errorf("%d requests made for invalid tokens", *hits)
	}
	if strings.Contains(endpointBase, "ashbyhq") {
		t.Errorf("test left the real endpoint in place")
	}
}
