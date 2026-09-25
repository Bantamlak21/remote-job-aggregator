// Package lever reads a company's jobs from Lever's public postings API
// (https://api.lever.co/v0/postings/{company}?mode=json): no key, public data.
// Verified live (2026-09-25) against real boards: an unknown company answers
// 404 {"ok":false,"error":"Document not found"}, a real board with no openings
// answers 200 with [], and every posting carries hostedUrl, createdAt (Unix
// milliseconds), categories (location, commitment, team, department),
// workplaceType ("remote", "hybrid", "on-site" or "unspecified") and its text
// in descriptionPlain, lists and additionalPlain.
package lever

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
)

// endpointBase is a var only so tests can point it at a fake server.
var endpointBase = "https://api.lever.co/v0/postings"

// boardTokenPattern is the character set of a Lever company slug. A token is
// placed in a URL path, so anything else is refused before it can reshape the
// request.
var boardTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const (
	// maxDescriptionRunes bounds one stored description.
	maxDescriptionRunes = 20000
	// maxLocationRunes bounds one stored location text.
	maxLocationRunes = 300
)

// Doer is the slice of *httpclient.Client the Client needs.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client lists a Lever company's postings.
type Client struct {
	http Doer
	now  func() time.Time
}

// New returns a Client over the shared, pooled HTTP client.
func New(doer Doer) *Client { return &Client{http: doer, now: time.Now} }

type posting struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	HostedURL  string `json:"hostedUrl"`
	CreatedAt  int64  `json:"createdAt"`
	Workplace  string `json:"workplaceType"`
	Country    string `json:"country"`
	Categories struct {
		Commitment   string   `json:"commitment"`
		Location     string   `json:"location"`
		AllLocations []string `json:"allLocations"`
	} `json:"categories"`
	DescriptionPlain string `json:"descriptionPlain"`
	Lists            []struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	} `json:"lists"`
	AdditionalPlain string `json:"additionalPlain"`
}

// ListJobs fetches every posting on the company's board. A board with no
// openings is a valid empty list; an unknown company is ats.ErrBoardNotFound.
func (c *Client) ListJobs(ctx context.Context, boardToken string) ([]ats.Job, error) {
	if !boardTokenPattern.MatchString(boardToken) {
		return nil, fmt.Errorf("lever: %q: %w", boardToken, ats.ErrInvalidBoardToken)
	}
	reqURL := fmt.Sprintf("%s/%s?mode=json", endpointBase, url.PathEscape(boardToken))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("lever: building request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lever: %q: %w", boardToken, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("lever: %q: %w", boardToken, ats.ErrBoardNotFound)
	case resp.StatusCode != http.StatusOK:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("lever: %q: unexpected status %d: %s", boardToken, resp.StatusCode, snippet)
	}

	// Postings are decoded one at a time so a single odd record costs that job,
	// not the board. The list itself must be a JSON array: anything else is a
	// changed format, never "no jobs" (that would close every stored job).
	var raw []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("lever: %q: response is not the expected JSON array: %w", boardToken, err)
	}
	jobs := make([]ats.Job, 0, len(raw))
	for _, item := range raw {
		var p posting
		if err := json.Unmarshal(item, &p); err != nil {
			continue
		}
		jobs = append(jobs, convert(p, boardToken))
	}
	return jobs, nil
}

func convert(p posting, board string) ats.Job {
	id := strings.TrimSpace(p.ID)
	location := ats.CleanText(p.Categories.Location)
	if len(p.Categories.AllLocations) > 1 {
		location = ats.CleanText(strings.Join(p.Categories.AllLocations, "; "))
	}
	// The posting's own page: the API's hostedUrl, only if it is on Lever's
	// public host (a defensive check; the field is Lever's).
	link := strings.TrimSpace(p.HostedURL)
	if u, err := url.Parse(link); err != nil || u.Scheme != "https" || (u.Hostname() != "jobs.lever.co" && !strings.HasSuffix(u.Hostname(), ".lever.co")) {
		link = ""
	}

	var published time.Time
	if p.CreatedAt > 0 {
		published = time.UnixMilli(p.CreatedAt).UTC()
	}
	return ats.Job{
		ExternalID:     id,
		Title:          ats.CleanText(p.Text),
		URL:            link,
		LocationRaw:    truncateRunes(location, maxLocationRunes),
		Description:    truncateRunes(description(p), maxDescriptionRunes),
		PublishedAt:    published,
		RemoteType:     ats.WorkplaceType(p.Workplace),
		EmploymentType: ats.EmploymentType(p.Categories.Commitment),
	}
}

// description joins the posting's opening text, its titled lists (duties,
// requirements) and its closing text.
func description(p posting) string {
	parts := []string{strings.TrimSpace(p.DescriptionPlain)}
	for _, l := range p.Lists {
		body := ats.HTMLToText(l.Content)
		if t := strings.TrimSpace(l.Text); t != "" {
			body = t + "\n" + body
		}
		parts = append(parts, strings.TrimSpace(body))
	}
	parts = append(parts, strings.TrimSpace(p.AdditionalPlain))
	var kept []string
	for _, s := range parts {
		if s != "" {
			kept = append(kept, ats.CleanText(s))
		}
	}
	return strings.Join(kept, "\n\n")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
