package careers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
)

var fixedNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// fakePages serves canned HTML per URL through the real page.Parse, so the
// tests exercise the same extraction the live fetcher does.
type fakePages struct {
	html map[string]string
	errs map[string]error

	mu      sync.Mutex
	fetched []string
}

func (f *fakePages) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fetched)
}

func (f *fakePages) Fetch(_ context.Context, rawURL string) (*page.Page, error) {
	f.mu.Lock()
	f.fetched = append(f.fetched, rawURL)
	f.mu.Unlock()
	if err := f.errs[rawURL]; err != nil {
		return nil, err
	}
	body, ok := f.html[rawURL]
	if !ok {
		return nil, fmt.Errorf("fake: %s: %w", rawURL, page.ErrNotFound)
	}
	return page.Parse(rawURL, []byte(body))
}

func newClient(f *fakePages) *Client {
	c := New(f, 0, 0, 0)
	c.now = func() time.Time { return fixedNow }
	return c
}

const listingHTML = `<html><body>
<nav><a href="/">Home</a><a href="/about">About</a><a href="/jobs/">Jobs</a></nav>
<div>
 <a href="/jobs/legal-counsel/"><img src="x.png"></a>
 <a href="/jobs/legal-counsel/">Legal Counsel</a>
 <a href="/jobs/data-analyst/?ref=home#apply">Read more</a>
 <a href="https://www.example.et/jobs/qa-engineer">QA Engineer</a>
 <a href="/jobs/feed/">RSS</a>
 <a href="/jobs/page/2/">Next</a>
 <a href="/jobs/apply/">Apply</a>
 <a href="/jobs/handbook.pdf">Handbook</a>
 <a href="/jobs/tag/remote/">remote</a>
 <a href="/blog/post-1/">Blog</a>
 <a href="https://other.example/jobs/x">Elsewhere</a>
 <a href="/jobs">Jobs again</a>
</div></body></html>`

func detailPage(title, date, extra string) string {
	meta := ""
	if date != "" {
		meta = `<meta property="article:published_time" content="` + date + `" />`
	}
	return `<html><head><title>` + title + ` - Example S.C</title>` + meta + extra + `</head>
<body><header><a href="/">Example</a></header><main><h1>` + title + `</h1><p>Responsibilities: do the work.</p></main></body></html>`
}

func TestListJobs_CrawlsSameSiteLinksBelowTheListingPath(t *testing.T) {
	f := &fakePages{html: map[string]string{
		"https://example.et/jobs/":                listingHTML,
		"https://example.et/jobs/legal-counsel/":  detailPage("Legal Counsel", "2026-09-10T08:00:00+00:00", ""),
		"https://example.et/jobs/data-analyst/":   detailPage("Data Analyst", "", ""),
		"https://www.example.et/jobs/qa-engineer": detailPage("QA Engineer", "2026-09-01", ""),
	}}
	jobs, err := newClient(f).ListJobs(context.Background(), "https://example.et/jobs/")
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}

	got := map[string]ats.Job{}
	for _, j := range jobs {
		got[j.ExternalID] = j
	}
	want := []string{"example.et/jobs/legal-counsel", "example.et/jobs/data-analyst", "example.et/jobs/qa-engineer"}
	if len(jobs) != len(want) {
		t.Fatalf("got %d jobs %v, want exactly %v (feed, pagination, apply, files, tags, other paths and other hosts are not jobs)",
			len(jobs), ids(jobs), want)
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("missing %q in %v", id, ids(jobs))
		}
	}

	lc := got["example.et/jobs/legal-counsel"]
	// The URL is kept exactly as the site linked it (trailing slash and all,
	// so no redirect is needed), minus query and fragment.
	if lc.Title != "Legal Counsel" || lc.URL != "https://example.et/jobs/legal-counsel/" {
		t.Errorf("legal-counsel = %+v", lc)
	}
	if !lc.PublishedAt.Equal(time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("PublishedAt = %v, want article:published_time", lc.PublishedAt)
	}
	if !strings.Contains(lc.Description, "do the work") {
		t.Errorf("Description = %q, want the page's main text", lc.Description)
	}
	if strings.Contains(lc.Description, "Example") && strings.Contains(lc.Description, "header") {
		t.Errorf("Description leaked navigation: %q", lc.Description)
	}
	// The query string and fragment are not part of a job's identity or URL.
	if da := got["example.et/jobs/data-analyst"]; da.URL != "https://example.et/jobs/data-analyst/" || da.Title != "Data Analyst" {
		t.Errorf("data-analyst = %+v (query/fragment must be dropped; generic 'Read more' anchor must not be the title)", da)
	}
	if !got["example.et/jobs/data-analyst"].PublishedAt.IsZero() {
		t.Errorf("undated job has PublishedAt %v, want zero", got["example.et/jobs/data-analyst"].PublishedAt)
	}
}

