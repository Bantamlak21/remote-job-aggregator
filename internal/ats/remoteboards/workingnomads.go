package remoteboards

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
)

// WorkingNomads reads https://www.workingnomads.com/api/exposed_jobs/, the
// feed Working Nomads publishes for exactly this use. Each job's URL is its
// own redirect page (/job/go/<id>/), which is the link back.
type WorkingNomads struct {
	board
	url string
}

// NewWorkingNomads returns a Working Nomads collector.
func NewWorkingNomads(doer Doer, logger *slog.Logger) *WorkingNomads {
	return &WorkingNomads{board: newBoard("workingnomads", doer, logger), url: "https://www.workingnomads.com/api/exposed_jobs/"}
}

type workingNomadsJob struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	CompanyName string `json:"company_name"`
	Location    string `json:"location"`
	PubDate     string `json:"pub_date"`
}

var workingNomadsID = regexp.MustCompile(`/job/go/(\d+)/?`)

// Collect returns the jobs in Working Nomads' feed.
func (w *WorkingNomads) Collect(ctx context.Context) ([]ats.Job, error) {
	body, err := w.get(ctx, w.url)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("workingnomads: response is not the expected JSON array: %w", err)
	}
	if len(raw) == 0 {
		return nil, errNoJobs("workingnomads")
	}
	var jobs []ats.Job
	for _, item := range raw {
		var r workingNomadsJob
		if err := json.Unmarshal(item, &r); err != nil {
			continue
		}
		m := workingNomadsID.FindStringSubmatch(r.URL)
		if m == nil {
			continue // no id in the URL: no stable identity
		}
		job := finish(ats.Job{
			ExternalID:  m[1],
			Title:       r.Title,
			URL:         strings.TrimSpace(r.URL),
			Employer:    r.CompanyName,
			LocationRaw: r.Location,
			Description: ats.HTMLToText(r.Description),
			PublishedAt: page.ParseDate(r.PubDate),
		})
		if w.accept(job) {
			jobs = append(jobs, job)
		}
	}
	return dedupe(jobs), nil
}
