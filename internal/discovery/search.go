package discovery

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
)

// SearchClient is the dependency CandidatesFromSearch needs from
// internal/search — defined here, on the consumer side, matching
// CompanyUpserter/TargetUpserter's pattern, so this file's own tests use
// a fake instead of a real Serper API key.
type SearchClient interface {
	Search(ctx context.Context, query string) ([]search.Result, error)
}

// providerPattern pairs an ATS provider with the regex that recognizes
// one of its real board URLs and extracts the board slug from it. Order
// matters only in that the first pattern to match a given result URL
// wins; the three don't overlap in host, so in practice it doesn't.
type providerPattern struct {
	provider string
	pattern  *regexp.Regexp
}

// knownProviders lists every ATS this project knows how to recognize
// from a search result URL. Adding a fourth provider here needs no
// change anywhere else in this file — CandidatesFromSearch just tries
// one more pattern.
//
// Each pattern is anchored at both ends of the slug: (?i) makes the
// scheme/host match case-insensitively (hosts are case-insensitive per
// RFC, and a bare case-sensitive match missed real URLs — found by
// adversarial review), and the trailing (?:[/?#]|$) requires the slug to
// end at a real path/query/fragment boundary rather than stopping
// wherever the character class happens to run out. Without that
// boundary, "https://boards.greenhouse.io/acme.inc" silently extracted
// "acme" — a truncated, wrong slug — instead of correctly not matching
// at all; a URL this project can't confidently parse must produce no
// candidate, never a guessed one. There is no www. variant on any of
// these hosts for real, so none of the patterns match one — a bare
// "https://www.boards.greenhouse.io/..." previously matched anyway,
// which would have meant treating a nonexistent host as valid.
var knownProviders = []providerPattern{
	{"greenhouse", regexp.MustCompile(`(?i)^https?://(?:boards|job-boards)\.greenhouse\.io/([a-zA-Z0-9_-]+)(?:[/?#]|$)`)},
	{"lever", regexp.MustCompile(`(?i)^https?://jobs\.lever\.co/([a-zA-Z0-9_-]+)(?:[/?#]|$)`)},
	{"ashby", regexp.MustCompile(`(?i)^https?://jobs\.ashbyhq\.com/([a-zA-Z0-9_-]+)(?:[/?#]|$)`)},
}

// greenhouseReservedSlugs are path segments Greenhouse itself uses in
// its URL structure, never a real company's board identifier — found by
// adversarial review: boards.greenhouse.io/embed/job_board?for=<company>
// is Greenhouse's embeddable-widget URL format, and it is routinely
// indexed by search engines. The pattern above matches it and would
// extract "embed" as if it were a board slug. Since target_companies is
// unique on (ats_provider, external_board_id), a SECOND company whose
// search result also contains an embed URL would collide on that same
// fake "embed" slug — internal/company's TargetStore.Upsert now refuses
// that reassignment outright (see its own comment), so this can no
// longer corrupt data, but it would still surface as a confusing
// "already registered to a different company" error instead of the
// clean "no board found" this project can give instead. The real board
// identifier in an embed URL lives in its `for` query parameter, not in
// this path position; parsing that isn't implemented since it hasn't
// been verified against a real Greenhouse embed URL — rejecting it
// outright (falling through to the next result or provider, same as
// any other non-match) is the conservative choice: a missed candidate,
// never a wrong one.
var greenhouseReservedSlugs = map[string]bool{
	"embed": true,
}

// buildSearchQuery constructs the query CandidatesFromSearch sends for
// one company: the name, quoted so the search engine treats it as a
// phrase rather than separate keywords, restricted via `site:` to hosts
// this project can recognize a board on. That restriction narrows what
// kind of result comes back, but does not by itself guarantee every hit
// matches a known pattern — irrelevant results (a company's LinkedIn or
// Wikipedia page) can and do come back too, which is exactly why
// firstKnownBoard below still checks every result against every pattern
// rather than trusting the first one.
//
// Quotes and newlines are stripped from companyName before it's
// embedded: an operator-supplied name containing a literal `"` could
// otherwise break out of the quoted phrase and inject its own `OR`/
// `site:` terms into the query (found by adversarial review — low
// severity, since names come from an operator-controlled file, not
// untrusted input, but cheap to close regardless).
func buildSearchQuery(companyName string) string {
	sanitized := strings.NewReplacer(`"`, "", "\n", " ", "\r", " ").Replace(companyName)
	return fmt.Sprintf(
		`"%s" (site:boards.greenhouse.io OR site:job-boards.greenhouse.io OR site:jobs.lever.co OR site:jobs.ashbyhq.com)`,
		sanitized,
	)
}

// ErrNoBoardFound is returned (per company, not for the whole batch)
// when a search returned results but none matched a known ATS URL
// pattern — the company may not use one of the three known providers,
// may not have a public board at all, or the search simply didn't
// surface it in the top 10 results.
var ErrNoBoardFound = errors.New("discovery: no known ATS board found in search results")

// CandidatesFromSearch turns a list of company names into discovery
// Candidates by searching for each one and recognizing a known ATS
// board URL in the results. It does not probe or persist anything
// itself — the returned Candidates are meant to be passed to
// Discoverer.Run, which does the HEAD/GET validation and the actual
// company/target persistence, exactly as it does for seed-file
// candidates. This keeps "how a candidate was found" (seed file, or
// search) fully decoupled from "what happens once we have one".
//
// One company's search failing (credits exhausted, no board found, a
// network error) does not stop the rest — every name is attempted, and
// every failure is collected and returned alongside whatever candidates
// were found. Searches run sequentially, not concurrently: Serper's free
// tier is a fixed pool of query credits, not a throughput problem worth
// a worker pool over — bursting it in parallel buys nothing but a
// faster trip to an exhausted account.
func CandidatesFromSearch(ctx context.Context, client SearchClient, companyNames []string) ([]Candidate, []error) {
	var candidates []Candidate
	var errs []error

	for _, name := range companyNames {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("discovery: search: %q: %w", name, err))
			continue
		}

		results, err := client.Search(ctx, buildSearchQuery(name))
		if err != nil {
			errs = append(errs, fmt.Errorf("discovery: search: %q: %w", name, err))
			continue
		}

		candidate, ok := firstKnownBoard(name, results)
		if !ok {
			errs = append(errs, fmt.Errorf("discovery: search: %q: %w", name, ErrNoBoardFound))
			continue
		}
		candidates = append(candidates, candidate)
	}

	return candidates, errs
}

// firstKnownBoard returns the first search result whose URL matches a
// known ATS provider's pattern AND isn't a reserved, non-company path
// (see greenhouseReservedSlugs) — "first" meaning highest-ranked in the
// search results, which is what a human skimming the same results would
// also click first.
func firstKnownBoard(companyName string, results []search.Result) (Candidate, bool) {
	for _, r := range results {
		for _, p := range knownProviders {
			m := p.pattern.FindStringSubmatch(r.URL)
			if m == nil {
				continue
			}
			slug := m[1]
			if p.provider == "greenhouse" && greenhouseReservedSlugs[strings.ToLower(slug)] {
				continue
			}
			return Candidate{
				CompanyName:     companyName,
				ATSProvider:     p.provider,
				ExternalBoardID: slug,
				BoardURL:        r.URL,
			}, true
		}
	}
	return Candidate{}, false
}
