package jobsearch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
)

// ---- fake ----

type pageCall struct {
	query   string
	recency search.Recency
	page    int
}

// fakePages answers by (keyword, page); the keyword is the text after
// `"Ethiopia" ` in the query.
type fakePages struct {
	mu    sync.Mutex
	calls []pageCall
	data  map[string][][]search.Result // keyword -> pages
	errs  map[string]error             // keyword -> error for every call
	// failFirst makes the first N calls fail with a transient error.
	failFirst int
}

func (f *fakePages) SearchRecentPage(_ context.Context, q string, r search.Recency, page int) ([]search.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pageCall{q, r, page})
	if f.failFirst > 0 {
		f.failFirst--
		return nil, errors.New("stream error: PROTOCOL_ERROR")
	}
	_, kw, _ := strings.Cut(q, `"Ethiopia" `)
	if err := f.errs[kw]; err != nil {
		return nil, err
	}
	pages := f.data[kw]
	if page > len(pages) {
		return nil, nil
	}
	return pages[page-1], nil
}

func newFresh(s PageSearcher, budget int, pages int, keywords ...string) *FreshClient {
	c := NewFresh(s, FreshConfig{Keywords: keywords, Pages: pages}, NewBudget(budget), "search", discardLogger())
	c.now = func() time.Time { return fixedNow }
	c.retryDelay = 0
	return c
}

// hiring builds a result in LinkedIn's "<Employer> hiring <Title> in ..." shape.
func hiring(employerSlug, employer, titleSlug, title string, id int) search.Result {
	return li(fmt.Sprintf("https://et.linkedin.com/jobs/view/%s-at-%s-%d", titleSlug, employerSlug, id),
		fmt.Sprintf("%s hiring %s in Addis Ababa, Ethiopia | LinkedIn", employer, title),
		fmt.Sprintf("Apply for %s at %s in Addis Ababa, Ethiopia.", title, employer), "5 hours ago")
}

func jobIDs(jobs []ats.Job) []string {
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.ExternalID)
	}
	slices.Sort(ids)
	return ids
}

// ---- freshJob ----

// Result shapes below were captured live from Serper on 2026-09-24 (queries
// like `site:linkedin.com/jobs/view "Ethiopia" marketing`, last day), trimmed.
func TestFreshJob_AcceptsRealResultShapesAndNamesTheEmployerFromTheSlug(t *testing.T) {
	cases := []struct {
		name         string
		r            search.Result
		wantEmployer string
		wantTitle    string
		wantLocation string
	}{
		{"employer name ends in a pipe",
			li("https://et.linkedin.com/jobs/view/growth-partnership-head-at-ethiopian-business-review-ebr-4471129402",
				"Growth & Partnership Head at Ethiopian Business Review | EBR",
				"Apply for Growth & Partnership Head at Ethiopian Business Review | EBR in Addis Ababa, Ethiopia. Full-time Entry level role.", "19 hours ago"),
			"Ethiopian Business Review | EBR", "Growth & Partnership Head", "Addis Ababa, Ethiopia"},
		{"hiring shape",
			li("https://et.linkedin.com/jobs/view/appointment-setter-at-sg-services-group-4469625307",
				"SG Services Group hiring Appointment Setter in Ethiopia | LinkedIn",
				"Get notified about new Appointment Setter jobs in Ethiopia. Sign in to create job alert.", "16 hours ago"),
			"SG Services Group", "Appointment Setter", "Ethiopia"},
		{"title names no company: the slug supplies it, capitalized",
			li("https://et.linkedin.com/jobs/view/functional-consultant-business-analyst-at-beltech-solutions-4471403510",
				"Functional Consultant/Business Analyst - LinkedIn Ethiopia",
				"Get notified about new Functional Consultant jobs in Addis Ababa, Ethiopia. Sign in to create job alert. Apply. Similar jobs.", "10 hours ago"),
			"Beltech Solutions", "Functional Consultant/Business Analyst", "Addis Ababa, Ethiopia"},
		{"truncated title: location cut off by an ellipsis is unknown, the Ethiopian host vouches",
			li("https://et.linkedin.com/jobs/view/account-executive-at-ethiopian-digital-id-proclamation-4469332706",
				"Ethiopian Digital ID Proclamation hiring Account Executive in Adis ...",
				"... Ethiopia. The program aims to improve access to public and private services ...", "23 hours ago"),
			"Ethiopian Digital ID Proclamation", "Account Executive", ""},
		{"a job title that itself contains ' at '",
			li("https://et.linkedin.com/jobs/view/engineer-at-scale-at-chapa-4470900003",
				"Engineer at Scale at Chapa - LinkedIn Ethiopia", "Sign in to apply.", ""),
			"Chapa", "Engineer at Scale", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, reason := freshJob(tc.r, fixedNow)
			if reason != "" {
				t.Fatalf("rejected (%s), want accepted", reason)
			}
			if job.Employer != tc.wantEmployer || job.Title != tc.wantTitle || job.LocationRaw != tc.wantLocation || job.Closed {
				t.Errorf("job = employer %q title %q location %q, want %q, %q, %q",
					job.Employer, job.Title, job.LocationRaw, tc.wantEmployer, tc.wantTitle, tc.wantLocation)
			}
			if job.PublishedAt.IsZero() || job.PublishedAt.After(fixedNow) {
				t.Errorf("PublishedAt = %v, want an estimate from the id, not in the future", job.PublishedAt)
			}
		})
	}
}

