// Package search wraps the Google Custom Search JSON API — the one
// external search dependency discovery uses to find a company's ATS
// board without a human having to look up and type the URL by hand. It
// knows nothing about ATS providers or job boards; it returns raw
// (title, URL, snippet) results for a query string, same as any other
// search API client would.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// endpoint is a var, not a const, purely so tests can point it at a fake
// server; nothing in this project ever talks to a different Custom
// Search endpoint at runtime.
var endpoint = "https://www.googleapis.com/customsearch/v1"

// Config configures a Client. Both fields come from the Google Cloud
// / Programmable Search Engine console — see README.md's Discovery
// section for the exact setup steps.
type Config struct {
	APIKey         string
	SearchEngineID string
}

// Result is one search result: title, canonical URL, and the snippet
// Google's index shows for it.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Client queries the Custom Search JSON API over a shared, pooled
// *httpclient.Client — the same retry/backoff/size-limit/redirect-cap
// behavior every other outbound call in this project gets, not a
// bespoke one-off http.Get.
type Client struct {
	http *httpclient.Client
	cfg  Config
}

// New returns a Client. httpClient is owned by the caller and shared —
// New does not construct its own, matching the project-wide rule that
// there is exactly one pooled HTTP client per process, not one per
// package that happens to make outbound calls.
func New(httpClient *httpclient.Client, cfg Config) *Client {
	return &Client{http: httpClient, cfg: cfg}
}

// apiResponse mirrors the subset of the Custom Search JSON API's
// response this package actually uses. The API returns many more fields
// (search metadata, spelling suggestions, pagination info) that nothing
// here needs.
//
// Error.Errors[].Reason exists specifically for quotaExceeded detection:
// Google's classic (pre-google.rpc) error shape reports a 403 with
// Status left blank and the real signal in errors[0].reason (e.g.
// "dailyLimitExceeded"), which the newer Status field alone misses.
type apiResponse struct {
	Items []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
	} `json:"items"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
	Errors  []struct {
		Reason string `json:"reason"`
	} `json:"errors"`
}

// ErrQuotaExceeded is returned when Google reports the daily query
// quota is exhausted — distinguished from other API errors because a
// caller might reasonably want to stop an entire batch immediately on
// this one, rather than continuing to burn requests against a search
// that will keep failing the same way for the rest of the day.
var ErrQuotaExceeded = errors.New("search: daily query quota exceeded")

// quotaExceeded reports whether apiErr (from a non-2xx response with a
// parseable JSON body) represents quota/rate-limit exhaustion rather
// than some other API error (bad key, malformed query, etc.).
//
// Checks both the shape this package has actually seen documented:
// Status == "RESOURCE_EXHAUSTED" (the newer google.rpc.Status
// convention) and the classic reason strings Google's older
// quota-project APIs, including Custom Search, are documented to use.
// statusCode == 429 is also treated as quota-like on its own, since a
// bare 429 with no other detail is what a generic rate limiter (not
// necessarily Google's own quota system) most often means.
//
// Caveat, stated honestly rather than assumed: the "dailyLimitExceeded"
// reason shape is documented by Google for API-Gateway-fronted APIs
// generally, but this package has not been run against the real Custom
// Search API with a live key to confirm Custom Search specifically
// returns it verbatim — flagged during adversarial review as probable,
// not proven against production.
func quotaExceeded(apiErr *apiError, statusCode int) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if apiErr.Status == "RESOURCE_EXHAUSTED" {
		return true
	}
	for _, e := range apiErr.Errors {
		switch e.Reason {
		case "dailyLimitExceeded", "rateLimitExceeded", "quotaExceeded", "userRateLimitExceeded":
			return true
		}
	}
	return false
}

// Search runs query against the Custom Search JSON API and returns up
// to 10 results (the API's own per-request maximum; nothing in this
// project's use case needs pagination beyond the first page — if the
// company's ATS board isn't in the top 10 results for a targeted
// site-restricted query, a human should look at it, not page through
// more results automatically).
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	q := url.Values{}
	q.Set("cx", c.cfg.SearchEngineID)
	q.Set("q", query)
	q.Set("num", "10")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("search: building request: %w", err)
	}
	// The API key travels as a header, never in the URL. Google's own
	// guidance (Cloud docs, "Use API keys to access APIs") says a
	// query-string key "expos[es] your key to theft through URL scans"
	// and recommends this header instead — and, found by adversarial
	// review, there's a concrete leak this closes in this codebase
	// specifically: Go's own http.Client embeds the full request URL
	// (query string included) verbatim in the *url.Error it returns on
	// a network failure or timeout, and that string is already baked in
	// by the time any %w wrapping sees it — there is no way to redact it
	// after the fact. A key that is never part of the URL cannot leak
	// through that path, or through a proxy/CDN that echoes the request
	// URI in an error body (which internal/httpclient's own req.URL
	// truncation would otherwise reproduce faithfully). Verified live
	// with the real binary, offline, that a key placed in the query
	// string leaks this way; not yet re-verified against the real
	// googleapis.com endpoint that the header form is accepted, since no
	// live key exists in this environment — confirm this once one does.
	req.Header.Set("X-goog-api-key", c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: %q: %w", query, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("search: %q: reading response: %w", query, err)
	}

	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// A non-200 with a body that isn't even JSON (a proxy's plain-text
		// error page, an outage message) must report the status, not the
		// unmarshal failure — "invalid character looking for beginning of
		// value" tells an operator nothing about what actually went wrong.
		// Only treat this as a genuine parse error when the request
		// otherwise looked fine (200) but still didn't decode.
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("search: %q: unexpected status %d: %s", query, resp.StatusCode, truncate(body, 500))
		}
		return nil, fmt.Errorf("search: %q: parsing response: %w", query, err)
	}

	if parsed.Error != nil {
		if quotaExceeded(parsed.Error, resp.StatusCode) {
			return nil, fmt.Errorf("search: %q: %w: %s", query, ErrQuotaExceeded, parsed.Error.Message)
		}
		return nil, fmt.Errorf("search: %q: API error (%s): %s", query, parsed.Error.Status, parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search: %q: unexpected status %d: %s", query, resp.StatusCode, truncate(body, 500))
	}

	results := make([]Result, 0, len(parsed.Items))
	for _, item := range parsed.Items {
		results = append(results, Result{Title: item.Title, URL: item.Link, Snippet: item.Snippet})
	}
	return results, nil
}

// truncate bounds how much of an unexpected response body an error
// message repeats, so a genuinely huge error page doesn't end up
// dumped whole into a log line.
func truncate(body []byte, limit int) string {
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "... (truncated)"
}
