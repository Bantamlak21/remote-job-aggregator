package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
)

// fakeSearchClient lets CandidatesFromSearch's own logic (result
// parsing, error collection, per-company independence) be tested without
// a real Google API key.
type fakeSearchClient struct {
	mu       sync.Mutex
	queries  []string
	byQuery  map[string][]search.Result
	errQuery map[string]error
}

func newFakeSearchClient() *fakeSearchClient {
	return &fakeSearchClient{byQuery: map[string][]search.Result{}, errQuery: map[string]error{}}
}

func (f *fakeSearchClient) Search(_ context.Context, query string) ([]search.Result, error) {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()

	if err, ok := f.errQuery[query]; ok {
		return nil, err
	}
	return f.byQuery[query], nil
}

func TestBuildSearchQuery(t *testing.T) {
	got := buildSearchQuery("Acme")
	want := `"Acme" (site:boards.greenhouse.io OR site:job-boards.greenhouse.io OR site:jobs.lever.co OR site:jobs.ashbyhq.com)`
	if got != want {
		t.Errorf("buildSearchQuery(%q) = %q, want %q", "Acme", got, want)
	}
}

func TestCandidatesFromSearch_FindsKnownGreenhouseBoard(t *testing.T) {
	client := newFakeSearchClient()
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{
		{Title: "Acme Careers", URL: "https://boards.greenhouse.io/acme", Snippet: "Join Acme"},
	}

	candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Acme"})

	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	c := candidates[0]
	if c.CompanyName != "Acme" || c.ATSProvider != "greenhouse" || c.ExternalBoardID != "acme" || c.BoardURL != "https://boards.greenhouse.io/acme" {
		t.Errorf("candidate = %+v, unexpected fields", c)
	}
}

func TestCandidatesFromSearch_RecognizesAllThreeProviders(t *testing.T) {
	cases := []struct {
		url      string
		provider string
		boardID  string
	}{
		{"https://boards.greenhouse.io/acme", "greenhouse", "acme"},
		{"https://job-boards.greenhouse.io/acme", "greenhouse", "acme"},
		{"https://jobs.lever.co/acme", "lever", "acme"},
		{"https://jobs.ashbyhq.com/acme", "ashby", "acme"},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			client := newFakeSearchClient()
			client.byQuery[buildSearchQuery("Acme")] = []search.Result{{URL: tc.url}}

			candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Acme"})
			if len(errs) != 0 || len(candidates) != 1 {
				t.Fatalf("candidates=%v errs=%v, want exactly one candidate", candidates, errs)
			}
			if candidates[0].ATSProvider != tc.provider || candidates[0].ExternalBoardID != tc.boardID {
				t.Errorf("candidate = %+v, want provider=%q boardID=%q", candidates[0], tc.provider, tc.boardID)
			}
		})
	}
}

// Regression test for the cross-company data-corruption bug found by
// adversarial review: a Greenhouse embed-widget URL
// (boards.greenhouse.io/embed/job_board?for=<company>) must never be
// treated as if "embed" were a real board slug — two different
// companies whose search results both surface an embed URL would
// otherwise collide on that same fake slug. The real board identifier
// lives in the URL's `for` query parameter, which this package does not
// parse (unverified against a live example); rejecting the match
// outright is the conservative choice over guessing.
func TestCandidatesFromSearch_RejectsGreenhouseEmbedURLAsASlug(t *testing.T) {
	client := newFakeSearchClient()
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{
		{URL: "https://boards.greenhouse.io/embed/job_board?for=acme"},
	}

	candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Acme"})
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want none: an embed URL must never be treated as a real board slug", candidates)
	}
	if len(errs) != 1 || !errors.Is(errs[0], ErrNoBoardFound) {
		t.Fatalf("errs = %v, want exactly one error matching ErrNoBoardFound", errs)
	}
}

// If the embed URL isn't the only result, a genuine board URL elsewhere
// in the results must still be found — rejecting embed URLs shouldn't
// also reject the rest of a company's results.
func TestCandidatesFromSearch_FindsRealBoardEvenWhenAnEmbedURLIsAlsoPresent(t *testing.T) {
	client := newFakeSearchClient()
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{
		{URL: "https://boards.greenhouse.io/embed/job_board?for=acme"},
		{URL: "https://boards.greenhouse.io/acme"},
	}

	candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Acme"})
	if len(errs) != 0 || len(candidates) != 1 {
		t.Fatalf("candidates=%v errs=%v, want exactly one real candidate", candidates, errs)
	}
	if candidates[0].ExternalBoardID != "acme" {
		t.Errorf("ExternalBoardID = %q, want %q", candidates[0].ExternalBoardID, "acme")
	}
}

func TestCandidatesFromSearch_HostMatchingIsCaseInsensitive(t *testing.T) {
	client := newFakeSearchClient()
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{
		{URL: "HTTPS://BOARDS.GREENHOUSE.IO/acme"},
	}

	candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Acme"})
	if len(errs) != 0 || len(candidates) != 1 {
		t.Fatalf("candidates=%v errs=%v, want exactly one candidate: hosts are case-insensitive", candidates, errs)
	}
	if candidates[0].ATSProvider != "greenhouse" {
		t.Errorf("ATSProvider = %q, want %q", candidates[0].ATSProvider, "greenhouse")
	}
}