// Every case here was accepted by an earlier, looser rule (any mention of
// Ethiopia in the result text) and stored a job from another country.
func TestFreshJob_RejectsWhatIsNotTiedToEthiopia(t *testing.T) {
	cases := []struct {
		name string
		r    search.Result
		want string
	}{
		{"phone-code list on a page for a US job",
			li("https://www.linkedin.com/jobs/view/senior-manager-customer-field-operations-at-techaviv-4471422889",
				"Senior Manager, Customer Field Operations - LinkedIn",
				"Ethiopia+251; Falkland Islands+500; Faroe Islands+298; Fiji+679; Finland+ ... Marketing Campaign Manager jobs", "9 hours ago"),
			"outside-ethiopia"},
		{"similar-jobs list on a UK page",
			li("https://uk.linkedin.com/jobs/view/creator-partnerships-manager-at-live-nation-entertainment-4471479346",
				"Creator Partnerships Manager at Live Nation Entertainment - LinkedIn",
				"Programmes and Partnerships Coordinator - Ethiopia. The Donkey Sanctuary. Sidmouth, England, United Kingdom ...", "4 hours ago"),
			"outside-ethiopia"},
		{"regional site of another country, Ethiopia only in the company's footprint",
			li("https://zm.linkedin.com/jobs/view/country-lead-wholesale-at-coca-cola-beverages-africa-4471105935",
				"Coca-Cola Beverages Africa hiring COUNTRY LEAD",
				"CCBA operates in 14 countries, including its six key markets of South Africa, Kenya, Ethiopia, Uganda ...", "21 hours ago"),
			"outside-ethiopia"},
		{"a place that merely starts like Addis (Addison, Texas)",
			li("https://et.linkedin.com/jobs/view/data-analyst-at-acme-corp-4470900011",
				"Data Analyst at Acme Corp - LinkedIn",
				"Apply for Data Analyst at Acme Corp in Addison, TX. Ethiopia+251; Falkland Islands+500", "3 hours ago"),
			"outside-ethiopia"},
		{"the Ethiopian host does not outvote a stated foreign location",
			li("https://et.linkedin.com/jobs/view/finance-executive-at-dreamtech-4470900007",
				"DreamTech hiring Finance Executive in Noida, India | LinkedIn", "Noida, Uttar Pradesh, India.", ""),
			"outside-ethiopia"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if job, reason := freshJob(tc.r, fixedNow); reason != tc.want {
				t.Errorf("reason = %q (job %+v), want %q", reason, job, tc.want)
			}
		})
	}
}

