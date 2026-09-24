// Package jobsearch finds LinkedIn jobs through web search. LinkedIn has no
// API for reading jobs and its robots.txt disallows crawling, so its pages
// are never fetched: a job comes only from a search result itself (URL,
// title, snippet, date), and its link sends the person to LinkedIn to apply.
//
// Two clients share the parsing and verification rules:
//
//   - Client (a per-company target, board token = company name) searches for
//     one configured company's jobs;
//   - FreshClient (a collector) runs a fixed set of keyword searches limited
//     to the last day and collects whatever employers the results name.
//
// Web search matches text, not entities, so nothing a search returns is
// trusted on its own. For a configured company, a result becomes a job only
// if LinkedIn's URL slug ends in "-at-<company>-<id>" and that company
// equals one of the configured names ("chapa-de-indian-health" is not
// "Chapa"); for every result an Ethiopia signal is required; and the posting
// date is estimated from LinkedIn's job id, because the date Google shows is
// often only the crawl date.
//
// A search shows a sample of jobs, not all of them, so absence from one run
// proves nothing. Both clients therefore declare StaleAfter: ingestion closes
// their jobs only after they have gone unseen that long, instead of on the
// first run that misses them.
package jobsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/companymatch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
)

const (
	// DefaultStaleAfter is how long a job may go unseen before it is closed.
	DefaultStaleAfter = 21 * 24 * time.Hour
	// linkedInMaxAge is how old a LinkedIn result's date may be. LinkedIn
	// postings are typically live for about a month.
	linkedInMaxAge = 45 * 24 * time.Hour
	// maxQueryNames bounds how many names go into one search query.
	maxQueryNames = 4
	// maxDescriptionRunes bounds one stored description.
	maxDescriptionRunes = 20000
)

// ErrBudgetExhausted is returned when the run's search-query budget is
// spent. Serper's free tier is a fixed number of queries, not a monthly
// allowance, so a runaway loop would burn it for good.
var ErrBudgetExhausted = errors.New("jobsearch: search query budget exhausted")

// Searcher runs a recency-limited web search (*search.Client).
type Searcher interface {
	SearchRecent(ctx context.Context, query string, recency search.Recency) ([]search.Result, error)
}

// Company is one company the search client may be asked about.
type Company struct {
	Name    string
	Aliases []string // other names the company posts under, e.g. "Kifiya Financial Technologies"
	// HiresOutsideEthiopia says the company also posts jobs located
	// elsewhere (Gebeya has openings in Nairobi and Lagos). Without it, a
	// LinkedIn result must show an Ethiopia signal to be accepted; see
	// ethiopiaSignal for why.
	HiresOutsideEthiopia bool
}

// Budget caps how many search queries one process run may spend. Safe for
// concurrent use.
type Budget struct {
	max  int64
	used atomic.Int64
}

// NewBudget returns a Budget allowing max queries.
func NewBudget(max int) *Budget { return &Budget{max: int64(max)} }

// Take spends one query, or returns ErrBudgetExhausted.
func (b *Budget) Take() error {
	if b.used.Add(1) > b.max {
		b.used.Add(-1)
		return fmt.Errorf("%w (limit %d)", ErrBudgetExhausted, b.max)
	}
	return nil
}

// Used is how many queries have been spent.
func (b *Budget) Used() int { return int(b.used.Load()) }

// Max is the budget's limit.
func (b *Budget) Max() int { return int(b.max) }

// Client lists jobs for companies via web search.
type Client struct {
	searcher  Searcher
	budget    *Budget
	logger    *slog.Logger
	now       func() time.Time
	companies map[string]companyMatch // by nameKey of the company's primary name

	searchMu   sync.Mutex
	retryDelay time.Duration
}

type companyMatch struct {
	company Company
	keys    map[string]bool // nameKeys of every accepted name
}

// New returns a Client that can answer for the given companies.
func New(searcher Searcher, companies []Company, budget *Budget, logger *slog.Logger) *Client {
	c := &Client{searcher: searcher, budget: budget, logger: logger, now: time.Now,
		retryDelay: 2 * time.Second, companies: make(map[string]companyMatch, len(companies))}
	for _, co := range companies {
		m := companyMatch{company: co, keys: map[string]bool{}}
		for _, n := range append([]string{co.Name}, co.Aliases...) {
			if k := nameKey(n); k != "" {
				m.keys[k] = true
			}
		}
		c.companies[nameKey(co.Name)] = m
	}
	return c
}

