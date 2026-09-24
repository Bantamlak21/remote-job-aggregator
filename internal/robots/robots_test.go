package robots

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDoer serves canned robots.txt responses per URL and counts calls.
type fakeDoer struct {
	status map[string]int
	body   map[string]string
	err    map[string]error
	calls  atomic.Int32
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	u := req.URL.String()
	if err := f.err[u]; err != nil {
		return nil, err
	}
	status, ok := f.status[u]
	if !ok {
		status = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(f.body[u])),
	}, nil
}

func checker(robotsURL string, status int, body string) (*Checker, *fakeDoer) {
	d := &fakeDoer{status: map[string]int{robotsURL: status}, body: map[string]string{robotsURL: body}}
	return New(d, "remote-job-aggregator"), d
}

func TestAllowed_TableOfRealisticRobotsFiles(t *testing.T) {
	// Shape of the real https://ethiojobs.net/robots.txt: several one-rule
	// "*" groups, then Allow: /.
	ethiojobs := "User-agent: *\nDisallow: /auth/*\n\nUser-agent: *\nDisallow: /api/*\n\nUser-agent: *\nDisallow: /users/*\n\nUser-agent: *\nAllow: /\n"

	cases := []struct {
		name string
		body string
		path string
		want bool
	}{
		{"ethiojobs job page", ethiojobs, "/job/vR99iaawhz-manager", true},
		{"ethiojobs api", ethiojobs, "/api/jobs", false},
		{"ethiojobs auth", ethiojobs, "/auth/login", false},
		{"disallow all", "User-agent: *\nDisallow: /\n", "/careers", false},
		{"empty disallow allows all", "User-agent: *\nDisallow:\n", "/careers", true},
		{"no rules", "", "/careers", true},
		{"prefix disallow", "User-agent: *\nDisallow: /private\n", "/private/x", false},
		{"prefix disallow, other path", "User-agent: *\nDisallow: /private\n", "/public", true},
		{"longer allow beats shorter disallow", "User-agent: *\nDisallow: /jobs\nAllow: /jobs/open\n", "/jobs/open/1", true},
		{"longer disallow beats shorter allow", "User-agent: *\nAllow: /jobs\nDisallow: /jobs/closed\n", "/jobs/closed/1", false},
		{"tie goes to allow", "User-agent: *\nDisallow: /a\nAllow: /a\n", "/a", true},
		{"wildcard", "User-agent: *\nDisallow: /*/secret\n", "/x/secret", false},
		{"wildcard no match", "User-agent: *\nDisallow: /*/secret\n", "/x/public", true},
		{"end anchor blocks exact", "User-agent: *\nDisallow: /*.pdf$\n", "/a/b.pdf", false},
		{"end anchor allows longer", "User-agent: *\nDisallow: /*.pdf$\n", "/a/b.pdf?x=1", true},
		{"query is part of the matched path", "User-agent: *\nDisallow: /*?session=\n", "/a?session=1", false},
		{"comments and case", "USER-AGENT: *  # everyone\nDISALLOW: /x # no\n", "/x", false},
		{"specific group beats wildcard", "User-agent: *\nDisallow: /\n\nUser-agent: remote-job-aggregator\nAllow: /\n", "/careers", true},
		{"specific group can be stricter", "User-agent: *\nAllow: /\n\nUser-agent: remote-job-aggregator\nDisallow: /careers\n", "/careers", false},
		{"other bot's group is ignored", "User-agent: badbot\nDisallow: /\n", "/careers", true},
		{"stacked user-agent lines share rules", "User-agent: badbot\nUser-agent: remote-job-aggregator\nDisallow: /careers\n", "/careers", false},
		// Regressions found by adversarial review: each of these made the
		// parser fail OPEN (treat a disallowed path as allowed).
		{"BOM before the first line", "\uFEFFUser-agent: *\nDisallow: /\n", "/careers", false},
		{"CR-only line endings", "User-agent: *\rDisallow: /private\r", "/private/x", false},
		{"CRLF line endings", "User-agent: *\r\nDisallow: /private\r\n", "/private/x", false},
		{"CR-only, rule on the last line without terminator", "User-agent: *\rDisallow: /private", "/private/x", false},
		{"pattern percent-encoded, path literal", "User-agent: *\nDisallow: /jobs/%7Efoo\n", "/jobs/~foo", false},
		{"pattern literal, path percent-encoded", "User-agent: *\nDisallow: /jobs/~foo\n", "/jobs/%7efoo", false},
		{"lower-case hex escape of a reserved char equals upper-case", "User-agent: *\nDisallow: /a%2fb\n", "/a%2Fb", false},
		{"an encoded slash is not a slash", "User-agent: *\nDisallow: /a/b\n", "/a%2Fb", true},
		{"group for a PREFIX of our token does not govern us", "User-agent: *\nDisallow: /\n\nUser-agent: r\nAllow: /\n", "/careers", false},
		{"group for a longer token does not govern us", "User-agent: *\nDisallow: /\n\nUser-agent: remote-job-aggregator-extra\nAllow: /\n", "/careers", false},
		{"garbage lines are ignored", "this is not robots\n\x01:::\nDisallow /x\n", "/x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := checker("https://example.com/robots.txt", 200, tc.body)
			got, err := c.Allowed(context.Background(), "https://example.com"+tc.path)
			if err != nil {
				t.Fatalf("Allowed() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Allowed(%q) = %t, want %t\nrobots.txt:\n%s", tc.path, got, tc.want, tc.body)
			}
		})
	}
}

