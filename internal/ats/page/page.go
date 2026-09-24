// Package page fetches and parses ordinary HTML pages for the job sources
// that have no API (company career sites, Ethiojobs job pages): a
// robots.txt-gated, size-bounded GET, then one pass over the document tree
// that collects what those sources need — title, first h1, meta tags, JSON-LD
// JobPosting objects, links, and the readable text of the main content.
//
// It knows nothing about any particular site. Site-specific rules (which
// links are jobs, what counts as too old) live in the source packages that
// use it.
package page

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// MaxBodyBytes bounds how much of one page is read and parsed. Career and
// job pages are far smaller; the cap exists so a hostile or broken server
// cannot make a single fetch consume unbounded memory.
const MaxBodyBytes = 2 << 20

var (
	// ErrDisallowed means robots.txt forbids fetching the URL (or could not
	// be read, which is treated the same way). The page was not requested.
	ErrDisallowed = errors.New("page: fetch not allowed by robots.txt")
	// ErrNotFound means the server answered 404 or 410.
	ErrNotFound = errors.New("page: not found")
	// ErrCrossHostRedirect means the URL redirected to a different host,
	// whose robots.txt was never consulted.
	ErrCrossHostRedirect = errors.New("page: redirected to a different host")
	// ErrTooLarge means the response body exceeded MaxBodyBytes. It is an
	// error, never a silent truncation: a listing cut short would look like
	// a listing with fewer jobs, and a full source closes the rest.
	ErrTooLarge = errors.New("page: response body exceeds the size limit")
)

// Doer is the slice of *httpclient.Client the Fetcher needs.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Allower answers whether a URL may be fetched. *robots.Checker satisfies it.
type Allower interface {
	Allowed(ctx context.Context, rawURL string) (bool, error)
}

// Fetcher performs robots-gated page GETs.
type Fetcher struct {
	http   Doer
	robots Allower
}

// NewFetcher returns a Fetcher. Both dependencies are required: there is
// deliberately no way to build a Fetcher that skips the robots check.
func NewFetcher(doer Doer, robots Allower) *Fetcher {
	return &Fetcher{http: doer, robots: robots}
}

// Fetch downloads and parses rawURL.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Page, error) {
	allowed, err := f.robots.Allowed(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("page: %s: %w: %v", rawURL, ErrDisallowed, err)
	}
	if !allowed {
		return nil, fmt.Errorf("page: %s: %w", rawURL, ErrDisallowed)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("page: building request for %s: %w", rawURL, err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req = req.WithContext(httpclient.WithRedirectCheck(ctx, RedirectGuard(ctx, req.URL.Host, f.robots)))

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("page: fetching %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, fmt.Errorf("page: %s: %w", rawURL, ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("page: %s: unexpected status %d", rawURL, resp.StatusCode)
	}

	final := req.URL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL
	}
	if !SameHost(req.URL.Host, final.Host) {
		return nil, fmt.Errorf("page: %s -> %s: %w", rawURL, final, ErrCrossHostRedirect)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("page: reading %s: %w", rawURL, err)
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("page: %s: %w (limit %d bytes)", rawURL, ErrTooLarge, MaxBodyBytes)
	}
	return Parse(final.String(), body)
}