// Regression test for silent slug truncation: before the fix,
// "acme.inc" matched the character class up to the "." and returned the
// wrong slug "acme" instead of correctly not matching at all. A URL
// this package can't confidently parse must produce no candidate, never
// a truncated, guessed one.
func TestCandidatesFromSearch_RejectsSlugFollowedByAnUnexpectedCharacter(t *testing.T) {
	client := newFakeSearchClient()
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{
		{URL: "https://boards.greenhouse.io/acme.inc"},
	}

	candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Acme"})
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want none: \"acme.inc\" must not silently truncate to \"acme\"", candidates)
	}
	if len(errs) != 1 || !errors.Is(errs[0], ErrNoBoardFound) {
		t.Fatalf("errs = %v, want exactly one error matching ErrNoBoardFound", errs)
	}
}

// There is no real www. subdomain for any of these three ATS hosts;
// matching one would mean treating a nonexistent host as valid.
func TestCandidatesFromSearch_RejectsNonexistentWWWSubdomain(t *testing.T) {
	client := newFakeSearchClient()
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{
		{URL: "https://www.boards.greenhouse.io/acme"},
	}

	candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Acme"})
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want none: www.boards.greenhouse.io is not a real Greenhouse host", candidates)
	}
	if len(errs) != 1 || !errors.Is(errs[0], ErrNoBoardFound) {
		t.Fatalf("errs = %v, want exactly one error matching ErrNoBoardFound", errs)
	}
}

func TestBuildSearchQuery_StripsQuotesAndNewlinesFromCompanyName(t *testing.T) {
	got := buildSearchQuery(`Acme" OR site:evil.com "`)
	if strings.Contains(got, `Acme" OR site:evil.com "`) {
		t.Errorf("buildSearchQuery(...) = %q, want the embedded quote stripped so it can't break out of the phrase", got)
	}
	if strings.Count(got, `"`) != 2 {
		t.Errorf("buildSearchQuery(...) = %q, want exactly the two quotes buildSearchQuery itself adds, none from the company name", got)
	}

	gotNewline := buildSearchQuery("Acme\nsite:evil.com")
	if strings.Contains(gotNewline, "\n") {
		t.Errorf("buildSearchQuery(...) = %q, want no raw newline in the query string", gotNewline)
	}
}

func TestCandidatesFromSearch_SkipsNonMatchingResultsToFindAMatch(t *testing.T) {
	client := newFakeSearchClient()
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{
		{Title: "Acme on LinkedIn", URL: "https://linkedin.com/company/acme"},
		{Title: "Acme Wikipedia", URL: "https://en.wikipedia.org/wiki/Acme"},
		{Title: "Acme Careers", URL: "https://boards.greenhouse.io/acme"},
	}

	candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Acme"})
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(candidates) != 1 || candidates[0].ATSProvider != "greenhouse" {
		t.Fatalf("candidates = %+v, want exactly one greenhouse candidate", candidates)
	}
}

func TestCandidatesFromSearch_NoMatchReturnsErrNoBoardFound(t *testing.T) {
	client := newFakeSearchClient()
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{
		{Title: "Acme on LinkedIn", URL: "https://linkedin.com/company/acme"},
	}

	candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Acme"})
	if len(candidates) != 0 {
		t.Errorf("candidates = %v, want none", candidates)
	}
	if len(errs) != 1 || !errors.Is(errs[0], ErrNoBoardFound) {
		t.Fatalf("errs = %v, want exactly one error matching ErrNoBoardFound", errs)
	}
}

func TestCandidatesFromSearch_OneCompanysSearchErrorDoesNotStopOthers(t *testing.T) {
	client := newFakeSearchClient()
	client.errQuery[buildSearchQuery("Broken")] = errors.New("quota exceeded")
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{
		{URL: "https://boards.greenhouse.io/acme"},
	}

	candidates, errs := CandidatesFromSearch(context.Background(), client, []string{"Broken", "Acme"})

	if len(candidates) != 1 || candidates[0].CompanyName != "Acme" {
		t.Fatalf("candidates = %+v, want Acme's candidate despite Broken's search failing", candidates)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "Broken") {
		t.Fatalf("errs = %v, want exactly one error naming Broken", errs)
	}
}

func TestCandidatesFromSearch_EachCompanyGetsItsOwnQuery(t *testing.T) {
	client := newFakeSearchClient()

	_, _ = CandidatesFromSearch(context.Background(), client, []string{"Acme", "Globex", "Initech"})

	want := []string{buildSearchQuery("Acme"), buildSearchQuery("Globex"), buildSearchQuery("Initech")}
	if fmt.Sprint(client.queries) != fmt.Sprint(want) {
		t.Errorf("queries = %v, want %v", client.queries, want)
	}
}

func TestCandidatesFromSearch_AlreadyCanceledContextFailsRemainingCompanies(t *testing.T) {
	client := newFakeSearchClient()
	client.byQuery[buildSearchQuery("Acme")] = []search.Result{{URL: "https://boards.greenhouse.io/acme"}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	candidates, errs := CandidatesFromSearch(ctx, client, []string{"Acme", "Globex"})

	if len(candidates) != 0 {
		t.Errorf("candidates = %v, want none for an already-canceled context", candidates)
	}
	if len(errs) != 2 {
		t.Fatalf("errs = %v, want one error per company", errs)
	}
	if len(client.queries) != 0 {
		t.Errorf("queries = %v, want none sent: an already-canceled context must be checked before searching, not after", client.queries)
	}
}
