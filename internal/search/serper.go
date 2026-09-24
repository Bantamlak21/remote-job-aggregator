// Package search wraps the Serper (google.serper.dev) search API — the
// one external search dependency discovery uses to find a company's ATS
// board without a human having to look up and type the URL by hand. It
// knows nothing about ATS providers or job boards; it returns raw
// (title, URL, snippet) results for a query string, same as any other
// search API client would.
//
// Serper, not Google's own Custom Search JSON API this package
// originally wrapped: Google's Custom Search offering was found to no
// longer be a viable option, and the other genuinely-free alternative
// considered earlier (Brave Search API) requires a credit card even for
// its free tier. Serper's free tier (2,500 queries, confirmed against
// serper.dev's own landing page — "No credit card required") does not,
// and it is simpler to configure than what it replaced: it wraps
// ordinary Google search directly rather than a separately provisioned
// "Programmable Search Engine," so only one credential (an API key)
// exists to configure, not two.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// endpoint is a var, not a const, purely so tests can point it at a fake
// server; nothing in this project ever talks to a different Serper
// endpoint at runtime.
var endpoint = "https://google.serper.dev/search"

// Config configures a Client.
type Config struct {
	APIKey string
}

// Result is one search result: title, canonical URL, and the snippet
// shown for it. Serper wraps Google's own search, so this is Google's
// snippet as re-served by Serper, not something Serper generates
// itself.
type Result struct {
	Title   string
	URL     string
	Snippet string
	// Date is Google's date for the result exactly as Serper returns it
	// ("19 hours ago", "Sep 14, 2026"), or "" when there is none. Not
	// parsed here: this package returns raw search data.
	Date string
}

// Recency limits results to pages Google saw within a time window (its
// "tbs=qdr:X" filter). Job pages go stale quickly, so a recency window is
// the cheapest way to keep a search from returning years-old listings.
type Recency string

const (
	RecencyDay   Recency = "d"
	RecencyWeek  Recency = "w"
	RecencyMonth Recency = "m"
	RecencyYear  Recency = "y"
)

func (r Recency) valid() bool {
	switch r {
	case RecencyDay, RecencyWeek, RecencyMonth, RecencyYear:
		return true
	}
	return false
}

// Client queries the Serper search API over a shared, pooled
// *httpclient.Client — the same retry/backoff/size-limit/redirect-cap
// behavior every other outbound call in this project gets, not a
// bespoke one-off http.Client.
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

// requestBody is what Serper's /search endpoint accepts. Num caps
// results at 10 — if a company's ATS board isn't in the top 10 results
// for a targeted query, a human should look at it, not page through
// more results automatically (the same reasoning, and the same limit,
// this package used against Google Custom Search before this rewrite).
type requestBody struct {
	Q   string `json:"q"`
	Num int    `json:"num"`
	// TBS is Google's time-based-search filter ("qdr:m" = past month);
	// omitted for an unrestricted search.
	TBS string `json:"tbs,omitempty"`
	// Page selects the 10-result page (1-based); omitted for page 1.
	Page int `json:"page,omitempty"`
}

// apiResponse mirrors the subset of Serper's response this package
// actually uses: the "organic" results array. Field names (organic,
// title, link, snippet) are confirmed against Serper's documented
// request/response shape and cross-checked against real client
// libraries that parse it (e.g. LangChain's GoogleSerperAPIWrapper,
// which reads results under the same "organic" key). Serper returns
// many more fields (knowledge graph, answer box, people-also-ask,
// related searches, etc.) that nothing here needs.
//
// Not yet confirmed against a live call with a real API key — no
// SERPER_API_KEY exists in this environment. Verified against
// documented/observed shape only; re-confirm against a real response
// once a key is available (see PROJECT_STATUS.md).
type apiResponse struct {
	Organic []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
		Date    string `json:"date"`
	} `json:"organic"`
}