// RedirectGuard returns the check every robots-gated fetch installs on its
// request (httpclient.WithRedirectCheck): a redirect may only lead to the
// same site, and only to a URL robots.txt allows. Checking just the URL
// that was asked for would let any allowed URL launder a request to a
// disallowed one. ctx must be the caller's own context, not one carrying
// this check, so robots.txt lookups made from inside a redirect are not
// themselves guarded by it.
func RedirectGuard(ctx context.Context, originHost string, robots Allower) httpclient.RedirectCheck {
	return func(next *http.Request) error {
		if !SameHost(originHost, next.URL.Host) {
			return fmt.Errorf("%w: %s", ErrCrossHostRedirect, next.URL)
		}
		// https -> http would send the request in the clear.
		if next.URL.Scheme == "http" && next.Response != nil && next.Response.Request != nil &&
			next.Response.Request.URL.Scheme == "https" {
			return fmt.Errorf("%w: redirect from https to http: %s", ErrCrossHostRedirect, next.URL)
		}
		allowed, err := robots.Allowed(ctx, next.URL.String())
		if err != nil {
			return fmt.Errorf("%w: redirect to %s: %v", ErrDisallowed, next.URL, err)
		}
		if !allowed {
			return fmt.Errorf("%w: redirect to %s", ErrDisallowed, next.URL)
		}
		return nil
	}
}

// SameHost reports whether two URL hosts are the same site, ignoring case,
// a leading "www.", and the port.
func SameHost(a, b string) bool {
	return normalizeHost(a) == normalizeHost(b)
}

func normalizeHost(h string) string {
	h = strings.ToLower(h)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimPrefix(h, "www.")
}

// Link is one <a href> resolved against the page URL.
type Link struct {
	URL  string // absolute http(s) URL, fragment removed
	Text string // visible anchor text, whitespace-collapsed
}

// JobPosting is the subset of a schema.org JobPosting JSON-LD object the
// sources use.
type JobPosting struct {
	Title          string
	Description    string // HTML, as published
	DatePosted     time.Time
	ValidThrough   time.Time
	Location       string
	EmploymentType string
}

// Page is a parsed document.
type Page struct {
	URL         string
	Title       string
	H1          string
	Meta        map[string]string // lower-cased name/property/itemprop -> content; first occurrence wins
	JobPostings []JobPosting
	Links       []Link
	// Scripts maps a <script id="..."> to its body, for pages that ship
	// their data as an embedded JSON blob (Next.js's __NEXT_DATA__).
	Scripts map[string]string

	root *html.Node
}

// Parse parses body as HTML fetched from pageURL.
func Parse(pageURL string, body []byte) (*Page, error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, fmt.Errorf("page: parsing page URL %q: %w", pageURL, err)
	}
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("page: parsing HTML of %s: %w", pageURL, err)
	}
	p := &Page{URL: pageURL, Meta: map[string]string{}, Scripts: map[string]string{}, root: root}
	p.walk(root, base)
	return p, nil
}

func (p *Page) walk(n *html.Node, base *url.URL) {
	if n.Type == html.ElementNode {
		switch n.Data {
		case "title":
			if p.Title == "" {
				p.Title = ats.CleanText(collapse(textOf(n)))
			}
		case "h1":
			if p.H1 == "" {
				p.H1 = ats.CleanText(collapse(textOf(n)))
			}
		case "meta":
			key := strings.ToLower(firstNonEmpty(attr(n, "property"), attr(n, "name"), attr(n, "itemprop")))
			if key != "" {
				if _, seen := p.Meta[key]; !seen {
					p.Meta[key] = ats.CleanText(attr(n, "content"))
				}
			}
		case "a":
			if link, ok := resolveLink(base, attr(n, "href")); ok {
				p.Links = append(p.Links, Link{URL: link, Text: ats.CleanText(collapse(textOf(n)))})
			}
		case "script":
			if strings.EqualFold(attr(n, "type"), "application/ld+json") {
				p.JobPostings = append(p.JobPostings, parseJSONLD(rawText(n))...)
			}
			if id := attr(n, "id"); id != "" {
				if _, seen := p.Scripts[id]; !seen {
					p.Scripts[id] = rawText(n)
				}
			}
			return // never descend into script bodies
		case "style", "noscript", "template":
			return
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		p.walk(c, base)
	}
}

// Text returns the readable plain text of the page's main content: the
// first <main>, else the first <article>, else <body>.
func (p *Page) Text() string {
	// Site furniture is not the job: drop navigation, footers and sidebars
	// (idempotent; links were already collected during Parse).
	removeElements(p.root, "nav", "footer", "aside")
	for _, tag := range []string{"main", "article", "body"} {
		if n := findElement(p.root, tag); n != nil {
			var buf bytes.Buffer
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if err := html.Render(&buf, c); err != nil {
					return ""
				}
			}
			return ats.HTMLToText(buf.String())
		}
	}
	return ""
}

