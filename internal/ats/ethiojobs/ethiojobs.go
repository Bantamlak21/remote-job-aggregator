// Package ethiojobs collects the newest jobs from ethiojobs.net, Ethiopia's
// largest job board, straight from its public listing pages.
//
// ethiojobs.net/jobs?page=N lists every active posting newest first (12 per
// page, about 100 pages, roughly 1,200 jobs), and each page embeds its data
// as JSON in the Next.js __NEXT_DATA__ script: title, employer, description,
// location, published date and application deadline, all to the minute. The
// site's robots.txt allows these pages (it disallows only /api/*, which is
// never used), so every fetch goes through the robots-gated page.Fetcher.
//
// Ethiojobs is a many-employer board, so this is a Collector, not a
// per-company client: it returns jobs naming their Employer and the
// ingester creates a company and target for each.
package ethiojobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
	"github.com/Bantamlak12/remote-job-aggregator/internal/companymatch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

const (
	// baseURL is the site root.
	baseURL = "https://ethiojobs.net"
	// DefaultMaxPages covers every active posting (the site had 100 pages).
	DefaultMaxPages = 100
	// hardMaxPages is the most pages one run may ever request, whatever the
	// caller asks for.
	hardMaxPages = 200
	// DefaultMaxAge is how old a posting may be and still be collected.
	DefaultMaxAge = 20 * 24 * time.Hour
	// DefaultStaleAfter is how long a stored job may go unseen before it is
	// closed. The listing is a sample whenever fewer pages than exist are
	// read, so absence from one run proves nothing.
	DefaultStaleAfter = 14 * 24 * time.Hour
	// maxDescriptionRunes bounds one stored description.
	maxDescriptionRunes = 20000
)

// Fetcher fetches and parses a page (*page.Fetcher).
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (*page.Page, error)
}

// Collector reads the newest-jobs listing.
type Collector struct {
	pages    Fetcher
	maxPages int
	maxAge   time.Duration
	pause    time.Duration // between page fetches, to be polite
	logger   *slog.Logger
	now      func() time.Time
}

// New returns a Collector. maxPages <= 0 means DefaultMaxPages (and is
// capped at 200); maxAge <= 0 means DefaultMaxAge; pause is the delay
// between page fetches.
func New(pages Fetcher, maxPages int, maxAge, pause time.Duration, logger *slog.Logger) *Collector {
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}
	maxPages = min(maxPages, hardMaxPages)
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	return &Collector{pages: pages, maxPages: maxPages, maxAge: maxAge, pause: pause, logger: logger, now: time.Now}
}

// StaleAfter says the listing is a sample; see DefaultStaleAfter.
func (c *Collector) StaleAfter() time.Duration { return DefaultStaleAfter }

// Market says these are jobs in Ethiopia.
func (c *Collector) Market() market.Market { return market.Ethiopia }

// nextData is the part of a listing page's __NEXT_DATA__ this package reads.
type nextData struct {
	Props struct {
		PageProps struct {
			Jobs *struct {
				// Decoded one job at a time so a single odd record (a field of an
				// unexpected type) costs that job, not the page.
				Data []json.RawMessage `json:"data"`
				Meta struct {
					LastPage int `json:"lastPage"`
				} `json:"meta"`
			} `json:"jobs"`
		} `json:"pageProps"`
	} `json:"props"`
}

type listedJob struct {
	Title         string `json:"title"`
	Slug          string `json:"slug"`
	Description   string `json:"description"`
	DatePublished string `json:"date_published"`
	DateExpiry    string `json:"date_expiry"`
	State         string `json:"state"`
	City          string `json:"city"`
	Company       *struct {
		Name string `json:"name"`
	} `json:"company"`
}

