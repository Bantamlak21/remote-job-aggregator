// Package careers lists a company's openings from its own careers page
// when it has no ATS and no feed. The board token is the URL of the page
// that lists the openings (for example https://kifiya.com/jobs/). Every
// same-site link that points one level or more below that page's path is a
// candidate job (/jobs/legal-counsel/, /careers/<uuid>); each candidate is
// fetched for its title, description and dates.
//
// This is a heuristic, not a parser for any one site, and it is
// deliberately conservative about what it trusts:
//
//   - a page listing no candidate links is an error (ErrNoJobLinks), never
//     "the company has no openings": a redesigned or script-rendered page
//     looks identical, and an empty result would close every open job;
//   - a job with a published date older than MaxAge, or a validThrough in
//     the past, is not a current opening and is dropped (Kifiya's own site,
//     checked live, still lists postings from February 2025);
//   - all fetching goes through page.Fetcher, so robots.txt is honored and
//     every response is size-bounded.
//
// Which companies it is pointed at is a curated list (configs/), verified
// by hand; it is not run against arbitrary sites.
package careers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
)

// ErrNoJobLinks is returned when the listing page has no candidate job
// links. See the package comment for why this is an error.
var ErrNoJobLinks = errors.New("careers: no job links found on the listing page")

// ErrTooManyJobs is returned when a listing links more candidate jobs than
// the client will fetch. An error, not a truncation: see ListJobs.
var ErrTooManyJobs = errors.New("careers: too many candidate job links")

const (
	// DefaultMaxAge is how old a dated posting may be and still count.
	DefaultMaxAge = 120 * 24 * time.Hour
	// DefaultMaxJobs bounds how many detail pages one listing may cause. A
	// listing with more candidates fails (ErrTooManyJobs) rather than being cut.
	DefaultMaxJobs = 100
	// maxDescriptionRunes bounds one stored description.
	maxDescriptionRunes = 20000
)

// Fetcher fetches and parses a page (*page.Fetcher).
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (*page.Page, error)
}

// Client lists jobs from careers pages.
type Client struct {
	pages   Fetcher
	maxAge  time.Duration
	maxJobs int
	pause   time.Duration // delay between detail fetches, to be polite to small sites
	now     func() time.Time
}

// New returns a Client. maxAge <= 0 means DefaultMaxAge; maxJobs <= 0
// means DefaultMaxJobs; pause is the delay between detail-page fetches.
func New(pages Fetcher, maxAge time.Duration, maxJobs int, pause time.Duration) *Client {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	if maxJobs <= 0 {
		maxJobs = DefaultMaxJobs
	}
	return &Client{pages: pages, maxAge: maxAge, maxJobs: maxJobs, pause: pause, now: time.Now}
}

type candidate struct {
	key  string // host/path, the job's stable identity
	url  string
	text string // anchor text on the listing page
}

