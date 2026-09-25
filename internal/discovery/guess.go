package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/companymatch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// A company name is enough to find its job board on the three big public
// ATSs: their boards are addressed by a slug that is nearly always the
// company's name, and each answers 404 for a board that does not exist. The
// danger is a different company using the slug ("wise", "ghost", "neon"), and
// on the first live run 6 of 178 registered boards were exactly that. So a
// board is registered only on positive proof, and ambiguity is refused rather
// than guessed:
//
//   - A domain given in the names file ("Ramp | ramp.com") that appears in the
//     board's job text is proof: the strongest signal, and the only one that
//     works for a name that is also an everyday word.
//   - Otherwise Greenhouse must name the board exactly as the company is
//     named, and Lever and Ashby must show the company's name, as a whole word
//     in the case it is written, in at least half of the first eight listed
//     jobs, on a board of at least two jobs.
//   - A name that is an everyday word ("Close", "Wise", "Neon") is never
//     accepted on name alone: it needs a domain proof.
//   - A name that verifies on more than one board is refused as ambiguous,
//     unless every one of those boards is domain-proven.
//   - For an employer a job board showed (Entry.Titles are its listed job
//     titles), a board proven only by name must also list at least one of those
//     titles: a same-named company's board does not.
//
// Everything refused is reported so a person can decide.

const (
	// identitySample is how many of a board's listed jobs are read to confirm
	// who owns it (Lever and Ashby).
	identitySample  = 8
	maxSlugVariants = 4
	// maxProbeErrors is how many failed probes (a rate limit, a network or
	// server error) one run tolerates before it stops: past that it is being
	// blocked, and every further probe would look like "not found".
	maxProbeErrors = 25
	// maxBoardBytes bounds what one probe may read from one board. Ashby has
	// no way to ask for fewer jobs and its largest boards are a few MB; the
	// probe decodes as a stream and stops after identitySample jobs, so this is
	// only a backstop.
	maxBoardBytes = 32 << 20
)

// Entry is one company to look up: its name and, optionally, its web domain.
type Entry struct {
	Name   string
	Domain string // "ramp.com"; "" when not known
	// Titles are the job titles a job board lists for this employer, when the
	// entry came from one. A board proven only by name must then share a title
	// with them.
	Titles []string
}

// ParseEntry reads a names-file line: "Name" or "Name | domain.tld". The
// domain is lower-cased and stripped of a scheme, "www." and any path; a
// domain that is not one (no dot, or spaces) is ignored.
func ParseEntry(line string) Entry {
	name, domain, _ := strings.Cut(line, "|")
	e := Entry{Name: strings.Join(strings.Fields(name), " ")}
	d := strings.ToLower(strings.TrimSpace(domain))
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	d = strings.TrimPrefix(d, "www.")
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	if strings.Contains(d, ".") && !strings.ContainsAny(d, " \t") {
		e.Domain = d
	}
	return e
}

// boardBases are the API roots the guesser probes; tests replace them.
type boardBases struct {
	greenhouse, lever, ashby string
}

var defaultBases = boardBases{
	greenhouse: "https://boards-api.greenhouse.io/v1/boards",
	lever:      "https://api.lever.co/v0/postings",
	ashby:      "https://api.ashbyhq.com/posting-api/job-board",
}

// GuessReport says what one Guess run found and what it refused.
type GuessReport struct {
	Names       int
	Found       int       // names with at least one verified board
	Boards      int       // verified boards
	NotFound    []string  // names with no usable board on any of the three ATSs
	Refused     []Refusal // boards that exist but do not show they are the company's
	Ambiguous   []string  // names that verified on more than one board and were refused
	NeedsDomain []string  // everyday-word names with no domain given: not looked up
	Failed      []Failure // probes that failed, so the name was not fully checked
	Unreached   []string  // names not reached because the run was canceled or aborted
	Aborted     bool      // too many probes failed; the run stopped early
}

// RefusalKind says how strong the case against a refused board is.
type RefusalKind int

