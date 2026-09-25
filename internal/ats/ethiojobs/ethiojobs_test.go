package ethiojobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

var fixedNow = time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakePages serves listing pages by number through the real page.Parse, so
// the tests exercise the same __NEXT_DATA__ extraction the live fetcher does.
type fakePages struct {
	mu      sync.Mutex
	pages   map[int]string // page number -> HTML
	errs    map[int]error
	fetched []string
}

func (f *fakePages) Fetch(_ context.Context, rawURL string) (*page.Page, error) {
	f.mu.Lock()
	f.fetched = append(f.fetched, rawURL)
	f.mu.Unlock()
	var n int
	if _, err := fmt.Sscanf(rawURL, "https://ethiojobs.net/jobs?page=%d", &n); err != nil {
		return nil, fmt.Errorf("fake: unexpected URL %s", rawURL)
	}
	if err := f.errs[n]; err != nil {
		return nil, err
	}
	body, ok := f.pages[n]
	if !ok {
		return nil, fmt.Errorf("fake: page %d: %w", n, page.ErrNotFound)
	}
	return page.Parse(rawURL, []byte(body))
}

func (f *fakePages) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fetched)
}

type lj map[string]any

// listing builds a listing page's HTML the way ethiojobs.net ships it.
func listing(lastPage int, jobs ...lj) string {
	data := make([]any, len(jobs))
	for i, j := range jobs {
		data[i] = j
	}
	doc := map[string]any{"props": map[string]any{"pageProps": map[string]any{
		"jobs": map[string]any{"data": data, "meta": map[string]any{"lastPage": lastPage, "total": 1195}},
	}}}
	b, _ := json.Marshal(doc)
	return `<html><body><script id="__NEXT_DATA__" type="application/json">` + string(b) + `</script></body></html>`
}

func posting(slug, title, employer string, published time.Time, expires time.Time) lj {
	j := lj{
		"slug": slug, "title": title, "description": "<p>Do <b>great</b> work.</p>",
		"date_published": published.Format("2006-01-02T15:04:05.000000Z"),
		"state":          "Addis Ababa", "city": nil,
		"company": map[string]any{"name": employer},
	}
	if !expires.IsZero() {
		j["date_expiry"] = expires.Format("2006-01-02T15:04:05.000000Z")
	}
	return j
}

func newCollector(f *fakePages, maxPages int) *Collector {
	c := New(f, maxPages, 0, 0, discardLogger())
	c.now = func() time.Time { return fixedNow }
	return c
}

func hoursAgo(h int) time.Time  { return fixedNow.Add(-time.Duration(h) * time.Hour) }
func daysAhead(d int) time.Time { return fixedNow.Add(time.Duration(d) * 24 * time.Hour) }

func TestCollect_MapsAListedJobToAnATSJob(t *testing.T) {
	f := &fakePages{pages: map[int]string{1: listing(1,
		posting("Ubd2cAJy92-senior-compliance-officer", "Senior Compliance Officer", "Ethswitch S.C.", hoursAgo(3), daysAhead(6)),
	)}}
	jobs, err := newCollector(f, 0).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.ExternalID != "Ubd2cAJy92" || j.Employer != "Ethswitch S.C." || j.Title != "Senior Compliance Officer" {
		t.Errorf("job = %+v", j)
	}
	if j.URL != "https://ethiojobs.net/job/Ubd2cAJy92-senior-compliance-officer" {
		t.Errorf("URL = %q", j.URL)
	}
	if j.LocationRaw != "Addis Ababa" || j.Description != "Do great work." {
		t.Errorf("location = %q, description = %q", j.LocationRaw, j.Description)
	}
	if !j.PublishedAt.Equal(hoursAgo(3)) || !j.ExpiresAt.Equal(daysAhead(6)) {
		t.Errorf("PublishedAt = %v, ExpiresAt = %v", j.PublishedAt, j.ExpiresAt)
	}
	if j.Closed {
		t.Errorf("a live job was marked closed")
	}
}