// removeElements detaches every element named in tags from the tree under n.
func removeElements(n *html.Node, tags ...string) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode && slices.Contains(tags, c.Data) {
			n.RemoveChild(c)
		} else {
			removeElements(c, tags...)
		}
		c = next
	}
}

func findElement(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElement(c, tag); found != nil {
			return found
		}
	}
	return nil
}

// resolveLink resolves href against base, keeping only absolute http(s)
// URLs and dropping the fragment.
func resolveLink(base *url.URL, href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return "", false
	}
	u, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	abs := base.ResolveReference(u)
	if (abs.Scheme != "http" && abs.Scheme != "https") || abs.Host == "" {
		return "", false
	}
	abs.Fragment = ""
	return abs.String(), true
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// textOf concatenates the text nodes under n, skipping script/style.
func textOf(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

// rawText returns the direct text children of n, which is where the HTML
// parser puts a script element's body (textOf deliberately skips it).
func rawText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// parseJSONLD extracts JobPosting objects from one ld+json script body,
// which may be a single object, an array, or an object with an @graph.
func parseJSONLD(raw string) []JobPosting {
	var doc any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &doc); err != nil {
		return nil
	}
	var out []JobPosting
	var visit func(v any, depth int)
	visit = func(v any, depth int) {
		if depth > 4 {
			return
		}
		switch t := v.(type) {
		case []any:
			for _, e := range t {
				visit(e, depth+1)
			}
		case map[string]any:
			if hasType(t["@type"], "JobPosting") {
				out = append(out, jobPostingFrom(t))
			}
			if g, ok := t["@graph"]; ok {
				visit(g, depth+1)
			}
		}
	}
	visit(doc, 0)
	return out
}

func hasType(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return strings.EqualFold(t, want)
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && strings.EqualFold(s, want) {
				return true
			}
		}
	}
	return false
}

func jobPostingFrom(m map[string]any) JobPosting {
	str := func(k string) string {
		s, _ := m[k].(string)
		return ats.CleanText(s)
	}
	jp := JobPosting{
		Title:          str("title"),
		Description:    str("description"),
		DatePosted:     ParseDate(str("datePosted")),
		ValidThrough:   ParseDate(str("validThrough")),
		EmploymentType: str("employmentType"),
	}
	if jp.EmploymentType == "" {
		if l, ok := m["employmentType"].([]any); ok && len(l) > 0 {
			jp.EmploymentType, _ = l[0].(string)
		}
	}
	jp.Location = locationOf(m["jobLocation"])
	return jp
}

// locationOf renders a JobPosting jobLocation (object or array) as
// "City, Region, Country" from whatever address parts exist.
func locationOf(v any) string {
	if l, ok := v.([]any); ok {
		if len(l) == 0 {
			return ""
		}
		v = l[0]
	}
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	addr, ok := m["address"].(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, k := range []string{"addressLocality", "addressRegion", "addressCountry"} {
		switch s := addr[k].(type) {
		case string:
			if s = ats.CleanText(s); s != "" {
				parts = append(parts, s)
			}
		case map[string]any:
			if name, _ := s["name"].(string); ats.CleanText(name) != "" {
				parts = append(parts, ats.CleanText(name))
			}
		}
	}
	return strings.Join(parts, ", ")
}

// ParseDate parses the date formats job pages actually publish: RFC 3339,
// a timestamp without zone, or a bare date. It returns the zero time for
// anything else, including the empty string.
func ParseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04:05.000000Z", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