func TestFreshJob_RejectsUnusableResults(t *testing.T) {
	cases := []struct {
		name string
		r    search.Result
		want string
	}{
		{"not a job url", li("https://www.linkedin.com/company/acme", "Acme hiring X in Ethiopia | LinkedIn", "Ethiopia", ""), "not-a-job-url"},
		{"no company in the slug", li("https://et.linkedin.com/jobs/view/senior-portfolio-manager-4470900004", "Senior Portfolio Manager - LinkedIn Ethiopia", "", ""), "no-employer"},
		{"empty company after the last -at-", li("https://et.linkedin.com/jobs/view/nurse-at--4470900009", "Nurse", "", ""), "no-employer"},
		{"employer name is a web address", li("https://et.linkedin.com/jobs/view/volunteer-at-https-africanyouthuniondo-or-4470900010",
			"Volunteer at https://africanyouthuniondo.or/ - LinkedIn", "Ethiopia", ""), "no-employer"},
		{"employer hidden", li("https://et.linkedin.com/jobs/view/nurse-at-confidential-4470900005",
			"Confidential hiring Nurse in Ethiopia | LinkedIn", "Ethiopia", ""), "no-employer"},
		{"employer name absurdly long", li("https://et.linkedin.com/jobs/view/x-at-"+strings.Repeat("a", 130)+"-4470900008",
			"X", "", ""), "no-employer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if job, reason := freshJob(tc.r, fixedNow); reason != tc.want {
				t.Errorf("reason = %q (job %+v), want %q", reason, job, tc.want)
			}
		})
	}
}

func TestSplitFreshSlug(t *testing.T) {
	cases := []struct {
		slug, title, wantTitleSlug, wantCompany string
		ok                                      bool
	}{
		{"cook-at-acme", "Acme hiring Cook in Ethiopia", "cook", "Acme", true},
		{"cook-at-acme", "...", "cook", "Acme", true},
		{"cook-at-acme-plc", "Cook at Acme PLC - LinkedIn", "cook", "Acme PLC", true},
		// A title-confirmed earlier split beats the last one.
		{"analyst-at-work-at-home-solutions", "Analyst at Work at Home Solutions - LinkedIn", "analyst-at-work", "Home Solutions", true},
		// Nothing in the title confirms a split: the last one is used.
		{"analyst-at-work-at-home-solutions", "...", "analyst-at-work", "Home Solutions", true},
		// A company name that itself contains "-at-": the hiring shape decides.
		{"engineer-at-look-at-me-inc", "Look at Me Inc hiring Engineer in Ethiopia", "engineer", "Look at Me Inc", true},
		{"engineer-at-look-at-me-inc", "...", "engineer-at-look", "Me Inc", true},
		{"no-separator-here", "x", "", "", false},
		{"-at-acme", "x", "", "", false},
	}
	for _, tc := range cases {
		ts, co, ok := splitFreshSlug(tc.slug, tc.title)
		if ts != tc.wantTitleSlug || co != tc.wantCompany || ok != tc.ok {
			t.Errorf("splitFreshSlug(%q, %q) = %q, %q, %v; want %q, %q, %v", tc.slug, tc.title, ts, co, ok, tc.wantTitleSlug, tc.wantCompany, tc.ok)
		}
	}
}

func TestFreshJob_EndedAndOldPostingsBecomeClosureMarkersWithTheirEmployer(t *testing.T) {
	ended := hiring("acme", "Acme", "cook", "Cook", 4470900010)
	ended.Snippet = "No longer accepting applications. Ethiopia."
	old := hiring("acme", "Acme", "cook", "Cook", 4000000000) // years ago by id
	for _, r := range []search.Result{ended, old} {
		job, reason := freshJob(r, fixedNow)
		if reason != "" || !job.Closed || job.Employer != "Acme" || job.ExternalID == "" {
			t.Errorf("freshJob(%s) = %+v, %q; want a closure marker for employer Acme", r.URL, job, reason)
		}
	}
}

// ---- Collect ----

