package remoteboards

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
)

// Remotive reads https://remotive.com/api/remote-jobs. Remotive's terms: link
// back to the URL it gives and name Remotive as the source; do not pass its
// jobs on to other job sites; call it a couple of times a day at most (it
// advises 4). Its free feed is delayed by 24 hours and holds only the jobs it
// chooses to share (19 on 2026-09-24).
type Remotive struct {
	board
	url string
}

// NewRemotive returns a Remotive collector.
func NewRemotive(doer Doer, logger *slog.Logger) *Remotive {
	return &Remotive{board: newBoard("remotive", doer, logger), url: "https://remotive.com/api/remote-jobs?limit=500"}
}

type remotiveJob struct {
	ID                        json.Number `json:"id"`
	URL                       string      `json:"url"`
	Title                     string      `json:"title"`
	CompanyName               string      `json:"company_name"`
	JobType                   string      `json:"job_type"`
	PublicationDate           string      `json:"publication_date"`
	CandidateRequiredLocation string      `json:"candidate_required_location"`
	Description               string      `json:"description"`
}

// Collect returns the jobs in Remotive's feed.
func (r *Remotive) Collect(ctx context.Context) ([]ats.Job, error) {
	body, err := r.get(ctx, r.url)
	if err != nil {
		return nil, err
	}
	// Records are decoded one at a time so a single odd record (an id that is
	// not a number) costs that job, not the feed.
	var resp struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("remotive: response is not the expected JSON: %w", err)
	}
	if len(resp.Jobs) == 0 {
		return nil, errNoJobs("remotive")
	}
	var jobs []ats.Job
	for _, raw := range resp.Jobs {
		var j remotiveJob
		if err := json.Unmarshal(raw, &j); err != nil {
			continue
		}
		job := finish(ats.Job{
			ExternalID:     j.ID.String(),
			Title:          j.Title,
			URL:            strings.TrimSpace(j.URL),
			Employer:       j.CompanyName,
			LocationRaw:    j.CandidateRequiredLocation,
			Description:    ats.HTMLToText(j.Description),
			PublishedAt:    page.ParseDate(j.PublicationDate), // UTC, no zone in the feed
			EmploymentType: employment(j.JobType),
		})
		if _, err := strconv.ParseInt(j.ID.String(), 10, 64); err != nil {
			continue // an id that is not a number is not an id
		}
		if r.accept(job) {
			jobs = append(jobs, job)
		}
	}
	return dedupe(jobs), nil
}