// A UTF-16 robots.txt reads as NUL-riddled garbage to a byte parser; ignoring
// it would treat every rule as absent.
func TestAllowed_NonTextRobotsFailsClosed(t *testing.T) {
	utf16 := "U\x00s\x00e\x00r\x00-\x00a\x00g\x00e\x00n\x00t\x00:\x00 \x00*\x00\n\x00D\x00i\x00s\x00a\x00l\x00l\x00o\x00w\x00:\x00 \x00/\x00\n\x00"
	c, _ := checker("https://example.com/robots.txt", 200, utf16)
	got, err := c.Allowed(context.Background(), "https://example.com/careers")
	if err != nil || got {
		t.Errorf("Allowed() = %t, %v for a UTF-16 robots.txt; want false, nil", got, err)
	}
}

func TestAllowed_MissingRobotsMeansAllowed(t *testing.T) {
	for _, status := range []int{404, 410, 401, 403} {
		c, _ := checker("https://example.com/robots.txt", status, "Disallow: /")
		got, err := c.Allowed(context.Background(), "https://example.com/careers")
		if err != nil || !got {
			t.Errorf("status %d: Allowed() = %t, %v; want true, nil (no robots.txt means no rules)", status, got, err)
		}
	}
}

func TestAllowed_UnreadableRobotsIsAnErrorNotPermission(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		c, _ := checker("https://example.com/robots.txt", status, "")
		got, err := c.Allowed(context.Background(), "https://example.com/careers")
		if got || !errors.Is(err, ErrUnavailable) {
			t.Errorf("status %d: Allowed() = %t, %v; want false, ErrUnavailable", status, got, err)
		}
	}

	d := &fakeDoer{err: map[string]error{"https://example.com/robots.txt": errors.New("connection refused")}}
	got, err := New(d, "remote-job-aggregator").Allowed(context.Background(), "https://example.com/careers")
	if got || !errors.Is(err, ErrUnavailable) {
		t.Errorf("network error: Allowed() = %t, %v; want false, ErrUnavailable", got, err)
	}
}

func TestAllowed_FetchesEachHostOnce(t *testing.T) {
	c, d := checker("https://example.com/robots.txt", 200, "User-agent: *\nDisallow: /x\n")
	for _, p := range []string{"/a", "/b", "/x", "/c"} {
		if _, err := c.Allowed(context.Background(), "https://example.com"+p); err != nil {
			t.Fatalf("Allowed(%s) error = %v", p, err)
		}
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("robots.txt fetched %d times, want 1 (cached per host)", got)
	}
}

func TestAllowed_HostsAreIndependentAndCaseInsensitive(t *testing.T) {
	d := &fakeDoer{
		status: map[string]int{"https://a.example/robots.txt": 200, "https://b.example/robots.txt": 200},
		body:   map[string]string{"https://a.example/robots.txt": "User-agent: *\nDisallow: /\n", "https://b.example/robots.txt": ""},
	}
	c := New(d, "remote-job-aggregator")
	if ok, _ := c.Allowed(context.Background(), "https://A.example/x"); ok {
		t.Errorf("a.example should be disallowed")
	}
	if ok, _ := c.Allowed(context.Background(), "https://b.example/x"); !ok {
		t.Errorf("b.example should be allowed")
	}
	if got := d.calls.Load(); got != 2 {
		t.Errorf("fetched robots.txt %d times, want 2", got)
	}
}

func TestAllowed_RejectsNonHTTPURLs(t *testing.T) {
	c, d := checker("https://example.com/robots.txt", 200, "")
	for _, u := range []string{"", "ftp://example.com/x", "javascript:alert(1)", "/relative", "https://", "://bad"} {
		if ok, err := c.Allowed(context.Background(), u); ok || err == nil {
			t.Errorf("Allowed(%q) = %t, %v; want false and an error", u, ok, err)
		}
	}
	if d.calls.Load() != 0 {
		t.Errorf("made %d network calls for invalid URLs, want 0", d.calls.Load())
	}
}

// A hostile robots.txt made of stars must not make matching explode.
func TestGlobMatch_HostileWildcardsStayFast(t *testing.T) {
	pattern := "/" + strings.Repeat("*a", 40) + "b"
	path := "/" + strings.Repeat("a", 2000)

	done := make(chan bool, 1)
	go func() { done <- matches(pattern, path) }()
	select {
	case got := <-done:
		if got {
			t.Errorf("matches() = true, want false (no trailing b)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("matches() took over 2s on a wildcard-heavy pattern (exponential backtracking)")
	}
}

// A single line longer than the scanner buffer stops parsing. Dropping the
// rules after it would silently turn "disallowed" into "allowed", so the
// site is treated as fully disallowed instead.
func TestParse_OversizedLineFailsClosed(t *testing.T) {
	body := "User-agent: *\nDisallow: /" + strings.Repeat("a", 200*1024) + "\nDisallow: /secret\n"
	c, _ := checker("https://example.com/robots.txt", 200, body)
	got, err := c.Allowed(context.Background(), "https://example.com/anything")
	if err != nil {
		t.Fatalf("Allowed() error = %v", err)
	}
	if got {
		t.Errorf("Allowed() = true for a robots.txt the parser could not finish reading; want fail-closed false")
	}
}
