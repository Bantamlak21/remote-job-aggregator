// Package ashby reads a company's jobs from Ashby's public job-board API
// (https://api.ashbyhq.com/posting-api/job-board/{board}): no key, public
// data. Verified live (2026-09-25): an unknown board answers 404 with the text
// "Not Found", a real board answers {"jobs":[...],"apiVersion":"1"}, and each
// job carries jobUrl, publishedAt, location, secondaryLocations, isListed,
// isRemote, workplaceType ("Remote", "Hybrid", "OnSite"), employmentType
// ("FullTime", "PartTime", "Contract", "Intern", "Temporary") and its text in
// descriptionPlain. Jobs with isListed false are not on the public board and
// are left out.
package ashby

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
var endpointBase = "https://api.ashbyhq.com/posting-api/job-board"

// boardTokenPattern is the character set of an Ashby board name (a hostname
// label plus dots, as Ashby allows). A token is placed in a URL path.
var boardTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

const (
	maxDescriptionRunes = 20000
	maxLocationRunes    = 300
)

// Doer is the slice of *httpclient.Client the Client needs.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client lists an Ashby company's jobs.
type Client struct {
	http Doer
}

// New returns a Client over the shared, pooled HTTP client.
func New(doer Doer) *Client { return &Client{http: doer} }

type job struct {
	ID                 string `json:"id"`
	Title              string `json:"title"`
	Location           string `json:"location"`
	SecondaryLocations []struct {
		Location string `json:"location"`
	} `json:"secondaryLocations"`
	PublishedAt    string  `json:"publishedAt"`
	IsListed       *bool   `json:"isListed"`
	IsRemote       bool    `json:"isRemote"`
	WorkplaceType  string  `json:"workplaceType"`
	EmploymentType string  `json:"employmentType"`
	JobURL         string  `json:"jobUrl"`
	Description    string  `json:"descriptionPlain"`
	DescriptionHTM *string `json:"descriptionHtml"`
}

// ListJobs fetches every listed job on the board. A board with no openings is
// a valid empty list; an unknown board is ats.ErrBoardNotFound.
func (c *Client) ListJobs(ctx context.Context, boardToken string) ([]ats.Job, error) {
	if !boardTokenPattern.MatchString(boardToken) {
		return nil, fmt.Errorf("ashby: %q: %w", boardToken, ats.ErrInvalidBoardToken)
	}
	reqURL := fmt.Sprintf("%s/%s", endpointBase, url.PathEscape(boardToken))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ashby: building request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ashby: %q: %w", boardToken, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("ashby: %q: %w", boardToken, ats.ErrBoardNotFound)
	case resp.StatusCode != http.StatusOK:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("ashby: %q: unexpected status %d: %s", boardToken, resp.StatusCode, snippet)
	}

	// The jobs array must be present (a 200 without it is a changed format,
	// never "no jobs"); each job is decoded on its own so one odd record costs
	// that job only.
	var body struct {
		Jobs *[]json.RawMessage `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("ashby: %q: parsing response: %w", boardToken, err)
	}
	if body.Jobs == nil {
		return nil, fmt.Errorf("ashby: %q: response has no \"jobs\" field", boardToken)
	}
	out := make([]ats.Job, 0, len(*body.Jobs))
	for _, raw := range *body.Jobs {
		var j job
		if err := json.Unmarshal(raw, &j); err != nil {
			continue
		}
		if j.IsListed != nil && !*j.IsListed {
			continue // not on the public board
		}
		out = append(out, convert(j))
	}
	return out, nil
}

func convert(j job) ats.Job {
	locations := []string{}
	if l := strings.TrimSpace(j.Location); l != "" {
		locations = append(locations, l)
	}
	for _, s := range j.SecondaryLocations {
		if l := strings.TrimSpace(s.Location); l != "" {
			locations = append(locations, l)
		}
	}
	remote := ats.WorkplaceType(j.WorkplaceType)
	if remote == "" && j.IsRemote {
		remote = "remote"
	}
	link := strings.TrimSpace(j.JobURL)
	if u, err := url.Parse(link); err != nil || u.Scheme != "https" || (u.Hostname() != "jobs.ashbyhq.com" && !strings.HasSuffix(u.Hostname(), ".ashbyhq.com")) {
		link = ""
	}
	description := strings.TrimSpace(j.Description)
	if description == "" && j.DescriptionHTM != nil {
		description = ats.HTMLToText(*j.DescriptionHTM)
	}
	var published time.Time
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(j.PublishedAt)); err == nil {
		published = t.UTC()
	}
	return ats.Job{
		ExternalID:     strings.TrimSpace(j.ID),
		Title:          ats.CleanText(j.Title),
		URL:            link,
		LocationRaw:    truncateRunes(ats.CleanText(strings.Join(locations, "; ")), maxLocationRunes),
		Description:    truncateRunes(ats.CleanText(description), maxDescriptionRunes),
		PublishedAt:    published,
		RemoteType:     remote,
		EmploymentType: ats.EmploymentType(j.EmploymentType),
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
