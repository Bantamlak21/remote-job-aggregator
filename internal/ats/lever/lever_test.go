package lever

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
		if !strings.HasSuffix(r.URL.Path, "/acme") || r.URL.Query().Get("mode") != "json" {
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

// Live capture (2026-09-25): two Toptal postings and one Spotify posting, trimmed.
func TestListJobs_MapsRealPostings(t *testing.T) {
	body, err := os.ReadFile("testdata/postings.json")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := serve(t, 200, string(body))
	jobs, err := c.ListJobs(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ListJobs() failed: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs, want 3", len(jobs))
	}
	remote := jobs[0]
	if remote.Title != "Account Executive" || remote.RemoteType != "remote" || remote.EmploymentType != "full_time" ||
		remote.LocationRaw != "Canada, Central America, South America" {
		t.Errorf("remote posting = %+v", remote)
	}
	if !strings.HasPrefix(remote.URL, "https://jobs.lever.co/toptal/") || remote.ExternalID == "" || remote.Description == "" {
		t.Errorf("remote posting URL/id/description = %q / %q / %q", remote.URL, remote.ExternalID, remote.Description)
	}
	if want := time.UnixMilli(1789027672366).UTC(); !remote.PublishedAt.Equal(want) {
		t.Errorf("PublishedAt = %v, want %v (createdAt is Unix milliseconds)", remote.PublishedAt, want)
	}
	hybrid := jobs[2]
	if hybrid.RemoteType != "hybrid" || hybrid.EmploymentType != "full_time" || hybrid.LocationRaw != "London; Stockholm" {
		t.Errorf("hybrid posting = %+v (Permanent is full time)", hybrid)
	}
}

func TestListJobs_ABoardWithNoOpeningsIsAnEmptyListAnUnknownOneIsNotFound(t *testing.T) {
	c, _ := serve(t, 200, "[]")
	jobs, err := c.ListJobs(context.Background(), "acme")
	if err != nil || len(jobs) != 0 {
		t.Errorf("empty board: %d jobs, err = %v; want none and no error", len(jobs), err)
	}
	c, _ = serve(t, 404, `{"ok":false,"error":"Document not found"}`)
	if _, err := c.ListJobs(context.Background(), "acme"); !errors.Is(err, ats.ErrBoardNotFound) {
		t.Errorf("unknown board: err = %v, want ErrBoardNotFound", err)
	}
}

func TestListJobs_ChangedFormatsAndFailuresAreErrorsNeverAnEmptyList(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"object instead of array": {200, `{"ok":true}`},
		"html":                    {200, `<html>maintenance</html>`},
		"empty body":              {200, ``},
		"server error":            {500, `oops`},
		"rate limited":            {429, `slow down`},
	} {
		c, _ := serve(t, tc.status, tc.body)
		if jobs, err := c.ListJobs(context.Background(), "acme"); err == nil {
			t.Errorf("%s: %d jobs, nil; want an error (an empty result would close every stored job)", name, len(jobs))
		}
	}
}

func TestListJobs_OneOddPostingCostsOnlyThatJob_AndAnOffHostURLIsDropped(t *testing.T) {
	body := `[
	 {"id":"1","text":"Good","hostedUrl":"https://jobs.lever.co/acme/1","createdAt":1789027672366,"workplaceType":"remote","categories":{"location":"Remote","commitment":"Contract"},"descriptionPlain":"Do it."},
	 {"id":"2","text":42,"hostedUrl":"https://jobs.lever.co/acme/2"},
	 {"id":"3","text":"Elsewhere","hostedUrl":"https://evil.example/acme/3","createdAt":1789027672366},
	 {"id":"4","text":"Unspecified","hostedUrl":"https://jobs.lever.co/acme/4","workplaceType":"unspecified","categories":{"commitment":"Whatever"}},
	 {"id":"5","text":"Plain http","hostedUrl":"http://jobs.lever.co/acme/5"},
	 {"id":"6","text":"Lookalike","hostedUrl":"https://jobs.lever.co.evil.example/acme/6"}
	]`
	c, _ := serve(t, 200, body)
	jobs, err := c.ListJobs(context.Background(), "acme")
	if err != nil || len(jobs) != 5 {
		t.Fatalf("jobs = %d, err = %v; want 5 (the numeric title is one odd record)", len(jobs), err)
	}
	if jobs[0].EmploymentType != "contract" || jobs[0].RemoteType != "remote" {
		t.Errorf("good = %+v", jobs[0])
	}
	if jobs[1].URL != "" {
		t.Errorf("off-host URL kept: %q (the job is then skipped by ingestion)", jobs[1].URL)
	}
	if jobs[2].RemoteType != "" || jobs[2].EmploymentType != "" {
		t.Errorf("unspecified posting = %+v, want the source's silence kept as empty", jobs[2])
	}
	if jobs[3].URL != "" || jobs[4].URL != "" {
		t.Errorf("plain-http and lookalike-host URLs kept: %q, %q", jobs[3].URL, jobs[4].URL)
	}
}

func TestListJobs_MultipleLocationsAreJoined_AndTheDescriptionComposes(t *testing.T) {
	body := `[{"id":"1","text":"T","hostedUrl":"https://jobs.lever.co/acme/1","categories":{"location":"Berlin","allLocations":["Berlin","London","Remote"]},
	  "descriptionPlain":"Opening.","lists":[{"text":"What you'll do","content":"<li>Build</li><li>Ship</li>"}],"additionalPlain":"Closing."}]`
	c, _ := serve(t, 200, body)
	jobs, _ := c.ListJobs(context.Background(), "acme")
	if len(jobs) != 1 || jobs[0].LocationRaw != "Berlin; London; Remote" {
		t.Fatalf("jobs = %+v", jobs)
	}
	d := jobs[0].Description
	if !strings.Contains(d, "Opening.") || !strings.Contains(d, "What you'll do") || !strings.Contains(d, "Build") || !strings.Contains(d, "Closing.") {
		t.Errorf("description = %q, want the opening, the titled list and the closing", d)
	}
	if strings.Index(d, "Opening.") > strings.Index(d, "Closing.") {
		t.Errorf("description parts are out of order: %q", d)
	}
}

func TestListJobs_RefusesAnUnsafeBoardTokenWithoutARequest(t *testing.T) {
	c, hits := serve(t, 200, "[]")
	for _, tok := range []string{"", "a/b", "../x", "a?b", "a b", "a#b"} {
		if _, err := c.ListJobs(context.Background(), tok); !errors.Is(err, ats.ErrInvalidBoardToken) {
			t.Errorf("token %q: err = %v, want ErrInvalidBoardToken", tok, err)
		}
	}
	if *hits != 0 {
		t.Errorf("%d requests made for invalid tokens", *hits)
	}
}