// ListJobs fetches the listing page at listingURL and every candidate job
// page below it.
func (c *Client) ListJobs(ctx context.Context, listingURL string) ([]ats.Job, error) {
	u, err := url.Parse(listingURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		strings.Trim(u.Path, "/") == "" {
		// A root URL would make every link on the site a "job".
		return nil, fmt.Errorf("careers: %q: %w", listingURL, ats.ErrInvalidBoardToken)
	}

	listing, err := c.pages.Fetch(ctx, listingURL)
	if err != nil {
		if errors.Is(err, page.ErrNotFound) {
			return nil, fmt.Errorf("careers: %s: %w", listingURL, ats.ErrBoardNotFound)
		}
		return nil, fmt.Errorf("careers: listing %s: %w", listingURL, err)
	}

	cands := candidates(listing)
	if len(cands) == 0 {
		return nil, fmt.Errorf("careers: %s: %w", listingURL, ErrNoJobLinks)
	}
	// Never silently truncate: this is a full source, so a job whose link
	// fell past the cut would be closed while still posted.
	if len(cands) > c.maxJobs {
		return nil, fmt.Errorf("careers: %s: %d candidate links, more than the %d this client will fetch: %w",
			listingURL, len(cands), c.maxJobs, ErrTooManyJobs)
	}

	jobs := make([]ats.Job, 0, len(cands))
	for i, cand := range cands {
		if i > 0 && c.pause > 0 {
			select {
			case <-time.After(c.pause):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		job, keep := c.detail(ctx, cand)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if keep {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}

// skipSegments are path segments that mark listing furniture, not a job.
var skipSegments = map[string]bool{
	"feed": true, "rss": true, "page": true, "apply": true, "category": true, "tag": true, "search": true,
	// Company-information pages that sit under a careers path ("/careers/culture").
	"culture": true, "benefits": true, "perks": true, "values": true, "about": true, "team": true,
	"faq": true, "faqs": true, "contact": true, "departments": true, "locations": true, "diversity": true,
	"life": true, "why-join-us": true, "how-we-hire": true, "hiring-process": true,
}

// skipExtensions are link targets that are files, not job pages.
var skipExtensions = map[string]bool{".pdf": true, ".doc": true, ".docx": true, ".png": true, ".jpg": true,
	".jpeg": true, ".gif": true, ".svg": true, ".zip": true, ".xml": true, ".css": true, ".js": true}

// candidates returns the listing's links that sit below the listing's own
// path on the same site, deduplicated by host+path, in page order.
func candidates(listing *page.Page) []candidate {
	base, err := url.Parse(listing.URL)
	if err != nil {
		return nil
	}
	basePath := strings.TrimRight(base.Path, "/")

	var out []candidate
	index := map[string]int{}
	for _, l := range listing.Links {
		lu, err := url.Parse(l.URL)
		if err != nil || !page.SameHost(base.Host, lu.Host) {
			continue
		}
		p := strings.TrimRight(lu.Path, "/")
		if !strings.HasPrefix(p, basePath+"/") || p == basePath {
			continue
		}
		rest := strings.TrimPrefix(p, basePath+"/")
		if hasSkippedSegment(rest) || skipExtensions[strings.ToLower(path.Ext(p))] {
			continue
		}
		// Tracking parameters are noise, but any other query parameter may be
		// the job's identity (/careers/job?id=7): dropping it would merge
		// distinct openings and store an apply link that points nowhere.
		lu.RawQuery = stripTracking(lu.Query())
		lu.Fragment = ""
		key := strings.ToLower(strings.TrimPrefix(strings.ToLower(lu.Host), "www.")) + p
		if lu.RawQuery != "" {
			key += "?" + lu.RawQuery
		}
		if i, seen := index[key]; seen {
			// Keep the first anchor that has text: the same job is often
			// linked twice, once by an image and once by its title.
			if out[i].text == "" {
				out[i].text = l.Text
			}
			continue
		}
		index[key] = len(out)
		out = append(out, candidate{key: key, url: lu.String(), text: l.Text})
	}
	return out
}

// stripTracking re-encodes q without analytics parameters, sorted by key so
// the same link written two ways gets one identity.
func stripTracking(q url.Values) string {
	kept := url.Values{}
	for k, v := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || lk == "ref" || lk == "fbclid" || lk == "gclid" || lk == "source" {
			continue
		}
		kept[k] = v
	}
	return kept.Encode()
}

func hasSkippedSegment(rest string) bool {
	for seg := range strings.SplitSeq(rest, "/") {
		if skipSegments[strings.ToLower(seg)] {
			return true
		}
	}
	return false
}

// genericAnchors are link texts that name an action, not the job.
var genericAnchors = map[string]bool{"read more": true, "apply": true, "apply now": true, "view": true,
	"view details": true, "details": true, "learn more": true, "more": true, "see more": true, "view job": true, "view more": true}

// maxAnchorTitleRunes is the longest link text still plausible as a job
// title. Longer text is a whole card (title, location, blurb) that happens
// to be one link.
const maxAnchorTitleRunes = 120

// anchorTitle returns the part of a listing link's text that can serve as
// a job title, or "". Career pages often make the whole job card one link:
// Zare's reads "Full-Stack AI Engineer→Addis Ababa · Remote · Full-Time
// As a Full-Stack AI Engineer at ..." (checked live), where the title is
// what precedes the arrow.
func anchorTitle(text string) string {
	text, _, _ = strings.Cut(text, "→")
	text = strings.TrimSpace(text)
	if genericAnchors[strings.ToLower(text)] || utf8.RuneCountInString(text) > maxAnchorTitleRunes {
		return ""
	}
	return text
}

// detail fetches one candidate. A page that cannot be fetched is not a
// reason to drop the job (it was on the listing): it is kept with the
// listing's own information, and stays "seen" so it is not closed.
func (c *Client) detail(ctx context.Context, cand candidate) (ats.Job, bool) {
	anchor := anchorTitle(cand.text)
	job := ats.Job{ExternalID: cand.key, Title: firstNonEmpty(anchor, titleFromSlug(cand.url)), URL: cand.url}

	p, err := c.pages.Fetch(ctx, cand.url)
	if err != nil {
		// A dead link (404/410) is not a job. Any other failure (timeout,
		// robots) keeps the job from the listing so it stays "seen".
		return job, !errors.Is(err, page.ErrNotFound)
	}

	var jp page.JobPosting
	if len(p.JobPostings) > 0 {
		jp = p.JobPostings[0]
	}

	posted := firstTime(jp.DatePosted, page.ParseDate(p.Meta["article:published_time"]), page.ParseDate(p.Meta["dateposted"]))
	now := c.now()
	if !posted.IsZero() && posted.Before(now.Add(-c.maxAge)) {
		return job, false
	}
	if !jp.ValidThrough.IsZero() && jp.ValidThrough.Before(now) {
		return job, false
	}

	job.Title = firstNonEmpty(jp.Title, p.H1, anchor, cleanPageTitle(p.Title), job.Title)
	job.PublishedAt = posted
	job.ExpiresAt = jp.ValidThrough
	job.LocationRaw = jp.Location
	if jp.Description != "" {
		job.Description = ats.HTMLToText(jp.Description)
	} else {
		job.Description = p.Text()
	}
	job.Description = truncateRunes(job.Description, maxDescriptionRunes)
	return job, true
}

// cleanPageTitle strips the " - Company" / " | Company" suffix WordPress
// and most CMSs append to <title>.
func cleanPageTitle(t string) string {
	for _, sep := range []string{" | ", " - ", " – ", " — "} {
		if i := strings.LastIndex(t, sep); i > 0 {
			t = t[:i]
		}
	}
	return ats.CleanText(t)
}

// titleFromSlug is the last resort: "legal-counsel" -> "Legal Counsel". A
// UUID or numeric slug is not a title and yields "".
func titleFromSlug(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	slug := path.Base(strings.TrimRight(u.Path, "/"))
	if slug == "." || slug == "/" || looksLikeID(slug) {
		return ""
	}
	words := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		r, size := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[size:]
	}
	return strings.Join(words, " ")
}

// looksLikeID reports whether s is mostly hex digits and dashes (a UUID or
// database id) rather than words.
func looksLikeID(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r == '-', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return len(s) >= 8
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstTime(vals ...time.Time) time.Time {
	for _, v := range vals {
		if !v.IsZero() {
			return v
		}
	}
	return time.Time{}
}

// truncateRunes cuts s to at most n runes on a rune boundary.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
