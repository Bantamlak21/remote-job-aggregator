package remoteboards

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

// fixedNow is the day the fixtures in testdata/ were captured live.
var fixedNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

// fakeSite serves canned responses and records every request it receives.
type fakeSite struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string // "path?query"
	handler  func(w http.ResponseWriter, r *http.Request)
}

func newSite(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fakeSite {
	t.Helper()
	s := &fakeSite{handler: handler}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.RequestURI())
		s.mu.Unlock()
		s.handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func serveBody(t *testing.T, status int, body []byte) *fakeSite {
	t.Helper()
	return newSite(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func (s *fakeSite) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// realClient is the same pooled client production uses, so the tests cover the
// real request path (headers, no retries) and not a fake.
func realClient() *httpclient.Client { return httpclient.New(httpclient.DefaultConfig()) }

func byID(jobs []ats.Job) map[string]ats.Job {
	m := map[string]ats.Job{}
	for _, j := range jobs {
		m[j.ExternalID] = j
	}
	return m
}

func ids(jobs []ats.Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.ExternalID)
	}
	return out
}

// checkCommon asserts what must hold for every job from every board.
func checkCommon(t *testing.T, jobs []ats.Job, wantURLPrefix string) {
	t.Helper()
	for _, j := range jobs {
		if j.ExternalID == "" || j.Title == "" || j.Employer == "" {
			t.Errorf("incomplete job %+v", j)
		}
		if !strings.HasPrefix(strings.ToLower(j.URL), strings.ToLower(wantURLPrefix)) {
			t.Errorf("job %s URL = %q, want the board's own page (%s...) for the link back", j.ExternalID, j.URL, wantURLPrefix)
		}
		if j.RemoteType != "remote" {
			t.Errorf("job %s remote type = %q, want remote (the board lists nothing else)", j.ExternalID, j.RemoteType)
		}
		if j.Closed {
			t.Errorf("job %s is a closure marker", j.ExternalID)
		}
	}
}

// ---- interface conformance ----

type collector interface {
	Collect(ctx context.Context) ([]ats.Job, error)
	StaleAfter() time.Duration
	Market() market.Market
}

var (
	_ collector       = (*Remotive)(nil)
	_ collector       = (*RemoteOK)(nil)
	_ collector       = (*Himalayas)(nil)
	_ collector       = (*Jobicy)(nil)
	_ collector       = (*WeWorkRemotely)(nil)
	_ collector       = (*WorkingNomads)(nil)
	_ market.Provider = (*Remotive)(nil)
)

// ---- Remotive ----

func newRemotive(url string) *Remotive {
	r := NewRemotive(realClient(), discardLogger())
	r.url, r.now = url, func() time.Time { return fixedNow }
	return r
}

func TestRemotive_MapsTheLiveFeed(t *testing.T) {
	site := serveBody(t, 200, fixture(t, "remotive.json"))
	jobs, err := newRemotive(site.URL).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	if got := ids(jobs); !slices.Equal(got, []string{"2091144", "2091141", "2091140", "2091139"}) {
		t.Fatalf("ids = %v", got)
	}
	checkCommon(t, jobs, "https://remotive.com/remote-jobs/")
	m := byID(jobs)
	if j := m["2091140"]; j.Employer != "Sanctuary Computer Inc" || j.Title != "Senior Shopify Developer" ||
		j.LocationRaw != "Worldwide" || j.EmploymentType != "contract" ||
		!j.PublishedAt.Equal(time.Date(2026, 9, 18, 15, 10, 28, 0, time.UTC)) || j.Description == "" {
		t.Errorf("Shopify job = %+v", j)
	}
	if got := m["2091144"].EmploymentType; got != "part_time" {
		t.Errorf("part_time job employment = %q", got)
	}
	if got := m["2091139"].EmploymentType; got != "contract" { // freelance
		t.Errorf("freelance job employment = %q, want contract", got)
	}
	if got := m["2091141"].LocationRaw; got != "USA, Canada, Argentina, Mexico, Peru" {
		t.Errorf("eligibility location = %q, want the candidate-required countries kept", got)
	}
	if !strings.Contains(m["2091139"].Title, "Kundenservice") {
		t.Errorf("non-ASCII title lost: %q", m["2091139"].Title)
	}
}

// ---- Remote OK ----

func newRemoteOK(url string) *RemoteOK {
	r := NewRemoteOK(realClient(), discardLogger())
	r.url, r.now = url, func() time.Time { return fixedNow }
	return r
}

func TestRemoteOK_SkipsTheLegalNoticeAndMapsJobs(t *testing.T) {
	site := serveBody(t, 200, fixture(t, "remoteok.json"))
	jobs, err := newRemoteOK(site.URL).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	if got := ids(jobs); !slices.Equal(got, []string{"1137429", "1137428", "1137427", "1137421"}) {
		t.Fatalf("ids = %v (the first element is the API's legal notice, not a job)", got)
	}
	checkCommon(t, jobs, "https://remoteok.com/remote-jobs/")
	j := byID(jobs)["1137421"]
	if j.Employer != "Bjak" || j.LocationRaw != "Germany" {
		t.Errorf("job = employer %q location %q; want the trailing space trimmed and the location kept", j.Employer, j.LocationRaw)
	}
	if want := time.Date(2026, 9, 23, 0, 0, 10, 0, time.UTC); !j.PublishedAt.Equal(want) {
		t.Errorf("PublishedAt = %v, want %v (from epoch)", j.PublishedAt, want)
	}
}

func TestRemoteOK_AFeedWithOnlyTheNoticeIsAnError(t *testing.T) {
	site := serveBody(t, 200, []byte(`[{"last_updated":1,"legal":"terms"}]`))
	if _, err := newRemoteOK(site.URL).Collect(context.Background()); err == nil {
		t.Errorf("a feed with no jobs was accepted as success (it would age out every stored job)")
	}
}

// ---- Jobicy ----

func newJobicy(url string) *Jobicy {
	r := NewJobicy(realClient(), discardLogger())
	r.url, r.now = url, func() time.Time { return fixedNow }
	return r
}

func TestJobicy_MapsTheLiveFeed(t *testing.T) {
	site := serveBody(t, 200, fixture(t, "jobicy.json"))
	jobs, err := newJobicy(site.URL).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	if got := ids(jobs); !slices.Equal(got, []string{"154057", "154059", "154051", "154052"}) {
		t.Fatalf("ids = %v", got)
	}
	checkCommon(t, jobs, "https://jobicy.com/jobs/")
	j := byID(jobs)["154051"]
	if j.Employer != "Dune" || j.Title != "Staff Software Engineer - Curated Data" || j.EmploymentType != "full_time" {
		t.Errorf("job = %+v", j)
	}
	if j.LocationRaw != "Europe, USA" { // "Europe,  USA" with a double space in the feed
		t.Errorf("location = %q, want the doubled space collapsed", j.LocationRaw)
	}
	if !j.PublishedAt.Equal(time.Date(2026, 9, 24, 18, 46, 46, 0, time.UTC)) {
		t.Errorf("PublishedAt = %v", j.PublishedAt)
	}
}

// ---- Working Nomads ----

func newWorkingNomads(url string) *WorkingNomads {
	r := NewWorkingNomads(realClient(), discardLogger())
	r.url, r.now = url, func() time.Time { return fixedNow }
	return r
}

func TestWorkingNomads_TakesTheIDFromTheRedirectURL(t *testing.T) {
	site := serveBody(t, 200, fixture(t, "workingnomads.json"))
	jobs, err := newWorkingNomads(site.URL).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	if got := ids(jobs); !slices.Equal(got, []string{"1889808", "1885527", "1884997", "1879612"}) {
		t.Fatalf("ids = %v", got)
	}
	checkCommon(t, jobs, "https://www.workingnomads.com/job/go/")
	j := byID(jobs)["1889808"]
	if j.Employer != "CloudGeometry" || j.LocationRaw != "Brazil" {
		t.Errorf("job = %+v", j)
	}
	// 2026-09-24T10:57:36-04:00
	if want := time.Date(2026, 9, 24, 14, 57, 36, 0, time.UTC); !j.PublishedAt.Equal(want) {
		t.Errorf("PublishedAt = %v, want %v", j.PublishedAt, want)
	}
}

func TestWorkingNomads_ARecordWithoutAnIDInItsURLIsSkipped(t *testing.T) {
	body := `[{"url":"https://www.workingnomads.com/jobs","title":"No id","company_name":"X","pub_date":"2026-09-24T10:00:00-04:00"},
	         {"url":"https://www.workingnomads.com/job/go/77/","title":"Has id","company_name":"X","pub_date":"2026-09-24T10:00:00-04:00"}]`
	jobs, err := newWorkingNomads(serveBody(t, 200, []byte(body)).URL).Collect(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].ExternalID != "77" {
		t.Errorf("jobs = %+v, err = %v; want only the job whose URL carries an id", jobs, err)
	}
}

// ---- We Work Remotely ----

func newWWR(url string) *WeWorkRemotely {
	r := NewWeWorkRemotely(realClient(), discardLogger())
	r.url, r.now = url, func() time.Time { return fixedNow }
	return r
}

func TestWWR_SplitsCompanyFromTitleAndMapsTheFeed(t *testing.T) {
	site := serveBody(t, 200, fixture(t, "wwr.xml"))
	jobs, err := newWWR(site.URL).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	if len(jobs) != 4 {
		t.Fatalf("got %d jobs, want 4: %v", len(jobs), ids(jobs))
	}
	checkCommon(t, jobs, "https://weworkremotely.com/remote-jobs/")
	j := byID(jobs)["thehivecareers-co-devops-engineer-remote"]
	if j.Employer != "thehivecareers.co" || j.Title != "DevOps Engineer (Remote)" || j.EmploymentType != "full_time" {
		t.Errorf("job = %+v", j)
	}
	if !j.PublishedAt.Equal(time.Date(2026, 9, 25, 0, 33, 21, 0, time.UTC)) || !j.ExpiresAt.Equal(time.Date(2026, 10, 25, 0, 33, 21, 0, time.UTC)) {
		t.Errorf("dates = %v / %v", j.PublishedAt, j.ExpiresAt)
	}
	if strings.Contains(j.LocationRaw, "\U0001F1E7") {
		t.Errorf("location %q still has flag emoji", j.LocationRaw)
	}
	if !strings.Contains(j.LocationRaw, "Barbados") || !strings.Contains(j.LocationRaw, "United States of America") {
		t.Errorf("location = %q, want the countries the listing names", j.LocationRaw)
	}
}

func TestWWR_ExpiredAndUnsplittableItemsAreDropped(t *testing.T) {
	feed := `<?xml version="1.0"?><rss><channel>
	<item><title>Acme: Live job</title><region>Anywhere in the World</region><country></country><type>Full-Time</type><description>x</description>
	  <pubDate>Fri, 25 Sep 2026 00:33:21 +0000</pubDate><expires_at>Sun, 25 Oct 2026 00:33:21 +0000</expires_at><link>https://weworkremotely.com/remote-jobs/acme-live-job</link></item>
	<item><title>Acme: Expired job</title><region>Anywhere in the World</region><description>x</description>
	  <pubDate>Fri, 25 Sep 2026 00:33:21 +0000</pubDate><expires_at>Thu, 24 Sep 2026 00:00:00 +0000</expires_at><link>https://weworkremotely.com/remote-jobs/acme-expired-job</link></item>
	<item><title>No colon in this title</title><region>Anywhere in the World</region><description>x</description>
	  <pubDate>Fri, 25 Sep 2026 00:33:21 +0000</pubDate><link>https://weworkremotely.com/remote-jobs/no-colon</link></item>
	<item><title>Acme: Bad link</title><region>Anywhere in the World</region><description>x</description>
	  <pubDate>Fri, 25 Sep 2026 00:33:21 +0000</pubDate><link>https://weworkremotely.com/somewhere-else</link></item>
	</channel></rss>`
	jobs, err := newWWR(serveBody(t, 200, []byte(feed)).URL).Collect(context.Background())
	if err != nil || !slices.Equal(ids(jobs), []string{"acme-live-job"}) {
		t.Errorf("jobs = %v, err = %v; want only the live, well-formed item", ids(jobs), err)
	}
}

// ---- Himalayas ----

func newHimalayas(url string, maxPages int) *Himalayas {
	h := NewHimalayas(realClient(), maxPages, 0, discardLogger())
	h.url, h.now = url, func() time.Time { return fixedNow }
	return h
}

func himalayasJob(slug string, pub time.Time) string {
	return fmt.Sprintf(`{"title":"Job %s","companyName":"Co %s","employmentType":"Full Time","locationRestrictions":[],"pubDate":%d,"expiryDate":%d,
		"applicationLink":"https://himalayas.app/companies/co/jobs/%s","guid":"https://himalayas.app/companies/co/jobs/%s","description":"<p>Body</p>"}`,
		slug, slug, pub.Unix(), pub.Add(30*24*time.Hour).Unix(), slug, slug)
}

func himalayasPageBody(cursor string, jobs ...string) string {
	return fmt.Sprintf(`{"nextCursor":%q,"jobs":[%s]}`, cursor, strings.Join(jobs, ","))
}

func TestHimalayas_MapsTheLiveFeedAndReadsTheCursor(t *testing.T) {
	// The fixture's nextCursor is set, so a second page is requested; the
	// server ends the feed there.
	site := newSite(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "" {
			_, _ = w.Write([]byte(`{"nextCursor":"","jobs":[]}`))
			return
		}
		_, _ = w.Write(fixture(t, "himalayas.json"))
	})
	jobs, err := newHimalayas(site.URL, 5).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	if len(jobs) != 4 {
		t.Fatalf("got %d jobs, want 4", len(jobs))
	}
	checkCommon(t, jobs, "https://himalayas.app/companies/")
	j := jobs[1] // Medical Biller, restricted to South Africa
	if j.Employer != "ReWorks Solutions" || j.LocationRaw != "South Africa" || j.EmploymentType != "full_time" {
		t.Errorf("job = %+v", j)
	}
	if j.ExternalID != "companies/reworks-solutions/jobs/medical-biller" {
		t.Errorf("ExternalID = %q, want the listing path", j.ExternalID)
	}
	if j.ExpiresAt.IsZero() || !j.PublishedAt.Equal(time.Unix(1790306284, 0).UTC()) {
		t.Errorf("dates = %v / %v", j.PublishedAt, j.ExpiresAt)
	}
	// Page 1 has no cursor, page 2 carries the one page 1 returned, and both ask for 20.
	if site.count() != 2 || !strings.Contains(site.requests[0], "limit=20") || strings.Contains(site.requests[0], "cursor=") ||
		!strings.Contains(site.requests[1], "cursor=") {
		t.Errorf("requests = %v", site.requests)
	}
}