func TestCollect_ReadsPagesNewestFirstAndStopsPastTheAgeLimit(t *testing.T) {
	f := &fakePages{pages: map[int]string{
		1: listing(50, posting("aaaaaa1111-new", "New", "A", hoursAgo(2), time.Time{}), posting("aaaaaa2222-new2", "New2", "A", hoursAgo(30), time.Time{})),
		2: listing(50, posting("bbbbbb1111-mid", "Mid", "B", hoursAgo(24*10), time.Time{}), posting("bbbbbb2222-old", "Old", "B", hoursAgo(24*25), time.Time{})),
		3: listing(50, posting("cccccc1111-older", "Older", "C", hoursAgo(24*40), time.Time{})),
		4: listing(50, posting("dddddd1111-never", "Never", "D", hoursAgo(24*60), time.Time{})),
	}}
	jobs, err := newCollector(f, 0).Collect(context.Background()) // default max age 20 days
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.ExternalID)
	}
	want := "aaaaaa1111 aaaaaa2222 bbbbbb1111"
	if strings.Join(ids, " ") != want {
		t.Errorf("ids = %v, want %s (the 25-day-old job is dropped)", ids, want)
	}
	// Page 2 contains a job past the cutoff, so page 3 is never requested.
	if f.count() != 2 {
		t.Errorf("fetched %d pages %v, want 2 (stop once a page passes the age limit)", f.count(), f.fetched)
	}
}

func TestCollect_StopsAtTheLastPageTheSiteReports(t *testing.T) {
	f := &fakePages{pages: map[int]string{
		1: listing(2, posting("aaaaaa1111-a", "A", "X", hoursAgo(1), time.Time{})),
		2: listing(2, posting("bbbbbb1111-b", "B", "X", hoursAgo(2), time.Time{})),
		3: listing(2, posting("cccccc1111-c", "C", "X", hoursAgo(3), time.Time{})),
	}}
	jobs, err := newCollector(f, 0).Collect(context.Background())
	if err != nil || len(jobs) != 2 || f.count() != 2 {
		t.Errorf("jobs = %d, fetches = %d, err = %v; want 2 jobs from 2 pages (lastPage=2)", len(jobs), f.count(), err)
	}
}

func TestCollect_MaxPagesBoundsTheRunAndIsCapped(t *testing.T) {
	pages := map[int]string{}
	for n := 1; n <= 300; n++ {
		pages[n] = listing(300, posting(fmt.Sprintf("id%07d-job", n), "J", "X", hoursAgo(1), time.Time{}))
	}
	f := &fakePages{pages: pages}
	if _, err := newCollector(f, 5).Collect(context.Background()); err != nil || f.count() != 5 {
		t.Errorf("maxPages=5: fetches = %d, err = %v; want 5", f.count(), err)
	}
	g := &fakePages{pages: pages}
	if _, err := newCollector(g, 100000).Collect(context.Background()); err != nil || g.count() != hardMaxPages {
		t.Errorf("maxPages=100000: fetches = %d, err = %v; want the hard cap %d", g.count(), err, hardMaxPages)
	}
}

func TestCollect_DropsDuplicatesThatAppearWhenNewJobsShiftThePages(t *testing.T) {
	same := posting("aaaaaa1111-dup", "Dup", "X", hoursAgo(1), time.Time{})
	f := &fakePages{pages: map[int]string{
		1: listing(2, same, posting("bbbbbb1111-b", "B", "X", hoursAgo(2), time.Time{})),
		2: listing(2, same, posting("cccccc1111-c", "C", "X", hoursAgo(3), time.Time{})), // shifted down by a new posting
	}}
	jobs, err := newCollector(f, 0).Collect(context.Background())
	if err != nil || len(jobs) != 3 {
		t.Errorf("jobs = %d, err = %v; want 3 unique jobs", len(jobs), err)
	}
}