// apiError mirrors Serper's documented error shape, confirmed against
// two independent real-world reports of it actually firing:
// {"message":"Missing query parameter"} for a 400, and
// {"message":"Unauthorized.","statusCode":403} for a bad or missing key
// (see ErrUnauthorized).
type apiError struct {
	Message    string `json:"message"`
	StatusCode int    `json:"statusCode"`
}

// ErrUnauthorized is returned when Serper responds 401 or 403. Stated
// honestly rather than assumed: Serper's own error message ("Unauthorized.")
// is identical whether the API key is invalid/missing or the account has
// simply run out of free-tier credits — real-world reports confirm the
// same generic message and status code fire for both — so, unlike the
// Google Custom Search client this replaced (which could recognize a
// specific daily-quota-exceeded reason code), this client cannot tell a
// caller which case it is. Check the Serper dashboard directly to find
// out.
var ErrUnauthorized = errors.New("search: unauthorized (invalid API key, or out of free-tier credits)")

// Search runs query against Serper's search endpoint and returns up to
// 10 results.
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	return c.search(ctx, query, "", 1)
}

// SearchRecent is Search limited to pages Google saw within recency.
func (c *Client) SearchRecent(ctx context.Context, query string, recency Recency) ([]Result, error) {
	return c.SearchRecentPage(ctx, query, recency, 1)
}

// maxPage bounds SearchRecentPage: deeper pages of a recency-limited
// search are noise, and a bug must not be able to page (and pay) forever.
const maxPage = 10

// SearchRecentPage is SearchRecent for the given 1-based page of results.
func (c *Client) SearchRecentPage(ctx context.Context, query string, recency Recency, page int) ([]Result, error) {
	if !recency.valid() {
		return nil, fmt.Errorf("search: invalid recency %q", recency)
	}
	if page < 1 || page > maxPage {
		return nil, fmt.Errorf("search: page must be between 1 and %d, got %d", maxPage, page)
	}
	// One call is exactly one wire request: SearchRecent serves the metered
	// job-search source, which counts calls against a query budget, so the
	// shared client's own transparent retries (up to 3 more requests per
	// call) must not multiply what Serper bills. That caller owns retrying.
	return c.search(httpclient.WithoutRetries(ctx), query, "qdr:"+string(recency), page)
}

func (c *Client) search(ctx context.Context, query, tbs string, page int) ([]Result, error) {
	if page == 1 {
		page = 0 // omitted from the request: page 1 is the default
	}
	body, err := json.Marshal(requestBody{Q: query, Num: 10, TBS: tbs, Page: page})
	if err != nil {
		return nil, fmt.Errorf("search: encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("search: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The API key travels as a header, exactly as it did for the Google
	// Custom Search client this replaced, and for the same reason: never
	// let a credential become part of a URL, where Go's own *url.Error
	// would embed it verbatim on a network failure with no way to redact
	// it after the fact. That vulnerability class cannot occur for this
	// client at all, independent of this header choice: the query itself
	// travels in the POST body, not the URL, so req.URL never contains
	// anything but the fixed endpoint.
	req.Header.Set("X-API-KEY", c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: %q: %w", query, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("search: %q: reading response: %w", query, err)
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("search: %q: %w", query, ErrUnauthorized)
		}
		var apiErr apiError
		if jsonErr := json.Unmarshal(respBody, &apiErr); jsonErr == nil && apiErr.Message != "" {
			return nil, fmt.Errorf("search: %q: API error (status %d): %s", query, resp.StatusCode, apiErr.Message)
		}
		// A non-200 with a body that isn't the documented JSON error
		// shape (a proxy's plain-text error page, an outage message)
		// must still report the status, not swallow it silently.
		return nil, fmt.Errorf("search: %q: unexpected status %d: %s", query, resp.StatusCode, truncate(respBody, 500))
	}

	var parsed apiResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("search: %q: parsing response: %w", query, err)
	}

	results := make([]Result, 0, len(parsed.Organic))
	for _, item := range parsed.Organic {
		results = append(results, Result{Title: item.Title, URL: item.Link, Snippet: item.Snippet, Date: item.Date})
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
