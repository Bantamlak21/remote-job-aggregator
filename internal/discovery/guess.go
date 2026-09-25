package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/companymatch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// A company name is enough to find its job board on the three big public
// ATSs: their boards are addressed by a slug that is nearly always the
// company's name, and each answers 404 for a board that does not exist. The
// danger is a different company using the slug ("close", "signal", "front"),
// so a board is accepted only when it proves it belongs to the company:
//
//   - Greenhouse names its board: the name must match the company's.
//   - Lever and Ashby do not, so the company's name must appear in the text of
//     at least half of the board's first jobs.
//
// Everything else is left alone: a board whose owner cannot be confirmed is
// reported and not registered.

const (
	// identitySample is how many of a board's jobs are read to confirm who owns
	// it (Lever and Ashby).
	identitySample  = 8
	maxSlugVariants = 4
)

// boardBases are the API roots the guesser probes; tests replace them.
type boardBases struct {
	greenhouse, lever, ashby string
}

var defaultBases = boardBases{
	greenhouse: "https://boards-api.greenhouse.io/v1/boards",
	lever:      "https://api.lever.co/v0/postings",
	ashby:      "https://api.ashbyhq.com/posting-api/job-board",
}

// GuessReport counts what one Guess run found.
type GuessReport struct {
	Names       int
	Found       int      // names with at least one verified board
	Boards      int      // verified boards
	NotFound    []string // names with no board on any of the three ATSs
	Unconfirmed []string // "name provider/slug": a board exists but does not show it is the company's
}

// Guesser finds boards from company names.
type Guesser struct {
	http    Doer
	logger  *slog.Logger
	workers int
	pause   time.Duration
	bases   boardBases
}

// Doer is the slice of *httpclient.Client the guesser needs.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewGuesser returns a Guesser. workers <= 0 means 4; pause is the delay each
// worker leaves between requests, to stay polite to the three ATS APIs.
func NewGuesser(doer Doer, workers int, pause time.Duration, logger *slog.Logger) *Guesser {
	if workers <= 0 {
		workers = 4
	}
	return &Guesser{http: doer, logger: logger, workers: workers, pause: pause, bases: defaultBases}
}

