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

// RemoteOK reads https://remoteok.com/api. Its terms: link back to the job's
// Remote OK URL (with follow, no nofollow) and name Remote OK as the source;
// never use its logo. This project stores no logos, and the job URL is the
// Remote OK page.
type RemoteOK struct {
	board
	url string
}

// NewRemoteOK returns a Remote OK collector.
func NewRemoteOK(doer Doer, logger *slog.Logger) *RemoteOK {
	return &RemoteOK{board: newBoard("remoteok", doer, logger), url: "https://remoteok.com/api"}
}

type remoteOKJob struct {
	ID          string `json:"id"`
	Epoch       int64  `json:"epoch"`
	Date        string `json:"date"`
	Company     string `json:"company"`
	Position    string `json:"position"`
	Description string `json:"description"`
	Location    string `json:"location"`
	URL         string `json:"url"`
	// Legal is set only on the first array element, a notice about the API's
	// terms rather than a job.
	Legal string `json:"legal"`
}

// Collect returns the jobs in Remote OK's feed.
func (r *RemoteOK) Collect(ctx context.Context) ([]ats.Job, error) {
	body, err := r.get(ctx, r.url)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("remoteok: response is not the expected JSON array: %w", err)
	}

	var jobs []ats.Job
	listed := 0
	for _, item := range raw {
		var j remoteOKJob
		if err := json.Unmarshal(item, &j); err != nil {
			continue // one odd record costs that job only
		}
		if j.Legal != "" {
			continue
		}
		listed++
		published := epoch(j.Epoch)
		if published.IsZero() {
			published = page.ParseDate(j.Date)
		}
		job := finish(ats.Job{
			ExternalID:  j.ID,
			Title:       j.Position,
			URL:         strings.TrimSpace(j.URL),
			Employer:    j.Company,
			LocationRaw: j.Location,
			Description: ats.HTMLToText(j.Description),
			PublishedAt: published,
		})
		if r.accept(job) {
			jobs = append(jobs, job)
		}
	}
	if listed == 0 {
		return nil, errNoJobs("remoteok")
	}
	return dedupe(jobs), nil
}
