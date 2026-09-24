package feed

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
	"github.com/Bantamlak12/remote-job-aggregator/internal/robots"
)

type fakeDoer struct {
	status int
	body   string
	err    error
	calls  int
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(f.body)), Request: req}, nil
}

type allowFunc func(string) (bool, error)

func (a allowFunc) Allowed(_ context.Context, u string) (bool, error) { return a(u) }

var allowAll = allowFunc(func(string) (bool, error) { return true, nil })

var fixedNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func newClient(d *fakeDoer, robots Allower) *Client {
	c := New(d, robots, 0)
	c.now = func() time.Time { return fixedNow }
	return c
}

// Trimmed from the real https://zalatechs.com/jobs/feed/ and
// https://ethswitch.com/jobs/feed/ shapes (WordPress: CDATA, content:encoded,
// entity-encoded punctuation), with dates moved so the age filter can be
// exercised against fixedNow.
const wordpressFeed = `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"
	xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel>
	<title>Jobs &#8211; Example S.C</title>
	<link>https://example.et</link>
	<item>
		<title>Senior Backend Engineer</title>
		<link>https://example.et/jobs/senior-backend-engineer/</link>
		<dc:creator><![CDATA[Admin]]></dc:creator>
		<pubDate>Mon, 14 Sep 2026 10:00:00 +0000</pubDate>
		<guid isPermaLink="false">https://example.et/?post_type=jobpost&#038;p=101</guid>
		<description><![CDATA[<p>short excerpt [&#8230;]</p>]]></description>
		<content:encoded><![CDATA[<p class="x">Build <strong>payment</strong> rails.</p><ul><li>Go</li><li>Postgres</li></ul>]]></content:encoded>
	</item>
	<item>
		<title>Excerpt Only Role</title>
		<link>https://example.et/jobs/excerpt-only/</link>
		<pubDate>Tue, 15 Sep 2026 10:00:00 GMT</pubDate>
		<guid>job-102</guid>
		<description><![CDATA[<p>Only a description here.</p>]]></description>
	</item>
	<item>
		<title>Ancient Posting</title>
		<link>https://example.et/jobs/ancient/</link>
		<pubDate>Tue, 23 Jan 2024 10:46:10 +0000</pubDate>
		<guid>job-old</guid>
	</item>
	<item>
		<title>No Date Role</title>
		<link>https://example.et/jobs/no-date/</link>
		<guid>job-nodate</guid>
	</item>
	<item>
		<title>Bad Link Role</title>
		<link>javascript:alert(1)</link>
		<pubDate>Tue, 15 Sep 2026 10:00:00 GMT</pubDate>
		<guid>job-badlink</guid>
	</item>
	<item>
		<title>No Guid Role</title>
		<link>https://example.et/jobs/no-guid/</link>
		<pubDate>Tue, 15 Sep 2026 10:00:00 GMT</pubDate>
	</item>
</channel>
</rss>`

func TestListJobs_ParsesWordPressFeedAndDropsStaleItems(t *testing.T) {
	d := &fakeDoer{status: 200, body: wordpressFeed}
	jobs, err := newClient(d, allowAll).ListJobs(context.Background(), "https://example.et/jobs/feed/")
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}

	byID := map[string]ats.Job{}
	for _, j := range jobs {
		byID[j.ExternalID] = j
	}
	if _, ok := byID["job-old"]; ok {
		t.Errorf("the 2024 posting was returned; items older than the max age are not current openings")
	}
	wantIDs := []string{
		"https://example.et/?post_type=jobpost&p=101", // entity-decoded guid
		"job-102", "job-nodate", "job-badlink", "https://example.et/jobs/no-guid/",
	}
	if len(jobs) != len(wantIDs) {
		t.Fatalf("got %d jobs %v, want %d", len(jobs), keys(byID), len(wantIDs))
	}
	for _, id := range wantIDs {
		if _, ok := byID[id]; !ok {
			t.Errorf("missing job with ExternalID %q; have %v", id, keys(byID))
		}
	}

	be := byID["https://example.et/?post_type=jobpost&p=101"]
	if be.Title != "Senior Backend Engineer" || be.URL != "https://example.et/jobs/senior-backend-engineer/" {
		t.Errorf("job = %+v", be)
	}
	if !be.PublishedAt.Equal(time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("PublishedAt = %v", be.PublishedAt)
	}
	if !strings.Contains(be.Description, "Build payment rails.") || !strings.Contains(be.Description, "Postgres") ||
		strings.Contains(be.Description, "<") || strings.Contains(be.Description, "excerpt") {
		t.Errorf("Description = %q, want plain text from content:encoded, not the excerpt", be.Description)
	}

	if got := byID["job-102"].Description; got != "Only a description here." {
		t.Errorf("excerpt-only Description = %q, want the plain-text <description>", got)
	}
	if got := byID["job-nodate"].PublishedAt; !got.IsZero() {
		t.Errorf("undated item PublishedAt = %v, want zero (and the item kept)", got)
	}
	if got := byID["job-badlink"].URL; got != "" {
		t.Errorf("javascript: link kept as URL %q; only http(s) links may pass through", got)
	}
}

