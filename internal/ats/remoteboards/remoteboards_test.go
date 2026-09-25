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
func realClient() *httpclient.Client {
	cfg := httpclient.DefaultConfig()
	cfg.MaxResponseBytes = 64 << 20 // above the package's own 8 MiB cap, as ingestion configures it
	return httpclient.New(cfg)
}

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

func himJob(slug string, pub time.Time) string {
	return fmt.Sprintf(`{"title":"Job %s","companyName":"Co %s","employmentType":"Full Time","locationRestrictions":[],"pubDate":%d,"expiryDate":%d,
		"applicationLink":"https://himalayas.app/companies/co/jobs/%s","guid":"https://himalayas.app/companies/co/jobs/%s","description":"<p>Body</p>"}`,
		slug, slug, pub.Unix(), pub.Add(30*24*time.Hour).Unix(), slug, slug)
}

func himPage(cursor string, jobs ...string) string {
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
	site := serveBody(t, 200, []byte(himPage("", himJob("a", fixedNow.Add(-time.Hour)))))
	jobs, err := newHimalayas(site.URL, 1).Collect(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].LocationRaw != "Worldwide" {
		t.Errorf("jobs = %+v, err = %v; want location Worldwide", jobs, err)
	}
}

// The cursor is by creation time and the dates by publication, so one old job
// on a page proves nothing about the pages after it: only a page with nothing
// inside the window ends the run.
func TestHimalayas_StopsOnlyWhenAWholePageIsOutsideTheWindow(t *testing.T) {
	pages := map[string]string{
		"":   himPage("c2", himJob("new1", fixedNow.Add(-1*time.Hour)), himJob("new2", fixedNow.Add(-2*time.Hour))),
		"c2": himPage("c3", himJob("new3", fixedNow.Add(-3*time.Hour)), himJob("old1", fixedNow.Add(-40*24*time.Hour)), himJob("new4", fixedNow.Add(-4*time.Hour))),
		"c3": himPage("c4", himJob("new5", fixedNow.Add(-5*time.Hour))),
		"c4": himPage("c5", himJob("old2", fixedNow.Add(-41*24*time.Hour)), himJob("old3", fixedNow.Add(-42*24*time.Hour))),
		"c5": himPage("c6", himJob("old4", fixedNow.Add(-43*24*time.Hour))),
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
	if !slices.Equal(got, []string{"Job new1", "Job new2", "Job new3", "Job new4", "Job new5"}) {
		t.Errorf("jobs = %v, want every job inside the window, including those after an old one on the same page", got)
	}
	if site.count() != 4 {
		t.Errorf("%d requests, want 4 (page c4 is wholly outside the window, so c5 must not be read)", site.count())
	}
}

func TestHimalayas_MaxPagesBoundsTheRun(t *testing.T) {
	n := 0
	site := newSite(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		_, _ = w.Write([]byte(himPage("more", himJob(fmt.Sprintf("j%d", n), fixedNow.Add(-time.Hour)))))
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

func TestHimalayas_ALaterPageFailingReturnsTheJobsWithAPartialResultError(t *testing.T) {
	site := newSite(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(himPage("c2", himJob("a", fixedNow.Add(-time.Hour)))))
	})
	jobs, err := newHimalayas(site.URL, 5).Collect(context.Background())
	if len(jobs) != 1 {
		t.Errorf("later page 429: %d jobs, want the first page's job kept", len(jobs))
	}
	if !errors.Is(err, ats.ErrPartialResult) || !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want it to wrap ats.ErrPartialResult and ErrRateLimited so the ingester stores the jobs and still reports the failure", err)
	}
	if site.count() != 2 {
		t.Errorf("%d requests, want 2 (a 429 is not retried)", site.count())
	}

	first := serveBody(t, http.StatusTooManyRequests, nil)
	jobs, err = newHimalayas(first.URL, 5).Collect(context.Background())
	if !errors.Is(err, ErrRateLimited) || errors.Is(err, ats.ErrPartialResult) || len(jobs) != 0 {
		t.Errorf("first page 429: %d jobs, err = %v; want a plain ErrRateLimited failure", len(jobs), err)
	}
	empty := serveBody(t, 200, []byte(himPage("")))
	if _, err := newHimalayas(empty.URL, 5).Collect(context.Background()); err == nil {
		t.Errorf("an empty first page was accepted as success")
	}
}

