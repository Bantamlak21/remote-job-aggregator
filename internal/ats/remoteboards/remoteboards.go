// Package remoteboards collects remote jobs from public remote-job boards:
// Remotive, Remote OK, Himalayas, Jobicy, We Work Remotely and Working
// Nomads. Each publishes a free API or RSS feed meant for exactly this use,
// and each asks for the same thing in return: a visible credit and a link
// back to its own listing. So every job's URL is the board's own page for it
// (never the employer's), and Job.Source, reported by the API, lets the UI
// print "via <board>".
//
// Every board is a many-employer source, so each is an ingestion.Collector:
// it returns jobs naming their employer and the ingester creates a company and
// target per employer. All of them are worldwide-market sources and all of
// their jobs are remote by definition (the boards list nothing else).
//
// A board's feed is a sample (the newest N jobs), so absence from one run
// proves nothing: jobs are closed only after DefaultStaleAfter unseen.
//
// Rate limits are respected by design, not by retrying: each Collect makes a
// handful of requests (one for most boards, one per page for Himalayas), and a
// 429 or 5xx is reported, never retried. Remotive asks for at most 4 requests
// a day, and Himalayas refreshes its data every 24 hours, so a daily run is
// the intended rhythm.
package remoteboards

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/companymatch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

const (
	// DefaultMaxAge is how old a posting may be and still be collected.
	DefaultMaxAge = 20 * 24 * time.Hour
	// DefaultStaleAfter is how long a stored job may go unseen before it is
	// closed; a board's feed shows only its newest jobs.
	DefaultStaleAfter = 14 * 24 * time.Hour

	// maxBodyBytes bounds one response (the largest, We Work Remotely's feed,
	// is under 1 MiB).
	maxBodyBytes = 8 << 20
	// maxDescriptionRunes bounds one stored description.
	maxDescriptionRunes = 20000
)

// Doer is the slice of *httpclient.Client the boards need.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ErrRateLimited is returned when a board answers 429: the run stops and the
// board is left alone until the next run.
var ErrRateLimited = errors.New("remoteboards: rate limited by the board")

// board holds what every board client shares.
type board struct {
	name   string // for messages and logs
	http   Doer
	logger *slog.Logger
	now    func() time.Time
	maxAge time.Duration
}

func newBoard(name string, doer Doer, logger *slog.Logger) board {
	return board{name: name, http: doer, logger: logger, now: time.Now, maxAge: DefaultMaxAge}
}

// StaleAfter says the board's feed is a sample; see DefaultStaleAfter.
func (board) StaleAfter() time.Duration { return DefaultStaleAfter }

// Market says every job here is in the worldwide list.
func (board) Market() market.Market { return market.Worldwide }

// get fetches url once (never retried: these boards ask for few requests and
// block excessive ones) and returns the body, bounded by maxBodyBytes.
func (b board) get(ctx context.Context, url string) ([]byte, error) {
	ctx = httpclient.WithoutRetries(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: building request: %w", b.name, err)
	}
	req.Header.Set("Accept", "application/json, application/rss+xml, application/xml;q=0.9, */*;q=0.5")
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", b.name, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%s: %w", b.name, ErrRateLimited)
	case resp.StatusCode != http.StatusOK:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("%s: unexpected status %d: %s", b.name, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: reading response: %w", b.name, err)
	}
	if len(body) > maxBodyBytes {
		// Never a silently shorter list: a truncated feed would look complete.
		return nil, fmt.Errorf("%s: response is larger than %d bytes", b.name, maxBodyBytes)
	}
	return body, nil
}

// cutoff is the oldest publish time still collected.
func (b board) cutoff() time.Time { return b.now().Add(-b.maxAge) }

// accept reports whether a job may be returned: an id, a title, a real
// employer, and a publish date (when known) inside the age window. A job
// with an unknown date is kept: the store falls back to first-seen time.
func (b board) accept(j ats.Job) bool {
	if j.ExternalID == "" || j.Title == "" || j.URL == "" {
		return false
	}
	if companymatch.IsPlaceholderEmployer(j.Employer) {
		return false
	}
	if !j.PublishedAt.IsZero() && j.PublishedAt.Before(b.cutoff()) {
		return false
	}
	return true
}

// finish applies the fields every board job shares and returns the job:
// cleaned text, remote type, bounded description.
func finish(j ats.Job) ats.Job {
	j.Title = ats.CleanText(j.Title)
	j.Employer = ats.CleanText(j.Employer)
	j.LocationRaw = cleanLocation(j.LocationRaw)
	j.RemoteType = "remote"
	j.Description = truncateRunes(j.Description, maxDescriptionRunes)
	return j
}

// dedupe drops later jobs with an id already seen (a board can repeat a job
// across pages while new ones shift the pages).
func dedupe(jobs []ats.Job) []ats.Job {
	seen := make(map[string]bool, len(jobs))
	out := jobs[:0:0]
	for _, j := range jobs {
		if seen[j.ExternalID] {
			continue
		}
		seen[j.ExternalID] = true
		out = append(out, j)
	}
	return out
}

var (
	spaceRun     = regexp.MustCompile(`\s+`)
	trailingJunk = regexp.MustCompile(`[,;\s]+$`)
)

// cleanLocation tidies a board's location text ("Agra, " becomes "Agra",
// runs of spaces collapse).
func cleanLocation(s string) string {
	s = ats.CleanText(s)
	s = spaceRun.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, " ,", ",")
	return trailingJunk.ReplaceAllString(s, "")
}

// employment maps a board's job type text to the jobs table's vocabulary,
// "" when it says nothing usable.
func employment(s string) string {
	k := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(s))
	switch k {
	case "fulltime":
		return "full_time"
	case "parttime":
		return "part_time"
	case "contract", "contractor", "freelance", "temporary", "temp":
		return "contract"
	case "intern", "internship":
		return "internship"
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// epoch converts Unix seconds (0 or negative: unknown) to a time.
func epoch(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// errNoJobs is what a board returns when a response decodes but lists
// nothing: never mistaken for "no jobs", because an empty list would age out
// every job the board has stored.
func errNoJobs(name string) error {
	return fmt.Errorf("%s: the response lists no jobs (the board's format may have changed)", name)
}