const (
	// Unproven: the board does not show it is the company's. Not evidence that
	// it is another's (a company's jobs often never say its name).
	Unproven RefusalKind = iota
	// NoSharedTitle: the board names the company but lists none of the jobs a
	// job board showed for it. Weak: those may be few, or long filled.
	NoSharedTitle
	// NamedForAnother: the board says, in its own name, that it is another
	// company's ("Fin" for Intercom, "Acme Rocket Co" for Acme). Positive
	// evidence, the only kind a re-check may deactivate a board on.
	NamedForAnother
)

// Refusal is a board that exists under a company's slug but is not registered
// as that company's.
type Refusal struct {
	Name, Provider, Slug string
	Kind                 RefusalKind
	Reason               string
}

func (r Refusal) String() string {
	return fmt.Sprintf("%s %s/%s (%s)", r.Name, r.Provider, r.Slug, r.Reason)
}

// Failure is a probe that could not be completed.
type Failure struct {
	Name, Provider, Slug string
	Err                  string
}

func (f Failure) String() string {
	return fmt.Sprintf("%s %s/%s: %s", f.Name, f.Provider, f.Slug, f.Err)
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

func hasNonASCIILetter(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII && unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// slugVariants lists the board slugs worth trying for a company name, most
// likely first: the name run together with its dots dropped ("customerio"),
// dash-joined ("customer-io"), then without a trailing corporate word
// ("Acme Inc" also tries "acme"), and finally, for a name written with a web
// address ending, without it ("Customer.io" also tries "customer"; such a
// slug is only ever accepted with the domain proven). A name with non-ASCII
// letters has no reliable slug and gets none.
func slugVariants(name string) []string {
	if hasNonASCIILetter(name) {
		return nil
	}
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
	dash := companymatch.Slug(strings.ReplaceAll(name, ".", ""))
	add(strings.ReplaceAll(dash, "-", ""))
	add(dash)
	key := companymatch.Key(name)
	add(strings.ReplaceAll(key, "-", ""))
	add(key)
	if stripped := stripDomain(name); stripped != strings.TrimSpace(name) {
		s := companymatch.Slug(stripped)
		add(strings.ReplaceAll(s, "-", ""))
		add(s)
	}
	return out
}

var domainEndings = []string{".com", ".io", ".ai", ".dev", ".co", ".app", ".net", ".org", ".so", ".sh"}

// stripDomain drops a trailing web-address ending ("Honeycomb.io" is
// "Honeycomb"): a board may be named either way.
func stripDomain(name string) string {
	n := strings.TrimSpace(name)
	l := strings.ToLower(n)
	for _, e := range domainEndings {
		if strings.HasSuffix(l, e) && len(n) > len(e) {
			return strings.TrimSpace(n[:len(n)-len(e)])
		}
	}
	return n
}

// namesMatch reports whether a Greenhouse board's own name is the company's:
// equal once corporate suffixes are dropped ("Acme Inc" is "Acme"). A web
// address ending is dropped from the board's name only ("Honeycomb.io" is
// "Honeycomb"), never from ours: "Customer.io" is not a board named
// "Customer".
func namesMatch(ours, board string) bool {
	ko := companymatch.Key(ours)
	if ko == "" {
		return false
	}
	if ko == companymatch.Key(board) {
		return true
	}
	return stripDomain(ours) == strings.TrimSpace(ours) && ko == companymatch.Key(stripDomain(board))
}

// commonWords are everyday words that are also company names on the boards
// this looks up. A name made only of them cannot be told from another
// company's by name, so it is never accepted without a domain.
var commonWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
		arc axiom boundary bubble calm census close coda cursor daily element expo front gem ghost glitch
		guru harvest heap hex hired hopin jam kit knock loom loops matrix medium mercury modal neon
		nomad orb oxide pitch polar pulse railway relay remote render resend rive sanity segment signal split terminal
		temporal vault warp wise`) {
		commonWords[w] = true
	}
}

// isCommonName reports whether every word of the name is an everyday word.
func isCommonName(name string) bool {
	toks := tokens(stripDomain(name))
	if len(toks) == 0 {
		return false
	}
	for _, t := range toks {
		if !commonWords[strings.ToLower(t)] {
			return false
		}
	}
	return true
}

// tokens splits text into words of letters and digits (any script).
func tokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

var legalForms = map[string]bool{"inc": true, "llc": true, "ltd": true, "plc": true, "gmbh": true, "corp": true, "corporation": true, "co": true, "sa": true, "ag": true, "bv": true}

// nameTokens is the company name as words with legal-form endings dropped.
func nameTokens(name string) []string {
	toks := tokens(name)
	for len(toks) > 1 && legalForms[strings.ToLower(toks[len(toks)-1])] {
		toks = toks[:len(toks)-1]
	}
	return toks
}

// mentions reports whether text contains the company name as a run of whole
// words. A name written with a capital only at its start ("Ramp", "Kit") is
// matched in that case, so a lower-case "ramp up" is not the company; a name
// with capitals inside ("FullStory", "GitLab") or all in one case matches any
// case, since such names are rarely everyday words and are written both ways.
// "Ramp" is not in "Trampoline" or "Program Partner", and "Kit" is not in
// "Kitchen".
func mentions(text string, name []string) bool {
	if len(name) == 0 {
		return false
	}
	fold := false
	for _, t := range name {
		rs := []rune(t)
		for i, r := range rs {
			if i > 0 && unicode.IsUpper(r) {
				fold = true // a capital inside the word
			}
		}
		if t == strings.ToLower(t) || t == strings.ToUpper(t) {
			fold = true
		}
	}
	words := tokens(text)
	eq := func(a, b string) bool {
		if fold {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	for i := 0; i+len(name) <= len(words); i++ {
		ok := true
		for j := range name {
			if !eq(words[i+j], name[j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// hasDomain reports whether text contains the domain with word boundaries
// ("ramp.com" is in "visit ramp.com/careers", not in "trampoline.company").
func hasDomain(text, domain string) bool {
	if domain == "" {
		return false
	}
	t := strings.ToLower(text)
	for from := 0; from < len(t); {
		i := strings.Index(t[from:], domain)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(domain)
		before := start == 0 || !isDomainChar(rune(t[start-1]))
		after := end == len(t) || !isDomainChar(rune(t[end]))
		if before && after {
			return true
		}
		from = end
	}
	return false
}

func isDomainChar(r rune) bool {
	return r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// domainProof reports whether any sampled text carries the entry's domain.
func domainProof(e Entry, texts []string) bool {
	for _, t := range texts {
		if hasDomain(t, e.Domain) {
			return true
		}
	}
	return false
}

// nameProof decides, from the name alone, whether the sampled job texts of a
// Lever or Ashby board show it is the company's. listed is how many jobs the
// board has (at least the sample).
func nameProof(e Entry, texts []string, listed int) bool {
	if len(texts) == 0 || isCommonName(e.Name) || listed < 2 {
		return false
	}
	name := nameTokens(e.Name)
	hits := 0
	for _, t := range texts {
		if mentions(t, name) {
			hits++
		}
	}
	return hits >= (len(texts)+1)/2
}

// sharesTitle reports whether any of the board's job titles is one of the
// entry's employer titles (compared as companymatch.TitleKey).
func sharesTitle(e Entry, boardTitles []string) bool {
	want := make(map[string]bool, len(e.Titles))
	for _, t := range e.Titles {
		if k := companymatch.TitleKey(t); k != "" {
			want[k] = true
		}
	}
	for _, t := range boardTitles {
		if want[companymatch.TitleKey(t)] {
			return true
		}
	}
	return false
}

// sample is what a probe read of a Lever or Ashby board.
type sample struct {
	texts  []string // job texts of the first identitySample listed jobs
	titles []string // job titles of the board's listed jobs (all of them when the entry has Titles)
	listed int      // listed jobs seen
}

// classify turns a sampled Lever or Ashby board into a verdict. (A name that
// lost its web-address ending needs no special case: its name tokens still
// include the ending, so the text must say "Customer.io".)
func classify(e Entry, s sample) verdict {
	switch {
	case s.listed == 0:
		return boardEmpty
	case domainProof(e, s.texts):
		return boardProven
	case nameProof(e, s.texts, s.listed):
		if len(e.Titles) > 0 && !sharesTitle(e, s.titles) {
			return boardUncorroborated
		}
		return boardOwned
	}
	return boardForeign
}

// outcome is what one entry's probes found.
type outcome struct {
	name        string
	done        bool // false: not completed (canceled, aborted or never started)
	boards      []Candidate
	allProven   bool // every board in boards carries a domain proof
	refused     []Refusal
	failed      []Failure
	needsDomain bool
	domain      string
}

// Guess probes every entry against Greenhouse, Lever and Ashby and returns a
// Candidate (Verified) for each verified board with at least one job. Entries
// are worked through by a small pool of workers. Cancellation, or too many
// failed probes, stops the run; the entries not completed are reported.
func (g *Guesser) Guess(ctx context.Context, entries []Entry) ([]Candidate, GuessReport) {
	var uniq []Entry
	seen := map[string]bool{}
	for _, e := range entries {
		k := strings.ToLower(strings.TrimSpace(e.Name))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		uniq = append(uniq, e)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var probeErrors atomic.Int64
	var aborted atomic.Bool
	results := make([]outcome, len(uniq))

	feed := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < g.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range feed {
				results[idx] = g.guessOne(ctx, uniq[idx], &probeErrors, &aborted, cancel)
			}
		}()
	}
loop:
	for i := range uniq {
		select {
		case feed <- i:
		case <-ctx.Done():
			break loop
		}
	}
	close(feed)
	wg.Wait()

	report := GuessReport{Names: len(uniq), Aborted: aborted.Load()}
	var candidates []Candidate
	for i, o := range results {
		if !o.done {
			report.Unreached = append(report.Unreached, uniq[i].Name)
			continue
		}
		report.Refused = append(report.Refused, o.refused...)
		report.Failed = append(report.Failed, o.failed...)
		switch {
		case o.needsDomain:
			report.NeedsDomain = append(report.NeedsDomain, o.name)
		case len(o.boards) > 1 && !o.allProven:
			// The same name verified on more than one board: two companies
			// share it, or one has moved: not something to guess.
			report.Ambiguous = append(report.Ambiguous, o.name)
		case len(o.boards) == 0:
			if len(o.refused) == 0 && len(o.failed) == 0 {
				report.NotFound = append(report.NotFound, o.name)
			}
		default:
			report.Found++
			report.Boards += len(o.boards)
			candidates = append(candidates, o.boards...)
		}
	}
	return candidates, report
}

func (g *Guesser) guessOne(ctx context.Context, e Entry, probeErrors *atomic.Int64, aborted *atomic.Bool, cancel context.CancelFunc) outcome {
	out := outcome{name: e.Name, domain: e.Domain, allProven: true}
	// A name that is an everyday word is never looked up without a domain.
	if e.Domain == "" && isCommonName(e.Name) {
		out.needsDomain, out.done = true, true
		return out
	}
	seen := map[string]bool{}
	for _, slug := range slugVariants(e.Name) {
		for _, provider := range []string{string(ats.ProviderGreenhouse), string(ats.ProviderLever), string(ats.ProviderAshby)} {
			if ctx.Err() != nil {
				return out // not done
			}
			key := provider + "/" + slug
			if seen[key] {
				continue
			}
			seen[key] = true
			var v verdict
			var err error
			switch provider {
			case "greenhouse":
				v, err = g.probeGreenhouse(ctx, e, slug)
			case "lever":
				v, err = g.probeLever(ctx, e, slug)
			case "ashby":
				v, err = g.probeAshby(ctx, e, slug)
			}
			if err != nil {
				if ctx.Err() != nil {
					return out // canceled mid-probe: not done, not a failure of the name
				}
				out.failed = append(out.failed, Failure{Name: e.Name, Provider: provider, Slug: slug, Err: err.Error()})
				if probeErrors.Add(1) >= maxProbeErrors && aborted.CompareAndSwap(false, true) {
					g.logger.Error("too many failed probes: the ATS APIs are refusing or failing; stopping", "failed_probes", maxProbeErrors)
					cancel()
				}
				continue
			}
			switch v {
			case boardOwned, boardProven:
				if v != boardProven {
					out.allProven = false
				}
				out.boards = append(out.boards, Candidate{CompanyName: e.Name, ATSProvider: provider, ExternalBoardID: slug,
					BoardURL: publicURL(provider, slug), Verified: true})
			case boardForeign:
				out.refused = append(out.refused, Refusal{e.Name, provider, slug, Unproven, "not shown to be the company's"})
			case boardNamedOther:
				out.refused = append(out.refused, Refusal{e.Name, provider, slug, NamedForAnother, "the board is named for another company"})
			case boardUncorroborated:
				out.refused = append(out.refused, Refusal{e.Name, provider, slug, NoSharedTitle, "lists none of the employer's job titles"})
			}
		}
	}
	out.done = true
	return out
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

// verdict is what one probe learned about a slug on one ATS.
type verdict int

const (
	boardNone           verdict = iota // no such board
	boardEmpty                         // the company's board, with no jobs to ingest
	boardForeign                       // a board with jobs whose owner is not shown to be the company
	boardNamedOther                    // a board that names itself as another company's
	boardUncorroborated                // a board that names the company but lists none of the employer's jobs
	boardOwned                         // the company's board, with jobs, by its name
	boardProven                        // the company's board, with jobs, proven by its domain
)

// errProbe marks a probe that could not be completed (as opposed to a 404): a
// rate limit, a server or network error, an unreadable body. The name is then
// not fully checked, and the run reports it instead of calling it "not found".
var errProbe = errors.New("probe failed")

// get fetches a URL. It returns the body (the caller closes it) for a 200,
// (nil, nil) for a 404, and an error for anything else.
func (g *Guesser) get(ctx context.Context, u string) (io.ReadCloser, error) {
	if g.pause > 0 {
		select {
		case <-time.After(g.pause):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	req, err := http.NewRequestWithContext(httpclient.WithoutRetries(ctx), http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errProbe, err)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errProbe, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, nil
	default:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: status %d", errProbe, resp.StatusCode)
	}
}

// decodeJSON decodes one JSON value from a 200 body, bounded.
func decodeJSON(body io.ReadCloser, v any) error {
	defer body.Close()
	if err := json.NewDecoder(io.LimitReader(body, maxBoardBytes)).Decode(v); err != nil {
		return fmt.Errorf("%w: %v", errProbe, err)
	}
	return nil
}

func (g *Guesser) probeGreenhouse(ctx context.Context, e Entry, slug string) (verdict, error) {
	body, err := g.get(ctx, fmt.Sprintf("%s/%s", g.bases.greenhouse, url.PathEscape(slug)))
	if err != nil || body == nil {
		return boardNone, err
	}
	var b struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(body, &b); err != nil {
		return boardNone, err
	}
	nameOK := namesMatch(e.Name, b.Name)
	if !nameOK && e.Domain == "" {
		return boardNamedOther, nil // a board exists under this slug, named for some other company
	}

	listBody, err := g.get(ctx, fmt.Sprintf("%s/%s/jobs", g.bases.greenhouse, url.PathEscape(slug)))
	if err != nil || listBody == nil {
		return boardNone, err
	}
	var list struct {
		Jobs []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"jobs"`
	}
	if err := decodeJSON(listBody, &list); err != nil {
		return boardNone, err
	}
	if len(list.Jobs) == 0 {
		if nameOK {
			return boardEmpty, nil
		}
		return boardNamedOther, nil
	}
	// A domain was given: the board's first job saying it is proof, whatever the
	// board calls itself (Greenhouse carries a job's text only in the job's own
	// record).
	if e.Domain != "" && list.Jobs[0].ID != 0 {
		oneBody, err := g.get(ctx, fmt.Sprintf("%s/%s/jobs/%d", g.bases.greenhouse, url.PathEscape(slug), list.Jobs[0].ID))
		if err != nil {
			return boardNone, err
		}
		if oneBody != nil {
			var one struct {
				Content string `json:"content"`
			}
			if err := decodeJSON(oneBody, &one); err != nil {
				return boardNone, err
			}
			if hasDomain(one.Content, e.Domain) || hasDomain(ats.HTMLToText(one.Content), e.Domain) {
				return boardProven, nil
			}
		}
	}
	// No domain proof: the exact board name is enough unless the name is an
	// everyday word.
	if !nameOK {
		return boardNamedOther, nil
	}
	if isCommonName(e.Name) {
		return boardForeign, nil
	}
	if len(e.Titles) > 0 {
		var titles []string
		for _, j := range list.Jobs {
			titles = append(titles, j.Title)
		}
		if !sharesTitle(e, titles) {
			return boardUncorroborated, nil
		}
	}
	return boardOwned, nil
}