// StaleAfter tells ingestion this client's listings are partial samples;
// see the package comment.
func (c *Client) StaleAfter() time.Duration { return DefaultStaleAfter }

// ListJobs searches LinkedIn for boardToken (a company name) and returns
// the verified, current jobs. It spends one query. (Ethiojobs is not
// searched here: internal/ats/ethiojobs reads that site directly and far
// more freshly.)
func (c *Client) ListJobs(ctx context.Context, boardToken string) ([]ats.Job, error) {
	m, ok := c.companies[nameKey(boardToken)]
	if !ok {
		return nil, fmt.Errorf("jobsearch: %q: %w", boardToken, ats.ErrInvalidBoardToken)
	}

	var stats runStats
	linkedIn, err := c.linkedInJobs(ctx, m, &stats)
	if err != nil {
		return nil, err
	}

	jobs := dedupeOpenings(linkedIn)
	c.logger.Info("jobsearch: company searched",
		"company", m.company.Name, "jobs", len(jobs),
		"linkedin_results", stats.linkedInResults, "linkedin_open", len(linkedIn)-countClosed(linkedIn),
		"ended_reported", countClosed(jobs),
		"skipped", stats.skipped, "queries_used", c.budget.Used(), "queries_max", c.budget.Max())
	return jobs, nil
}

// runStats counts what one ListJobs saw and why results were rejected, so
// a "0 jobs" outcome can be told apart from "search found nothing".
type runStats struct {
	linkedInResults int
	skipped         map[string]int
}

func (s *runStats) skip(reason string) {
	if s.skipped == nil {
		s.skipped = map[string]int{}
	}
	s.skipped[reason]++
}

// search runs one query. Calls are serialized across all companies: the
// ingester runs several targets at once, and Serper's HTTP/2 endpoint was
// seen (live, 2 of 25 companies) resetting streams under that parallelism.
func (c *Client) search(ctx context.Context, m companyMatch, site string) ([]search.Result, error) {
	c.searchMu.Lock()
	defer c.searchMu.Unlock()

	q := searchQuery(m.company, site)
	results, err := metered(ctx, c.budget, c.retryDelay, func() ([]search.Result, error) {
		return c.searcher.SearchRecent(ctx, q, search.RecencyMonth)
	})
	if err != nil {
		if errors.Is(err, ErrBudgetExhausted) || ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("jobsearch: %s: %w", m.company.Name, err)
	}
	return results, nil
}

// metered runs one search under a query budget. A transient failure is
// retried once after retryDelay; every attempt spends budget, since a
// request that failed after Serper received it may still have been billed.
// A canceled context and an expired or invalid key are not transient and are
// never retried.
func metered(ctx context.Context, budget *Budget, retryDelay time.Duration, do func() ([]search.Result, error)) ([]search.Result, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryDelay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err := budget.Take(); err != nil {
			return nil, err
		}
		results, err := do()
		if err == nil {
			return results, nil
		}
		lastErr = err
		if ctx.Err() != nil || errors.Is(err, search.ErrUnauthorized) {
			break
		}
	}
	return nil, lastErr
}

// searchQuery quotes the company's names (OR-ed, at most maxQueryNames)
// and restricts the search to one site.
func searchQuery(co Company, site string) string {
	var quoted []string
	seen := map[string]bool{}
	for _, n := range append([]string{co.Name}, co.Aliases...) {
		n = strings.TrimSpace(strings.ReplaceAll(n, `"`, ""))
		if k := nameKey(n); n != "" && !seen[k] && len(quoted) < maxQueryNames {
			seen[k] = true
			quoted = append(quoted, `"`+n+`"`)
		}
	}
	names := strings.Join(quoted, " OR ")
	if len(quoted) > 1 {
		names = "(" + names + ")"
	}
	return names + " site:" + site
}

// ---- LinkedIn ----

var linkedInURL = regexp.MustCompile(`(?i)^https://(?:www|[a-z]{2,3})\.linkedin\.com/jobs/view/([a-z0-9-]+)-(\d{6,})/?$`)

