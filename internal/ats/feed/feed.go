// Package feed reads a company's job openings from an RSS 2.0 feed — the
// shape WordPress career plugins (WP Job Openings, Job Manager) publish at
// /jobs/feed/. The board token is the feed URL itself.
//
// A feed lists every post a site ever published, including years-old
// openings nobody deleted (EthSwitch's and ZalaTech's feeds, verified live,
// end in 2024 and 2023). Anything older than MaxAge is therefore not a
// current opening and is left out, so it is never shown and, once left out,
// is closed by the normal "missing from the board" rule.
package feed

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// maxFeedBytes bounds how much of one feed is read.
const maxFeedBytes = 4 << 20

// DefaultMaxAge is how old an item may be and still count as an opening.
const DefaultMaxAge = 120 * 24 * time.Hour

// Doer is the slice of *httpclient.Client the Client needs.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Allower answers whether a URL may be fetched (*robots.Checker).
type Allower interface {
	Allowed(ctx context.Context, rawURL string) (bool, error)
}

// Client lists jobs from RSS feeds.
type Client struct {
	http   Doer
	robots Allower
	maxAge time.Duration
	now    func() time.Time
}

// New returns a Client. maxAge <= 0 means DefaultMaxAge.
func New(doer Doer, robots Allower, maxAge time.Duration) *Client {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	return &Client{http: doer, robots: robots, maxAge: maxAge, now: time.Now}
}

// rss names its root element, so decoding an HTML error page or any other
// XML document fails instead of yielding an empty (and therefore
// "everything was closed") job list. A genuine feed with zero items still
// decodes fine.
type rss struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []item `xml:"item"`
	} `xml:"channel"`
}

type item struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
	Content     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
}

// ListJobs fetches feedURL and returns its current openings.
func (c *Client) ListJobs(ctx context.Context, feedURL string) ([]ats.Job, error) {
	u, err := url.Parse(feedURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("feed: %q: %w", feedURL, ats.ErrInvalidBoardToken)
	}

	allowed, err := c.robots.Allowed(ctx, feedURL)
	if err != nil {
		return nil, fmt.Errorf("feed: %s: checking robots.txt: %w", feedURL, err)
	}
	if !allowed {
		return nil, fmt.Errorf("feed: %s: fetch not allowed by robots.txt", feedURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("feed: building request: %w", err)
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, text/xml;q=0.8")
	// A redirect must stay on the site and land on a URL robots.txt allows.
	req = req.WithContext(httpclient.WithRedirectCheck(ctx, page.RedirectGuard(ctx, u.Host, c.robots)))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feed: %s: %w", feedURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, fmt.Errorf("feed: %s: %w", feedURL, ats.ErrBoardNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("feed: %s: unexpected status %d: %s", feedURL, resp.StatusCode, snippet)
	}

	dec := xml.NewDecoder(io.LimitReader(resp.Body, maxFeedBytes))
	dec.CharsetReader = charset.NewReaderLabel
	var doc rss
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("feed: %s: parsing RSS: %w", feedURL, err)
	}

	cutoff := c.now().Add(-c.maxAge)
	jobs := make([]ats.Job, 0, len(doc.Channel.Items))
	for _, it := range doc.Channel.Items {
		published := parsePubDate(it.PubDate)
		// A date that is present but unreadable cannot be shown to be
		// fresh: skip the item. (An item with no date at all is kept.)
		if published.IsZero() && strings.TrimSpace(it.PubDate) != "" {
			continue
		}
		if !published.IsZero() && published.Before(cutoff) {
			continue
		}
		link := ats.CleanText(it.Link)
		if !isHTTPURL(link) {
			link = ""
		}
		id := ats.CleanText(it.GUID)
		if id == "" {
			id = link
		}
		body := it.Content
		if strings.TrimSpace(body) == "" {
			body = it.Description
		}
		jobs = append(jobs, ats.Job{
			ExternalID:  id,
			Title:       ats.CleanText(it.Title),
			URL:         link,
			Description: ats.HTMLToText(body),
			PublishedAt: published,
		})
	}
	return jobs, nil
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// parsePubDate parses the RFC 822 family RSS uses; zero on anything else.
func parsePubDate(s string) time.Time {
	s = strings.TrimSpace(s)
	// Unpadded-day forms ("Mon, 5 Feb 2023") are common and must parse: a
	// date that silently fails to parse would exempt an old item from the
	// age filter.
	for _, layout := range []string{
		time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC3339,
		"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
		"2 Jan 2006 15:04:05 -0700", "2 Jan 2006", "2006-01-02", "2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
