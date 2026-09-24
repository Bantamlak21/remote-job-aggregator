package jobsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/companymatch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
)

const (
	// maxKeywords bounds how many keyword searches one config may list, and
	// so how much of the Serper budget one run can spend.
	maxKeywords = 40
	// maxPagesPerKeyword bounds result pages per keyword.
	maxPagesPerKeyword = 3
	// maxKeywordRunes bounds one keyword phrase.
	maxKeywordRunes = 80
	// maxEmployerRunes bounds an employer name read from a result title.
	maxEmployerRunes = 120
)

// PageSearcher runs one page of a recency-limited web search
// (*search.Client).
type PageSearcher interface {
	SearchRecentPage(ctx context.Context, query string, recency search.Recency, page int) ([]search.Result, error)
}

// FreshConfig says which keyword searches the FreshClient runs.
type FreshConfig struct {
	// Keywords are phrases such as "software engineer"; each becomes one
	// search for LinkedIn jobs in Ethiopia matching it.
	Keywords []string `json:"keywords"`
	// Pages is how many result pages to read per keyword (default 1).
	Pages int `json:"pages"`
}

// LoadFreshConfig reads and validates the keyword list at path.
func LoadFreshConfig(path string) (FreshConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return FreshConfig{}, fmt.Errorf("jobsearch: opening %s: %w", path, err)
	}
	defer f.Close()

	var cfg FreshConfig
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return FreshConfig{}, fmt.Errorf("jobsearch: parsing %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return FreshConfig{}, fmt.Errorf("jobsearch: parsing %s: unexpected data after the config object", path)
	}
	if cfg.Pages == 0 {
		cfg.Pages = 1
	}
	if err := cfg.Validate(); err != nil {
		return FreshConfig{}, fmt.Errorf("jobsearch: %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the config is usable. Keywords are searched as plain
// text: quotes and search operators are refused so a keyword cannot widen
// the search beyond LinkedIn job pages in Ethiopia.
func (c FreshConfig) Validate() error {
	if len(c.Keywords) == 0 {
		return errors.New("no keywords listed")
	}
	if len(c.Keywords) > maxKeywords {
		return fmt.Errorf("%d keywords listed, more than the limit of %d", len(c.Keywords), maxKeywords)
	}
	if c.Pages < 1 || c.Pages > maxPagesPerKeyword {
		return fmt.Errorf("pages is %d, want 1 to %d", c.Pages, maxPagesPerKeyword)
	}
	seen := map[string]bool{}
	for i, k := range c.Keywords {
		k = strings.Join(strings.Fields(k), " ")
		switch {
		case k == "":
			return fmt.Errorf("keyword %d is blank", i+1)
		case utf8.RuneCountInString(k) > maxKeywordRunes:
			return fmt.Errorf("keyword %q is longer than %d characters", k, maxKeywordRunes)
		case hasSearchSyntax(k):
			return fmt.Errorf("keyword %q contains search syntax (quotes, parentheses, ':', OR, AND, or a word starting with '-' or '+'); write plain words", k)
		case seen[strings.ToLower(k)]:
			return fmt.Errorf("keyword %q is listed twice", k)
		}
		seen[strings.ToLower(k)] = true
	}
	return nil
}

// hasSearchSyntax reports whether a keyword carries Google operators.
func hasSearchSyntax(k string) bool {
	if strings.ContainsAny(k, `"()|:*~`) {
		return true
	}
	for _, w := range strings.Fields(k) {
		switch lw := strings.ToLower(w); {
		case lw == "or" || lw == "and":
			return true
		case strings.HasPrefix(w, "-") || strings.HasPrefix(w, "+"):
			return true
		}
	}
	return false
}

// Queries is how many search queries a full run spends (before retries).
func (c FreshConfig) Queries() int { return len(c.Keywords) * c.Pages }

// FreshClient is a collector of the newest LinkedIn jobs in Ethiopia. It
// searches the web (never LinkedIn itself) for each configured keyword,
// limited to results Google saw in the last day, and returns every verified
// job together with the employer LinkedIn names, so employers do not need to
// be listed in advance.
type FreshClient struct {
	searcher   PageSearcher
	cfg        FreshConfig
	budget     *Budget
	logger     *slog.Logger
	now        func() time.Time
	retryDelay time.Duration
	priority   string
}

// NewFresh returns a FreshClient. priorityProvider is the provider a priority
// company's jobs are stored under (see ingestion.PriorityRouter).
func NewFresh(searcher PageSearcher, cfg FreshConfig, budget *Budget, priorityProvider string, logger *slog.Logger) *FreshClient {
	return &FreshClient{searcher: searcher, cfg: cfg, budget: budget, logger: logger, now: time.Now,
		retryDelay: 2 * time.Second, priority: priorityProvider}
}

// StaleAfter says a run's results are a sample; see DefaultStaleAfter.
func (c *FreshClient) StaleAfter() time.Duration { return DefaultStaleAfter }

// Market says these are jobs in Ethiopia (the search is limited to them).
func (c *FreshClient) Market() market.Market { return market.Ethiopia }

// PriorityProvider is where a priority company's jobs from this collector
// are stored.
func (c *FreshClient) PriorityProvider() string { return c.priority }

// freshQuery builds the search for one keyword.
func freshQuery(keyword string) string {
	return `site:linkedin.com/jobs/view "Ethiopia" ` + strings.Join(strings.Fields(keyword), " ")
}

// Collect runs the keyword searches in order and returns the verified jobs
// and closure markers. It stops early when the query budget is spent or the
// key is rejected, keeping what it has; it fails only when nothing at all
// could be searched.
func (c *FreshClient) Collect(ctx context.Context) ([]ats.Job, error) {
	if err := c.cfg.Validate(); err != nil {
		return nil, fmt.Errorf("jobsearch: fresh config: %w", err)
	}
	now := c.now()

	var (
		jobs     []ats.Job
		seen     = map[string]bool{}
		skipped  = map[string]int{}
		results  int
		searched int
		failed   int
		firstErr error
	)
collect:
	for _, kw := range c.cfg.Keywords {
		q := freshQuery(kw)
		for page := 1; page <= c.cfg.Pages; page++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			rs, err := metered(ctx, c.budget, c.retryDelay, func() ([]search.Result, error) {
				return c.searcher.SearchRecentPage(ctx, q, search.RecencyDay, page)
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if errors.Is(err, ErrBudgetExhausted) || errors.Is(err, search.ErrUnauthorized) {
					if firstErr == nil {
						firstErr = err
					}
					c.logger.Warn("jobsearch: keyword searches stopped early; keeping what was found",
						"keyword", kw, "found", len(jobs), "error", err)
					break collect
				}
				failed++
				if firstErr == nil {
					firstErr = err
				}
				c.logger.Warn("jobsearch: a keyword search failed", "keyword", kw, "page", page, "error", err)
				break // later pages of a failed keyword are not tried
			}
			searched++
			results += len(rs)
			for _, r := range rs {
				job, reason := freshJob(r, now)
				if reason != "" {
					skipped[reason]++
					continue
				}
				if seen[job.ExternalID] {
					continue
				}
				seen[job.ExternalID] = true
				jobs = append(jobs, job)
			}
			if len(rs) == 0 {
				break // no further pages
			}
		}
	}
	if searched == 0 && firstErr != nil {
		return nil, fmt.Errorf("jobsearch: keyword search: %w", firstErr)
	}

	jobs = dedupeOpenings(jobs)
	c.logger.Info("jobsearch: keyword searches done",
		"keywords", len(c.cfg.Keywords), "searches", searched, "failed", failed,
		"results", results, "jobs", len(jobs)-countClosed(jobs), "ended_reported", countClosed(jobs),
		"skipped", skipped, "queries_used", c.budget.Used(), "queries_max", c.budget.Max())
	return jobs, nil
}

// freshEthiopia is the Ethiopia check for the keyword collector. Here nothing
// but the search text ties a result to Ethiopia, and that text is noisy: a
// LinkedIn page lists "similar jobs" and a phone-code list ("Ethiopia+251;
// Falkland Islands+500") that mention Ethiopia for jobs elsewhere, and
// LinkedIn serves any job on any country subdomain. So only the job's own
// location counts: when the result states one it must be Ethiopian (whole
// words, so "Addison, TX" is not), and when it states none the result must at
// least come from LinkedIn's Ethiopian site.
func freshEthiopia(host, location string, _ search.Result) bool {
	if location != "" {
		return companymatch.InEthiopia(location)
	}
	return strings.EqualFold(host, "et.linkedin.com")
}

// splitFreshSlug splits a URL slug "<title>-at-<company>" and returns the
// title part and the company's name. The slug, not the result title, is the
// source of truth for the company: result titles come in many shapes
// ("X at Acme - LinkedIn", "Acme hiring X in Y", "X at Acme | ACME", or no
// company at all). The title only supplies the company's real capitalization,
// when a run of its words spells the slug's company; otherwise the name is
// the slug's words capitalized.
//
// A job title may contain "-at-" ("engineer-at-scale-at-chapa") and so may a
// company name ("look-at-me-inc"); the slug alone cannot say which. The last
// "-at-" is used, except when the result title has the "<Company> hiring
// <Title>" shape, whose company text is unambiguous and picks the split.
func splitFreshSlug(slug, resultTitle string) (titleSlug, company string, ok bool) {
	const sep = "-at-"
	last := strings.LastIndex(slug, sep)
	if last <= 0 {
		return "", "", false
	}
	if before, _, found := strings.Cut(resultTitle, " hiring "); found {
		if want := slugify(before); want != "" {
			for i := last; i > 0; i = strings.LastIndex(slug[:i], sep) {
				if slug[i+len(sep):] == want {
					return slug[:i], ats.CleanText(before), true
				}
			}
		}
	}
	companySlug := slug[last+len(sep):]
	if companySlug == "" {
		return "", "", false
	}
	if name, found := spanSpelling(resultTitle, companySlug); found {
		return slug[:last], name, true
	}
	return slug[:last], capitalizeSlug(companySlug), true
}

// spanSpelling finds the shortest run of words in text whose slug is want.
func spanSpelling(text, want string) (string, bool) {
	if want == "" {
		return "", false
	}
	words := strings.Fields(text)
	const maxSpan = 14
	for i := range words {
		for j := i + 1; j <= len(words) && j-i <= maxSpan; j++ {
			if slugify(strings.Join(words[i:j], " ")) == want {
				return ats.CleanText(strings.Join(words[i:j], " ")), true
			}
		}
	}
	return "", false
}

func capitalizeSlug(slug string) string {
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// freshJob turns one result into a job, or returns why it was rejected.
func freshJob(r search.Result, now time.Time) (ats.Job, string) {
	u, slug, id, ok := parseLinkedInURL(r.URL)
	if !ok {
		return ats.Job{}, "not-a-job-url"
	}
	titleSlug, employer, ok := splitFreshSlug(slug, r.Title)
	if !ok || employer == "" || utf8.RuneCountInString(employer) > maxEmployerRunes {
		return ats.Job{}, "no-employer"
	}
	if companymatch.IsPlaceholderEmployer(employer) {
		return ats.Job{}, "no-employer"
	}
	job, reason := finishLinkedInJob(r, u, titleSlug, id, freshEthiopia, now)
	if reason != "" {
		return ats.Job{}, reason
	}
	job.Employer = employer
	return job, ""
}