func (c *Client) linkedInJobs(ctx context.Context, m companyMatch, stats *runStats) ([]ats.Job, error) {
	results, err := c.search(ctx, m, "linkedin.com/jobs/view")
	if err != nil {
		return nil, err
	}
	stats.linkedInResults = len(results)

	var jobs []ats.Job
	for _, r := range results {
		job, reason := linkedInJob(r, m, c.now())
		if reason != "" {
			stats.skip("linkedin:" + reason)
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// linkedInJob turns one search result into a job, or returns why it was
// rejected. Pure function of its inputs (no network, no clock but now).
func linkedInJob(r search.Result, m companyMatch, now time.Time) (ats.Job, string) {
	u, slug, id, ok := parseLinkedInURL(r.URL)
	if !ok {
		return ats.Job{}, "not-a-job-url"
	}
	titleSlug, ok := splitCompanySlug(slug, m.keys)
	if !ok {
		return ats.Job{}, "other-company"
	}
	inEthiopia := ethiopiaSignal
	if m.company.HiresOutsideEthiopia {
		inEthiopia = func(string, string, search.Result) bool { return true }
	}
	return finishLinkedInJob(r, u, titleSlug, id, inEthiopia, now)
}

// parseLinkedInURL accepts only a LinkedIn job-view URL and returns it
// without query or fragment, its lower-cased slug and its numeric id.
func parseLinkedInURL(raw string) (u *url.URL, slug, id string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, "", "", false
	}
	u.RawQuery, u.Fragment = "", ""
	match := linkedInURL.FindStringSubmatch(u.String())
	if match == nil {
		return nil, "", "", false
	}
	return u, strings.ToLower(match[1]), match[2], true
}

// finishLinkedInJob applies the checks and mapping every LinkedIn result
// goes through once its company is settled: ended postings become closure
// markers, results without an Ethiopia signal are rejected (unless the
// company hires outside Ethiopia), and the posting date is estimated from
// the id. inEthiopia decides the Ethiopia check: the per-company client
// (whose company match already narrows things) and the keyword collector
// (where nothing does) need different strictness.
func finishLinkedInJob(r search.Result, u *url.URL, titleSlug, id string, inEthiopia func(host, location string, r search.Result) bool, now time.Time) (ats.Job, string) {
	// The result itself says the job has ended: report it as closed, so an
	// open row for it is closed now rather than after the stale window.
	if strings.Contains(strings.ToLower(r.Snippet), "no longer accepting applications") {
		return ats.Job{ExternalID: "linkedin:" + id, Closed: true}, ""
	}
	location := linkedInLocation(r)
	if !inEthiopia(u.Host, location, r) {
		return ats.Job{}, "outside-ethiopia"
	}
	// Age. Google's date beside a result is often the day it crawled the
	// page, not the day the job was posted (found live: a job ~18 months
	// old showed "1 month ago"), so it is only ever used to REJECT. The
	// posting date is estimated from LinkedIn's job id, which grows steadily
	// with time; see estimatePostedFromID.
	if d := parseResultDate(r.Date, now); !d.IsZero() && now.Sub(d) > linkedInMaxAge {
		return ats.Job{ExternalID: "linkedin:" + id, Closed: true}, ""
	}
	published := time.Time{}
	if n, err := strconv.ParseInt(id, 10, 64); err == nil {
		published = estimatePostedFromID(n)
		if published.After(now) {
			published = now
		}
		if now.Sub(published) > linkedInMaxAge {
			return ats.Job{ExternalID: "linkedin:" + id, Closed: true}, ""
		}
	}

	title := prettyTitle(titleSlug, r.Title)
	if title == "" {
		return ats.Job{}, "no-title"
	}
	return ats.Job{
		ExternalID:  "linkedin:" + id,
		Title:       title,
		URL:         u.String(),
		LocationRaw: linkedInLocation(r),
		Description: ats.CleanText(r.Snippet),
		PublishedAt: published,
	}, ""
}

// LinkedIn job ids increase with time at a roughly constant rate (checked
// live against about 70 results: the linear model below is within about two
// weeks across a year; e.g. it puts id 4404170965 at 2026-04-19 where Google
// said 2026-04-23, and id 4341349159 at 2025-11-22 where Google said
// 2025-11-18). Two anchors fix the line:
//
//   - id 4437765344 was posted 2026-07-07T08:16:27Z: Zare Innovations' own
//     careers page (JSON-LD datePosted) for the same "Full-Stack AI
//     Engineer" posting.
//   - id 4471199696 was posted no earlier than 2026-09-24T00:00Z: Google
//     showed it as "9 hours ago" on 2026-09-24.
//
// The rate is about 425,000 ids per day. A linear model drifts if LinkedIn's
// rate changes; re-anchor (new id + date pair) every few months, or when
// freshly posted jobs start looking old. Beyond the newest anchor the
// estimate can exceed "now"; callers clamp it.
const (
	anchorOldID    = 4437765344
	anchorNewID    = 4471199696
	idsPerDayScale = float64(anchorNewID-anchorOldID) / 78.6467 // days between the anchors
)

var anchorOldTime = time.Date(2026, 7, 7, 8, 16, 27, 0, time.UTC)

// estimatePostedFromID estimates when a LinkedIn job was posted from its id.
func estimatePostedFromID(id int64) time.Time {
	days := float64(id-anchorOldID) / idsPerDayScale
	return anchorOldTime.Add(time.Duration(days * float64(24*time.Hour)))
}

// splitCompanySlug splits "<title>-at-<company>" at an "-at-" such that the
// company part is one of the accepted names. It tries the last "-at-"
// first (a title may itself contain "-at-": "engineer-at-scale-at-chapa"),
// and returns the title part.
func splitCompanySlug(slug string, accepted map[string]bool) (titleSlug string, ok bool) {
	const sep = "-at-"
	for i := strings.LastIndex(slug, sep); i > 0; i = strings.LastIndex(slug[:i], sep) {
		if accepted[nameKey(slug[i+len(sep):])] {
			return slug[:i], true
		}
	}
	return "", false
}

// ethiopiaSignal reports whether a LinkedIn result is tied to Ethiopia: it
// is served from LinkedIn's Ethiopian site (et.linkedin.com), or its
// location text or snippet names Ethiopia or Addis Ababa.
//
// A company name is not a unique identifier. The slug check proves the
// result is posted by *a* company with the configured name; it cannot tell
// two companies with the same name apart. Found live: LinkedIn's
// "Finance Executive at DreamTech" in Noida, India matched the Ethiopian
// DreamTech exactly. Requiring an Ethiopia signal is what separates them.
func ethiopiaSignal(host, location string, r search.Result) bool {
	if strings.EqualFold(host, "et.linkedin.com") {
		return true
	}
	// The location is parsed (a bare "Addis" is fine there); the snippet and
	// title are free text, where "Addis" may be part of a company name.
	return companymatch.InEthiopia(location) || companymatch.InEthiopiaText(r.Snippet+" "+r.Title)
}

var (
	snippetLocation = regexp.MustCompile(`(?i)\bat .{1,120}? in ([^.]{2,80})\.`)
	titleLocation   = regexp.MustCompile(`(?i)\bhiring .+? in ([^|]{2,80}?)\s*(?:\||$)`)
	alertLocation   = regexp.MustCompile(`(?i)\bget notified about new .{1,120}? jobs in ([^.]{2,80})\.`)
)

// linkedInLocation extracts the location LinkedIn puts in its result text:
// "... at Chapa in Ethiopia. Full-time ...", "Chapa hiring X in Ethiopia
// | LinkedIn" or "Get notified about new X jobs in Addis Ababa, Ethiopia."
// Best effort; "" when no shape matches, or when the text was cut off with
// an ellipsis (a truncated "Adis ..." says nothing).
func linkedInLocation(r search.Result) string {
	for _, m := range []*regexp.Regexp{snippetLocation, alertLocation} {
		if match := m.FindStringSubmatch(r.Snippet); match != nil {
			return cleanLocation(match[1])
		}
	}
	if m := titleLocation.FindStringSubmatch(r.Title); m != nil {
		return cleanLocation(m[1])
	}
	return ""
}

func cleanLocation(s string) string {
	s = ats.CleanText(s)
	if strings.HasSuffix(s, "...") || strings.HasSuffix(s, "\u2026") {
		return ""
	}
	return s
}

// prettyTitle restores the real capitalization and punctuation of a job
// title from its URL slug. The slug is lower-case ASCII ("ui-ux-designer");
// the result's own title text contains the original ("UI/UX Designer"), so
// the contiguous run of words in the result title whose slug equals the URL
// slug is used. When none does (a truncated title), the slug's words are
// capitalized instead.
func prettyTitle(titleSlug, resultTitle string) string {
	words := strings.Fields(resultTitle)
	const maxSpan = 14
	for i := range words {
		for j := i + 1; j <= len(words) && j-i <= maxSpan; j++ {
			span := strings.Join(words[i:j], " ")
			if slugify(span) == titleSlug {
				return ats.CleanText(span)
			}
		}
	}
	parts := strings.Split(titleSlug, "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return ats.CleanText(strings.Join(parts, " "))
}

// ---- shared ----

// The comparison rules live in internal/companymatch so every source that
// matches employer names does it the same way.
var (
	nameKey  = companymatch.Key
	slugify  = companymatch.Slug
	titleKey = companymatch.TitleKey
)

// dedupeOpenings drops a later job that is the same opening as an earlier
// one: the same employer (empty for a per-company client, where every job has
// the same one), the same title (case-insensitively) and the same place, as
// companymatch.SamePlace defines it. The same opening is often posted on both
// sites; the same title in a different city (Addis Ababa and Hawassa) or
// country (Addis Ababa and Nairobi) is a different opening and both stay.
// Closed markers are never dropped. Callers order the input so the richer
// record comes first.
//
// A duplicate that is dropped is replaced by a closure marker for its own
// id: a copy stored on an earlier run (before the other site's copy was
// found) would otherwise stay open beside the kept one until the stale
// window ran out, showing the same opening twice.
func dedupeOpenings(jobs []ats.Job) []ats.Job {
	// Two passes so the outcome does not depend on result order: SamePlace is
	// not transitive (a location with no city matches every Ethiopian city,
	// which do not match each other), so jobs naming a city are settled first,
	// among themselves, and city-less ones are then compared with everything
	// kept.
	kept := map[string][]string{} // employer|title -> locations kept
	drop := make([]bool, len(jobs))
	for _, cityPass := range []bool{true, false} {
		for i, j := range jobs {
			if j.Closed || companymatch.HasCity(j.LocationRaw) != cityPass {
				continue
			}
			k := nameKey(j.Employer) + "|" + titleKey(j.Title)
			for _, loc := range kept[k] {
				if companymatch.SamePlace(loc, j.LocationRaw) {
					drop[i] = true
					break
				}
			}
			if !drop[i] {
				kept[k] = append(kept[k], j.LocationRaw)
			}
		}
	}
	out := jobs[:0:0]
	for i, j := range jobs {
		if drop[i] {
			j = ats.Job{ExternalID: j.ExternalID, Employer: j.Employer, Closed: true}
		}
		out = append(out, j)
	}
	return out
}

func countClosed(jobs []ats.Job) int {
	n := 0
	for _, j := range jobs {
		if j.Closed {
			n++
		}
	}
	return n
}

var relativeDate = regexp.MustCompile(`(?i)^(\d+)\s+(minute|hour|day|week|month|year)s?\s+ago$`)

// parseResultDate reads the date Google shows beside a result: "19 hours
// ago", "3 days ago", or "Sep 14, 2026". Zero when it is neither.
func parseResultDate(s string, now time.Time) time.Time {
	s = strings.TrimSpace(s)
	if m := relativeDate.FindStringSubmatch(s); m != nil {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil || n < 0 {
			return time.Time{}
		}
		unit := map[string]time.Duration{
			"minute": time.Minute, "hour": time.Hour, "day": 24 * time.Hour,
			"week": 7 * 24 * time.Hour, "month": 30 * 24 * time.Hour, "year": 365 * 24 * time.Hour,
		}[strings.ToLower(m[2])]
		// Anything older than a century is not a date: refuse it before the
		// multiplication can overflow time.Duration (about 292 years) and
		// wrap into the future.
		const maxAge = 100 * 365 * 24 * time.Hour
		if n > int(maxAge/unit) {
			return time.Time{}
		}
		return now.Add(-time.Duration(n) * unit)
	}
	for _, layout := range []string{"Jan 2, 2006", "2 Jan 2006", "January 2, 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
