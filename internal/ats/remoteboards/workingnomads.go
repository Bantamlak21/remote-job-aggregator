package remoteboards

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
)

// WorkingNomads reads https://www.workingnomads.com/api/exposed_jobs/, the
// feed Working Nomads publishes for this use. Each job's URL is its own
// redirect page (/job/go/<id>/), which is the link back; that page sends the
// visitor on to the application.
type WorkingNomads struct {
	board
	url string
}

// NewWorkingNomads returns a Working Nomads collector.
func NewWorkingNomads(doer Doer, logger *slog.Logger) *WorkingNomads {
	return &WorkingNomads{
		board: newBoard("workingnomads", []string{"workingnomads.com"}, time.Hour, doer, logger),
		url:   "https://www.workingnomads.com/api/exposed_jobs/",
	}
}

type workingNomadsJob struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	CompanyName string `json:"company_name"`
	Location    string `json:"location"`
	PubDate     string `json:"pub_date"`
}

var workingNomadsPath = regexp.MustCompile(`^/job/go/(\d+)/?$`)

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
	records, broken := decodeRecords[workingNomadsJob](raw)
	t := w.newTally()
	for i := 0; i < broken; i++ {
		t.addBroken()
	}
	for _, r := range records {
		// The id is the number in the job's own /job/go/<id>/ path.
		id := ""
		if u, err := url.Parse(strings.TrimSpace(r.URL)); err == nil {
			if m := workingNomadsPath.FindStringSubmatch(u.Path); m != nil {
				id = m[1]
			}
		}
		published := parseTime(r.PubDate)
		t.add(finish(ats.Job{
			ExternalID:  id,
			Title:       r.Title,
			URL:         r.URL,
			Employer:    r.CompanyName,
			LocationRaw: r.Location,
			Description: ats.HTMLToText(r.Description),
			PublishedAt: published,
		}), dateProblem(r.PubDate, published))
	}
	return t.result()
}