// slugVariants lists the board slugs worth trying for a company name: the
// name run together ("grafanalabs"), dash-joined ("grafana-labs"), and both
// again without a trailing corporate word ("Acme Inc" also tries "acme").
func slugVariants(name string) []string {
	var out []string
	add := func(s string) {
		if s = strings.Trim(s, "-"); s == "" || len(out) >= maxSlugVariants {
			return
		}
		for _, o := range out {
			if o == s {
				return
			}
		}
		out = append(out, s)
	}
	name = stripDomain(name)
	dash := companymatch.Slug(strings.ReplaceAll(name, ".", ""))
	add(strings.ReplaceAll(dash, "-", ""))
	add(dash)
	key := companymatch.Key(name)
	add(strings.ReplaceAll(key, "-", ""))
	add(key)
	return out
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// squash lower-cases and drops everything but letters and digits, for
// "does this text mention the company" checks.
func squash(s string) string { return nonAlnum.ReplaceAllString(strings.ToLower(s), "") }

var domainSuffix = regexp.MustCompile(`(?i)\.(com|io|ai|dev|co|app|net|org|so|sh)\s*$`)

// stripDomain drops a trailing web-address ending ("Honeycomb.io" is
// "Honeycomb"): companies write their name both ways.
func stripDomain(name string) string {
	return domainSuffix.ReplaceAllString(strings.TrimSpace(name), "")
}

// namesMatch reports whether two company names are the same company for
// this purpose: equal after dropping corporate suffixes, or one is the other
// followed by more words ("Grafana" and "Grafana Labs").
func namesMatch(a, b string) bool {
	ka, kb := companymatch.Key(stripDomain(a)), companymatch.Key(stripDomain(b))
	if ka == "" || kb == "" {
		return false
	}
	return ka == kb || strings.HasPrefix(ka+"-", kb+"-") || strings.HasPrefix(kb+"-", ka+"-")
}

// Guess probes every name against Greenhouse, Lever and Ashby and returns a
// Candidate for each verified board with at least one job. Names are worked
// through by a small pool of workers; cancellation stops it.
func (g *Guesser) Guess(ctx context.Context, names []string) ([]Candidate, GuessReport) {
	type outcome struct {
		name        string
		boards      []Candidate
		unconfirmed []string
	}
	jobs := make(chan string)
	outcomes := make(chan outcome)
	var wg sync.WaitGroup
	for i := 0; i < g.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range jobs {
				b, u := g.guessOne(ctx, name)
				outcomes <- outcome{name, b, u}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, n := range names {
			select {
			case jobs <- n:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(outcomes) }()

	report := GuessReport{Names: len(names)}
	byName := map[string]outcome{}
	for o := range outcomes {
		byName[o.name] = o
	}
	// Report in the order the names were given, whatever order the workers
	// finished in.
	var candidates []Candidate
	for _, n := range names {
		o, ok := byName[n]
		if !ok {
			continue // not reached (canceled)
		}
		report.Unconfirmed = append(report.Unconfirmed, o.unconfirmed...)
		if len(o.boards) == 0 {
			if len(o.unconfirmed) == 0 {
				report.NotFound = append(report.NotFound, n)
			}
			continue
		}
		report.Found++
		report.Boards += len(o.boards)
		candidates = append(candidates, o.boards...)
	}
	return candidates, report
}

func (g *Guesser) guessOne(ctx context.Context, name string) (boards []Candidate, unconfirmed []string) {
	seen := map[string]bool{}
	for _, slug := range slugVariants(name) {
		for _, provider := range []string{string(ats.ProviderGreenhouse), string(ats.ProviderLever), string(ats.ProviderAshby)} {
			if ctx.Err() != nil {
				return boards, unconfirmed
			}
			key := provider + "/" + slug
			if seen[key] {
				continue
			}
			seen[key] = true
			var v verdict
			switch provider {
			case "greenhouse":
				v = g.probeGreenhouse(ctx, name, slug)
			case "lever":
				v = g.probeLever(ctx, name, slug)
			case "ashby":
				v = g.probeAshby(ctx, name, slug)
			}
			switch v {
			case boardOwned:
				boards = append(boards, Candidate{CompanyName: name, ATSProvider: provider, ExternalBoardID: slug, BoardURL: publicURL(provider, slug)})
			case boardForeign:
				unconfirmed = append(unconfirmed, fmt.Sprintf("%s %s/%s", name, provider, slug))
			}
		}
	}
	return boards, unconfirmed
}

func publicURL(provider, slug string) string {
	switch provider {
	case "greenhouse":
		return "https://boards.greenhouse.io/" + slug
	case "lever":
		return "https://jobs.lever.co/" + slug
	default:
		return "https://jobs.ashbyhq.com/" + slug
	}
}

// get fetches a URL and returns the status and up to maxProbeBytes of body.
func (g *Guesser) get(ctx context.Context, u string) (int, []byte) {
	if g.pause > 0 {
		select {
		case <-time.After(g.pause):
		case <-ctx.Done():
			return 0, nil
		}
	}
	req, err := http.NewRequestWithContext(httpclient.WithoutRetries(ctx), http.MethodGet, u, nil)
	if err != nil {
		return 0, nil
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProbeBytes))
	return resp.StatusCode, body
}

// maxProbeBytes bounds what one probe reads: enough for the first jobs of a
// board, never a whole 2 MB board.
const maxProbeBytes = 1 << 20

// verdict is what one probe learned about a slug on one ATS.
type verdict int

const (
	boardNone    verdict = iota // no board, or nothing to read
	boardEmpty                  // the company's board, with no jobs to ingest
	boardForeign                // a board with jobs whose owner is not shown to be the company
	boardOwned                  // the company's board, with jobs
)

func (g *Guesser) probeGreenhouse(ctx context.Context, name, slug string) verdict {
	status, body := g.get(ctx, fmt.Sprintf("%s/%s", g.bases.greenhouse, url.PathEscape(slug)))
	if status != http.StatusOK {
		return boardNone
	}
	var b struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(body, &b) != nil {
		return boardNone
	}
	if !namesMatch(name, b.Name) {
		return boardForeign // a board exists under this slug, for some other company
	}
	// Owned by the company; worth registering only if it has jobs.
	status, body = g.get(ctx, fmt.Sprintf("%s/%s/jobs", g.bases.greenhouse, url.PathEscape(slug)))
	if status != http.StatusOK {
		return boardNone
	}
	var jobs struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if json.Unmarshal(body, &jobs) != nil {
		return boardNone
	}
	if len(jobs.Jobs) == 0 {
		return boardEmpty
	}
	return boardOwned
}

func (g *Guesser) probeLever(ctx context.Context, name, slug string) verdict {
	status, body := g.get(ctx, fmt.Sprintf("%s/%s?mode=json&limit=%d", g.bases.lever, url.PathEscape(slug), identitySample))
	if status != http.StatusOK {
		return boardNone
	}
	var postings []struct {
		Text             string `json:"text"`
		DescriptionPlain string `json:"descriptionPlain"`
		AdditionalPlain  string `json:"additionalPlain"`
	}
	if json.Unmarshal(body, &postings) != nil {
		return boardNone
	}
	if len(postings) == 0 {
		return boardEmpty // nothing to ingest, and nothing to say whose it is
	}
	texts := make([]string, 0, len(postings))
	for _, p := range postings {
		texts = append(texts, p.Text+" "+p.DescriptionPlain+" "+p.AdditionalPlain)
	}
	return ownership(name, texts)
}

// ownership turns the name check into a verdict for a board that has jobs.
func ownership(name string, texts []string) verdict {
	if ownedByName(name, texts) {
		return boardOwned
	}
	return boardForeign
}

func (g *Guesser) probeAshby(ctx context.Context, name, slug string) verdict {
	status, body := g.get(ctx, fmt.Sprintf("%s/%s", g.bases.ashby, url.PathEscape(slug)))
	if status != http.StatusOK {
		return boardNone
	}
	var b struct {
		Jobs []struct {
			Title            string `json:"title"`
			DescriptionPlain string `json:"descriptionPlain"`
			IsListed         *bool  `json:"isListed"`
		} `json:"jobs"`
	}
	if json.Unmarshal(body, &b) != nil {
		return boardNone
	}
	var texts []string
	for _, j := range b.Jobs {
		if j.IsListed != nil && !*j.IsListed {
			continue
		}
		texts = append(texts, j.Title+" "+j.DescriptionPlain)
		if len(texts) == identitySample {
			break
		}
	}
	if len(texts) == 0 {
		return boardEmpty // no listed job: nothing to ingest
	}
	return ownership(name, texts)
}

// ownedByName reports whether the company's name appears in at least half
// (rounded up, at least one) of the sampled job texts.
func ownedByName(name string, texts []string) bool {
	want := squash(strings.ReplaceAll(name, ".com", ""))
	if want == "" || len(texts) == 0 {
		return false
	}
	hits := 0
	for _, t := range texts {
		if strings.Contains(squash(t), want) {
			hits++
		}
	}
	need := (len(texts) + 1) / 2
	return hits >= need
}