func ids(jobs []ats.Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ExternalID
	}
	return out
}

func TestListJobs_DropsStaleAndExpiredPostings(t *testing.T) {
	ldExpired := `<script type="application/ld+json">{"@type":"JobPosting","title":"Expired","validThrough":"2026-09-01"}</script>`
	ldFresh := `<script type="application/ld+json">{"@type":"JobPosting","title":"Fresh","datePosted":"2026-09-20","validThrough":"2026-10-30",
		"description":"<p>Real <b>description</b></p>","jobLocation":{"address":{"addressLocality":"Addis Ababa"}}}</script>`
	listing := `<a href="/careers/a">A</a><a href="/careers/b">B</a><a href="/careers/c">C</a><a href="/careers/d">D</a>`
	f := &fakePages{html: map[string]string{
		"https://example.et/careers":   listing,
		"https://example.et/careers/a": detailPage("Kifiya-era posting", "2025-02-25T08:35:52+00:00", ""),
		"https://example.et/careers/b": detailPage("Expired", "", ldExpired),
		"https://example.et/careers/c": detailPage("Fresh", "", ldFresh),
		"https://example.et/careers/d": detailPage("Just under the limit", "2026-06-01", ""),
	}}
	jobs, err := newClient(f).ListJobs(context.Background(), "https://example.et/careers")
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %v, want only the fresh and the recent postings (19-month-old and expired ones dropped)", ids(jobs))
	}
	byID := map[string]ats.Job{}
	for _, j := range jobs {
		byID[j.ExternalID] = j
	}
	fresh := byID["example.et/careers/c"]
	if fresh.Title != "Fresh" || fresh.Description != "Real description" || fresh.LocationRaw != "Addis Ababa" {
		t.Errorf("JSON-LD fields not used: %+v", fresh)
	}
	if !fresh.PublishedAt.Equal(time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("PublishedAt = %v, want JSON-LD datePosted", fresh.PublishedAt)
	}
	if _, ok := byID["example.et/careers/d"]; !ok {
		t.Errorf("a posting from 115 days ago was dropped; the limit is 120 days")
	}
}