func TestCollect_AnExpiredPostingIsReportedAsClosedNotStored(t *testing.T) {
	f := &fakePages{pages: map[int]string{1: listing(1,
		posting("aaaaaa1111-live", "Live", "X", hoursAgo(5), daysAhead(3)),
		posting("bbbbbb1111-dead", "Dead", "Y", hoursAgo(80), fixedNow.Add(-time.Minute)),
		posting("cccccc1111-now", "Now", "Z", hoursAgo(80), fixedNow), // deadline exactly now is over
	)}}
	jobs, err := newCollector(f, 0).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	byID := map[string]bool{}
	for _, j := range jobs {
		byID[j.ExternalID] = j.Closed
	}
	if len(jobs) != 3 || byID["aaaaaa1111"] || !byID["bbbbbb1111"] || !byID["cccccc1111"] {
		t.Errorf("closed by id = %v, want only the two expired ones closed", byID)
	}
	for _, j := range jobs {
		if j.Closed && (j.Employer == "" || j.Title != "") {
			t.Errorf("closure marker %+v must carry its employer (for grouping) and no job data", j)
		}
	}
}

func TestCollect_SkipsUnusableListings(t *testing.T) {
	good := posting("aaaaaa1111-good", "Good", "X", hoursAgo(1), time.Time{})
	f := &fakePages{pages: map[int]string{1: listing(1,
		good,
		posting("bbbbbb1111-notitle", "   ", "X", hoursAgo(1), time.Time{}),
		posting("cccccc1111-noemployer", "No Employer", "  ", hoursAgo(1), time.Time{}),
		lj{"slug": "dddddd1111-nocompany", "title": "No Company", "date_published": hoursAgo(1).Format(time.RFC3339)},
		posting("x", "Short id", "X", hoursAgo(1), time.Time{}),
		posting("has space-and-slash/../evil", "Evil slug", "X", hoursAgo(1), time.Time{}),
		posting("", "No slug", "X", hoursAgo(1), time.Time{}),
		lj{"slug": "eeeeee1111-nodate", "title": "No date", "company": map[string]any{"name": "X"}},
		lj{"slug": "ffffff1111-baddate", "title": "Bad date", "date_published": "yesterday", "company": map[string]any{"name": "X"}},
	)}}
	jobs, err := newCollector(f, 0).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(jobs) != 1 || jobs[0].ExternalID != "aaaaaa1111" {
		t.Errorf("got %+v, want only the good job", jobs)
	}
}

// One record with a field of an unexpected type must cost only that job.
func TestCollect_AJobThatCannotBeDecodedIsSkippedNotThePage(t *testing.T) {
	odd := lj{"slug": "bbbbbb1111-odd", "title": 42, "company": "not an object", "date_published": []int{1}}
	f := &fakePages{pages: map[int]string{1: listing(1,
		posting("aaaaaa1111-a", "A", "X", hoursAgo(1), time.Time{}),
		odd,
		posting("cccccc1111-c", "C", "X", hoursAgo(2), time.Time{}),
	)}}
	jobs, err := newCollector(f, 0).Collect(context.Background())
	if err != nil || len(jobs) != 2 {
		t.Errorf("jobs = %d, err = %v; want the two well-formed jobs", len(jobs), err)
	}
}

func TestCollect_FirstPageProblemsAreErrorsNeverAnEmptyResult(t *testing.T) {
	cases := map[string]*fakePages{
		"fetch fails":          {errs: map[int]error{1: errors.New("timeout")}},
		"robots disallowed":    {errs: map[int]error{1: page.ErrDisallowed}},
		"404":                  {pages: map[int]string{}},
		"no embedded data":     {pages: map[int]string{1: "<html><body>Maintenance</body></html>"}},
		"not json":             {pages: map[int]string{1: `<script id="__NEXT_DATA__">{oops</script>`}},
		"page 1 lists nothing": {pages: map[int]string{1: listing(1)}},
		"no jobs list":         {pages: map[int]string{1: `<script id="__NEXT_DATA__">{"props":{"pageProps":{"other":1}}}</script>`}},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			jobs, err := newCollector(f, 0).Collect(context.Background())
			if err == nil {
				t.Fatalf("Collect() = %v, nil; want an error (an empty list would age out every stored job)", jobs)
			}
		})
	}
}