func TestHimalayas_TheURLIsTheListingPageNeverTheApplicationLink(t *testing.T) {
	job := `{"title":"T","companyName":"Acme","employmentType":"Full Time","locationRestrictions":[],"pubDate":%d,"expiryDate":0,
	  "applicationLink":%q,"guid":%q,"description":"x"}`
	pub := fixedNow.Add(-time.Hour).Unix()
	body := himPage("",
		fmt.Sprintf(job, pub, "https://acme.example/apply", "https://himalayas.app/companies/acme/jobs/t-1"), // off-host apply link: ignored
		fmt.Sprintf(job, pub, "https://himalayas.app/companies/acme/jobs/t-2", ""),                           // no guid: the own-host link is the fallback
		fmt.Sprintf(job, pub, "https://acme.example/apply", ""),                                              // neither is on the board: refused
		fmt.Sprintf(job, pub, "javascript:alert(1)", "javascript:alert(2)"),                                  // not a web URL: refused
	)
	jobs, err := newHimalayas(serveBody(t, 200, []byte(body)).URL, 1).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	var urls []string
	for _, j := range jobs {
		urls = append(urls, j.URL)
	}
	want := []string{"https://himalayas.app/companies/acme/jobs/t-1", "https://himalayas.app/companies/acme/jobs/t-2"}
	if !slices.Equal(urls, want) {
		t.Errorf("urls = %v, want %v", urls, want)
	}
}

func TestHimalayas_ACursorIsEscapedIntoTheQuery(t *testing.T) {
	const cursor = "a+b/c=d&e f"
	var got string
	site := newSite(t, func(w http.ResponseWriter, r *http.Request) {
		if c := r.URL.Query().Get("cursor"); c != "" {
			got = c
			_, _ = w.Write([]byte(himPage("")))
			return
		}
		_, _ = w.Write([]byte(himPage(cursor, himJob("a", fixedNow.Add(-time.Hour)))))
	})
	if _, err := newHimalayas(site.URL, 3).Collect(context.Background()); err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	if got != cursor {
		t.Errorf("the server received cursor %q, want %q intact", got, cursor)
	}
}

func TestHimalayas_PagesAreSpaced(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	site := newSite(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		n := len(times)
		mu.Unlock()
		_, _ = w.Write([]byte(himPage("more", himJob(fmt.Sprintf("p%d", n), fixedNow.Add(-time.Hour)))))
	})
	h := NewHimalayas(realClient(), 3, 60*time.Millisecond, discardLogger())
	h.url, h.now = site.URL, func() time.Time { return fixedNow }
	if _, err := h.Collect(context.Background()); err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < 55*time.Millisecond {
			t.Errorf("requests %d and %d were %v apart, want at least the 60ms pause", i, i+1, gap)
		}
	}
}

func TestHimalayas_OneOddRecordCostsOnlyThatJob(t *testing.T) {
	pub := fixedNow.Add(-time.Hour).Unix()
	body := fmt.Sprintf(`{"nextCursor":"","jobs":[
	  {"title":"String restrictions","companyName":"Acme","employmentType":"Full Time","locationRestrictions":"Brazil","pubDate":%[1]d,"guid":"https://himalayas.app/companies/acme/jobs/a","description":"x"},
	  {"title":"Broken date type","companyName":"Acme","pubDate":{"not":"a number"},"guid":"https://himalayas.app/companies/acme/jobs/b"},
	  {"title":"Fine","companyName":"Acme","employmentType":"Full Time","locationRestrictions":["Kenya"],"pubDate":%[1]d,"guid":"https://himalayas.app/companies/acme/jobs/c","description":"x"}
	]}`, pub)
	jobs, err := newHimalayas(serveBody(t, 200, []byte(body)).URL, 1).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v (one odd record must cost only that job)", err)
	}
	got := map[string]string{}
	for _, j := range jobs {
		got[j.Title] = j.LocationRaw
	}
	if len(got) != 2 || got["String restrictions"] != "Brazil" || got["Fine"] != "Kenya" {
		t.Errorf("jobs = %v, want the two decodable jobs (a string restriction read as one country)", got)
	}
}