func TestListJobs_ListingWithNoJobLinksIsAnErrorNotAnEmptyBoard(t *testing.T) {
	cases := map[string]string{
		"script-rendered shell":   `<html><body><div id="root"></div><script src="/app.js"></script></body></html>`,
		"only site navigation":    `<a href="/">Home</a><a href="/about">About</a><a href="/blog/x">Blog</a>`,
		"only furniture":          `<a href="/jobs/feed/">RSS</a><a href="/jobs/page/2">2</a><a href="/jobs/x.pdf">pdf</a>`,
		"links to other sites":    `<a href="https://other.example/jobs/a">a</a>`,
		"link to the page itself": `<a href="/jobs">self</a><a href="/jobs/">self</a>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakePages{html: map[string]string{"https://example.et/jobs": body}}
			jobs, err := newClient(f).ListJobs(context.Background(), "https://example.et/jobs")
			if !errors.Is(err, ErrNoJobLinks) {
				t.Fatalf("ListJobs() = %v, %v; want ErrNoJobLinks (an empty list would close every open job)", jobs, err)
			}
		})
	}
}

func TestListJobs_UnfetchableDetailKeepsTheJobFromTheListing(t *testing.T) {
	f := &fakePages{
		html: map[string]string{
			"https://example.et/jobs": `<a href="/jobs/legal-counsel">Legal Counsel</a><a href="/jobs/senior-data-engineer">Read more</a><a href="/jobs/0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d">Read more</a>`,
		},
		errs: map[string]error{
			"https://example.et/jobs/legal-counsel":                        errors.New("timeout"),
			"https://example.et/jobs/senior-data-engineer":                 fmt.Errorf("robots: %w", page.ErrDisallowed),
			"https://example.et/jobs/0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d": errors.New("boom"),
		},
	}
	jobs, err := newClient(f).ListJobs(context.Background(), "https://example.et/jobs")
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %v, want all 3 kept (they were on the listing, so they must stay 'seen')", ids(jobs))
	}
	titles := map[string]string{}
	for _, j := range jobs {
		titles[j.ExternalID] = j.Title
	}
	if titles["example.et/jobs/legal-counsel"] != "Legal Counsel" {
		t.Errorf("anchor text not used as title: %v", titles)
	}
	if titles["example.et/jobs/senior-data-engineer"] != "Senior Data Engineer" {
		t.Errorf("slug not used as title when the anchor is generic: %v", titles)
	}
	if titles["example.et/jobs/0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d"] != "" {
		t.Errorf("a UUID became a title: %v", titles)
	}
}

func TestListJobs_ListingErrors(t *testing.T) {
	t.Run("404 means the board is gone", func(t *testing.T) {
		_, err := newClient(&fakePages{}).ListJobs(context.Background(), "https://example.et/jobs")
		if !errors.Is(err, ats.ErrBoardNotFound) {
			t.Errorf("error = %v, want ErrBoardNotFound", err)
		}
	})
	t.Run("robots disallow is a plain error and not 'gone'", func(t *testing.T) {
		f := &fakePages{errs: map[string]error{"https://example.et/jobs": page.ErrDisallowed}}
		_, err := newClient(f).ListJobs(context.Background(), "https://example.et/jobs")
		if err == nil || errors.Is(err, ats.ErrBoardNotFound) {
			t.Errorf("error = %v, want a non-'gone' error (never deactivate a board over robots.txt)", err)
		}
	})
	t.Run("network error", func(t *testing.T) {
		f := &fakePages{errs: map[string]error{"https://example.et/jobs": errors.New("timeout")}}
		if _, err := newClient(f).ListJobs(context.Background(), "https://example.et/jobs"); err == nil {
			t.Error("succeeded on a network error")
		}
	})
}

func TestListJobs_RejectsUnusableListingURLs(t *testing.T) {
	f := &fakePages{}
	for _, u := range []string{"", "https://example.et", "https://example.et/", "ftp://example.et/jobs", "/jobs", "javascript:1", "https:///jobs"} {
		_, err := newClient(f).ListJobs(context.Background(), u)
		if !errors.Is(err, ats.ErrInvalidBoardToken) {
			t.Errorf("ListJobs(%q) error = %v, want ErrInvalidBoardToken (a root URL would make every link a job)", u, err)
		}
	}
	if len(f.fetched) != 0 {
		t.Errorf("fetched %v for invalid URLs, want nothing", f.fetched)
	}
}

func TestListJobs_CapsDetailFetches(t *testing.T) {
	var links strings.Builder
	html := map[string]string{}
	for i := range 100 {
		fmt.Fprintf(&links, `<a href="/jobs/role-%d">Role %d</a>`, i, i)
		html[fmt.Sprintf("https://example.et/jobs/role-%d", i)] = detailPage(fmt.Sprintf("Role %d", i), "", "")
	}
	html["https://example.et/jobs"] = links.String()
	f := &fakePages{html: html}
	c := New(f, 0, 7, 0)
	c.now = func() time.Time { return fixedNow }

	// 100 candidates against a limit of 7: an error, not the first 7. A
	// truncated result from a full source would close the other 93 jobs
	// while they are still posted.
	jobs, err := c.ListJobs(context.Background(), "https://example.et/jobs")
	if !errors.Is(err, ErrTooManyJobs) {
		t.Fatalf("ListJobs() = %d jobs, error = %v; want ErrTooManyJobs", len(jobs), err)
	}
	if len(f.fetched) != 1 {
		t.Errorf("fetched %d pages, want only the listing before giving up", len(f.fetched))
	}

	// Exactly at the limit is fine.
	at := New(f, 0, 100, 0)
	at.now = func() time.Time { return fixedNow }
	jobs, err = at.ListJobs(context.Background(), "https://example.et/jobs")
	if err != nil || len(jobs) != 100 {
		t.Fatalf("ListJobs() at the limit = %d jobs, %v; want 100, nil", len(jobs), err)
	}
}

func TestListJobs_OversizedListingPageIsAnErrorNotAShorterList(t *testing.T) {
	f := &fakePages{errs: map[string]error{"https://example.et/jobs": page.ErrTooLarge}}
	jobs, err := newClient(f).ListJobs(context.Background(), "https://example.et/jobs")
	if !errors.Is(err, page.ErrTooLarge) || jobs != nil {
		t.Errorf("jobs = %v, err = %v; want the size error surfaced", jobs, err)
	}
}

func TestListJobs_StopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakePages{html: map[string]string{
		"https://example.et/jobs":   `<a href="/jobs/a">A</a><a href="/jobs/b">B</a>`,
		"https://example.et/jobs/a": detailPage("A", "", ""),
		"https://example.et/jobs/b": detailPage("B", "", ""),
	}}
	c := New(f, 0, 0, time.Hour) // a long pause: only cancellation can end the wait
	go func() {
		for f.count() < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	done := make(chan error, 1)
	go func() { _, err := c.ListJobs(ctx, "https://example.et/jobs"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListJobs() did not return after cancellation")
	}
}

// Found by adversarial review: dropping the whole query string merged
// /careers/job?id=1 and ?id=2 into one job with a link that pointed nowhere.
func TestListJobs_QueryStringIdentifiedJobsStayDistinctAndKeepTheirLinks(t *testing.T) {
	f := &fakePages{html: map[string]string{
		"https://example.et/careers": `<a href="/careers/job?id=1&utm_source=x">A</a><a href="/careers/job?id=2">B</a>` +
			`<a href="/careers/job?utm_medium=y&id=1">A again</a><a href="/careers/job.php?id=3&ref=home">C</a>`,
		"https://example.et/careers/job?id=1":     detailPage("Role One", "", ""),
		"https://example.et/careers/job?id=2":     detailPage("Role Two", "", ""),
		"https://example.et/careers/job.php?id=3": detailPage("Role Three", "", ""),
	}}
	jobs, err := newClient(f).ListJobs(context.Background(), "https://example.et/careers")
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	urls := map[string]string{}
	for _, j := range jobs {
		urls[j.URL] = j.Title
	}
	want := map[string]string{
		"https://example.et/careers/job?id=1":     "Role One",
		"https://example.et/careers/job?id=2":     "Role Two",
		"https://example.et/careers/job.php?id=3": "Role Three",
	}
	if len(jobs) != 3 {
		t.Fatalf("got %v, want 3 distinct jobs (id=1 linked twice with different tracking parameters is one job)", urls)
	}
	for u, title := range want {
		if urls[u] != title {
			t.Errorf("urls = %v, missing %s -> %s", urls, u, title)
		}
	}
}

func TestListJobs_DeadLinksAndCompanyInfoPagesAreNotJobs(t *testing.T) {
	f := &fakePages{html: map[string]string{
		"https://example.et/careers": `<a href="/careers/culture">Culture</a><a href="/careers/benefits/">Benefits</a>` +
			`<a href="/careers/departments/engineering">Engineering</a><a href="/careers/gone">Gone</a>` +
			`<a href="/careers/data-analyst">Data Analyst</a>`,
		"https://example.et/careers/data-analyst": detailPage("Data Analyst", "", ""),
		// /careers/gone has no page: the fake answers 404.
	}}
	jobs, err := newClient(f).ListJobs(context.Background(), "https://example.et/careers")
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	if len(jobs) != 1 || jobs[0].Title != "Data Analyst" {
		t.Errorf("got %v, want only Data Analyst (info pages skipped, the 404 link dropped)", ids(jobs))
	}
}

func TestAnchorTitle(t *testing.T) {
	long := strings.Repeat("word ", 40)
	cases := map[string]string{
		"Legal Counsel":    "Legal Counsel",
		"  Legal Counsel ": "Legal Counsel",
		// Real card text from https://zareinnovations.com/careers.
		"Full-Stack AI Engineer→Addis Ababa · Remote · Full-TimeAs a Full-Stack AI Engineer at Zare Innovations, you are a master of your craft.": "Full-Stack AI Engineer",
		"Read more": "",
		"READ MORE": "",
		"Apply now": "",
		"":          "",
		long:        "",
		"→ only":    "",
		"Title→":    "Title",
	}
	for in, want := range cases {
		if got := anchorTitle(in); got != want {
			t.Errorf("anchorTitle(%.40q) = %q, want %q", in, got, want)
		}
	}
}

func TestListJobs_CardShapedAnchorsYieldTheTitleWhenTheDetailPageFails(t *testing.T) {
	f := &fakePages{
		html: map[string]string{
			"https://example.et/careers": `<a href="/careers/8358e635-f717-4783-b96b-7cdcd012ad0a">Full-Stack AI Engineer→Addis Ababa · Remote · Full-Time As a Full-Stack AI Engineer...</a>`,
		},
		errs: map[string]error{"https://example.et/careers/8358e635-f717-4783-b96b-7cdcd012ad0a": errors.New("timeout")},
	}
	jobs, err := newClient(f).ListJobs(context.Background(), "https://example.et/careers")
	if err != nil || len(jobs) != 1 || jobs[0].Title != "Full-Stack AI Engineer" {
		t.Fatalf("jobs = %+v, err = %v; want the title before the arrow", jobs, err)
	}
}

func TestTitleFromSlug(t *testing.T) {
	cases := map[string]string{
		"https://x.et/jobs/legal-counsel/":                          "Legal Counsel",
		"https://x.et/jobs/full-stack-developer-48":                 "Full Stack Developer 48",
		"https://x.et/careers/0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d": "",
		"https://x.et/jobs/12345678":                                "",
		"https://x.et/jobs/écrivain-sénior":                         "Écrivain Sénior",
	}
	for in, want := range cases {
		if got := titleFromSlug(in); got != want {
			t.Errorf("titleFromSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanPageTitle(t *testing.T) {
	cases := map[string]string{
		"Legal Counsel - Kifiya Financial Technology": "Legal Counsel",
		"Data Analyst | Example":                      "Data Analyst",
		"Plain":                                       "Plain",
		" Full Stack Developer | My Website ":         "Full Stack Developer",
	}
	for in, want := range cases {
		if got := cleanPageTitle(in); got != want {
			t.Errorf("cleanPageTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("héllo wörld", 5); got != "héllo" {
		t.Errorf("truncateRunes = %q, want a cut on a rune boundary", got)
	}
	if got := truncateRunes("short", 50); got != "short" {
		t.Errorf("truncateRunes = %q", got)
	}
}
