// Package ats defines the shape every ATS provider client normalizes
// its own API response into, and the shared HTML-to-plain-text helper
// every provider needs (Greenhouse's job descriptions are HTML; Lever's
// and Ashby's are too, per their own public APIs). Provider-specific
// response models stay inside each provider's own subpackage
// (internal/ats/greenhouse today); only this common shape crosses into
// internal/ingestion, so adding a second or third provider later never
// requires internal/ingestion to change.
package ats

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
)

// ErrBoardNotFound is returned by any provider's ListJobs when the ATS
// reports the board itself doesn't exist (e.g. Greenhouse's 404
// {"status":404,"error":"Job not found"}) — a definitive, permanent
// signal, unlike a transient network/5xx error. Shared across providers
// (not defined per-package) so internal/ingestion can react to it
// (deactivating the target) without importing any specific provider
// package and recoupling itself to one ATS.
var ErrBoardNotFound = errors.New("ats: board not found")

// ErrPartialResult marks an error that came with usable jobs: a collector
// that read part of its source and then failed (a later page hit a rate
// limit) returns the jobs it has together with an error wrapping this. The
// ingester stores those jobs and still reports the failure, so a truncated
// run is never mistaken for a complete one.
var ErrPartialResult = errors.New("ats: partial result")

// ErrInvalidBoardToken is returned when a board token is not safe to
// place in a request URL. Not a "board is gone" signal: the token itself
// is malformed, so it never reaches the network.
var ErrInvalidBoardToken = errors.New("ats: invalid board token")

// Provider names an ATS. Deliberately a plain string, not a Go
// CHECK-constrained enum backed by the database: target_companies.ats_provider
// has no CHECK constraint either, by the same reasoning documented on
// that column — new providers are meant to be added without a schema
// migration.
type Provider string

// The ats_provider values in use. The last three are not ATS vendors but
// the other ways a company's jobs reach the board; they share the same
// target_companies/jobs model, so ingestion treats them uniformly:
//
//   - ProviderFeed: an RSS feed (board id = the feed URL)
//   - ProviderCareersSite: a careers page listing job links (board id = the page URL)
//   - ProviderSearch: web search over LinkedIn (board id = the company name)
//
// Two more providers, "ethiojobs" and "linkedin", belong to many-employer
// collectors (ingestion.Collector). They have no client of their own: the
// collector names each job's employer and the ingester creates one target
// per employer (board id = the employer's lower-cased name).
const (
	ProviderGreenhouse  Provider = "greenhouse"
	ProviderFeed        Provider = "feed"
	ProviderCareersSite Provider = "careers-site"
	ProviderSearch      Provider = "search"
)

// Job is one job posting as fetched from an ATS, before it becomes a
// job.Record — provider-agnostic (every field here is something every
// provider's public API can supply), plain text (Description has
// already had HTML stripped via HTMLToText), and carries no
// company/target linkage, since a raw ATS response knows nothing about
// this project's companies/target_companies rows.
type Job struct {
	// ExternalID is the provider's own stable identifier for this job —
	// e.g. Greenhouse's numeric id, formatted as a string. Combined with
	// Provider by the caller to form the jobs table's (source,
	// source_job_id) identity.
	ExternalID  string
	Title       string
	URL         string
	LocationRaw string
	Description string
	PublishedAt time.Time // zero means the provider didn't supply one
	// ExpiresAt is the application deadline when the source publishes one
	// (zero: none known). The public API stops serving the job after it.
	ExpiresAt time.Time
	// RemoteType and EmploymentType are what the source itself says about the
	// role, in the jobs table's vocabulary ("remote", "hybrid", "onsite";
	// "full_time", "part_time", "contract", "internship"). "" means the
	// source does not say, and a stored value is then left as it is. Remote
	// job boards fill RemoteType; ATS boards mostly cannot.
	RemoteType     string
	EmploymentType string
	// Employer names the company that posted the job. Only multi-employer
	// sources (job boards, search) fill it; a per-company board (Greenhouse,
	// a feed, a careers page) leaves it empty because the target already
	// says whose it is.
	Employer string
	// Closed means the source itself reports this job as ended (an expired
	// or closed posting). Only ExternalID is meaningful then. Ingestion
	// does not store such a job; it closes any open row with that
	// ExternalID, so a job the source has retired disappears at once
	// instead of lingering until a stale window runs out.
	Closed bool
}

// CleanText makes an external string safe to store: drops NUL bytes
// (Postgres text columns reject them outright), replaces invalid UTF-8,
// and trims surrounding whitespace. Applied to every free-text field an
// ATS supplies — one bad byte in one job's title must never be able to
// abort ingestion of a whole board.
func CleanText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.TrimSpace(s)
}