func TestHimalayas_AnAlreadyExpiredJobIsNotStored(t *testing.T) {
	pub := fixedNow.Add(-time.Hour).Unix()
	body := fmt.Sprintf(`{"nextCursor":"","jobs":[
	  {"title":"Expired","companyName":"Acme","pubDate":%d,"expiryDate":%d,"guid":"https://himalayas.app/companies/acme/jobs/x","description":"x"},
	  {"title":"Live","companyName":"Acme","pubDate":%d,"expiryDate":%d,"guid":"https://himalayas.app/companies/acme/jobs/y","description":"x"}]}`,
		pub, fixedNow.Add(-time.Minute).Unix(), pub, fixedNow.Add(time.Hour).Unix())
	jobs, err := newHimalayas(serveBody(t, 200, []byte(body)).URL, 1).Collect(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].Title != "Live" {
		t.Errorf("jobs = %+v, err = %v; want only the live job", jobs, err)
	}
}

// A later page where no record can be decoded means the format changed: stop,
// keep what was read, report the failure.
func TestHimalayas_ALaterPageOfUndecodableRecordsEndsTheRunWithAPartialResult(t *testing.T) {
	site := newSite(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "" {
			_, _ = w.Write([]byte(`{"nextCursor":"more","jobs":[{"pubDate":{"x":1}},{"pubDate":[1]}]}`))
			return
		}
		_, _ = w.Write([]byte(himPage("c2", himJob("a", fixedNow.Add(-time.Hour)))))
	})
	jobs, err := newHimalayas(site.URL, 20).Collect(context.Background())
	if len(jobs) != 1 || !errors.Is(err, ats.ErrPartialResult) {
		t.Errorf("jobs = %d, err = %v; want the first page's job and a partial-result error", len(jobs), err)
	}
	if site.count() != 2 {
		t.Errorf("%d requests, want 2 (no paging on through a page nothing can be read from)", site.count())
	}
}

func TestHimalayas_CancellationIsReturned(t *testing.T) {
	site := serveBody(t, 200, []byte(himPage("c", himJob("a", fixedNow.Add(-time.Hour)))))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newHimalayas(site.URL, 5).Collect(ctx); err == nil {
		t.Errorf("a canceled context returned success")
	}
}

// ---- behavior every board shares ----

func allBoards() map[string]func(url string) collector {
	return map[string]func(url string) collector{
		"remotive":      func(u string) collector { return newRemotive(u) },
		"remoteok":      func(u string) collector { return newRemoteOK(u) },
		"jobicy":        func(u string) collector { return newJobicy(u) },
		"workingnomads": func(u string) collector { return newWorkingNomads(u) },
		"wwr":           func(u string) collector { return newWWR(u) },
		"himalayas":     func(u string) collector { return newHimalayas(u, 1) },
	}
}

