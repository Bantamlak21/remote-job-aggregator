package page

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const sampleHTML = `<!doctype html>
<html><head>
<title>  Legal Counsel -
  Kifiya Financial Technology </title>
<meta property="article:published_time" content="2025-02-25T08:35:52+00:00" />
<meta itemprop="datePosted" content="2025-11-07">
<meta name="Description" content="first">
<meta name="description" content="second wins? no, first wins">
<script type="application/ld+json">
{"@context":"https://schema.org","@graph":[
  {"@type":"WebSite","name":"x"},
  {"@type":["Thing","JobPosting"],"title":"Legal Counsel","description":"<p>Draft &amp; review contracts.</p>",
   "datePosted":"2026-09-01","validThrough":"2026-10-01T00:00:00Z","employmentType":["FULL_TIME"],
   "jobLocation":{"@type":"Place","address":{"addressLocality":"Addis Ababa","addressCountry":{"@type":"Country","name":"Ethiopia"}}}}
]}
</script>
<script>var jobs = "<a href='/evil'>not a link</a>";</script>
<style>a { color: red }</style>
</head><body>
<nav><a href="/">Home</a> <a href="/jobs/">All jobs</a></nav>
<main>
  <h1> Legal   Counsel </h1>
  <h1>Second heading</h1>
  <p>Draft and review contracts.</p>
  <ul><li>Law degree</li><li>5 years</li></ul>
  <a href="/jobs/other-job/?utm=1#apply">  Other   job </a>
  <a href="https://other.example/x">External</a>
  <a href="mailto:hr@kifiya.com">mail</a>
  <a href="javascript:void(0)">js</a>
  <a href="#top">top</a>
  <a href="">empty</a>
</main>
</body></html>`