// Collect reads listing pages newest first until it passes maxAge, reaches
// the last page or maxPages, and returns the jobs. Page 1 failing is an
// error (nothing was learned, and an empty result must never be mistaken
// for "no jobs"); a later page failing ends the run with what was already
// collected, which is a valid, shorter sample.
func (c *Collector) Collect(ctx context.Context) ([]ats.Job, error) {
	now := c.now()
	cutoff := now.Add(-c.maxAge)

	var jobs []ats.Job
	seen := map[string]bool{}
	lastPage := c.maxPages
	for n := 1; n <= min(lastPage, c.maxPages); n++ {
		if n > 1 && c.pause > 0 {
			select {
			case <-time.After(c.pause):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		list, meta, err := c.fetchPage(ctx, n)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if n == 1 {
				return nil, err
			}
			c.logger.Warn("ethiojobs: stopping at a page that failed; keeping the jobs already collected",
				"page", n, "collected", len(jobs), "error", err)
			break
		}
		if n == 1 && len(list) == 0 {
			// A page that decodes but lists nothing is a changed site or a
			// broken response, not "no jobs": the site holds about 1,200.
			return nil, errors.New("ethiojobs: page 1 lists no jobs (the site's layout may have changed)")
		}
		if meta > 0 {
			lastPage = meta
		}

		oldest := now
		for _, lj := range list {
			job, ok := c.convert(lj, now, cutoff)
			if pub := parseTime(lj.DatePublished); !pub.IsZero() && pub.Before(oldest) {
				oldest = pub
			}
			if !ok || seen[job.ExternalID] {
				continue
			}
			seen[job.ExternalID] = true
			jobs = append(jobs, job)
		}
		// Newest first: once a page's oldest job is past the cutoff, every
		// later page is older still.
		if len(list) == 0 || oldest.Before(cutoff) {
			break
		}
	}
	return jobs, nil
}

// fetchPage returns one listing page's jobs and the reported last page.
func (c *Collector) fetchPage(ctx context.Context, n int) ([]listedJob, int, error) {
	u := fmt.Sprintf("%s/jobs?page=%d", baseURL, n)
	p, err := c.pages.Fetch(ctx, u)
	if err != nil {
		if errors.Is(err, page.ErrNotFound) && n > 1 {
			return nil, 0, nil // past the last page
		}
		return nil, 0, fmt.Errorf("ethiojobs: page %d: %w", n, err)
	}
	raw := p.Scripts["__NEXT_DATA__"]
	if raw == "" {
		return nil, 0, fmt.Errorf("ethiojobs: page %d: no embedded data (the site's layout may have changed)", n)
	}
	var doc nextData
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, 0, fmt.Errorf("ethiojobs: page %d: embedded data is not JSON: %w", n, err)
	}
	if doc.Props.PageProps.Jobs == nil {
		return nil, 0, fmt.Errorf("ethiojobs: page %d: no jobs list in the embedded data (the site's layout may have changed)", n)
	}
	list := make([]listedJob, 0, len(doc.Props.PageProps.Jobs.Data))
	bad := 0
	for _, raw := range doc.Props.PageProps.Jobs.Data {
		var lj listedJob
		if err := json.Unmarshal(raw, &lj); err != nil {
			bad++
			continue
		}
		list = append(list, lj)
	}
	if bad > 0 {
		c.logger.Warn("ethiojobs: skipped listed jobs that could not be decoded", "page", n, "skipped", bad)
	}
	return list, doc.Props.PageProps.Jobs.Meta.LastPage, nil
}

// convert maps one listed job. ok is false for a job that must not be
// stored and needs no closure either (too old, unreadable, no employer).
// A posting past its deadline comes back as a closure marker so a stored
// copy is closed.
func (c *Collector) convert(lj listedJob, now, cutoff time.Time) (ats.Job, bool) {
	id, slug := jobID(lj.Slug)
	employer := ""
	if lj.Company != nil {
		employer = ats.CleanText(lj.Company.Name)
	}
	title := ats.CleanText(lj.Title)
	if id == "" || companymatch.IsPlaceholderEmployer(employer) || title == "" {
		return ats.Job{}, false
	}

	expires := parseTime(lj.DateExpiry)
	if !expires.IsZero() && !expires.After(now) {
		return ats.Job{ExternalID: id, Employer: employer, Closed: true}, true
	}
	published := parseTime(lj.DatePublished)
	if published.IsZero() || published.Before(cutoff) {
		return ats.Job{}, false
	}
	return ats.Job{
		ExternalID:  id,
		Employer:    employer,
		Title:       title,
		URL:         baseURL + "/job/" + slug,
		LocationRaw: joinLocation(lj.City, lj.State),
		Description: truncateRunes(ats.HTMLToText(lj.Description), maxDescriptionRunes),
		PublishedAt: published,
		ExpiresAt:   expires,
	}, true
}

// jobID splits a slug like "vR99iaawhz-manager-product-development" into the
// site's own job id (the part before the first dash) and the whole slug. Both
// are "" unless the slug is made only of letters, digits and dashes and the id
// looks like one, so nothing odd can end up in a URL.
func jobID(slug string) (id, cleanSlug string) {
	slug = strings.TrimSpace(slug)
	if slug == "" || len(slug) > 300 {
		return "", ""
	}
	for _, r := range slug {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return "", ""
		}
	}
	id, _, _ = strings.Cut(slug, "-")
	if len(id) < 6 || len(id) > 16 {
		return "", ""
	}
	return id, slug
}

func joinLocation(city, state string) string {
	city, state = unspecified(ats.CleanText(city)), unspecified(ats.CleanText(state))
	switch {
	case city == "" || strings.EqualFold(city, state):
		return state
	case state == "":
		return city
	}
	return city + ", " + state
}

// unspecified turns the site's "Not Specified" filler into "".
func unspecified(s string) string {
	switch strings.ToLower(s) {
	case "not specified", "n/a", "none":
		return ""
	}
	return s
}

func parseTime(s string) time.Time { return page.ParseDate(strings.TrimSpace(s)) }

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