func TestEveryBoard_FailsLoudlyOnBadResponsesAndNeverRetries(t *testing.T) {
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
	for name, mk := range allBoards() {
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

// The size cap must be what rejects an oversized response: the body here is
// valid JSON with usable jobs, only too large.
func TestEveryBoard_AResponseOverTheSizeCapIsAnErrorNotAShorterList(t *testing.T) {
	ok := `{"jobs":[{"id":1,"url":"https://remotive.com/remote-jobs/a-1","title":"Good","company_name":"Acme","publication_date":"2026-09-24T10:00:00"}]` +
		strings.Repeat(" ", maxBodyBytes+10) + `}`
	site := serveBody(t, 200, []byte(ok))
	if _, err := newRemotive(site.URL).Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want the size-cap error", err)
	}
	// The same body under the cap parses and returns the job.
	small := `{"jobs":[{"id":1,"url":"https://remotive.com/remote-jobs/a-1","title":"Good","company_name":"Acme","publication_date":"2026-09-24T10:00:00"}]}`
	jobs, err := newRemotive(serveBody(t, 200, []byte(small)).URL).Collect(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Errorf("under the cap: %d jobs, err = %v", len(jobs), err)
	}
}

func TestEveryBoard_OldJobsAreDroppedAndAnEmptyResultIsNotAnError(t *testing.T) {
	// Same live fixtures, read a year later: every job is outside the window,
	// which is a quiet feed, not a broken one.
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

// If every record in a response is unusable (a renamed field, a changed
// format), that is an error: an empty success would age out every stored job.
func TestEveryBoard_AFormatChangeThatBreaksEveryRecordIsAnError(t *testing.T) {
	rename := func(fixtureName, from, to string) []byte {
		return []byte(strings.ReplaceAll(string(fixture(t, fixtureName)), from, to))
	}
	cases := []struct {
		name string
		mk   func(url string) collector
		body []byte
	}{
		{"remotive renames url", func(u string) collector { return newRemotive(u) }, rename("remotive.json", `"url"`, `"link"`)},
		{"remotive non-numeric ids", func(u string) collector { return newRemotive(u) }, rename("remotive.json", `"id": 2091`, `"id": "x2091`)},
		{"remoteok renames company", func(u string) collector { return newRemoteOK(u) }, rename("remoteok.json", `"company"`, `"employer"`)},
		{"jobicy renames companyName", func(u string) collector { return newJobicy(u) }, rename("jobicy.json", `"companyName"`, `"company"`)},
		{"workingnomads renames url", func(u string) collector { return newWorkingNomads(u) }, rename("workingnomads.json", `"url"`, `"link"`)},
		{"wwr titles lose the colon", func(u string) collector { return newWWR(u) }, rename("wwr.xml", ": ", " - ")},
		{"himalayas renames companyName", func(u string) collector { return newHimalayas(u, 1) }, rename("himalayas.json", `"companyName"`, `"company"`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if bytes := string(tc.body); bytes == "" {
				t.Fatal("empty fixture")
			}
			jobs, err := tc.mk(serveBody(t, 200, tc.body).URL).Collect(context.Background())
			if err == nil {
				t.Fatalf("Collect() = %d jobs, nil; want an error, not a silent empty success", len(jobs))
			}
		})
	}
}

// Every job's URL must be on the board's own host over https: the credit and
// link-back terms are about the board's page.
func TestEveryBoard_RefusesURLsOffTheBoardsOwnHost(t *testing.T) {
	remotive := func(u string) string {
		return fmt.Sprintf(`{"jobs":[{"id":1,"url":%q,"title":"T","company_name":"Acme","publication_date":"2026-09-24T10:00:00"}]}`, u)
	}
	remoteOK := func(u string) string {
		return fmt.Sprintf(`[{"legal":"x"},{"id":"1","epoch":%d,"company":"Acme","position":"T","url":%q}]`, fixedNow.Add(-time.Hour).Unix(), u)
	}
	jobicy := func(u string) string {
		return fmt.Sprintf(`{"jobs":[{"id":1,"url":%q,"jobTitle":"T","companyName":"Acme","pubDate":"2026-09-24T10:00:00+00:00"}]}`, u)
	}
	nomads := func(u string) string {
		return fmt.Sprintf(`[{"url":%q,"title":"T","company_name":"Acme","pub_date":"2026-09-24T10:00:00-04:00"}]`, u)
	}
	wwr := func(u string) string {
		return fmt.Sprintf(`<rss><channel><item><title>Acme: T</title><pubDate>Fri, 25 Sep 2026 00:33:21 +0000</pubDate><link>%s</link></item></channel></rss>`, u)
	}
	him := func(u string) string {
		return fmt.Sprintf(`{"nextCursor":"","jobs":[{"title":"T","companyName":"Acme","pubDate":%d,"guid":%q}]}`, fixedNow.Add(-time.Hour).Unix(), u)
	}
	boards := []struct {
		name string
		mk   func(url string) collector
		body func(u string) string
		good string
	}{
		{"remotive", func(u string) collector { return newRemotive(u) }, remotive, "https://remotive.com/remote-jobs/x-1"},
		{"remoteok", func(u string) collector { return newRemoteOK(u) }, remoteOK, "https://remoteOK.com/remote-jobs/x-1"},
		{"jobicy", func(u string) collector { return newJobicy(u) }, jobicy, "https://jobicy.com/jobs/1-x"},
		{"workingnomads", func(u string) collector { return newWorkingNomads(u) }, nomads, "https://www.workingnomads.com/job/go/1/"},
		{"wwr", func(u string) collector { return newWWR(u) }, wwr, "https://weworkremotely.com/remote-jobs/acme-t"},
		{"himalayas", func(u string) collector { return newHimalayas(u, 1) }, him, "https://himalayas.app/companies/acme/jobs/t"},
	}
	for _, b := range boards {
		host := strings.SplitN(strings.TrimPrefix(b.good, "https://"), "/", 2)[0]
		path := "/" + strings.SplitN(strings.TrimPrefix(b.good, "https://"), "/", 2)[1]
		bad := []string{
			"https://evil.example" + path,
			"http://" + host + path, // not https
			"javascript:alert(1)",
			"https://" + host + ".evil.example" + path, // the board's name as a subdomain of another site
			"https://evil.example/?u=https://" + host + path,
			"//" + host + path, // no scheme
		}
		// workingnomads: an id only counts in the board's own /job/go/<id>/ path.
		if b.name == "workingnomads" {
			bad = append(bad, "https://evil.example/job/go/7/",
				"https://www.workingnomads.com/other/job/go/7/", // an id only counts at the board's own /job/go/<id>/
				"https://www.workingnomads.com/job/go/7/extra")
		}
		for _, u := range bad {
			t.Run(b.name+"/"+u, func(t *testing.T) {
				jobs, err := b.mk(serveBody(t, 200, []byte(b.body(u))).URL).Collect(context.Background())
				if len(jobs) != 0 {
					t.Errorf("accepted %q: %+v", u, jobs)
				}
				if err == nil {
					t.Errorf("a response whose only record is unusable must be an error, got success for %q", u)
				}
			})
		}
		t.Run(b.name+"/own host is accepted", func(t *testing.T) {
			jobs, err := b.mk(serveBody(t, 200, []byte(b.body(b.good))).URL).Collect(context.Background())
			if err != nil || len(jobs) != 1 {
				t.Errorf("%d jobs, err = %v; want the own-host URL accepted", len(jobs), err)
			}
		})
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
	 {"id":7,"url":"https://remotive.com/remote-jobs/a-7","title":"Unreadable date","company_name":"Acme","publication_date":"last tuesday"},
	 {"id":1,"url":"https://remotive.com/remote-jobs/a-1","title":"Good again","company_name":"Acme","publication_date":"2026-09-24T10:00:00"}
	]}`
	jobs, err := newRemotive(serveBody(t, 200, []byte(body)).URL).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v (one odd record must cost only that job)", err)
	}
	if !slices.Equal(ids(jobs), []string{"1"}) || jobs[0].Title != "Good" {
		t.Errorf("jobs = %v, want only the first good job, once (an unreadable date is not kept: it cannot be shown to be fresh)", ids(jobs))
	}
}

func TestJobicy_OneOddRecordCostsOnlyThatJob_AndTheListOrStringJobTypeBothRead(t *testing.T) {
	body := `{"jobs":[
	 {"id":1,"url":"https://jobicy.com/jobs/1-a","jobTitle":"List type","companyName":"Acme","jobType":["Contract"],"jobGeo":"USA","pubDate":"2026-09-24T18:46:47+00:00"},
	 {"id":2,"url":"https://jobicy.com/jobs/2-b","jobTitle":"String type","companyName":"Acme","jobType":"Part-Time","jobGeo":"USA","pubDate":"2026-09-24T18:46:47+00:00"},
	 {"id":"x","url":"https://jobicy.com/jobs/3-c","jobTitle":"Broken id","companyName":"Acme","pubDate":"2026-09-24T18:46:47+00:00"},
	 {"id":4,"url":"https://jobicy.com/jobs/4-d","jobTitle":"Space date","companyName":"Acme","jobType":[],"jobGeo":"Anywhere","pubDate":"2026-09-24 18:46:47"}
	]}`
	jobs, err := newJobicy(serveBody(t, 200, []byte(body)).URL).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	m := byID(jobs)
	if len(m) != 3 || m["1"].EmploymentType != "contract" || m["2"].EmploymentType != "part_time" {
		t.Errorf("jobs = %v; want the list and string job types read and only the broken id lost", ids(jobs))
	}
	if want := time.Date(2026, 9, 24, 18, 46, 47, 0, time.UTC); !m["4"].PublishedAt.Equal(want) {
		t.Errorf("space-separated date read as %v, want %v", m["4"].PublishedAt, want)
	}
}

func TestWWR_FallbacksIDRulesAndDates(t *testing.T) {
	item := func(title, region, country, link, guid, pub string) string {
		return fmt.Sprintf(`<item><title>%s</title><region>%s</region><country>%s</country><pubDate>%s</pubDate><link>%s</link><guid>%s</guid></item>`,
			title, region, country, pub, link, guid)
	}
	feed := `<?xml version="1.0"?><rss><channel>` +
		item("Acme: No country", "Anywhere in the World", "", "https://weworkremotely.com/remote-jobs/acme-a", "", "Fri, 25 Sep 2026 00:33:21 +0000") +
		item("Acme: No link", "Europe Only", "", "", "https://weworkremotely.com/remote-jobs/acme-b", "Fri, 25 Sep 2026 00:33:21 +0000") +
		item("Acme: With tracking", "Anywhere in the World", "", "https://weworkremotely.com/remote-jobs/acme-c?utm_source=x#top", "", "Fri, 25 Sep 2026 00:33:21 +0000") +
		item("Acme: Nested path", "Anywhere in the World", "", "https://weworkremotely.com/remote-jobs/x/acme-d", "", "Fri, 25 Sep 2026 00:33:21 +0000") +
		item("Acme: One digit day", "Anywhere in the World", "", "https://weworkremotely.com/remote-jobs/acme-e", "", "Tue, 8 Sep 2026 10:00:00 +0000") +
		item("Acme: Ends: with colons", "Anywhere in the World", "", "https://weworkremotely.com/remote-jobs/acme-f", "", "Fri, 25 Sep 2026 00:33:21 +0000") +
		`</channel></rss>`
	jobs, err := newWWR(serveBody(t, 200, []byte(feed)).URL).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	m := byID(jobs)
	if got := m["acme-a"].LocationRaw; got != "Anywhere in the World" {
		t.Errorf("region fallback: location = %q", got)
	}
	if j, ok := m["acme-b"]; !ok || j.URL != "https://weworkremotely.com/remote-jobs/acme-b" {
		t.Errorf("guid fallback: %+v", j)
	}
	if j, ok := m["acme-c"]; !ok || j.ExternalID != "acme-c" {
		t.Errorf("a link with a query string and fragment must keep its slug id: %+v", m)
	}
	if _, ok := m["x/acme-d"]; ok || len(m["acme-d"].ExternalID) > 0 {
		t.Errorf("a nested path is not a listing slug: %+v", m)
	}
	if j := m["acme-e"]; j.PublishedAt.IsZero() {
		t.Errorf("a one-digit day of month must parse: %+v", j)
	}
	if j := m["acme-f"]; j.Employer != "Acme" || j.Title != "Ends: with colons" {
		t.Errorf("a title is split at the first ': ': employer %q title %q", j.Employer, j.Title)
	}
}

func TestRemoteOK_DateFallbackWhenNoEpochAndUnreadableDateIsUnusable(t *testing.T) {
	body := `[{"legal":"x"},
	 {"id":"1","epoch":0,"date":"2026-09-24T10:00:00+00:00","company":"Acme","position":"Dated","url":"https://remoteOK.com/remote-jobs/a-1"},
	 {"id":"2","epoch":0,"date":"yesterday","company":"Acme","position":"Bad date","url":"https://remoteOK.com/remote-jobs/a-2"}]`
	jobs, err := newRemoteOK(serveBody(t, 200, []byte(body)).URL).Collect(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].ExternalID != "1" || !jobs[0].PublishedAt.Equal(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("jobs = %+v, err = %v; want the date field used when there is no epoch, and an unreadable one refused", jobs, err)
	}
}

func TestBoardsAreWorldwideAndSamplesAndRateLimited(t *testing.T) {
	for name, mk := range allBoards() {
		c := mk("")
		if c.Market() != market.Worldwide {
			t.Errorf("%s market = %q, want worldwide", name, c.Market())
		}
		if c.StaleAfter() <= 0 {
			t.Errorf("%s StaleAfter = %v, want positive (a feed is a sample)", name, c.StaleAfter())
		}
	}
	// Remotive asks for at most 4 requests a day; Himalayas refreshes daily.
	if d := newRemotive("").MinInterval(); d < 6*time.Hour {
		t.Errorf("Remotive MinInterval = %v, want at least 6h (its terms: at most 4 requests a day)", d)
	}
	if d := newHimalayas("", 1).MinInterval(); d < 12*time.Hour {
		t.Errorf("Himalayas MinInterval = %v, want at least 12h", d)
	}
	for name, mk := range allBoards() {
		if mi, ok := mk("").(interface{ MinInterval() time.Duration }); !ok || mi.MinInterval() <= 0 {
			t.Errorf("%s declares no MinInterval", name)
		}
	}
}

func TestLocationsAreBounded(t *testing.T) {
	long := strings.Repeat("Country, ", 200)
	body := fmt.Sprintf(`{"jobs":[{"id":1,"url":"https://remotive.com/remote-jobs/a-1","title":"T","company_name":"Acme","publication_date":"2026-09-24T10:00:00","candidate_required_location":%q}]}`, long)
	jobs, err := newRemotive(serveBody(t, 200, []byte(body)).URL).Collect(context.Background())
	if err != nil || len(jobs) != 1 || len([]rune(jobs[0].LocationRaw)) > maxLocationRunes {
		t.Errorf("location has %d runes, want at most %d (err %v)", len([]rune(jobs[0].LocationRaw)), maxLocationRunes, err)
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"full_time": "full_time", "Full-Time": "full_time", "Full Time": "full_time", "part_time": "part_time",
		"Contractor": "contract", "freelance": "contract", "Temporary": "contract", "Intern": "internship",
		"Internship": "internship", "": "", "whatever": "",
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
	for in, want := range map[string]time.Time{
		"2026-09-24T10:00:00+00:00":       time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		"2026-09-24T10:00:00":             time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		"2026-09-24 10:00:00":             time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		"2026-09-24T10:00:00-04:00":       time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC),
		"Fri, 25 Sep 2026 00:33:21 +0000": time.Date(2026, 9, 25, 0, 33, 21, 0, time.UTC),
		"Fri, 4 Sep 2026 10:00:00 +0000":  time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC),
		"":                                {}, "soon": {},
	} {
		if got := parseTime(in); !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, want %v", in, got, want)
		}
	}
}