func TestFreshCollect_SearchesEachKeywordForTheLastDayAndReturnsEmployerNamedJobs(t *testing.T) {
	f := &fakePages{data: map[string][][]search.Result{
		"software engineer": {{hiring("acme", "Acme", "software-engineer", "Software Engineer", 4470900101)}},
		"accountant":        {{hiring("beta-plc", "Beta PLC", "accountant", "Accountant", 4470900102)}},
	}}
	c := newFresh(f, 10, 1, "software engineer", "accountant")

	jobs, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	if got := jobIDs(jobs); !slices.Equal(got, []string{"linkedin:4470900101", "linkedin:4470900102"}) {
		t.Fatalf("jobs = %v", got)
	}
	byID := map[string]ats.Job{}
	for _, j := range jobs {
		byID[j.ExternalID] = j
	}
	if byID["linkedin:4470900101"].Employer != "Acme" || byID["linkedin:4470900102"].Employer != "Beta PLC" {
		t.Errorf("employers = %q, %q", byID["linkedin:4470900101"].Employer, byID["linkedin:4470900102"].Employer)
	}
	if len(f.calls) != 2 {
		t.Fatalf("%d searches, want 2", len(f.calls))
	}
	for _, call := range f.calls {
		if call.recency != search.RecencyDay || call.page != 1 ||
			!strings.HasPrefix(call.query, `site:linkedin.com/jobs/view "Ethiopia" `) {
			t.Errorf("call = %+v, want a day-limited page-1 LinkedIn search for Ethiopia", call)
		}
	}
	if c.budget.Used() != 2 {
		t.Errorf("budget used = %d, want 2", c.budget.Used())
	}
}

func TestFreshCollect_ReadsLaterPagesUntilOneIsEmpty(t *testing.T) {
	f := &fakePages{data: map[string][][]search.Result{
		"developer": {
			{hiring("acme", "Acme", "developer", "Developer", 4470900201)},
			{hiring("beta", "Beta", "developer", "Developer", 4470900202)},
		},
	}}
	c := newFresh(f, 10, 3, "developer")
	jobs, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	if len(jobs) != 2 {
		t.Errorf("%d jobs, want 2 (one per page)", len(jobs))
	}
	var pages []int
	for _, call := range f.calls {
		pages = append(pages, call.page)
	}
	if !slices.Equal(pages, []int{1, 2, 3}) {
		t.Errorf("pages requested = %v, want 1,2,3 (3 is the empty one that ends the keyword)", pages)
	}
}

func TestFreshCollect_SameJobFromTwoKeywordsIsOneJob_SameOpeningReposted(t *testing.T) {
	same := hiring("acme", "Acme", "developer", "Developer", 4470900301)
	repost := hiring("acme", "Acme", "developer", "Developer", 4470900302) // a new id, same opening
	other := hiring("beta", "Beta", "developer", "Developer", 4470900303)  // same title, other employer
	f := &fakePages{data: map[string][][]search.Result{
		"developer": {{same, other}}, "engineer": {{same, repost}},
	}}
	jobs, err := newFresh(f, 10, 1, "developer", "engineer").Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	var open, closed []string
	for _, j := range jobs {
		if j.Closed {
			closed = append(closed, j.ExternalID+"/"+j.Employer)
		} else {
			open = append(open, j.ExternalID)
		}
	}
	slices.Sort(open)
	if !slices.Equal(open, []string{"linkedin:4470900301", "linkedin:4470900303"}) {
		t.Errorf("open = %v, want the first Acme posting and Beta's (same title, other employer, is a different opening)", open)
	}
	if !slices.Equal(closed, []string{"linkedin:4470900302/Acme"}) {
		t.Errorf("closed = %v, want the repost closed under its employer", closed)
	}
}

func TestFreshCollect_TheBudgetCapsSpendingAndKeepsWhatWasFound(t *testing.T) {
	f := &fakePages{data: map[string][][]search.Result{
		"a": {{hiring("acme", "Acme", "cook", "Cook", 4470900401)}},
		"b": {{hiring("beta", "Beta", "chef", "Chef", 4470900402)}},
		"c": {{hiring("gamma", "Gamma", "nurse", "Nurse", 4470900403)}},
	}}
	c := newFresh(f, 2, 1, "a", "b", "c")
	jobs, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() failed: %v, want the two jobs found before the budget ran out", err)
	}
	if len(jobs) != 2 || len(f.calls) != 2 || c.budget.Used() != 2 {
		t.Errorf("jobs %d, searches %d, used %d; want 2, 2, 2", len(jobs), len(f.calls), c.budget.Used())
	}
}

func TestFreshCollect_AZeroBudgetIsAnErrorNotAnEmptySuccess(t *testing.T) {
	f := &fakePages{}
	_, err := newFresh(f, 0, 1, "a").Collect(context.Background())
	if !errors.Is(err, ErrBudgetExhausted) || len(f.calls) != 0 {
		t.Errorf("err = %v, searches = %d; want ErrBudgetExhausted and no search", err, len(f.calls))
	}
}

