// Package remoteboards collects remote jobs from public remote-job boards:
// Remotive, Remote OK, Himalayas, Jobicy, We Work Remotely and Working
// Nomads. Each publishes a free API or RSS feed meant for exactly this use,
// and each asks for the same thing in return: a visible credit and a link
// back to its own listing. So every job's URL is the board's own page for it
// (never the employer's; a URL on any other host is refused), and Job.Source,
// reported by the API, lets the UI print "via <board>".
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
// 429 or 5xx is reported, never retried. Each board also declares a
// MinInterval, and the ingester skips a board that succeeded more recently
// (Remotive asks for at most 4 requests a day).
//
// A response that decodes but yields no usable job is an error, not "no
// jobs": an empty result would age out every job the board has stored. A
// single odd record costs only that job.
package remoteboards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	// maxLocationRunes bounds one stored location text (a board can list
	// dozens of countries).
	maxLocationRunes = 300
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
	name        string // for messages and logs
	hosts       []string
	minInterval time.Duration
	http        Doer
	logger      *slog.Logger
	now         func() time.Time
	maxAge      time.Duration
}

func newBoard(name string, hosts []string, minInterval time.Duration, doer Doer, logger *slog.Logger) board {
	return board{name: name, hosts: hosts, minInterval: minInterval, http: doer, logger: logger, now: time.Now, maxAge: DefaultMaxAge}
}

// StaleAfter says the board's feed is a sample; see DefaultStaleAfter.
func (board) StaleAfter() time.Duration { return DefaultStaleAfter }

// Market says every job here is in the worldwide list.
func (board) Market() market.Market { return market.Worldwide }

// MinInterval is how long the ingester leaves the board alone after a
// successful run, to stay inside its rate terms.
func (b board) MinInterval() time.Duration { return b.minInterval }

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

// ownURL reports whether raw is an https URL on one of the board's own
// hosts. Every job's URL must pass: the credit and link-back terms are about
// the board's page, and a feed must not be able to put another site (or a
// non-web scheme) behind an "Apply on <board>" button.
func (b board) ownURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range b.hosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// verdict is what happened to one record.
type verdict int

const (
	kept    verdict = iota
	dropped         // valid, but not wanted (too old, expired, a hidden employer)
	invalid         // not a usable record: missing id, title, employer or own-host URL
)

// judge decides whether a finished job may be returned. badDate says the
// record carried a date the parser could not read: it cannot be shown to be
// fresh, so it is not kept (an unreadable date must not bypass the age window).
func (b board) judge(j ats.Job, badDate bool) verdict {
	if j.ExternalID == "" || j.Title == "" || strings.TrimSpace(j.Employer) == "" || !b.ownURL(j.URL) || badDate {
		return invalid
	}
	if companymatch.IsPlaceholderEmployer(j.Employer) {
		return dropped
	}
	if !j.PublishedAt.IsZero() && j.PublishedAt.Before(b.cutoff()) {
		return dropped
	}
	if !j.ExpiresAt.IsZero() && !j.ExpiresAt.After(b.now()) {
		return dropped
	}
	return kept
}

// tally collects one run's jobs and counts how many records were structurally
// unusable, so a format change (every record now unusable) is told apart from
// a quiet day (every record too old).
type tally struct {
	b       board
	seen    int
	invalid int
	jobs    []ats.Job
}

func (b board) newTally() *tally { return &tally{b: b} }

// add records one finished job. badDate: see judge.
func (t *tally) add(j ats.Job, badDate bool) {
	t.seen++
	switch t.b.judge(j, badDate) {
	case kept:
		t.jobs = append(t.jobs, j)
	case invalid:
		t.invalid++
	}
}

// addBroken records a record that could not even be decoded.
func (t *tally) addBroken() {
	t.seen++
	t.invalid++
}

// result returns the jobs, or an error when the response held records but
// none was usable.
func (t *tally) result() ([]ats.Job, error) {
	if t.seen == 0 {
		return nil, errNoJobs(t.b.name)
	}
	if t.invalid == t.seen {
		return nil, fmt.Errorf("%s: none of the %d records in the response was usable (the board's format may have changed)", t.b.name, t.seen)
	}
	return dedupe(t.jobs), nil
}

// finish applies the fields every board job shares and returns the job:
// cleaned text, remote type, bounded description and location.
func finish(j ats.Job) ats.Job {
	j.Title = ats.CleanText(j.Title)
	j.Employer = ats.CleanText(j.Employer)
	j.URL = strings.TrimSpace(j.URL)
	j.LocationRaw = truncateRunes(cleanLocation(j.LocationRaw), maxLocationRunes)
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

// employment maps a board's job type text to the jobs table's vocabulary.
func employment(s string) string { return ats.EmploymentType(s) }

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

// parseTime reads the timestamp forms the boards use; the zero time when s is
// empty or in none of them. Timestamps without a zone are UTC.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04:05Z07:00", "2006-01-02",
		time.RFC1123Z, time.RFC1123, "Mon, _2 Jan 2006 15:04:05 -0700", time.RFC822Z,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// dateProblem reports whether text is present but unreadable as a date.
func dateProblem(text string, parsed time.Time) bool {
	return strings.TrimSpace(text) != "" && parsed.IsZero()
}

// decodeRecords decodes each element of a JSON array separately, so one odd
// record (a field of an unexpected type) costs that job, not the feed. bad
// counts the elements that could not be decoded.
func decodeRecords[T any](raws []json.RawMessage) (records []T, bad int) {
	for _, raw := range raws {
		var r T
		if err := json.Unmarshal(raw, &r); err != nil {
			bad++
			continue
		}
		records = append(records, r)
	}
	return records, bad
}

// errNoJobs is what a board returns when a response decodes but lists
// nothing: never mistaken for "no jobs", because an empty list would age out
// every job the board has stored.
func errNoJobs(name string) error {
	return fmt.Errorf("%s: the response lists no jobs (the board's format may have changed)", name)
}
