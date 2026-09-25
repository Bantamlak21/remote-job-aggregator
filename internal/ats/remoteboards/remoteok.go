package remoteboards

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
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
	return &RemoteOK{
		board: newBoard("remoteok", []string{"remoteok.com"}, time.Hour, doer, logger),
		url:   "https://remoteok.com/api",
	}
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
	t := r.newTally()
	for _, item := range raw {
		var j remoteOKJob
		if err := json.Unmarshal(item, &j); err != nil {
			t.addBroken()
			continue
		}
		if j.Legal != "" {
			continue // the API's terms notice, not a job
		}
		published := epoch(j.Epoch)
		bad := false
		if published.IsZero() {
			published = parseTime(j.Date)
			bad = dateProblem(j.Date, published)
		}
		t.add(finish(ats.Job{
			ExternalID:  j.ID,
			Title:       j.Position,
			URL:         j.URL,
			Employer:    j.Company,
			LocationRaw: j.Location,
			Description: ats.HTMLToText(j.Description),
			PublishedAt: published,
		}), bad)
	}
	return t.result()
}