func TestHimalayas_NoRestrictionsMeansWorldwide(t *testing.T) {
	site := serveBody(t, 200, []byte(himalayasPageBody("", himalayasJob("a", fixedNow.Add(-time.Hour)))))
	jobs, err := newHimalayas(site.URL, 1).Collect(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].LocationRaw != "Worldwide" {
		t.Errorf("jobs = %+v, err = %v; want location Worldwide", jobs, err)
	}
}

func TestHimalayas_StopsAtTheFirstPagePastTheAgeWindow(t *testing.T) {
	pages := map[string]string{
		"":   himalayasPageBody("c2", himalayasJob("new1", fixedNow.Add(-1*time.Hour)), himalayasJob("new2", fixedNow.Add(-2*time.Hour))),
		"c2": himalayasPageBody("c3", himalayasJob("new3", fixedNow.Add(-24*time.Hour)), himalayasJob("old1", fixedNow.Add(-40*24*time.Hour))),
		"c3": himalayasPageBody("c4", himalayasJob("old2", fixedNow.Add(-41*24*time.Hour))),
	}
	site := newSite(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pages[r.URL.Query().Get("cursor")]))
	})
	jobs, err := newHimalayas(site.URL, 10).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	var got []string
	for _, j := range jobs {
		got = append(got, j.Title)
	}
	if !slices.Equal(got, []string{"Job new1", "Job new2", "Job new3"}) {
		t.Errorf("jobs = %v, want the three inside the window", got)
	}
	if site.count() != 2 {
		t.Errorf("%d requests, want 2 (page 3 must not be read once page 2 passed the window)", site.count())
	}
}