func (g *Guesser) probeLever(ctx context.Context, e Entry, slug string) (verdict, error) {
	// Without employer titles to compare, the first postings are enough; with
	// them the whole board is read (Lever's list is modest).
	u := fmt.Sprintf("%s/%s?mode=json&limit=%d", g.bases.lever, url.PathEscape(slug), identitySample)
	if len(e.Titles) > 0 {
		u = fmt.Sprintf("%s/%s?mode=json", g.bases.lever, url.PathEscape(slug))
	}
	body, err := g.get(ctx, u)
	if err != nil || body == nil {
		return boardNone, err
	}
	defer body.Close()
	// A JSON array, decoded one posting at a time.
	dec := json.NewDecoder(io.LimitReader(body, maxBoardBytes))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return boardNone, fmt.Errorf("%w: not a JSON array", errProbe)
	}
	var s sample
	for dec.More() {
		if len(e.Titles) == 0 && s.listed >= identitySample {
			break
		}
		var p struct {
			Text             string `json:"text"`
			DescriptionPlain string `json:"descriptionPlain"`
			AdditionalPlain  string `json:"additionalPlain"`
		}
		if err := dec.Decode(&p); err != nil {
			return boardNone, fmt.Errorf("%w: %v", errProbe, err)
		}
		s.listed++
		s.titles = append(s.titles, p.Text)
		if len(s.texts) < identitySample {
			s.texts = append(s.texts, p.Text+" "+p.DescriptionPlain+" "+p.AdditionalPlain)
		}
	}
	return classify(e, s), nil
}