func TestFreshCollect_ATransientFailureIsRetriedOnceAndSpendsBudgetEachTime(t *testing.T) {
	f := &fakePages{failFirst: 1, data: map[string][][]search.Result{
		"a": {{hiring("acme", "Acme", "cook", "Cook", 4470900501)}},
	}}
	c := newFresh(f, 5, 1, "a")
	jobs, err := c.Collect(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Collect() = %d jobs, %v; want the retry to succeed", len(jobs), err)
	}
	if c.budget.Used() != 2 {
		t.Errorf("budget used = %d, want 2 (both attempts are billed)", c.budget.Used())
	}
}

func TestFreshCollect_OneKeywordFailingDoesNotStopTheOthers_AllFailingIsAnError(t *testing.T) {
	boom := errors.New("serper: 500")
	f := &fakePages{
		errs: map[string]error{"bad": boom},
		data: map[string][][]search.Result{"good": {{hiring("acme", "Acme", "cook", "Cook", 4470900601)}}},
	}
	jobs, err := newFresh(f, 10, 1, "bad", "good").Collect(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Collect() = %d jobs, %v; want the good keyword's job and no error", len(jobs), err)
	}

	f2 := &fakePages{errs: map[string]error{"bad": boom, "worse": boom}}
	if _, err := newFresh(f2, 10, 1, "bad", "worse").Collect(context.Background()); !errors.Is(err, boom) {
		t.Errorf("all keywords failing: err = %v, want it to wrap the search error", err)
	}
}

func TestFreshCollect_ARejectedKeyIsNotRetriedAndStopsTheRun(t *testing.T) {
	f := &fakePages{errs: map[string]error{"a": search.ErrUnauthorized}}
	c := newFresh(f, 10, 1, "a", "b", "c")
	_, err := c.Collect(context.Background())
	if !errors.Is(err, search.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if len(f.calls) != 1 || c.budget.Used() != 1 {
		t.Errorf("%d searches, %d used; want 1 and 1 (no retry, no further keyword)", len(f.calls), c.budget.Used())
	}
}

func TestFreshCollect_CancellationIsReturnedNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakePages{}
	_, err := newFresh(f, 10, 1, "a").Collect(ctx)
	if !errors.Is(err, context.Canceled) || len(f.calls) != 0 {
		t.Errorf("err = %v, searches = %d; want context.Canceled and no search", err, len(f.calls))
	}
}

func TestFreshCollect_RefusesAnInvalidConfigBeforeSpendingAnything(t *testing.T) {
	f := &fakePages{}
	c := NewFresh(f, FreshConfig{Keywords: []string{`"quoted"`}, Pages: 1}, NewBudget(5), "search", discardLogger())
	if _, err := c.Collect(context.Background()); err == nil || len(f.calls) != 0 {
		t.Errorf("err = %v, searches = %d; want a config error and no search", err, len(f.calls))
	}
}

func TestFreshClient_IsASamplePartnerWithAPriorityProvider(t *testing.T) {
	c := newFresh(&fakePages{}, 1, 1, "a")
	if c.StaleAfter() <= 0 {
		t.Errorf("StaleAfter = %v, want positive (a search is a sample)", c.StaleAfter())
	}
	if c.PriorityProvider() != "search" {
		t.Errorf("PriorityProvider = %q, want search", c.PriorityProvider())
	}
}

