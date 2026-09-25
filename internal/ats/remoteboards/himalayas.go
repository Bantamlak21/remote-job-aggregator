package remoteboards

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
)

const (
	// DefaultHimalayasPages is how many pages (20 jobs each) one run reads at
	// most. The feed holds about 100,000 jobs, newest first; a run stops as
	// soon as it passes the age window, and this caps the rest.
	DefaultHimalayasPages = 25
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
	return &Himalayas{board: newBoard("himalayas", doer, logger), url: "https://himalayas.app/jobs/api",
		maxPages: min(maxPages, hardHimalayasPages), pause: pause}
}

type himalayasPage struct {
	NextCursor string `json:"nextCursor"`
	Jobs       []struct {
		Title                string      `json:"title"`
		CompanyName          string      `json:"companyName"`
		EmploymentType       string      `json:"employmentType"`
		LocationRestrictions []string    `json:"locationRestrictions"`
		PubDate              json.Number `json:"pubDate"`
		ExpiryDate           json.Number `json:"expiryDate"`
		ApplicationLink      string      `json:"applicationLink"`
		GUID                 string      `json:"guid"`
		Description          string      `json:"description"`
		Excerpt              string      `json:"excerpt"`
	} `json:"jobs"`
}

// Collect reads pages newest first until a job passes the age window, the
// feed ends, or maxPages is reached. Page 1 failing is an error; a later page
// failing (including a rate limit) ends the run with what was collected.
func (h *Himalayas) Collect(ctx context.Context) ([]ats.Job, error) {
	cutoff := h.cutoff()
	var jobs []ats.Job
	cursor := ""
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
		body, err := h.get(ctx, u)
		if err == nil {
			var pg himalayasPage
			if err = json.Unmarshal(body, &pg); err != nil {
				err = fmt.Errorf("himalayas: page %d is not the expected JSON: %w", n, err)
			} else if n == 1 && len(pg.Jobs) == 0 {
				err = errNoJobs("himalayas")
			} else {
				pastWindow := len(pg.Jobs) == 0
				for _, j := range pg.Jobs {
					var pub, exp int64
					pub, _ = j.PubDate.Int64()
					exp, _ = j.ExpiryDate.Int64()
					if p := epoch(pub); !p.IsZero() && p.Before(cutoff) {
						pastWindow = true
						continue
					}
					id := strings.TrimPrefix(strings.TrimSpace(j.GUID), "https://himalayas.app/")
					link := strings.TrimSpace(j.ApplicationLink)
					if link == "" {
						link = strings.TrimSpace(j.GUID)
					}
					location := "Worldwide"
					if len(j.LocationRestrictions) > 0 {
						location = strings.Join(j.LocationRestrictions, ", ")
					}
					description := j.Description
					if description == "" {
						description = j.Excerpt
					}
					job := finish(ats.Job{
						ExternalID:     id,
						Title:          j.Title,
						URL:            link,
						Employer:       j.CompanyName,
						LocationRaw:    location,
						Description:    ats.HTMLToText(description),
						PublishedAt:    epoch(pub),
						ExpiresAt:      epoch(exp),
						EmploymentType: employment(j.EmploymentType),
					})
					if h.accept(job) {
						jobs = append(jobs, job)
					}
				}
				if pastWindow || pg.NextCursor == "" {
					return dedupe(jobs), nil
				}
				cursor = pg.NextCursor
				continue
			}
		}
		// A failure. The first page failing is the run's failure; later, keep
		// what was collected.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if n == 1 {
			return nil, err
		}
		h.logger.Warn("himalayas: stopping at a page that failed; keeping the jobs already collected",
			"page", n, "collected", len(jobs), "error", err)
		break
	}
	return dedupe(jobs), nil
}
