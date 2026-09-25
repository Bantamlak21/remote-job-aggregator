package discovery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// fakeATS serves the three ATS APIs from one httptest server: /gh, /lever and
// /ashby prefixes, answering from canned bodies keyed by "prefix/slug[/jobs]".
type fakeATS struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	bodies   map[string]string // "gh/gitlab" -> JSON; missing key -> 404
}

func newFakeATS(t *testing.T, bodies map[string]string) (*Guesser, *fakeATS) {
	t.Helper()
	f := &fakeATS{bodies: bodies}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.Path)
		f.mu.Unlock()
		if body, ok := f.bodies[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.Close)
	g := NewGuesser(httpclient.New(httpclient.DefaultConfig()), 3, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.bases = boardBases{greenhouse: f.URL + "/gh", lever: f.URL + "/lever", ashby: f.URL + "/ashby"}
	return g, f
}

func (f *fakeATS) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func boards(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.CompanyName+" "+c.ATSProvider+"/"+c.ExternalBoardID+" "+c.BoardURL)
	}
	return out
}

func TestGuess_FindsAVerifiedGreenhouseBoardAndSkipsOnesOwnedByAnotherCompany(t *testing.T) {
	g, _ := newFakeATS(t, map[string]string{
		"gh/gitlab":        `{"name":"GitLab","content":""}`,
		"gh/gitlab/jobs":   `{"jobs":[{"id":1}]}`,
		"gh/intercom":      `{"name":"Fin","content":""}`, // a real board, but named for someone else
		"gh/intercom/jobs": `{"jobs":[{"id":1}]}`,
		"gh/acme":          `{"name":"Acme Inc","content":""}`,
		"gh/acme/jobs":     `{"jobs":[]}`, // owned, but nothing to ingest
	})
	got, report := g.Guess(context.Background(), []string{"GitLab", "Intercom", "Acme", "Nobody"})

	if want := []string{"GitLab greenhouse/gitlab https://boards.greenhouse.io/gitlab"}; !slices.Equal(boards(got), want) {
		t.Errorf("candidates = %v, want %v", boards(got), want)
	}
	if !slices.Equal(report.NotFound, []string{"Acme", "Nobody"}) {
		t.Errorf("NotFound = %v, want [Acme Nobody] (Acme's board is empty)", report.NotFound)
	}
	if !slices.Equal(report.Unconfirmed, []string{"Intercom greenhouse/intercom"}) {
		t.Errorf("Unconfirmed = %v, want the Intercom board reported and not registered", report.Unconfirmed)
	}
	if report.Names != 4 || report.Found != 1 || report.Boards != 1 {
		t.Errorf("report = %+v", report)
	}
}

func TestGuess_LeverAndAshbyNeedTheCompanyNameInTheirJobsText(t *testing.T) {
	g, _ := newFakeATS(t, map[string]string{
		// Toptal's board: every posting names Toptal.
		"lever/toptal": `[{"text":"Engineer","descriptionPlain":"Join Toptal today","additionalPlain":""},{"text":"Designer","descriptionPlain":"Toptal is hiring","additionalPlain":""}]`,
		// "Signal" is a common word: this board is some other company's.
		"lever/signal": `[{"text":"Analyst","descriptionPlain":"We watch the markets","additionalPlain":""},{"text":"Trader","descriptionPlain":"Fast desk","additionalPlain":""}]`,
		// Linear's Ashby board, one unlisted job that must not count.
		"ashby/linear": `{"jobs":[{"title":"Engineer","descriptionPlain":"Linear builds software","isListed":true},{"title":"Hidden","descriptionPlain":"secret","isListed":false}]}`,
		// An Ashby board with no jobs cannot prove anything.
		"ashby/deel": `{"jobs":[]}`,
	})
	got, report := g.Guess(context.Background(), []string{"Toptal", "Signal", "Linear", "Deel"})

	want := []string{
		"Toptal lever/toptal https://jobs.lever.co/toptal",
		"Linear ashby/linear https://jobs.ashbyhq.com/linear",
	}
	if !slices.Equal(boards(got), want) {
		t.Errorf("candidates = %v, want %v", boards(got), want)
	}
	if !slices.Equal(report.Unconfirmed, []string{"Signal lever/signal"}) {
		t.Errorf("Unconfirmed = %v, want the Signal board", report.Unconfirmed)
	}
	if !slices.Equal(report.NotFound, []string{"Deel"}) {
		t.Errorf("NotFound = %v, want Deel (an empty board)", report.NotFound)
	}
}

// An unlisted job is not on the public board: it must not vouch for the owner.
func TestGuess_UnlistedAshbyJobsDoNotCountTowardsOwnership(t *testing.T) {
	g, _ := newFakeATS(t, map[string]string{
		"ashby/close": `{"jobs":[{"title":"Sales","descriptionPlain":"We sell things","isListed":true},{"title":"Close CRM engineer","descriptionPlain":"Close is hiring","isListed":false}]}`,
	})
	got, report := g.Guess(context.Background(), []string{"Close"})
	if len(got) != 0 || !slices.Equal(report.Unconfirmed, []string{"Close ashby/close"}) {
		t.Errorf("candidates = %v, unconfirmed = %v; want the board refused (only the unlisted job names the company)", boards(got), report.Unconfirmed)
	}
}