func (g *Guesser) probeAshby(ctx context.Context, e Entry, slug string) (verdict, error) {
	body, err := g.get(ctx, fmt.Sprintf("%s/%s", g.bases.ashby, url.PathEscape(slug)))
	if err != nil || body == nil {
		return boardNone, err
	}
	defer body.Close()
	// Ashby returns the whole board (a few MB for the big ones) and has no way
	// to ask for less, so the body is decoded as a stream: after the first
	// identitySample listed jobs the board is judged and the rest dropped,
	// unless the entry has employer titles to compare, which need every title.
	// Hitting the byte cap is an error, never "no such board".
	dec := json.NewDecoder(io.LimitReader(body, maxBoardBytes))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return boardNone, fmt.Errorf("%w: not a JSON object", errProbe)
	}
	var s sample
	sawJobs := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return boardNone, fmt.Errorf("%w: %v", errProbe, err)
		}
		if key, _ := keyTok.(string); key != "jobs" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return boardNone, fmt.Errorf("%w: %v", errProbe, err)
			}
			continue
		}
		sawJobs = true
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return boardNone, fmt.Errorf("%w: jobs is not an array", errProbe)
		}
		for dec.More() {
			var j struct {
				Title            string `json:"title"`
				DescriptionPlain string `json:"descriptionPlain"`
				IsListed         *bool  `json:"isListed"`
			}
			if err := dec.Decode(&j); err != nil {
				return boardNone, fmt.Errorf("%w: %v", errProbe, err)
			}
			if j.IsListed != nil && !*j.IsListed {
				continue
			}
			s.listed++
			s.titles = append(s.titles, j.Title)
			if len(s.texts) < identitySample {
				s.texts = append(s.texts, j.Title+" "+j.DescriptionPlain)
			}
			if len(e.Titles) == 0 && s.listed >= identitySample {
				return classify(e, s), nil // enough to judge; the rest is not read
			}
		}
		break
	}
	if !sawJobs {
		return boardNone, fmt.Errorf("%w: no jobs field", errProbe)
	}
	return classify(e, s), nil
}