func TestHimalayas_MaxPagesBoundsTheRun(t *testing.T) {
	n := 0
	site := newSite(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		_, _ = w.Write([]byte(himalayasPageBody("more", himalayasJob(fmt.Sprintf("j%d", n), fixedNow.Add(-time.Hour)))))
	})
	jobs, err := newHimalayas(site.URL, 3).Collect(context.Background())
	if err != nil || len(jobs) != 3 || site.count() != 3 {
		t.Errorf("jobs = %d, requests = %d, err = %v; want 3 pages read and no more", len(jobs), site.count(), err)
	}
	if h := NewHimalayas(realClient(), 100000, 0, discardLogger()); h.maxPages != hardHimalayasPages {
		t.Errorf("maxPages = %d, want it capped at %d", h.maxPages, hardHimalayasPages)
	}
	if h := NewHimalayas(realClient(), 0, 0, discardLogger()); h.maxPages != DefaultHimalayasPages {
		t.Errorf("maxPages = %d, want the default %d", h.maxPages, DefaultHimalayasPages)
	}
}

func TestHimalayas_ALaterPageFailingKeepsWhatWasCollected_TheFirstFailingIsAnError(t *testing.T) {
	site := newSite(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(himalayasPageBody("c2", himalayasJob("a", fixedNow.Add(-time.Hour)))))
	})
	jobs, err := newHimalayas(site.URL, 5).Collect(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Errorf("later page 429: jobs = %d, err = %v; want the first page's job kept", len(jobs), err)
	}
	if site.count() != 2 {
		t.Errorf("%d requests, want 2 (a 429 is not retried)", site.count())
	}

	first := serveBody(t, http.StatusTooManyRequests, nil)
	if _, err := newHimalayas(first.URL, 5).Collect(context.Background()); !errors.Is(err, ErrRateLimited) {
		t.Errorf("first page 429: err = %v, want ErrRateLimited", err)
	}
	empty := serveBody(t, 200, []byte(himalayasPageBody("")))
	if _, err := newHimalayas(empty.URL, 5).Collect(context.Background()); err == nil {
		t.Errorf("an empty first page was accepted as success")
	}
}