func TestGuess_ACompanyOnTwoATSsGetsBothBoards_AndTheDashedSlugIsTried(t *testing.T) {
	g, _ := newFakeATS(t, map[string]string{
		"lever/neon":           `[{"text":"Engineer","descriptionPlain":"Neon builds Postgres","additionalPlain":""}]`,
		"ashby/neon":           `{"jobs":[{"title":"Engineer","descriptionPlain":"Neon builds Postgres","isListed":true}]}`,
		"ashby/apollo-graphql": `{"jobs":[{"title":"Engineer","descriptionPlain":"Apollo GraphQL is hiring","isListed":true}]}`,
	})
	got, _ := g.Guess(context.Background(), []string{"Neon", "Apollo GraphQL"})
	want := []string{
		"Neon lever/neon https://jobs.lever.co/neon",
		"Neon ashby/neon https://jobs.ashbyhq.com/neon",
		"Apollo GraphQL ashby/apollo-graphql https://jobs.ashbyhq.com/apollo-graphql",
	}
	if !slices.Equal(boards(got), want) {
		t.Errorf("candidates = %v, want %v", boards(got), want)
	}
}

func TestGuess_ResultsFollowTheOrderOfTheNamesWhateverTheWorkersFinishFirst(t *testing.T) {
	bodies := map[string]string{}
	var names []string
	for i := 0; i < 12; i++ {
		n := fmt.Sprintf("Company%d", i)
		names = append(names, n)
		bodies[fmt.Sprintf("ashby/company%d", i)] = fmt.Sprintf(`{"jobs":[{"title":"T","descriptionPlain":"%s is hiring","isListed":true}]}`, n)
	}
	g, _ := newFakeATS(t, bodies)
	got, _ := g.Guess(context.Background(), names)
	var order []string
	for _, c := range got {
		order = append(order, c.CompanyName)
	}
	if !slices.Equal(order, names) {
		t.Errorf("order = %v, want %v", order, names)
	}
}

func TestGuess_EachMissingBoardCostsOneRequestPerProviderAndSlugAndNothingIsRetried(t *testing.T) {
	g, f := newFakeATS(t, map[string]string{})
	_, report := g.Guess(context.Background(), []string{"Grafana Labs"}) // slugs: grafanalabs, grafana-labs, grafana
	if len(report.NotFound) != 1 {
		t.Fatalf("report = %+v", report)
	}
	slugs := slugVariants("Grafana Labs")
	if want := len(slugs) * 3; f.count() != want {
		t.Errorf("%d requests, want %d (%d slugs x 3 ATSs, one each: a 404 is not retried)", f.count(), want, len(slugs))
	}
}

func TestGuess_ACanceledContextStopsWithoutRequests(t *testing.T) {
	g, f := newFakeATS(t, map[string]string{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, _ := g.Guess(ctx, []string{"A", "B", "C"})
	if len(got) != 0 || f.count() != 0 {
		t.Errorf("candidates %d, requests %d; want none", len(got), f.count())
	}
}

func TestSlugVariants(t *testing.T) {
	for name, want := range map[string][]string{
		"GitLab":           {"gitlab"},
		"Grafana Labs":     {"grafanalabs", "grafana-labs"},
		"Acme Inc":         {"acmeinc", "acme-inc", "acme"},
		"Remote.com":       {"remote"},
		"Help Scout":       {"helpscout", "help-scout"},
		"1Password":        {"1password"},
		"Weights & Biases": {"weightsbiases", "weights-biases"},
		"":                 nil,
		"!!!":              nil,
	} {
		if got := slugVariants(name); !slices.Equal(got, want) {
			t.Errorf("slugVariants(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNamesMatchAndOwnedByName(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"GitLab", "GitLab", true}, {"Acme Inc", "Acme", true}, {"Grafana", "Grafana Labs", true},
		{"Fivetran", "Fivetran ", true}, {"Intercom", "Fin", false}, {"Remote", "General Assembly Remote Jobs", false},
		{"Aha", "Animal Health Associates", false}, {"Honeycomb", "Honeycomb.io", true}, {"Remote.com", "Remote", true},
		{"Remote", "General Assembly Remote Jobs", false}, {"", "x", false}, {"x", "", false},
	} {
		if got := namesMatch(tc.a, tc.b); got != tc.want {
			t.Errorf("namesMatch(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
	texts := func(hits, total int) []string {
		var out []string
		for i := 0; i < total; i++ {
			if i < hits {
				out = append(out, "Join Acme Corp today")
			} else {
				out = append(out, "Something unrelated")
			}
		}
		return out
	}
	for _, tc := range []struct {
		hits, total int
		want        bool
	}{{1, 1, true}, {0, 1, false}, {1, 2, true}, {1, 3, false}, {2, 3, true}, {4, 8, true}, {3, 8, false}, {0, 0, false}} {
		if got := ownedByName("Acme Corp", texts(tc.hits, tc.total)); got != tc.want {
			t.Errorf("ownedByName with %d of %d mentioning = %t, want %t", tc.hits, tc.total, got, tc.want)
		}
	}
	if !ownedByName("Remote.com", []string{"Remote is hiring"}) {
		t.Errorf("a .com in the company name must not stop its text matching")
	}
}
