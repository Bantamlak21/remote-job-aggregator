package remoteboards

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
)

// Jobicy reads https://jobicy.com/api/v2/remote-jobs. Its terms: credit Jobicy
// with a direct link, and send application buttons to the job URL it provides
// (its own page for the job).
type Jobicy struct {
	board
	url string
}

// NewJobicy returns a Jobicy collector.
func NewJobicy(doer Doer, logger *slog.Logger) *Jobicy {
	return &Jobicy{board: newBoard("jobicy", doer, logger), url: "https://jobicy.com/api/v2/remote-jobs?count=100"}
}

type jobicyResponse struct {
	Jobs []struct {
		ID             json.Number `json:"id"`
		URL            string      `json:"url"`
		JobTitle       string      `json:"jobTitle"`
		CompanyName    string      `json:"companyName"`
		JobType        []string    `json:"jobType"`
		JobGeo         string      `json:"jobGeo"`
		JobDescription string      `json:"jobDescription"`
		PubDate        string      `json:"pubDate"`
	} `json:"jobs"`
}

// Collect returns the newest jobs in Jobicy's feed (its API caps one call at
// 100).
func (j *Jobicy) Collect(ctx context.Context) ([]ats.Job, error) {
	body, err := j.get(ctx, j.url)
	if err != nil {
		return nil, err
	}
	var resp jobicyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("jobicy: response is not the expected JSON: %w", err)
	}
	if len(resp.Jobs) == 0 {
		return nil, errNoJobs("jobicy")
	}
	var jobs []ats.Job
	for _, r := range resp.Jobs {
		var kind string
		if len(r.JobType) > 0 {
			kind = employment(r.JobType[0])
		}
		job := finish(ats.Job{
			ExternalID:     r.ID.String(),
			Title:          r.JobTitle,
			URL:            strings.TrimSpace(r.URL),
			Employer:       r.CompanyName,
			LocationRaw:    r.JobGeo,
			Description:    ats.HTMLToText(r.JobDescription),
			PublishedAt:    page.ParseDate(r.PubDate),
			EmploymentType: kind,
		})
		if j.accept(job) {
			jobs = append(jobs, job)
		}
	}
	return dedupe(jobs), nil
}