func TestHimalayas_CancellationIsReturned(t *testing.T) {
	site := serveBody(t, 200, []byte(himalayasPageBody("c", himalayasJob("a", fixedNow.Add(-time.Hour)))))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newHimalayas(site.URL, 5).Collect(ctx); err == nil {
		t.Errorf("a canceled context returned success")
	}
}

// ---- behavior every board shares ----

func TestEveryBoard_FailsLoudlyOnBadResponsesAndNeverRetries(t *testing.T) {
	makers := map[string]func(url string) collector{
		"remotive":      func(u string) collector { return newRemotive(u) },
		"remoteok":      func(u string) collector { return newRemoteOK(u) },
		"jobicy":        func(u string) collector { return newJobicy(u) },
		"workingnomads": func(u string) collector { return newWorkingNomads(u) },
		"wwr":           func(u string) collector { return newWWR(u) },
		"himalayas":     func(u string) collector { return newHimalayas(u, 1) },
	}
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"500", 500, "oops"},
		{"429", 429, "slow down"},
		{"403", 403, "no"},
		{"not the expected format", 200, "<html>maintenance</html>"},
		{"empty object", 200, "{}"},
		{"empty array", 200, "[]"},
	}
	for name, mk := range makers {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				site := serveBody(t, tc.status, []byte(tc.body))
				jobs, err := mk(site.URL).Collect(context.Background())
				if err == nil {
					t.Fatalf("Collect() = %d jobs, nil; want an error (an empty result would close every stored job)", len(jobs))
				}
				if site.count() != 1 {
					t.Errorf("%d requests, want exactly 1: these boards ask for few requests and block excessive ones", site.count())
				}
				if tc.status == 429 && !errors.Is(err, ErrRateLimited) {
					t.Errorf("err = %v, want ErrRateLimited", err)
				}
			})
		}
	}
}

