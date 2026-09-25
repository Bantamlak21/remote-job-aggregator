package remoteboards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
)

const (
	// DefaultHimalayasPages is how many pages (20 jobs each) one run reads at
	// most. The feed holds about 100,000 jobs, newest first. Measured on
	// 2026-09-25: 50 pages (1,000 jobs) reached back about 28 hours, so runs
	// 12 hours apart (the MinInterval) leave no gap.
	DefaultHimalayasPages = 50
	// hardHimalayasPages is the most pages one run may ever read.
	hardHimalayasPages = 100
	himalayasPageSize  = 20 // the API's maximum
)

// Himalayas reads https://himalayas.app/jobs/api. Its terms: show a visible
// link back to himalayas.app and say the data comes from Himalayas; the data
// refreshes every 24 hours (so polling more often gains nothing) and the API
// is rate limited. It is paged by a cursor, newest first.
type Himalayas struct {
	board
	url      string
	maxPages int
	pause    time.Duration
}

// NewHimalayas returns a Himalayas collector. maxPages <= 0 means
// DefaultHimalayasPages (capped at 100); pause is the delay between page
// requests.
func NewHimalayas(doer Doer, maxPages int, pause time.Duration, logger *slog.Logger) *Himalayas {
	if maxPages <= 0 {
		maxPages = DefaultHimalayasPages
	}
	return &Himalayas{
		board:    newBoard("himalayas", []string{"himalayas.app"}, 12*time.Hour, doer, logger),
		url:      "https://himalayas.app/jobs/api",
		maxPages: min(maxPages, hardHimalayasPages),
		pause:    pause,
	}
}

type himalayasJob struct {
	Title                string          `json:"title"`
	CompanyName          string          `json:"companyName"`
	EmploymentType       string          `json:"employmentType"`
	LocationRestrictions json.RawMessage `json:"locationRestrictions"` // a list of country names
	PubDate              json.Number     `json:"pubDate"`
	ExpiryDate           json.Number     `json:"expiryDate"`
	ApplicationLink      string          `json:"applicationLink"`
	GUID                 string          `json:"guid"`
	Description          string          `json:"description"`
	Excerpt              string          `json:"excerpt"`
}

type himalayasPage struct {
	NextCursor string            `json:"nextCursor"`
	Jobs       []json.RawMessage `json:"jobs"`
}

// restrictions reads the location restriction list ("" for none).
func restrictions(raw json.RawMessage) string {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, ", ")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// Collect reads pages newest first until a whole page lies outside the age
// window, the feed ends, or maxPages is reached. Page 1 failing is an error. A
// later page failing (a rate limit, say) ends the run and returns the jobs
// collected so far together with an error wrapping ats.ErrPartialResult, so
// the ingester stores them and still reports the failure.
func (h *Himalayas) Collect(ctx context.Context) ([]ats.Job, error) {
	cutoff := h.cutoff()
	t := h.newTally()
	cursor := ""
	var oldest time.Time
	pages := 0
	for n := 1; n <= h.maxPages; n++ {
		if n > 1 && h.pause > 0 {
			select {
			case <-time.After(h.pause):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		u := fmt.Sprintf("%s?limit=%d", h.url, himalayasPageSize)
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		pg, err := h.page(ctx, u, n)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if n == 1 {
				return nil, err
			}
			jobs, rerr := t.result()
			if rerr != nil {
				return nil, err
			}
			h.logger.Warn("himalayas: stopped at a page that failed; keeping the jobs already collected",
				"page", n, "collected", len(jobs), "error", err)
			return jobs, fmt.Errorf("himalayas: stopped at page %d after %d jobs: %w", n, len(jobs), errors.Join(ats.ErrPartialResult, err))
		}
		pages++

		records, broken := decodeRecords[himalayasJob](pg.Jobs)
		for i := 0; i < broken; i++ {
			t.addBroken()
		}
		inWindow := 0
		for _, j := range records {
			pub, _ := j.PubDate.Int64()
			exp, _ := j.ExpiryDate.Int64()
			published := epoch(pub)
			if !published.IsZero() {
				if oldest.IsZero() || published.Before(oldest) {
					oldest = published
				}
				if !published.Before(cutoff) {
					inWindow++
				}
			} else {
				inWindow++ // an unknown date cannot end the run
			}
			// The listing's own page is the link back; the application link
			// is only a fallback, and judge refuses any other host.
			link := strings.TrimSpace(j.GUID)
			if link == "" {
				link = strings.TrimSpace(j.ApplicationLink)
			}
			location := restrictions(j.LocationRestrictions)
			if location == "" {
				location = "Worldwide"
			}
			description := j.Description
			if description == "" {
				description = j.Excerpt
			}
			t.add(finish(ats.Job{
				ExternalID:     strings.TrimPrefix(link, "https://himalayas.app/"),
				Title:          j.Title,
				URL:            link,
				Employer:       j.CompanyName,
				LocationRaw:    location,
				Description:    ats.HTMLToText(description),
				PublishedAt:    published,
				ExpiresAt:      epoch(exp),
				EmploymentType: employment(j.EmploymentType),
			}), false)
		}
		// Stop when the whole page is outside the window (a page can mix ages:
		// the cursor is by creation time, the dates by publication), or the
		// feed ends.
		if len(records) > 0 && inWindow == 0 || pg.NextCursor == "" || len(pg.Jobs) == 0 {
			break
		}
		cursor = pg.NextCursor
	}
	jobs, err := t.result()
	if err == nil {
		h.logger.Info("himalayas: read", "pages", pages, "jobs", len(jobs), "oldest_published", oldest)
	}
	return jobs, err
}

// page fetches and decodes one page. Page 1 with no jobs is an error.
func (h *Himalayas) page(ctx context.Context, u string, n int) (himalayasPage, error) {
	body, err := h.get(ctx, u)
	if err != nil {
		return himalayasPage{}, err
	}
	var pg himalayasPage
	if err := json.Unmarshal(body, &pg); err != nil {
		return himalayasPage{}, fmt.Errorf("himalayas: page %d is not the expected JSON: %w", n, err)
	}
	if n == 1 && len(pg.Jobs) == 0 {
		return himalayasPage{}, errNoJobs("himalayas")
	}
	return pg, nil
}
