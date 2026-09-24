// Package greenhouse wraps Greenhouse's public Job Board API
// (https://boards-api.greenhouse.io) — no authentication, no API key,
// genuinely public data. Verified live against real boards (gitlab,
// figma) during Phase 3 research, not guessed from documentation alone:
// GET /v1/boards/{board_token}/jobs?content=true, a 404 with
// {"status":404,"error":"Job not found"} for an unknown board token
// (surfaced as the shared ats.ErrBoardNotFound, not a package-local
// error, so internal/ingestion can react to it without importing this
// package), and a "content" field that is itself HTML-entity-encoded
// at the outer layer (ats.HTMLToText handles this).
package greenhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// endpointBase is a var, not a const, purely so tests can point it at a
// fake server; nothing in this project ever talks to a different
// Greenhouse endpoint at runtime.
var endpointBase = "https://boards-api.greenhouse.io/v1/boards"

// boardTokenPattern is the same character set internal/discovery's
// board-URL recognizers accept for a slug. A token is interpolated into
// a URL path, so anything outside it (a "?", "#", "/" or ".." in a
// hand-edited seed file's external_board_id) is refused before it can
// reshape the request.
var boardTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Client fetches a Greenhouse board's job list over a shared, pooled
// *httpclient.Client — the same retry/backoff/redirect-cap behavior every
// other outbound call in this project gets. Its response-size cap is the
// caller's to choose (ingestion passes a client sized for real boards;
// see config.IngestionConfig.MaxResponseBytes).
type Client struct {
	http *httpclient.Client
}

// New returns a Client. httpClient is owned by the caller and shared —
// matching every other ATS/search client in this project, there is
// exactly one pooled HTTP client per process, not one per package.
func New(httpClient *httpclient.Client) *Client {
	return &Client{http: httpClient}
}

// apiResponse mirrors the subset of the Job Board API's list-jobs
// response this package actually uses. Jobs is a pointer so a 200 with
// no "jobs" key at all (an unexpected body — e.g. board metadata) is
// distinguishable from a real, empty board: the former is an error,
// the latter is legitimately zero jobs. Treating both as "no jobs"
// would make ingestion close out every job on the board.
type apiResponse struct {
	Jobs *[]apiJob `json:"jobs"`
}

type apiJob struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	AbsoluteURL string `json:"absolute_url"`
	Location    struct {
		Name string `json:"name"`
	} `json:"location"`
	Content        string `json:"content"`
	FirstPublished string `json:"first_published"`
}

// ListJobs fetches every job currently posted on boardToken's board,
// normalized into the shared ats.Job shape. content=true is always
// requested so Description is populated. The response body is
// stream-decoded rather than read into one buffer first, but the decoded
// board (every job's text) is still held in memory at once — roughly
// 10-13x the JSON size transiently (measured: ~124 MiB allocated over a
// 9.7 MB board) — so worker count times board size bounds ingestion's
// memory; the transport-level size cap applies underneath.
func (c *Client) ListJobs(ctx context.Context, boardToken string) ([]ats.Job, error) {
	if !boardTokenPattern.MatchString(boardToken) {
		return nil, fmt.Errorf("greenhouse: %q: %w", boardToken, ats.ErrInvalidBoardToken)
	}

	reqURL := fmt.Sprintf("%s/%s/jobs?content=true", endpointBase, url.PathEscape(boardToken))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("greenhouse: building request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("greenhouse: %q: %w", boardToken, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("greenhouse: %q: %w", boardToken, ats.ErrBoardNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("greenhouse: %q: unexpected status %d: %s", boardToken, resp.StatusCode, snippet)
	}

	var parsed apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("greenhouse: %q: parsing response: %w", boardToken, err)
	}
	if parsed.Jobs == nil {
		return nil, fmt.Errorf("greenhouse: %q: response has no \"jobs\" field", boardToken)
	}

	jobs := make([]ats.Job, 0, len(*parsed.Jobs))
	for _, j := range *parsed.Jobs {
		// A missing/zero id must not become the valid-looking identity
		// "0" (every id-less job would collide on it); an empty
		// ExternalID makes ingestion skip the job instead.
		externalID := ""
		if j.ID != 0 {
			externalID = strconv.FormatInt(j.ID, 10)
		}
		jobs = append(jobs, ats.Job{
			ExternalID:  externalID,
			Title:       ats.CleanText(j.Title),
			URL:         ats.CleanText(j.AbsoluteURL),
			LocationRaw: ats.CleanText(j.Location.Name),
			Description: ats.HTMLToText(j.Content),
			PublishedAt: parseTime(j.FirstPublished),
		})
	}
	return jobs, nil
}

// parseTime parses Greenhouse's RFC3339 timestamps (e.g.
// "2016-01-14T10:55:28-05:00"), returning the zero Time when absent or
// unparseable. Only first_published feeds PublishedAt: updated_at moves
// every time a recruiter touches the posting, and job.Store freezes
// published_at on first write, so falling back to it would permanently
// record "last edited" as "published". A missing published_at is fine —
// reads fall back to first_seen_at.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