func keys(m map[string]ats.Job) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestListJobs_EmptyFeedIsSuccessNotError(t *testing.T) {
	body := `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title></channel></rss>`
	jobs, err := newClient(&fakeDoer{status: 200, body: body}, allowAll).ListJobs(context.Background(), "https://example.et/feed/")
	if err != nil || len(jobs) != 0 {
		t.Fatalf("ListJobs() = %v, %v; want no jobs and no error (a company may have zero openings)", jobs, err)
	}
}

func TestListJobs_NonFeedDocumentsAreErrors(t *testing.T) {
	cases := map[string]string{
		"html error page": `<html><body>Maintenance</body></html>`,
		"other xml":       `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`,
		"truncated":       `<?xml version="1.0"?><rss><channel><item><title>x`,
		"empty":           ``,
		"json":            `{"jobs": []}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			jobs, err := newClient(&fakeDoer{status: 200, body: body}, allowAll).ListJobs(context.Background(), "https://example.et/feed/")
			if err == nil {
				t.Fatalf("ListJobs() = %v, nil; a malformed feed must be an error, never an empty job list "+
					"(an empty list would close every open job for this company)", jobs)
			}
		})
	}
}

func TestListJobs_StatusHandling(t *testing.T) {
	for _, status := range []int{404, 410} {
		_, err := newClient(&fakeDoer{status: status}, allowAll).ListJobs(context.Background(), "https://example.et/feed/")
		if !errors.Is(err, ats.ErrBoardNotFound) {
			t.Errorf("status %d: error = %v, want ErrBoardNotFound", status, err)
		}
	}
	for _, status := range []int{403, 500, 503} {
		_, err := newClient(&fakeDoer{status: status, body: "nope"}, allowAll).ListJobs(context.Background(), "https://example.et/feed/")
		if err == nil || errors.Is(err, ats.ErrBoardNotFound) {
			t.Errorf("status %d: error = %v, want a plain (transient) error", status, err)
		}
	}
}

func TestListJobs_NetworkErrorIsReturned(t *testing.T) {
	_, err := newClient(&fakeDoer{err: errors.New("dial tcp: refused")}, allowAll).ListJobs(context.Background(), "https://example.et/feed/")
	if err == nil {
		t.Fatal("ListJobs() succeeded on a network error")
	}
}

func TestListJobs_RobotsGate(t *testing.T) {
	d := &fakeDoer{status: 200, body: wordpressFeed}
	deny := allowFunc(func(string) (bool, error) { return false, nil })
	if _, err := newClient(d, deny).ListJobs(context.Background(), "https://example.et/feed/"); err == nil {
		t.Fatal("ListJobs() succeeded although robots.txt disallows the feed")
	}
	broken := allowFunc(func(string) (bool, error) { return false, errors.New("unavailable") })
	if _, err := newClient(d, broken).ListJobs(context.Background(), "https://example.et/feed/"); err == nil {
		t.Fatal("ListJobs() succeeded although robots.txt could not be read")
	}
	if d.calls != 0 {
		t.Errorf("feed requested %d times despite robots denial, want 0", d.calls)
	}
}

// With the real httpclient and robots.Checker: a feed URL that redirects
// into a robots-disallowed path must not be requested.
func TestListJobs_RedirectIntoADisallowedPathIsNeverRequested(t *testing.T) {
	var secretHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	})
	mux.HandleFunc("/jobs/feed/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/private/feed.xml", http.StatusFound)
	})
	mux.HandleFunc("/private/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		secretHits.Add(1)
		_, _ = w.Write([]byte(wordpressFeed))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := httpclient.DefaultConfig()
	cfg.Timeout = 5 * time.Second
	hc := httpclient.New(cfg)
	c := New(hc, robots.New(hc, "remote-job-aggregator"), 0)

	if _, err := c.ListJobs(context.Background(), srv.URL+"/jobs/feed/"); err == nil {
		t.Fatal("ListJobs() succeeded through a redirect into a disallowed path")
	}
	if secretHits.Load() != 0 {
		t.Errorf("the disallowed target was requested %d times", secretHits.Load())
	}
}

func TestListJobs_InvalidFeedURL(t *testing.T) {
	d := &fakeDoer{status: 200, body: wordpressFeed}
	for _, u := range []string{"", "not a url", "ftp://example.et/feed", "file:///etc/passwd", "/relative", "javascript:1"} {
		_, err := newClient(d, allowAll).ListJobs(context.Background(), u)
		if !errors.Is(err, ats.ErrInvalidBoardToken) {
			t.Errorf("ListJobs(%q) error = %v, want ErrInvalidBoardToken", u, err)
		}
	}
	if d.calls != 0 {
		t.Errorf("made %d requests for invalid URLs, want 0", d.calls)
	}
}

func TestListJobs_LegacyCharsetIsDecoded(t *testing.T) {
	body := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss version=\"2.0\"><channel><item>" +
		"<title>Caf\xe9 Manager</title><link>https://example.et/j/1</link><guid>1</guid></item></channel></rss>"
	jobs, err := newClient(&fakeDoer{status: 200, body: body}, allowAll).ListJobs(context.Background(), "https://example.et/feed/")
	if err != nil || len(jobs) != 1 || jobs[0].Title != "Café Manager" {
		t.Fatalf("ListJobs() = %+v, %v; want the ISO-8859-1 title decoded to UTF-8", jobs, err)
	}
}

func TestListJobs_NULBytesAreStripped(t *testing.T) {
	body := "<?xml version=\"1.0\"?><rss version=\"2.0\"><channel><item><title>Dev\u0000 Role</title>" +
		"<link>https://example.et/j/1</link><guid>g\u0000</guid></item></channel></rss>"
	jobs, err := newClient(&fakeDoer{status: 200, body: body}, allowAll).ListJobs(context.Background(), "https://example.et/feed/")
	if err != nil {
		// encoding/xml rejects a raw NUL as an illegal character; either
		// outcome is safe, what matters is that no NUL reaches Postgres.
		return
	}
	for _, j := range jobs {
		if strings.ContainsRune(j.Title+j.ExternalID, 0) {
			t.Errorf("NUL byte survived into %+v", j)
		}
	}
}

func TestListJobs_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &fakeDoer{err: ctx.Err()}
	if _, err := newClient(d, allowAll).ListJobs(ctx, "https://example.et/feed/"); err == nil {
		t.Fatal("ListJobs() succeeded with a canceled context")
	}
}

// Found by adversarial review: "Mon, 5 Feb 2023 ..." (unpadded day) did not
// parse, and an unparseable date meant "undated", which bypassed the age
// filter and returned 2023 postings as current.
func TestListJobs_OldItemsWithUnpaddedOrOddDatesAreStillDropped(t *testing.T) {
	body := `<?xml version="1.0"?><rss version="2.0"><channel>
	<item><title>Old A</title><link>https://e.et/a</link><guid>a</guid><pubDate>Mon, 5 Feb 2023 10:00:00 +0000</pubDate></item>
	<item><title>Old B</title><link>https://e.et/b</link><guid>b</guid><pubDate>5 Feb 2023</pubDate></item>
	<item><title>Old C</title><link>https://e.et/c</link><guid>c</guid><pubDate>2023-02-05</pubDate></item>
	<item><title>Garbled</title><link>https://e.et/d</link><guid>d</guid><pubDate>sometime last spring</pubDate></item>
	<item><title>Fresh</title><link>https://e.et/e</link><guid>e</guid><pubDate>Tue, 15 Sep 2026 10:00:00 GMT</pubDate></item>
	<item><title>Fresh unpadded</title><link>https://e.et/f</link><guid>f</guid><pubDate>Tue, 8 Sep 2026 10:00:00 +0000</pubDate></item>
	</channel></rss>`
	jobs, err := newClient(&fakeDoer{status: 200, body: body}, allowAll).ListJobs(context.Background(), "https://e.et/feed/")
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	got := map[string]bool{}
	for _, j := range jobs {
		got[j.ExternalID] = true
	}
	if len(jobs) != 2 || !got["e"] || !got["f"] {
		t.Errorf("got %v, want only the two fresh items (old items in any date form and an unreadable date are dropped)", got)
	}
}

func TestParsePubDate(t *testing.T) {
	want := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	for _, in := range []string{
		"Mon, 14 Sep 2026 10:00:00 +0000",
		"Mon, 14 Sep 2026 10:00:00 GMT",
		"14 Sep 2026 10:00:00 +0000",
		"Mon, 14 Sep 2026 10:00:00 UTC",
		"Mon, 14 Sep 2026 10:00:00 +0000",
		"Mon, 14 Sep 2026 10:00:00 GMT",
		"14 Sep 26 10:00 +0000",
		"2026-09-14T10:00:00Z",
	} {
		if got := parsePubDate(in); !got.Equal(want) {
			t.Errorf("parsePubDate(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "soon", "2026-13-45"} {
		if got := parsePubDate(in); !got.IsZero() {
			t.Errorf("parsePubDate(%q) = %v, want zero", in, got)
		}
	}
}