func TestCollect_HiddenEmployersAndFillerLocationsAreNotStored(t *testing.T) {
	filler := posting("gggggg1111-filler", "Filler location", "Acme", hoursAgo(1), time.Time{})
	filler["state"], filler["city"] = "Not Specified", "Not Specified"
	f := &fakePages{pages: map[int]string{1: listing(1,
		posting("aaaaaa1111-hidden", "Hidden employer", "Confidential", hoursAgo(1), time.Time{}),
		filler,
	)}}
	jobs, err := newCollector(f, 0).Collect(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d, err = %v; want only the Acme job", len(jobs), err)
	}
	if jobs[0].Employer != "Acme" || jobs[0].LocationRaw != "" {
		t.Errorf("job = %+v, want employer Acme and an empty location (not the filler text)", jobs[0])
	}
}

func TestCollect_ADescriptionIsCappedAtTheRuneLimit(t *testing.T) {
	long := posting("aaaaaa1111-long", "Long", "Acme", hoursAgo(1), time.Time{})
	long["description"] = "<p>" + strings.Repeat("é", maxDescriptionRunes+500) + "</p>"
	f := &fakePages{pages: map[int]string{1: listing(1, long)}}
	jobs, err := newCollector(f, 0).Collect(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d, err = %v", len(jobs), err)
	}
	if n := len([]rune(jobs[0].Description)); n != maxDescriptionRunes {
		t.Errorf("description has %d runes, want exactly %d (cut on a rune boundary)", n, maxDescriptionRunes)
	}
}

func TestCollect_ALaterPageFailingKeepsWhatWasCollected(t *testing.T) {
	f := &fakePages{
		pages: map[int]string{1: listing(9, posting("aaaaaa1111-a", "A", "X", hoursAgo(1), time.Time{}))},
		errs:  map[int]error{2: errors.New("boom")},
	}
	jobs, err := newCollector(f, 0).Collect(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Errorf("jobs = %d, err = %v; want the first page's job and no error", len(jobs), err)
	}
}

func TestCollect_CancellationIsReturned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakePages{errs: map[int]error{1: context.Canceled}}
	if _, err := newCollector(f, 0).Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestCollect_PagesAreSpaced(t *testing.T) {
	f := &fakePages{pages: map[int]string{
		1: listing(3, posting("aaaaaa1111-a", "A", "X", hoursAgo(1), time.Time{})),
		2: listing(3, posting("bbbbbb1111-b", "B", "X", hoursAgo(2), time.Time{})),
		3: listing(3, posting("cccccc1111-c", "C", "X", hoursAgo(3), time.Time{})),
	}}
	c := newCollector(f, 0)
	c.pause = 40 * time.Millisecond
	start := time.Now()
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 75*time.Millisecond {
		t.Errorf("3 pages took %v, want at least 2 pauses of 40ms", elapsed)
	}
}

func TestJobID(t *testing.T) {
	cases := []struct{ slug, id string }{
		{"vR99iaawhz-manager-product-development-and-planning", "vR99iaawhz"},
		{"  Ubd2cAJy92-x  ", "Ubd2cAJy92"},
		{"abcdef", "abcdef"},
		{"abc-def", ""},             // id too short
		{"abcdefghijklmnopq-x", ""}, // id too long
		{"abcdef1234-x/../y", ""},   // path characters
		{"abcdef1234-x y", ""},      // whitespace
		{"abcdef1234-é", ""},        // non-ASCII
		{"", ""},
		{strings.Repeat("a", 301), ""},
	}
	for _, tc := range cases {
		if id, _ := jobID(tc.slug); id != tc.id {
			t.Errorf("jobID(%q) = %q, want %q", tc.slug, id, tc.id)
		}
	}
}

// Compile-time and value check: without this, losing the method would put
// every Ethiojobs employer in the worldwide list with no test failing.
var _ market.Provider = (*Collector)(nil)

func TestCollectorIsInTheEthiopianMarket(t *testing.T) {
	if got := newCollector(&fakePages{}, 0).Market(); got != market.Ethiopia {
		t.Errorf("Market() = %q, want %q", got, market.Ethiopia)
	}
}

func TestStaleAfterIsPositive(t *testing.T) {
	if got := newCollector(&fakePages{}, 0).StaleAfter(); got <= 0 {
		t.Errorf("StaleAfter() = %v, want a positive window (a sample must never be treated as a full board)", got)
	}
}
