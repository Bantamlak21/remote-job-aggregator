package remoteboards

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
)

// Remotive reads https://remotive.com/api/remote-jobs. Remotive's terms: link
// back to the URL it gives and name Remotive as the source; do not pass its
// jobs on to other job sites; call it a couple of times a day at most (it
// advises 4). Its free feed is delayed by 24 hours and holds only the jobs it
// chooses to share (19 on 2026-09-24). Publication dates carry no zone and
// are read as UTC.
//
// How the project meets those terms: the job's URL is Remotive's page and the
// UI credits Remotive; the jobs are shown on this site only, and are never put
// into a feed, sitemap or JobPosting markup for other sites or search engines
// to pick up; nothing is collected in exchange for showing them; and the
// ingester leaves at least MinInterval (6 hours, so at most 4 requests a day)
// between runs.
type Remotive struct {
	board
	url string
}

// NewRemotive returns a Remotive collector.
func NewRemotive(doer Doer, logger *slog.Logger) *Remotive {
	return &Remotive{
		board: newBoard("remotive", []string{"remotive.com"}, 6*time.Hour, doer, logger),
		url:   "https://remotive.com/api/remote-jobs?limit=500",
	}
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
	var resp struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("remotive: response is not the expected JSON: %w", err)
	}
	records, broken := decodeRecords[remotiveJob](resp.Jobs)
	t := r.newTally()
	for i := 0; i < broken; i++ {
		t.addBroken()
	}
	for _, j := range records {
		published := parseTime(j.PublicationDate)
		t.add(finish(ats.Job{
			ExternalID:     j.ID.String(),
			Title:          j.Title,
			URL:            j.URL,
			Employer:       j.CompanyName,
			LocationRaw:    j.CandidateRequiredLocation,
			Description:    ats.HTMLToText(j.Description),
			PublishedAt:    published,
			EmploymentType: employment(j.JobType),
		}), dateProblem(j.PublicationDate, published))
	}
	return t.result()
}
