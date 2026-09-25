package remoteboards

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/url"
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
	return &WeWorkRemotely{
		board: newBoard("weworkremotely", []string{"weworkremotely.com"}, time.Hour, doer, logger),
		url:   "https://weworkremotely.com/remote-jobs.rss",
	}
}

type wwrItem struct {
	Title       string `xml:"title"`
	Region      string `xml:"region"`
	Country     string `xml:"country"`
	Type        string `xml:"type"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	ExpiresAt   string `xml:"expires_at"`
	GUID        string `xml:"guid"`
	Link        string `xml:"link"`
}

type wwrFeed struct {
	Items []wwrItem `xml:"channel>item"`
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

// wwrID is the slug of the item's own listing page: /remote-jobs/<slug>. The
// query string and fragment are not part of the identity; "" for any other
// path.
func wwrID(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	slug, ok := strings.CutPrefix(u.Path, "/remote-jobs/")
	if !ok || slug == "" || strings.Contains(slug, "/") {
		return ""
	}
	return slug
}

// Collect returns the jobs in the feed. A title reads "Company: Job title";
// it is split at the first ": ".
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
	t := w.newTally()
	for _, it := range feed.Items {
		employer, title, ok := strings.Cut(it.Title, ": ")
		if !ok {
			t.addBroken() // not "Company: Title": the employer cannot be told from the title
			continue
		}
		link := strings.TrimSpace(it.Link)
		if link == "" {
			link = strings.TrimSpace(it.GUID)
		}
		published, expires := parseTime(it.PubDate), parseTime(it.ExpiresAt)
		// The listing names the countries candidates may be in ("<flag> Barbados
		// and <flag> United States of America"); without any, its region.
		location := stripFlags(it.Country)
		if location == "" {
			location = strings.TrimSpace(it.Region)
		}
		// The item's expiry rides on ExpiresAt: judge drops an expired item,
		// and the ingester hides a stored one once the date passes.
		t.add(finish(ats.Job{
			ExternalID:     wwrID(link),
			Title:          title,
			URL:            link,
			Employer:       employer,
			LocationRaw:    location,
			Description:    ats.HTMLToText(it.Description),
			PublishedAt:    published,
			ExpiresAt:      expires,
			EmploymentType: employment(it.Type),
		}), dateProblem(it.PubDate, published) || dateProblem(it.ExpiresAt, expires))
	}
	return t.result()
}