func TestEveryBoard_AResponseOverTheSizeCapIsAnErrorNotAShorterList(t *testing.T) {
	big := []byte(`{"jobs":[` + strings.Repeat(" ", maxBodyBytes+10) + `]}`)
	site := serveBody(t, 200, big)
	if _, err := newRemotive(site.URL).Collect(context.Background()); err == nil {
		t.Errorf("an oversized response was accepted")
	}
}

func TestEveryBoard_OldJobsAreDroppedAndAnEmptyResultIsNotAnError(t *testing.T) {
	// Same live fixtures, read a year later: every job is outside the window.
	later := func() time.Time { return fixedNow.Add(365 * 24 * time.Hour) }
	r := newRemotive(serveBody(t, 200, fixture(t, "remotive.json")).URL)
	r.now = later
	if jobs, err := r.Collect(context.Background()); err != nil || len(jobs) != 0 {
		t.Errorf("old remotive jobs: %d, %v; want none and no error (the feed itself was fine)", len(jobs), err)
	}
	o := newRemoteOK(serveBody(t, 200, fixture(t, "remoteok.json")).URL)
	o.now = later
	if jobs, err := o.Collect(context.Background()); err != nil || len(jobs) != 0 {
		t.Errorf("old remoteok jobs: %d, %v; want none and no error", len(jobs), err)
	}
}

