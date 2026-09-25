package remoteboards

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
)

// WeWorkRemotely reads https://weworkremotely.com/remote-jobs.rss, the site's
// public RSS feed of its newest listings (robots.txt allows it). Each item's
// link is its own page for the job.
type WeWorkRemotely struct {
	board
	url string
}

// NewWeWorkRemotely returns a We Work Remotely collector.
func NewWeWorkRemotely(doer Doer, logger *slog.Logger) *WeWorkRemotely {
	return &WeWorkRemotely{board: newBoard("weworkremotely", doer, logger), url: "https://weworkremotely.com/remote-jobs.rss"}
}

type wwrFeed struct {
	Items []struct {
		Title       string `xml:"title"`
		Region      string `xml:"region"`
		Country     string `xml:"country"`
		Type        string `xml:"type"`
		Description string `xml:"description"`
		PubDate     string `xml:"pubDate"`
		ExpiresAt   string `xml:"expires_at"`
		GUID        string `xml:"guid"`
		Link        string `xml:"link"`
	} `xml:"channel>item"`
}

// stripFlags removes regional-indicator symbols (the flag emoji halves) and
// tidies the spaces they leave.
func stripFlags(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 0x1F1E6 && r <= 0x1F1FF {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// wwrDate parses the RSS date forms the feed uses.
func wwrDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// Collect returns the jobs in the feed. A title reads "Company: Job title".
func (w *WeWorkRemotely) Collect(ctx context.Context) ([]ats.Job, error) {
	body, err := w.get(ctx, w.url)
	if err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = charset.NewReaderLabel
	var feed wwrFeed
	if err := dec.Decode(&feed); err != nil {
		return nil, fmt.Errorf("weworkremotely: response is not the expected RSS: %w", err)
	}
	if len(feed.Items) == 0 {
		return nil, errNoJobs("weworkremotely")
	}
	now := w.now()
	var jobs []ats.Job
	for _, it := range feed.Items {
		employer, title, ok := strings.Cut(it.Title, ": ")
		if !ok {
			continue // not "Company: Title": the employer cannot be told from the title
		}
		link := strings.TrimSpace(it.Link)
		if link == "" {
			link = strings.TrimSpace(it.GUID)
		}
		// The site's own listing is the identity: its slug is unique and stable.
		_, id, _ := strings.Cut(link, "/remote-jobs/")
		if strings.ContainsAny(id, "/?# ") {
			id = ""
		}
		expires := wwrDate(it.ExpiresAt)
		if !expires.IsZero() && !expires.After(now) {
			continue
		}
		// The listing names the countries candidates may be in ("<flag> Barbados
		// and <flag> United States of America"); without any, its region.
		location := stripFlags(it.Country)
		if location == "" {
			location = strings.TrimSpace(it.Region)
		}
		job := finish(ats.Job{
			ExternalID:     id,
			Title:          title,
			URL:            link,
			Employer:       employer,
			LocationRaw:    location,
			Description:    ats.HTMLToText(it.Description),
			PublishedAt:    wwrDate(it.PubDate),
			ExpiresAt:      expires,
			EmploymentType: employment(it.Type),
		})
		if w.accept(job) {
			jobs = append(jobs, job)
		}
	}
	return dedupe(jobs), nil
}
