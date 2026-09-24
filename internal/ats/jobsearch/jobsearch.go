// Package jobsearch finds a company's open jobs on the public job sites it
// posts to, using web search instead of a per-site integration. The board
// token is the company's name (as listed in configs/ethiopian_companies.json).
//
// Two sites are covered, each handled differently because they allow
// different things:
//
//   - LinkedIn: pages are never fetched (robots.txt disallows everything and
//     the site is behind a login wall). A job comes only from the search
//     result itself: its URL, title, snippet and date.
//   - Ethiojobs: job pages are public and robots.txt allows them, so each
//     candidate page is fetched (robots-gated) and the job is read from the
//     page's embedded data, which carries an authoritative status, publish
//     date and expiry.
//
// Web search matches text, not entities, so nothing a search returns is
// trusted on its own. A result becomes a job only if a deterministic check
// proves it belongs to the company: LinkedIn's URL slug ends in
// "-at-<company>-<id>" and that company must equal one of the configured
// names ("chapa-de-indian-health" is not "Chapa"), and an Ethiojobs page's
// own company field must equal one too.
//
// A search shows a sample of a company's jobs, not all of them, so absence
// from one run proves nothing. This client therefore declares StaleAfter:
// ingestion closes its jobs only after they have gone unseen that long,
// instead of on the first run that misses them.
package jobsearch

import (
	"context"
	"encoding/json"
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
	"unicode"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
)