// ---- config ----

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "q.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFreshConfig(t *testing.T) {
	cfg, err := LoadFreshConfig(writeConfig(t, `{"keywords": ["software engineer", " data  analyst "]}`))
	if err != nil {
		t.Fatalf("LoadFreshConfig() failed: %v", err)
	}
	if cfg.Pages != 1 || len(cfg.Keywords) != 2 || cfg.Queries() != 2 {
		t.Errorf("cfg = %+v, want pages defaulted to 1 and 2 queries", cfg)
	}

	bad := map[string]string{
		"empty":               `{"keywords": []}`,
		"blank keyword":       `{"keywords": ["  "]}`,
		"duplicate":           `{"keywords": ["Nurse", "nurse"]}`,
		"quote":               `{"keywords": ["\"nurse\""]}`,
		"operator":            `{"keywords": ["nurse OR doctor"]}`,
		"site operator":       `{"keywords": ["site:example.com"]}`,
		"negation":            `{"keywords": ["-nurse"]}`,
		"leading OR":          `{"keywords": ["OR nurse"]}`,
		"trailing exclusion":  `{"keywords": ["nurse -linkedin"]}`,
		"AND":                 `{"keywords": ["nurse and doctor"]}`,
		"plus operator":       `{"keywords": ["+nurse"]}`,
		"wildcard":            `{"keywords": ["nurs*"]}`,
		"too long":            `{"keywords": ["` + strings.Repeat("a", 81) + `"]}`,
		"too many pages":      `{"keywords": ["a"], "pages": 4}`,
		"negative pages":      `{"keywords": ["a"], "pages": -1}`,
		"unknown field":       `{"keywords": ["a"], "keyword": ["b"]}`,
		"trailing data":       `{"keywords": ["a"]} {}`,
		"not json":            `nope`,
		"too many keywords":   `{"keywords": [` + strings.TrimSuffix(strings.Repeat(`"k",`, 41), ",") + `]}`,
		"unique but too many": manyKeywords(41),
	}
	for name, body := range bad {
		if _, err := LoadFreshConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: accepted, want an error", name)
		}
	}
	if _, err := LoadFreshConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Errorf("missing file accepted")
	}
}

func manyKeywords(n int) string {
	var ks []string
	for i := range n {
		ks = append(ks, fmt.Sprintf(`"k%d"`, i))
	}
	return `{"keywords": [` + strings.Join(ks, ",") + `]}`
}

// The shipped config must load, and stay affordable for one run.
func TestShippedLinkedInQueriesLoad(t *testing.T) {
	cfg, err := LoadFreshConfig("../../../configs/linkedin_queries.json")
	if err != nil {
		t.Fatalf("configs/linkedin_queries.json: %v", err)
	}
	if cfg.Queries() > 40 {
		t.Errorf("shipped config spends %d queries per run, want at most 40 (the free tier is a fixed 2,500)", cfg.Queries())
	}
}

// After a keyword's page fails (and its one retry), its later pages are not
// tried: they would spend budget on a keyword that is failing.
func TestFreshCollect_AFailedKeywordSpendsNothingOnItsLaterPages(t *testing.T) {
	boom := errors.New("serper: 500")
	f := &fakePages{errs: map[string]error{"bad": boom}, data: map[string][][]search.Result{
		"good": {{hiring("acme", "Acme", "cook", "Cook", 4470900701)}},
	}}
	c := newFresh(f, 20, 3, "bad", "good")
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect() failed: %v", err)
	}
	var badCalls []int
	for _, call := range f.calls {
		if strings.HasSuffix(call.query, " bad") {
			badCalls = append(badCalls, call.page)
		}
	}
	if !slices.Equal(badCalls, []int{1, 1}) {
		t.Errorf("pages requested for the failing keyword = %v, want [1 1] (page 1 and its one retry, no page 2)", badCalls)
	}
}

func TestDedupeOpenings_SameTitleInAnotherCityStaysAndUnknownCityMergesWithAnEthiopianOne(t *testing.T) {
	mk := func(id, place string) ats.Job {
		return ats.Job{ExternalID: id, Title: "Cashier", Employer: "Bank", LocationRaw: place, URL: "u"}
	}
	out := dedupeOpenings([]ats.Job{
		mk("a", "Addis Ababa"), mk("b", "Hawassa"), mk("c", "Ethiopia"), mk("d", "Addis Abeba, Ethiopia"), mk("e", "Nairobi, Kenya"),
	})
	closed := map[string]bool{}
	for _, j := range out {
		if j.Closed {
			closed[j.ExternalID] = true
			if j.Employer != "Bank" {
				t.Errorf("closure marker for %s lost its employer", j.ExternalID)
			}
		}
	}
	if len(out) != 5 || !closed["c"] || !closed["d"] || closed["a"] || closed["b"] || closed["e"] {
		t.Errorf("closed = %v, want only c (no city) and d (same city as a) closed", closed)
	}
}
