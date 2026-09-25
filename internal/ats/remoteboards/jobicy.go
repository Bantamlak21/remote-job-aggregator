package remoteboards

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
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
	return &Jobicy{
		board: newBoard("jobicy", []string{"jobicy.com"}, time.Hour, doer, logger),
		url:   "https://jobicy.com/api/v2/remote-jobs?count=100",
	}
}

type jobicyJob struct {
	ID             json.Number     `json:"id"`
	URL            string          `json:"url"`
	JobTitle       string          `json:"jobTitle"`
	CompanyName    string          `json:"companyName"`
	JobType        json.RawMessage `json:"jobType"` // a list of strings; tolerated as one string
	JobGeo         string          `json:"jobGeo"`
	JobDescription string          `json:"jobDescription"`
	PubDate        string          `json:"pubDate"`
}

// firstString reads the first string of a JSON list of strings, or the string
// itself, or "" for anything else.
func firstString(raw json.RawMessage) string {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return list[0]
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// Collect returns the newest jobs in Jobicy's feed (its API caps one call at
// 100).
func (j *Jobicy) Collect(ctx context.Context) ([]ats.Job, error) {
	body, err := j.get(ctx, j.url)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("jobicy: response is not the expected JSON: %w", err)
	}
	records, broken := decodeRecords[jobicyJob](resp.Jobs)
	t := j.newTally()
	for i := 0; i < broken; i++ {
		t.addBroken()
	}
	for _, r := range records {
		published := parseTime(r.PubDate)
		t.add(finish(ats.Job{
			ExternalID:     r.ID.String(),
			Title:          r.JobTitle,
			URL:            r.URL,
			Employer:       r.CompanyName,
			LocationRaw:    r.JobGeo,
			Description:    ats.HTMLToText(r.JobDescription),
			PublishedAt:    published,
			EmploymentType: employment(firstString(r.JobType)),
		}), dateProblem(r.PubDate, published))
	}
	return t.result()
}