// blockBreak tags separate paragraphs (a blank line); lineBreak tags
// separate lines. Everything else (strong, em, a, span, sup, ...) is
// inline and contributes no whitespace at all: "<strong>Note</strong>: x"
// must read "Note: x", not "Note : x".
var (
	blockBreak = map[string]bool{"p": true, "div": true, "h1": true, "h2": true, "h3": true,
		"h4": true, "h5": true, "h6": true, "blockquote": true, "table": true, "section": true, "article": true}
	lineBreak = map[string]bool{"br": true, "li": true, "ul": true, "ol": true, "tr": true, "dt": true, "dd": true}
	// skipContent tags hold text that is not prose (script/style bodies);
	// their contents are dropped entirely.
	skipContent = map[string]bool{"script": true, "style": true, "noscript": true, "template": true}
)

// HTMLToText converts an ATS's HTML job-description field into clean,
// readable plain text. It parses real HTML (not a regex tag-stripper,
// which breaks on nested tags and attributes containing ">") via
// golang.org/x/net/html, keeps text nodes, drops script/style bodies,
// puts blank lines between block-level elements and single newlines at
// <br>/list-item/row boundaries, and collapses source-formatting
// whitespace.
//
// Greenhouse entity-encodes its content field's outer HTML source itself
// — confirmed live: the JSON value is literally "&lt;div&gt;...", not
// "<div>...". When the input contains no literal '<' at all but does
// contain "&lt;", it is treated as outer-encoded and unescaped once
// before tokenizing; otherwise the tokenizer would see a single giant
// text token and hand back literal markup as "prose". Input that has
// real '<' tags is never pre-unescaped, so entities inside genuine HTML
// (e.g. a literal "&lt;b&gt;" meant as visible text) are preserved
// rather than mistaken for tags.
func HTMLToText(rawHTML string) string {
	if strings.TrimSpace(rawHTML) == "" {
		return ""
	}

	src := rawHTML
	if !strings.Contains(src, "<") && strings.Contains(src, "&lt;") {
		src = html.UnescapeString(src)
	}

	tokenizer := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder
	skipDepth := 0
	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			return CleanText(collapseWhitespace(b.String()))
		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			b.WriteString(breakStripper.Replace(collapseInline(string(tokenizer.Text()))))
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			tag := tokenizer.Token()
			if skipContent[tag.Data] {
				switch tt {
				case html.StartTagToken:
					skipDepth++
				case html.EndTagToken:
					if skipDepth > 0 {
						skipDepth--
					}
				}
				continue
			}
			switch {
			case blockBreak[tag.Data]:
				b.WriteRune(paragraphMark)
			case lineBreak[tag.Data]:
				b.WriteRune(lineMark)
			case tag.Data == "td" || tag.Data == "th":
				b.WriteByte(' ')
			}
		}
	}
}

// collapseInline turns every whitespace run inside one text node into a
// single space, keeping a leading/trailing space when the source had
// whitespace there (so "foo <b>bar</b>" keeps its space and
// "un<em>believable</em>" does not gain one).
func collapseInline(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	if space {
		b.WriteByte(' ')
	}
	return b.String()
}

// Private-use markers HTMLToText writes at block/line boundaries.
// collapseWhitespace resolves any run of them (with only whitespace
// between) to the single strongest break, so "</li><li>" yields one
// newline and "</ul></p>" one blank line — never the sum of each tag's
// own break.
const (
	lineMark      = '\x01'
	paragraphMark = '\x02'
)

var breakStripper = strings.NewReplacer(string(lineMark), "", string(paragraphMark), "")

// collapseWhitespace turns the marker/whitespace stream into final
// text: whitespace runs become one space, marker runs become the
// strongest pending break, and nothing is emitted before the first or
// after the last real character.
func collapseWhitespace(s string) string {
	var out strings.Builder
	pending := 0 // 0 none, 1 newline, 2 blank line
	space := false
	for _, r := range s {
		switch {
		case r == lineMark:
			pending = max(pending, 1)
		case r == paragraphMark:
			pending = 2
		case unicode.IsSpace(r):
			space = true
		default:
			if out.Len() > 0 {
				if pending > 0 {
					out.WriteString(strings.Repeat("\n", pending))
				} else if space {
					out.WriteByte(' ')
				}
			}
			pending, space = 0, false
			out.WriteRune(r)
		}
	}
	return out.String()
}