const (
	// DefaultStaleAfter is how long a job may go unseen before it is closed.
	DefaultStaleAfter = 21 * 24 * time.Hour
	// linkedInMaxAge is how old a LinkedIn result's date may be. LinkedIn
	// postings are typically live for about a month.
	linkedInMaxAge = 45 * 24 * time.Hour
	// maxEthiojobsFetches bounds page fetches per company per run.
	maxEthiojobsFetches = 10
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

// Pages fetches and parses a page (*page.Fetcher).
type Pages interface {
	Fetch(ctx context.Context, rawURL string) (*page.Page, error)
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
	pages     Pages
	budget    *Budget
	logger    *slog.Logger
	now       func() time.Time
	companies map[string]companyMatch // by nameKey of the company's primary name

	searchMu   sync.Mutex
	retryDelay time.Duration
	pagePause  time.Duration // between successive Ethiojobs page fetches
}

type companyMatch struct {
	company Company
	keys    map[string]bool // nameKeys of every accepted name
}

// New returns a Client that can answer for the given companies.
func New(searcher Searcher, pages Pages, companies []Company, budget *Budget, logger *slog.Logger) *Client {
	c := &Client{searcher: searcher, pages: pages, budget: budget, logger: logger, now: time.Now,
		retryDelay: 2 * time.Second, pagePause: 500 * time.Millisecond, companies: make(map[string]companyMatch, len(companies))}
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

// ListJobs searches LinkedIn and Ethiojobs for boardToken (a company name)
// and returns the verified, current jobs. It spends two queries.
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
	ethiojobs, err := c.ethiojobsJobs(ctx, m, &stats)
	if err != nil {
		return nil, err
	}

	jobs := dedupeOpenings(append(ethiojobs, linkedIn...)) // Ethiojobs first: its records are richer
	c.logger.Info("jobsearch: company searched",
		"company", m.company.Name, "jobs", len(jobs),
		"linkedin_results", stats.linkedInResults, "linkedin_open", len(linkedIn)-countClosed(linkedIn),
		"ethiojobs_results", stats.ethiojobsResults, "ethiojobs_open", len(ethiojobs)-countClosed(ethiojobs),
		"ended_reported", countClosed(jobs),
		"skipped", stats.skipped, "queries_used", c.budget.Used(), "queries_max", c.budget.Max())
	return jobs, nil
}

// runStats counts what one ListJobs saw and why results were rejected, so
// a "0 jobs" outcome can be told apart from "search found nothing".
type runStats struct {
	linkedInResults  int
	ethiojobsResults int
	skipped          map[string]int
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
// A transient failure is retried once after retryDelay; every attempt
// spends budget, since a request that failed after Serper received it may
// still have been billed. An expired or invalid key is not transient and
// is never retried.
func (c *Client) search(ctx context.Context, m companyMatch, site string) ([]search.Result, error) {
	c.searchMu.Lock()
	defer c.searchMu.Unlock()

	q := searchQuery(m.company, site)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(c.retryDelay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err := c.budget.Take(); err != nil {
			return nil, err
		}
		results, err := c.searcher.SearchRecent(ctx, q, search.RecencyMonth)
		if err == nil {
			return results, nil
		}
		lastErr = err
		if ctx.Err() != nil || errors.Is(err, search.ErrUnauthorized) {
			break
		}
	}
	return nil, fmt.Errorf("jobsearch: %s: %w", m.company.Name, lastErr)
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
	u, err := url.Parse(strings.TrimSpace(r.URL))
	if err != nil {
		return ats.Job{}, "bad-url"
	}
	u.RawQuery, u.Fragment = "", ""
	match := linkedInURL.FindStringSubmatch(u.String())
	if match == nil {
		return ats.Job{}, "not-a-job-url"
	}
	slug, id := strings.ToLower(match[1]), match[2]

	titleSlug, ok := splitCompanySlug(slug, m.keys)
	if !ok {
		return ats.Job{}, "other-company"
	}

	// The result itself says the job has ended: report it as closed, so an
	// open row for it is closed now rather than after the stale window.
	if strings.Contains(strings.ToLower(r.Snippet), "no longer accepting applications") {
		return ats.Job{ExternalID: "linkedin:" + id, Closed: true}, ""
	}
	location := linkedInLocation(r)
	if !m.company.HiresOutsideEthiopia && !ethiopiaSignal(u.Host, location, r) {
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
		LocationRaw: location,
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
	text := strings.ToLower(location + " " + r.Snippet + " " + r.Title)
	return strings.Contains(text, "ethiopia") || strings.Contains(text, "addis ababa") || strings.Contains(text, "addis abeba")
}

var (
	snippetLocation = regexp.MustCompile(`(?i)\bat .{1,120}? in ([^.]{2,80})\.`)
	titleLocation   = regexp.MustCompile(`(?i)\bhiring .+? in ([^|]{2,80}?)\s*(?:\||$)`)
)

// linkedInLocation extracts the location LinkedIn puts in its result text:
// "... at Chapa in Ethiopia. Full-time ..." or "Chapa hiring X in Ethiopia
// | LinkedIn". Best effort; "" when neither shape matches.
func linkedInLocation(r search.Result) string {
	if m := snippetLocation.FindStringSubmatch(r.Snippet); m != nil {
		return ats.CleanText(m[1])
	}
	if m := titleLocation.FindStringSubmatch(r.Title); m != nil {
		return ats.CleanText(m[1])
	}
	return ""
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

// slugify lower-cases s and collapses every run of non-alphanumeric
// characters into a single dash, trimming leading and trailing dashes.
func slugify(s string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(s) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// ---- Ethiojobs ----

var ethiojobsURL = regexp.MustCompile(`^https://ethiojobs\.net/job/([A-Za-z0-9]{6,16})-[a-z0-9-]+/?$`)

// ethiojobsData is the part of a job page's __NEXT_DATA__ this client reads.
type ethiojobsData struct {
	Props struct {
		PageProps struct {
			Data struct {
				Title         string `json:"title"`
				Description   string `json:"description"`
				Requirement   string `json:"requirement"`
				HowToApply    string `json:"how_to_apply"`
				Status        string `json:"status"`
				DatePublished string `json:"date_published"`
				DateExpiry    string `json:"date_expiry"`
				City          string `json:"city"`
				State         string `json:"state"`
				Company       struct {
					Name string `json:"name"`
				} `json:"company"`
			} `json:"data"`
		} `json:"pageProps"`
	} `json:"props"`
}

func (c *Client) ethiojobsJobs(ctx context.Context, m companyMatch, stats *runStats) ([]ats.Job, error) {
	results, err := c.search(ctx, m, "ethiojobs.net/job")
	if err != nil {
		return nil, err
	}
	stats.ethiojobsResults = len(results)

	var jobs []ats.Job
	fetches := 0
	for _, r := range results {
		u, err := url.Parse(strings.TrimSpace(r.URL))
		if err != nil {
			stats.skip("ethiojobs:bad-url")
			continue
		}
		u.RawQuery, u.Fragment = "", ""
		match := ethiojobsURL.FindStringSubmatch(u.String())
		if match == nil {
			stats.skip("ethiojobs:not-a-job-url")
			continue
		}
		if fetches >= maxEthiojobsFetches {
			stats.skip("ethiojobs:fetch-cap")
			continue
		}
		// Be polite to a small site: successive job-page fetches are spaced.
		if fetches > 0 && c.pagePause > 0 {
			select {
			case <-time.After(c.pagePause):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		fetches++

		p, err := c.pages.Fetch(ctx, u.String())
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			stats.skip("ethiojobs:fetch-failed")
			c.logger.Warn("jobsearch: ethiojobs page not fetched", "url", u.String(), "error", err)
			continue
		}
		job, reason := ethiojobsJob(p, match[1], u.String(), m, c.now())
		if reason != "" {
			stats.skip("ethiojobs:" + reason)
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// ethiojobsJob reads one fetched Ethiojobs job page, or returns why it is
// not a current job of this company.
func ethiojobsJob(p *page.Page, id, jobURL string, m companyMatch, now time.Time) (ats.Job, string) {
	raw := p.Scripts["__NEXT_DATA__"]
	if raw == "" {
		return ats.Job{}, "no-data"
	}
	var doc ethiojobsData
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return ats.Job{}, "bad-data"
	}
	d := doc.Props.PageProps.Data

	// Whose posting is it? Checked first: nothing below is acted on for
	// another company's page.
	if !m.keys[nameKey(d.Company.Name)] {
		return ats.Job{}, "other-company"
	}

	// Allowlist, not blocklist: only a posting the site itself marks active
	// is shown. Verified live: open postings say "active", ended ones
	// "closed". An ended posting (closed, or past its expiry) is returned as
	// a closure so an already-stored copy is closed at once; any other
	// status is not trusted either way.
	published, expiry := page.ParseDate(d.DatePublished), page.ParseDate(d.DateExpiry)
	ended := ats.Job{ExternalID: "ethiojobs:" + id, Closed: true}
	if d.Status == "closed" {
		return ended, ""
	}
	if d.Status != "active" {
		return ats.Job{}, "not-active"
	}
	if !expiry.IsZero() && !expiry.After(now) {
		return ended, ""
	}
	if expiry.IsZero() && (published.IsZero() || now.Sub(published) > linkedInMaxAge) {
		return ats.Job{}, "no-expiry-and-old"
	}
	title := ats.CleanText(d.Title)
	if title == "" {
		return ats.Job{}, "no-title"
	}

	var parts []string
	for _, h := range []string{d.Description, d.Requirement, d.HowToApply} {
		if t := ats.HTMLToText(h); t != "" {
			parts = append(parts, t)
		}
	}
	return ats.Job{
		ExternalID:  "ethiojobs:" + id,
		Title:       title,
		URL:         jobURL,
		LocationRaw: joinLocation(d.City, d.State),
		Description: truncateRunes(strings.Join(parts, "\n\n"), maxDescriptionRunes),
		PublishedAt: published,
	}, ""
}

func joinLocation(city, state string) string {
	city, state = ats.CleanText(city), ats.CleanText(state)
	switch {
	case city == "" || strings.EqualFold(city, state):
		return state
	case state == "":
		return city
	}
	return city + ", " + state
}

// ---- shared ----

// corporateSuffixes are trailing name tokens that differ between how a
// company writes its own name and how a job site does ("Ethswitch S.C.",
// "Kifiya Financial Technology PLC", "Gebeya Inc.").
var corporateSuffixes = map[string]bool{"inc": true, "plc": true, "sc": true, "s": true, "c": true, "ltd": true,
	"llc": true, "pvt": true, "co": true, "company": true, "limited": true, "corp": true, "corporation": true,
	"sa": true, "gmbh": true, "et": true, "ethiopia": true}

// nameKey reduces a company name to a comparison key: lower-case ASCII
// words joined by dashes, with trailing corporate-form words dropped (but
// never all of them). "Ethswitch S.C." and "EthSwitch" share a key;
// "Chaka Gebeya" and "Gebeya Inc." do not.
func nameKey(name string) string {
	tokens := strings.Split(slugify(name), "-")
	for len(tokens) > 1 && corporateSuffixes[tokens[len(tokens)-1]] {
		tokens = tokens[:len(tokens)-1]
	}
	return strings.Join(tokens, "-")
}

// dedupeOpenings drops a later job that is the same opening as an earlier
// one: the same title (case-insensitively) in the same place. The same
// opening is often posted on both sites; the same title in a different
// country (Addis Ababa and Nairobi) is a different opening and both stay.
// "Same place" is deliberately coarse, because the sites word locations
// differently ("Addis Ababa" vs "Ethiopia"): a location is Ethiopian when it
// is empty or names Ethiopia or Addis Ababa, and otherwise only equal
// locations match. Closed markers are never dropped. Callers order the input
// so the richer record comes first.
//
// A duplicate that is dropped is replaced by a closure marker for its own
// id: a copy stored on an earlier run (before the other site's copy was
// found) would otherwise stay open beside the kept one until the stale
// window ran out, showing the same opening twice.
func dedupeOpenings(jobs []ats.Job) []ats.Job {
	seen := map[string]bool{}
	out := jobs[:0:0]
	for _, j := range jobs {
		if j.Closed {
			out = append(out, j)
			continue
		}
		k := titleKey(j.Title) + "|" + placeClass(j.LocationRaw)
		if seen[k] {
			out = append(out, ats.Job{ExternalID: j.ExternalID, Closed: true})
			continue
		}
		seen[k] = true
		out = append(out, j)
	}
	return out
}

// titleKey is a comparison key for a job title: lower-cased, with every run
// of characters that are not letters, digits, '#' or '+' collapsed to one
// space. Unlike slugify it keeps non-ASCII letters (Amharic titles must not
// all collapse to "") and the characters that make "C# Developer",
// "C++ Developer" and "C Developer" three different jobs.
func titleKey(title string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '#' || r == '+' {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSuffix(b.String(), " ")
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

// placeClass reduces a location string to "et" (Ethiopian or unknown) or
// its own lower-cased text.
func placeClass(location string) string {
	l := strings.ToLower(strings.TrimSpace(location))
	if l == "" || strings.Contains(l, "ethiopia") || strings.Contains(l, "addis") {
		return "et"
	}
	return l
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