func TestParse_CollectsTitleH1MetaLinksAndText(t *testing.T) {
	p, err := Parse("https://kifiya.com/jobs/legal-counsel/", []byte(sampleHTML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if p.Title != "Legal Counsel - Kifiya Financial Technology" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.H1 != "Legal Counsel" {
		t.Errorf("H1 = %q, want the first h1 with whitespace collapsed", p.H1)
	}
	if got := p.Meta["article:published_time"]; got != "2025-02-25T08:35:52+00:00" {
		t.Errorf("Meta[article:published_time] = %q", got)
	}
	if got := p.Meta["dateposted"]; got != "2025-11-07" {
		t.Errorf("Meta[dateposted] = %q (itemprop keys are lower-cased)", got)
	}
	if got := p.Meta["description"]; got != "first" {
		t.Errorf("Meta[description] = %q, want first occurrence", got)
	}

	want := []Link{
		{URL: "https://kifiya.com/", Text: "Home"},
		{URL: "https://kifiya.com/jobs/", Text: "All jobs"},
		{URL: "https://kifiya.com/jobs/other-job/?utm=1", Text: "Other job"},
		{URL: "https://other.example/x", Text: "External"},
	}
	if len(p.Links) != len(want) {
		t.Fatalf("Links = %+v, want %+v (mailto, javascript, fragment-only and empty hrefs are dropped; fragments stripped)", p.Links, want)
	}
	for i := range want {
		if p.Links[i] != want[i] {
			t.Errorf("Links[%d] = %+v, want %+v", i, p.Links[i], want[i])
		}
	}

	text := p.Text()
	for _, must := range []string{"Draft and review contracts.", "Law degree", "5 years"} {
		if !strings.Contains(text, must) {
			t.Errorf("Text() = %q, missing %q", text, must)
		}
	}
	if strings.Contains(text, "Home") {
		t.Errorf("Text() includes navigation outside <main>: %q", text)
	}
	if strings.Contains(text, "evil") || strings.Contains(text, "color: red") {
		t.Errorf("Text() leaked script/style content: %q", text)
	}
}

func TestText_DropsNavigationFootersAndSidebarsEvenWithoutAMainElement(t *testing.T) {
	body := `<body><nav><a href="/x">All Jobs</a></nav><div class="job"><h1>Odoo Developer</h1><p>Build ERP modules.</p>
	<aside>Related posts</aside></div><footer>Copyright Acme</footer></body>`
	p, err := Parse("https://example.com/jobs/1", []byte(body))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(p.Links) != 1 {
		t.Fatalf("Links = %+v; links inside <nav> are still collected (they are the listing's furniture only for Text)", p.Links)
	}
	for i := 0; i < 2; i++ { // idempotent
		text := p.Text()
		for _, must := range []string{"Odoo Developer", "Build ERP modules."} {
			if !strings.Contains(text, must) {
				t.Errorf("call %d: Text() = %q, missing %q", i+1, text, must)
			}
		}
		for _, mustNot := range []string{"All Jobs", "Related posts", "Copyright"} {
			if strings.Contains(text, mustNot) {
				t.Errorf("call %d: Text() = %q, should not contain %q", i+1, text, mustNot)
			}
		}
	}
}

func TestParse_CollectsScriptsByID(t *testing.T) {
	body := `<script id="__NEXT_DATA__" type="application/json">{"props":{"a":1}}</script><script>var x</script><script id="other">y</script><script id="other">second</script>`
	p, err := Parse("https://example.com/", []byte(body))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := p.Scripts["__NEXT_DATA__"]; got != `{"props":{"a":1}}` {
		t.Errorf("Scripts[__NEXT_DATA__] = %q", got)
	}
	if got := p.Scripts["other"]; got != "y" {
		t.Errorf("Scripts[other] = %q, want the first occurrence", got)
	}
	if len(p.Scripts) != 2 {
		t.Errorf("Scripts = %v, want only scripts that carry an id", p.Scripts)
	}
}

func TestParse_JSONLDJobPostingInGraph(t *testing.T) {
	p, err := Parse("https://kifiya.com/x", []byte(sampleHTML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(p.JobPostings) != 1 {
		t.Fatalf("JobPostings = %+v, want exactly the one JobPosting from @graph (WebSite ignored)", p.JobPostings)
	}
	jp := p.JobPostings[0]
	if jp.Title != "Legal Counsel" || jp.EmploymentType != "FULL_TIME" {
		t.Errorf("JobPosting = %+v", jp)
	}
	if !jp.DatePosted.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("DatePosted = %v", jp.DatePosted)
	}
	if !jp.ValidThrough.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ValidThrough = %v", jp.ValidThrough)
	}
	if jp.Location != "Addis Ababa, Ethiopia" {
		t.Errorf("Location = %q", jp.Location)
	}
	if !strings.Contains(jp.Description, "<p>Draft &amp; review contracts.</p>") {
		t.Errorf("Description = %q, want the published HTML", jp.Description)
	}
}

func TestParse_MalformedJSONLDAndHTMLDoNotFail(t *testing.T) {
	cases := map[string]string{
		"broken json":      `<script type="application/ld+json">{"@type": "JobPosting", </script><p>x</p>`,
		"json scalar":      `<script type="application/ld+json">42</script>`,
		"deep nesting":     `<script type="application/ld+json">` + strings.Repeat("[", 50) + strings.Repeat("]", 50) + `</script>`,
		"unclosed tags":    `<div><p>hello<a href="/x">link`,
		"empty":            ``,
		"binary garbage":   "\x00\x01\x02<a href=\"/ok\">ok</a>\xff\xfe",
		"jobposting typed": `<script type="application/ld+json">{"@type":"JobPosting"}</script>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("https://example.com/", []byte(body)); err != nil {
				t.Errorf("Parse() error = %v, want the parser to tolerate it", err)
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-03-23T14:19:06.000000Z", time.Date(2026, 3, 23, 14, 19, 6, 0, time.UTC)},
		{"2025-02-25T08:35:52+00:00", time.Date(2025, 2, 25, 8, 35, 52, 0, time.UTC)},
		{"2025-11-07", time.Date(2025, 11, 7, 0, 0, 0, 0, time.UTC)},
		{"2025-11-07T10:00:00", time.Date(2025, 11, 7, 10, 0, 0, 0, time.UTC)},
		{"  2025-11-07  ", time.Date(2025, 11, 7, 0, 0, 0, 0, time.UTC)},
		{"", time.Time{}},
		{"yesterday", time.Time{}},
		{"07/11/2025", time.Time{}},
	}
	for _, tc := range cases {
		if got := ParseDate(tc.in); !got.Equal(tc.want) {
			t.Errorf("ParseDate(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSameHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"kifiya.com", "kifiya.com", true},
		{"kifiya.com", "www.kifiya.com", true},
		{"WWW.Kifiya.com:443", "kifiya.com", true},
		{"kifiya.com", "erp.kifiya.com", false},
		{"kifiya.com", "kifiya.com.evil.example", false},
	}
	for _, tc := range cases {
		if got := SameHost(tc.a, tc.b); got != tc.want {
			t.Errorf("SameHost(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}

// ---- Fetcher ----

type fakeDoer struct {
	handler func(req *http.Request) (*http.Response, error)
	calls   []string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.calls = append(f.calls, req.URL.String())
	return f.handler(req)
}

func resp(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

type allowFunc func(string) (bool, error)

func (a allowFunc) Allowed(_ context.Context, u string) (bool, error) { return a(u) }

var allowAll = allowFunc(func(string) (bool, error) { return true, nil })

func TestFetch_ReturnsParsedPage(t *testing.T) {
	d := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Accept") == "" {
			t.Errorf("Fetch sent no Accept header")
		}
		return resp(req, 200, "<title>Hi</title><a href='/x'>x</a>"), nil
	}}
	p, err := NewFetcher(d, allowAll).Fetch(context.Background(), "https://example.com/careers")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if p.Title != "Hi" || len(p.Links) != 1 || p.Links[0].URL != "https://example.com/x" {
		t.Errorf("page = %+v", p)
	}
}

func TestFetch_RobotsDisallowedNeverHitsTheNetwork(t *testing.T) {
	d := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) {
		t.Fatalf("network request made for a robots-disallowed URL: %s", req.URL)
		return nil, nil
	}}
	deny := allowFunc(func(string) (bool, error) { return false, nil })
	_, err := NewFetcher(d, deny).Fetch(context.Background(), "https://example.com/careers")
	if !errors.Is(err, ErrDisallowed) {
		t.Fatalf("Fetch() error = %v, want ErrDisallowed", err)
	}
}

func TestFetch_RobotsErrorIsTreatedAsDisallowed(t *testing.T) {
	d := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) {
		t.Fatalf("network request made when robots.txt was unreadable: %s", req.URL)
		return nil, nil
	}}
	broken := allowFunc(func(string) (bool, error) { return false, errors.New("robots.txt unavailable") })
	_, err := NewFetcher(d, broken).Fetch(context.Background(), "https://example.com/careers")
	if !errors.Is(err, ErrDisallowed) {
		t.Fatalf("Fetch() error = %v, want ErrDisallowed (fail closed)", err)
	}
}

func TestFetch_StatusHandling(t *testing.T) {
	for status, wantNotFound := range map[int]bool{404: true, 410: true, 500: false, 403: false, 302: false} {
		d := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) { return resp(req, status, "x"), nil }}
		_, err := NewFetcher(d, allowAll).Fetch(context.Background(), "https://example.com/x")
		if err == nil {
			t.Errorf("status %d: Fetch() succeeded, want an error", status)
			continue
		}
		if errors.Is(err, ErrNotFound) != wantNotFound {
			t.Errorf("status %d: errors.Is(ErrNotFound) = %t, want %t (err = %v)", status, !wantNotFound, wantNotFound, err)
		}
	}
}

func TestFetch_RejectsCrossHostRedirect(t *testing.T) {
	d := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) {
		moved, _ := http.NewRequest(http.MethodGet, "https://elsewhere.example/landing", nil)
		return resp(moved, 200, "<title>Other</title>"), nil
	}}
	_, err := NewFetcher(d, allowAll).Fetch(context.Background(), "https://example.com/careers")
	if !errors.Is(err, ErrCrossHostRedirect) {
		t.Fatalf("Fetch() error = %v, want ErrCrossHostRedirect", err)
	}
}

func TestFetch_AllowsWwwRedirectOnSameSite(t *testing.T) {
	d := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) {
		moved, _ := http.NewRequest(http.MethodGet, "https://www.example.com/careers/", nil)
		return resp(moved, 200, "<title>Careers</title>"), nil
	}}
	p, err := NewFetcher(d, allowAll).Fetch(context.Background(), "https://example.com/careers")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if p.URL != "https://www.example.com/careers/" {
		t.Errorf("Page.URL = %q, want the post-redirect URL so relative links resolve against it", p.URL)
	}
}

// An oversized body is an error, not a silent truncation: a cut-off
// listing would look like a listing with fewer jobs.
func TestFetch_OversizedBodyIsAnErrorNotATruncation(t *testing.T) {
	big := "<title>t</title>" + strings.Repeat("x", MaxBodyBytes)
	d := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) { return resp(req, 200, big), nil }}
	_, err := NewFetcher(d, allowAll).Fetch(context.Background(), "https://example.com/x")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Fetch() error = %v, want ErrTooLarge", err)
	}
}

func TestFetch_BodyExactlyAtTheLimitIsAccepted(t *testing.T) {
	head := "<title>t</title>"
	exact := head + strings.Repeat("x", MaxBodyBytes-len(head))
	d := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) { return resp(req, 200, exact), nil }}
	if _, err := NewFetcher(d, allowAll).Fetch(context.Background(), "https://example.com/x"); err != nil {
		t.Fatalf("Fetch() of a body of exactly MaxBodyBytes failed: %v", err)
	}
	over := exact + "x"
	d2 := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) { return resp(req, 200, over), nil }}
	if _, err := NewFetcher(d2, allowAll).Fetch(context.Background(), "https://example.com/x"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Fetch() of MaxBodyBytes+1 error = %v, want ErrTooLarge", err)
	}
}

func TestFetch_NetworkErrorIsReturned(t *testing.T) {
	d := &fakeDoer{handler: func(req *http.Request) (*http.Response, error) { return nil, errors.New("boom") }}
	if _, err := NewFetcher(d, allowAll).Fetch(context.Background(), "https://example.com/x"); err == nil {
		t.Fatal("Fetch() succeeded on a network error")
	}
}