func TestEveryBoard_HiddenEmployersAndBrokenRecordsAreSkipped(t *testing.T) {
	body := `{"jobs":[
	 {"id":1,"url":"https://remotive.com/remote-jobs/a-1","title":"Good","company_name":"Acme","job_type":"full_time","publication_date":"2026-09-24T10:00:00","candidate_required_location":"Worldwide","description":"<p>x</p>"},
	 {"id":2,"url":"https://remotive.com/remote-jobs/a-2","title":"Hidden","company_name":"Confidential","publication_date":"2026-09-24T10:00:00"},
	 {"id":3,"url":"https://remotive.com/remote-jobs/a-3","title":"   ","company_name":"Acme","publication_date":"2026-09-24T10:00:00"},
	 {"id":"abc","url":"https://remotive.com/remote-jobs/a-4","title":"Bad id","company_name":"Acme","publication_date":"2026-09-24T10:00:00"},
	 {"id":5,"url":"","title":"No url","company_name":"Acme","publication_date":"2026-09-24T10:00:00"},
	 {"id":6,"url":"https://remotive.com/remote-jobs/a-6","title":"No employer","company_name":"  ","publication_date":"2026-09-24T10:00:00"},
	 {"id":1,"url":"https://remotive.com/remote-jobs/a-1","title":"Good again","company_name":"Acme","publication_date":"2026-09-24T10:00:00"}
	]}`
	jobs, err := newRemotive(serveBody(t, 200, []byte(body)).URL).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v (one odd record must cost only that job)", err)
	}
	if !slices.Equal(ids(jobs), []string{"1"}) || jobs[0].Title != "Good" {
		t.Errorf("jobs = %v, want only the first good job, once", ids(jobs))
	}
}

func TestBoardsAreWorldwideAndSamples(t *testing.T) {
	for name, c := range map[string]collector{
		"remotive": newRemotive(""), "remoteok": newRemoteOK(""), "jobicy": newJobicy(""),
		"workingnomads": newWorkingNomads(""), "wwr": newWWR(""), "himalayas": newHimalayas("", 1),
	} {
		if c.Market() != market.Worldwide {
			t.Errorf("%s market = %q, want worldwide", name, c.Market())
		}
		if c.StaleAfter() <= 0 {
			t.Errorf("%s StaleAfter = %v, want positive (a feed is a sample)", name, c.StaleAfter())
		}
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"full_time": "full_time", "Full-Time": "full_time", "Full Time": "full_time", "part_time": "part_time",
		"Contractor": "contract", "freelance": "contract", "Temporary": "contract", "Intern": "internship",
		"": "", "whatever": "",
	} {
		if got := employment(in); got != want {
			t.Errorf("employment(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"Agra, ": "Agra", "Europe,  USA": "Europe, USA", "  Worldwide ": "Worldwide", "": "", "A ,B": "A,B",
	} {
		if got := cleanLocation(in); got != want {
			t.Errorf("cleanLocation(%q) = %q, want %q", in, got, want)
		}
	}
	if epoch(0) != (time.Time{}) || epoch(-5) != (time.Time{}) || epoch(1790306400).IsZero() {
		t.Errorf("epoch mishandles zero, negative or valid seconds")
	}
	long := strings.Repeat("é", maxDescriptionRunes+50)
	if got := []rune(truncateRunes(long, maxDescriptionRunes)); len(got) != maxDescriptionRunes {
		t.Errorf("truncateRunes kept %d runes, want %d", len(got), maxDescriptionRunes)
	}
}
