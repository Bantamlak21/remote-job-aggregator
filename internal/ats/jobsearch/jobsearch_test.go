package jobsearch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
)

var fixedNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ---- fakes ----

type fakeSearcher struct {
	mu      sync.Mutex
	queries []string
	recency []search.Recency
	bySite  map[string][]search.Result // keyed by the "site:" suffix of the query
	err     error
	// failTimes makes the next N calls fail with a transient error.
	failTimes int
	inFlight  int
	maxSeen   int
}

func (f *fakeSearcher) SearchRecent(_ context.Context, q string, r search.Recency) ([]search.Result, error) {
	// The in-flight counter is updated under the lock but the sleep is not
	// held under it, so two overlapping calls are actually observable.
	f.mu.Lock()
	f.inFlight++
	f.maxSeen = max(f.maxSeen, f.inFlight)
	f.mu.Unlock()
	time.Sleep(2 * time.Millisecond)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	f.queries = append(f.queries, q)
	f.recency = append(f.recency, r)
	if f.failTimes > 0 {
		f.failTimes--
		return nil, errors.New("stream error: PROTOCOL_ERROR")
	}
	if f.err != nil {
		return nil, f.err
	}
	for site, res := range f.bySite {
		if strings.HasSuffix(q, "site:"+site) {
			return res, nil
		}
	}
	return nil, nil
}

func newClient(s Searcher, budget int) *Client {
	c := New(s, []Company{
		{Name: "Chapa Financial Technologies", Aliases: []string{"Chapa"}},
		{Name: "Gebeya Inc.", HiresOutsideEthiopia: true},
		{Name: "DreamTech"},
		{Name: "Kifiya Financial Technology", Aliases: []string{"Kifiya Financial Technologies", "Kifiya"}},
		{Name: "EthSwitch"},
	}, NewBudget(budget), discardLogger())
	c.now = func() time.Time { return fixedNow }
	c.retryDelay = 0
	return c
}

// matchFor returns the fixture client's match set for a primary company
// name. It panics on an unknown name: a typo here would hand back an empty
// match set, and every "must be rejected" test would then pass vacuously.
func matchFor(name string) companyMatch {
	m, ok := newClient(nil, 1).companies[nameKey(name)]
	if !ok || len(m.keys) == 0 {
		panic("matchFor: no fixture company named " + name)
	}
	return m
}

func TestMatchFor_FixtureCompaniesAcceptTheirAliases(t *testing.T) {
	chapa := matchFor("Chapa Financial Technologies")
	for _, want := range []string{"chapa", "chapa-financial-technologies"} {
		if !chapa.keys[want] {
			t.Errorf("Chapa keys = %v, missing %q", chapa.keys, want)
		}
	}
	if chapa.keys["chapa-de-indian-health"] {
		t.Errorf("Chapa keys accept the unrelated Chapa De Indian Health")
	}
}

// ---- name matching ----

func TestSearchQuery(t *testing.T) {
	cases := []struct {
		co   Company
		site string
		want string
	}{
		{Company{Name: "Chapa"}, "linkedin.com/jobs/view", `"Chapa" site:linkedin.com/jobs/view`},
		{Company{Name: "Kifiya Financial Technology", Aliases: []string{"Kifiya"}}, "ethiojobs.net/job",
			`("Kifiya Financial Technology" OR "Kifiya") site:ethiojobs.net/job`},
		// Duplicates by name key and quote characters are neutralized; the
		// name list is capped.
		{Company{Name: `Ge"beya`, Aliases: []string{"GEBEYA inc", "a", "b", "c", "d"}}, "x.com",
			`("Gebeya" OR "a" OR "b" OR "c") site:x.com`},
	}
	for _, tc := range cases {
		if got := searchQuery(tc.co, tc.site); got != tc.want {
			t.Errorf("searchQuery(%v) = %q, want %q", tc.co, got, tc.want)
		}
	}
}

// ---- LinkedIn ----

// Real result shapes captured from Serper for "Chapa", "Gebeya" and
// "Kifiya Financial Technology" (2026-09-24), trimmed.
func li(url, title, snippet, date string) search.Result {
	return search.Result{URL: url, Title: title, Snippet: snippet, Date: date}
}

func TestLinkedInJob_AcceptsRealResultShapes(t *testing.T) {
	m := matchFor("Chapa Financial Technologies")
	cases := []struct {
		name         string
		r            search.Result
		wantID       string
		wantTitle    string
		wantLocation string
	}{
		{
			"apply-for snippet, pretty title from result title",
			li("https://et.linkedin.com/jobs/view/product-manager-payment-solutions-at-chapa-4470000001",
				"Product Manager - Payment Solutions at Chapa - LinkedIn Ethiopia",
				"Apply for Product Manager - Payment Solutions at Chapa in Ethiopia. Full-time Not Applicable role. See responsibilities...",
				"5 days ago"),
			"linkedin:4470000001", "Product Manager - Payment Solutions", "Ethiopia",
		},
		{
			"hiring-style title keeps punctuation the slug loses",
			li("https://et.linkedin.com/jobs/view/ui-ux-designer-at-chapa-4470000002",
				"Chapa hiring UI/UX Designer in Ethiopia | LinkedIn",
				"Chapa Ethiopia. 2 weeks ago Be among the first 25 applicants.", "2 weeks ago"),
			"linkedin:4470000002", "UI/UX Designer", "Ethiopia",
		},
		{
			"www host and a tracking query string",
			li("https://www.linkedin.com/jobs/view/senior-accountant-at-chapa-4470000003?trk=public_jobs&position=1#x",
				"Senior Accountant at Chapa - LinkedIn Ethiopia",
				"Apply for Senior Accountant at Chapa in Addis Ababa, Addis Ababa, Ethiopia. Full-time Mid-Senior level role.", "Sep 14, 2026"),
			"linkedin:4470000003", "Senior Accountant", "Addis Ababa, Addis Ababa, Ethiopia",
		},
		{
			"truncated result title falls back to the slug",
			li("https://et.linkedin.com/jobs/view/data-analyst-at-chapa-financial-technologies-s-c-4470000004",
				"...", "", ""),
			"linkedin:4470000004", "Data Analyst", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, reason := linkedInJob(tc.r, m, fixedNow)
			if reason != "" {
				t.Fatalf("rejected (%s), want accepted", reason)
			}
			if job.ExternalID != tc.wantID || job.Title != tc.wantTitle || job.LocationRaw != tc.wantLocation {
				t.Errorf("job = %+v\nwant id=%q title=%q location=%q", job, tc.wantID, tc.wantTitle, tc.wantLocation)
			}
			if strings.Contains(job.URL, "?") || strings.Contains(job.URL, "#") {
				t.Errorf("URL %q keeps a query/fragment; it must be the clean job URL", job.URL)
			}
		})
	}
}

// The heart of the "is this really that company" rule: real lookalikes
// Google returned for the company name.
func TestLinkedInJob_RejectsOtherCompanies(t *testing.T) {
	chapa := matchFor("Chapa Financial Technologies")
	gebeya := matchFor("Gebeya Inc.")
	cases := []struct {
		name string
		m    companyMatch
		url  string
		why  string
	}{
		{"chapa vs Chapa De Indian Health", chapa, "https://www.linkedin.com/jobs/view/psychologist-pt-or-ft-at-chapa-de-indian-health-4460593162", "other-company"},
		{"chapa vs Chapa Tax Business Solutions", chapa, "https://www.linkedin.com/jobs/view/bilingual-administrative-assistant-at-chapa-tax-business-solutions-llc-4463885636", "other-company"},
		{"gebeya vs Chaka Gebeya", gebeya, "https://et.linkedin.com/jobs/view/flutter-developer-at-chaka-gebeya-4014350468", "other-company"},
		{"company name only in the title", chapa, "https://et.linkedin.com/jobs/view/chapa-payments-integrator-at-acme-1234567", "other-company"},
		{"no -at- separator", chapa, "https://et.linkedin.com/jobs/view/chapa-4014350468", "other-company"},
		{"numeric-only job url (no company info)", chapa, "https://www.linkedin.com/jobs/view/4014350468", "not-a-job-url"},
		{"company listing page", chapa, "https://et.linkedin.com/jobs/business-units-jobs", "not-a-job-url"},
		{"other linkedin path", chapa, "https://www.linkedin.com/company/chapa-at-work-1234567", "not-a-job-url"},
		{"lookalike host", chapa, "https://linkedin.com.evil.example/jobs/view/dev-at-chapa-1234567", "not-a-job-url"},
		{"http downgrade", chapa, "http://et.linkedin.com/jobs/view/dev-at-chapa-1234567", "not-a-job-url"},
		{"empty", chapa, "", "not-a-job-url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, reason := linkedInJob(li(tc.url, "Dev at Chapa", "s", "1 day ago"), tc.m, fixedNow)
			if reason != tc.why {
				t.Errorf("reason = %q (job %+v), want %q", reason, job, tc.why)
			}
		})
	}
}

// Found live on 2026-09-24: the slug rule alone accepted "Finance
// Executive at DreamTech" in Noida, India as the Ethiopian DreamTech's job,
// because two unrelated companies share a name. An Ethiopia signal is
// required unless the company is configured as hiring elsewhere too.
func TestLinkedInJob_SameNamedCompanyInAnotherCountryIsRejected(t *testing.T) {
	dreamtech := matchFor("DreamTech")
	india := li("https://in.linkedin.com/jobs/view/finance-executive-at-dreamtech-4466175092",
		"Finance Executive at DreamTech - LinkedIn India",
		"Apply for Finance Executive at DreamTech in Noida, Uttar Pradesh, India. Full-time Mid-Senior level role.", "9 days ago")
	if job, reason := linkedInJob(india, dreamtech, fixedNow); reason != "outside-ethiopia" {
		t.Errorf("Indian DreamTech: reason = %q (job %+v), want outside-ethiopia", reason, job)
	}

	ethiopia := li("https://et.linkedin.com/jobs/view/finance-executive-at-dreamtech-4466175099",
		"Finance Executive at DreamTech - LinkedIn Ethiopia",
		"Apply for Finance Executive at DreamTech in Addis Ababa, Ethiopia.", "9 days ago")
	if _, reason := linkedInJob(ethiopia, dreamtech, fixedNow); reason != "" {
		t.Errorf("Ethiopian DreamTech rejected: %s", reason)
	}

	// No Ethiopian subdomain, but the text says so.
	byText := li("https://www.linkedin.com/jobs/view/qa-engineer-at-dreamtech-4466175101",
		"QA Engineer at DreamTech", "Apply for QA Engineer at DreamTech in Addis Ababa, Ethiopia.", "2 days ago")
	if _, reason := linkedInJob(byText, dreamtech, fixedNow); reason != "" {
		t.Errorf("Ethiopian-by-text DreamTech rejected: %s", reason)
	}
}

func TestLinkedInJob_CompaniesThatHireAbroadKeepTheirForeignJobs(t *testing.T) {
	gebeya := matchFor("Gebeya Inc.")
	kenya := li("https://ke.linkedin.com/jobs/view/talent-specialist-job-matching-at-gebeya-inc-4470000005",
		"Talent Specialist - Job Matching at Gebeya Inc. — Nairobi County",
		"Apply for Talent Specialist - Job Matching at Gebeya Inc. in Nairobi County, Kenya. Contract role.", "3 days ago")
	job, reason := linkedInJob(kenya, gebeya, fixedNow)
	if reason != "" || job.LocationRaw != "Nairobi County, Kenya" {
		t.Errorf("Gebeya Nairobi job: reason = %q, job = %+v; want it kept (HiresOutsideEthiopia)", reason, job)
	}
}

func TestEthiopiaSignal(t *testing.T) {
	cases := []struct {
		host, location, snippet, title string
		want                           bool
	}{
		{"et.linkedin.com", "", "", "", true},
		{"ET.LinkedIn.com", "", "", "", true},
		{"www.linkedin.com", "Addis Ababa, Ethiopia", "", "", true},
		{"www.linkedin.com", "", "Hiring in Addis Ababa now", "", true},
		{"www.linkedin.com", "", "", "Analyst - LinkedIn Ethiopia", true},
		{"www.linkedin.com", "Addis Abeba", "", "", true},
		{"in.linkedin.com", "Noida, Uttar Pradesh, India", "Apply for Finance Executive", "Finance Executive - LinkedIn India", false},
		{"ke.linkedin.com", "Nairobi County, Kenya", "", "", false},
		{"www.linkedin.com", "", "", "", false},
		{"www.linkedin.com", "Addison, TX", "", "", false},
		{"www.linkedin.com", "Nairobi", "Ethiopian Airlines hub", "", false},
	}
	for _, tc := range cases {
		got := ethiopiaSignal(tc.host, tc.location, search.Result{Snippet: tc.snippet, Title: tc.title})
		if got != tc.want {
			t.Errorf("ethiopiaSignal(%q, %q, %q, %q) = %t, want %t", tc.host, tc.location, tc.snippet, tc.title, got, tc.want)
		}
	}
}

func TestLinkedInJob_TitleThatItselfContainsAt(t *testing.T) {
	m := matchFor("Chapa Financial Technologies")
	job, reason := linkedInJob(li("https://et.linkedin.com/jobs/view/engineer-at-scale-at-chapa-4470000006",
		"Engineer at Scale at Chapa - LinkedIn", "", "1 day ago"), m, fixedNow)
	if reason != "" || job.Title != "Engineer at Scale" {
		t.Errorf("job = %+v, reason = %q; want title %q", job, reason, "Engineer at Scale")
	}
}

func TestLinkedInJob_FreshnessRules(t *testing.T) {
	m := matchFor("Chapa Financial Technologies")
	const u = "https://et.linkedin.com/jobs/view/data-analyst-at-chapa-4470000004"
	cases := []struct {
		name    string
		snippet string
		date    string
		want    string // "" accepted
	}{
		{"recent", "Apply for Data Analyst at Chapa in Ethiopia.", "3 days ago", ""},
		{"exactly at the limit is fine", "s", "6 weeks ago", ""}, // 42 days < 45
		{"too old by relative date", "s", "3 months ago", "too-old"},
		{"too old by absolute date", "s", "Jul 8, 2026", "too-old"},
		{"a year old", "s", "1 year ago", "too-old"},
		{"undated is accepted: the month-limited search itself vouches for it", "s", "", ""},
		{"unparseable date is treated as undated", "s", "sometime", ""},
		{"closed by snippet", "Chapa Ethiopia. No longer accepting applications · Report this job", "2 days ago", "closed"},
		{"closed by snippet, any case", "NO LONGER ACCEPTING APPLICATIONS", "2 days ago", "closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, reason := linkedInJob(li(u, "Data Analyst at Chapa", tc.snippet, tc.date), m, fixedNow)
			if tc.want == "closed" || tc.want == "too-old" {
				// Not a rejection: the result reports the job as ended, so it
				// is returned as a closure for ingestion to act on.
				if reason != "" || !job.Closed || job.ExternalID != "linkedin:4470000004" || job.Title != "" {
					t.Errorf("job = %+v, reason = %q; want a bare closure marker for linkedin:4470000004", job, reason)
				}
				return
			}
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
			if reason == "" && job.Closed {
				t.Errorf("a live job was marked closed: %+v", job)
			}
			// The posting date comes from the job id, never from Google's
			// date (which can be a crawl date).
			if tc.want == "" && !job.PublishedAt.Equal(estimatePostedFromID(4470000004)) {
				t.Errorf("PublishedAt = %v, want the id-based estimate %v", job.PublishedAt, estimatePostedFromID(4470000004))
			}
		})
	}
}

// The bug the user reported: old LinkedIn jobs showed "1 month ago" because
// Google's date beside the result is a crawl date. These are REAL results
// captured on 2026-09-24 where Google's date was recent (or absent) and the
// job was far older; the id says so.
func TestLinkedInJob_OldJobsWithARecentGoogleDateAreRejected(t *testing.T) {
	zare := matchFor("Kifiya Financial Technology") // any company in the fixture; the id is what matters
	cases := []struct {
		name, url, googleDate string
	}{
		{"Zare 'Virtual Assistant' (~18 months old) shown as 1 month ago", "https://et.linkedin.com/jobs/view/senior-accountant-at-kifiya-financial-technology-plc-4225170741", "Aug 24, 2026"},
		{"a 2025 posting with a '3 days ago' crawl date", "https://et.linkedin.com/jobs/view/senior-accountant-at-kifiya-financial-technology-plc-4266303883", "3 days ago"},
		{"a 2026-05 posting with a September crawl date", "https://et.linkedin.com/jobs/view/senior-accountant-at-kifiya-financial-technology-plc-4416249158", "Sep 10, 2026"},
		{"an old posting with no Google date at all", "https://et.linkedin.com/jobs/view/senior-accountant-at-kifiya-financial-technology-plc-4337931261", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, reason := linkedInJob(li(tc.url, "Senior Accountant at Kifiya", "Apply in Ethiopia.", tc.googleDate), zare, fixedNow)
			// Not shown, and reported as ended so a copy stored by an earlier
			// (wrongly dated) run is closed at once.
			if reason != "" || !job.Closed || job.Title != "" {
				t.Errorf("job = %+v, reason = %q; want a bare closure marker", job, reason)
			}
		})
	}
}

func TestEstimatePostedFromID(t *testing.T) {
	// Anchors (must be exact) and independent checkpoints from real results
	// (Google dates, which for old jobs are close to posting dates): within 12 days.
	if got := estimatePostedFromID(anchorOldID); !got.Equal(anchorOldTime) {
		t.Errorf("old anchor = %v, want %v", got, anchorOldTime)
	}
	checks := []struct {
		id   int64
		want time.Time
	}{
		{4404170965, time.Date(2026, 4, 23, 0, 0, 0, 0, time.UTC)},
		{4420905329, time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC)},
		{4341349159, time.Date(2025, 11, 18, 0, 0, 0, 0, time.UTC)},
		{4335092464, time.Date(2025, 11, 12, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range checks {
		got := estimatePostedFromID(c.id)
		if diff := got.Sub(c.want); diff > 12*24*time.Hour || diff < -12*24*time.Hour {
			t.Errorf("estimatePostedFromID(%d) = %v, more than 12 days from %v", c.id, got, c.want)
		}
	}
	// Monotonic.
	if !estimatePostedFromID(4470000000).After(estimatePostedFromID(4460000000)) {
		t.Error("a larger id must never be estimated as older")
	}
}

func TestLinkedInJob_ACurrentJobIsKeptAndDatedFromItsID(t *testing.T) {
	m := matchFor("Chapa Financial Technologies")
	// An id from the day of the run, and one clamped from the future.
	for _, id := range []string{"4471199696", "4999999999"} {
		job, reason := linkedInJob(li("https://et.linkedin.com/jobs/view/data-analyst-at-chapa-"+id, "Data Analyst at Chapa", "in Ethiopia.", ""), m, fixedNow)
		if reason != "" {
			t.Fatalf("id %s rejected: %s", id, reason)
		}
		if job.PublishedAt.After(fixedNow) || fixedNow.Sub(job.PublishedAt) > 5*24*time.Hour {
			t.Errorf("id %s: PublishedAt = %v, want within days before now (%v)", id, job.PublishedAt, fixedNow)
		}
	}
}

func TestParseResultDate(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"19 hours ago", fixedNow.Add(-19 * time.Hour)},
		{"1 hour ago", fixedNow.Add(-time.Hour)},
		{"3 days ago", fixedNow.Add(-72 * time.Hour)},
		{"2 weeks ago", fixedNow.Add(-14 * 24 * time.Hour)},
		{"2 months ago", fixedNow.Add(-60 * 24 * time.Hour)},
		{"1 year ago", fixedNow.Add(-365 * 24 * time.Hour)},
		{"Sep 14, 2026", time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)},
		{"14 Sep 2026", time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)},
		{"", time.Time{}},
		{"yesterday", time.Time{}},
		{"99999999999999999999 days ago", time.Time{}},
		{"-3 days ago", time.Time{}},
	}
	for _, tc := range cases {
		if got := parseResultDate(tc.in, fixedNow); !got.Equal(tc.want) {
			t.Errorf("parseResultDate(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPrettyTitle(t *testing.T) {
	cases := []struct{ slug, result, want string }{
		{"ui-ux-designer", "Chapa hiring UI/UX Designer in Ethiopia | LinkedIn", "UI/UX Designer"},
		{"customer-support-officer", "Customer Support Officer at Chapa - LinkedIn Ethiopia", "Customer Support Officer"},
		{"frontend-developer-react", "Frontend Developer (React) - Chapa - LinkedIn Ethiopia", "Frontend Developer (React)"},
		{"head-of-devops", "Head of DevOps at Kifiya Financial Technology PLC", "Head of DevOps"},
		{"senior-accountant", "...", "Senior Accountant"},
		{"senior-accountant", "", "Senior Accountant"},
		{"c-c-developer", "no match here", "C C Developer"},
	}
	for _, tc := range cases {
		if got := prettyTitle(tc.slug, tc.result); got != tc.want {
			t.Errorf("prettyTitle(%q, %q) = %q, want %q", tc.slug, tc.result, got, tc.want)
		}
	}
}

// ---- ListJobs end to end (fakes) ----

func TestListJobs_ReturnsVerifiedLinkedInJobsAndClosureMarkers(t *testing.T) {
	s := &fakeSearcher{bySite: map[string][]search.Result{
		"linkedin.com/jobs/view": {
			li("https://et.linkedin.com/jobs/view/senior-compliance-officer-at-ethswitch-4470000011", "Senior Compliance Officer at EthSwitch - LinkedIn", "", "2 days ago"),
			li("https://et.linkedin.com/jobs/view/senior-compliance-officer-at-ethswitch-4470000014", "Senior Compliance Officer at EthSwitch - LinkedIn", "", "1 day ago"), // same opening reposted
			li("https://et.linkedin.com/jobs/view/driver-at-ethswitch-4470000012", "Driver at EthSwitch - LinkedIn", "", "3 days ago"),
			li("https://et.linkedin.com/jobs/view/analyst-at-some-other-bank-4470000013", "Analyst at Some Other Bank", "", "1 day ago"),
			li("https://et.linkedin.com/jobs/view/old-role-at-ethswitch-4225170741", "Old Role at EthSwitch - LinkedIn", "", "2 days ago"),
		},
	}}
	c := newClient(s, 10)

	jobs, err := c.ListJobs(context.Background(), "ethswitch")
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	got := map[string]ats.Job{}
	for _, j := range jobs {
		got[j.ExternalID] = j
	}
	if len(jobs) != 4 {
		t.Fatalf("got %d jobs %v, want 4: the compliance officer, the driver, a closure marker for the repost "+
			"and one for the ~18-month-old role (the other bank is rejected)", len(jobs), ids(jobs))
	}
	for _, id := range []string{"linkedin:4470000011", "linkedin:4470000012"} {
		if got[id].Closed || got[id].Title == "" {
			t.Errorf("%s = %+v, want a live job", id, got[id])
		}
	}
	if !got["linkedin:4470000014"].Closed {
		t.Errorf("the reposted duplicate = %+v, want a closure marker", got["linkedin:4470000014"])
	}
	if !got["linkedin:4225170741"].Closed {
		t.Errorf("the old role = %+v, want a closure marker (too old)", got["linkedin:4225170741"])
	}

	// One query, month-limited, quoted company name, LinkedIn only.
	if len(s.queries) != 1 || !strings.Contains(s.queries[0], `"EthSwitch"`) || !strings.Contains(s.queries[0], "linkedin.com/jobs/view") {
		t.Errorf("queries = %v, want exactly one quoted-company LinkedIn query", s.queries)
	}
	if s.recency[0] != search.RecencyMonth {
		t.Errorf("recency = %q, want month", s.recency[0])
	}
}

func ids(jobs []ats.Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ExternalID
	}
	return out
}

func TestListJobs_UnknownCompanyIsInvalidBoardToken(t *testing.T) {
	s := &fakeSearcher{}
	c := newClient(s, 10)
	for _, name := range []string{"", "Nobody Inc", "Chaka Gebeya"} {
		if _, err := c.ListJobs(context.Background(), name); !errors.Is(err, ats.ErrInvalidBoardToken) {
			t.Errorf("ListJobs(%q) error = %v, want ErrInvalidBoardToken", name, err)
		}
	}
	if len(s.queries) != 0 {
		t.Errorf("spent %d queries on unknown companies, want 0", len(s.queries))
	}
}

func TestListJobs_MatchesBoardTokenLooselyButNotLoosely_Enough(t *testing.T) {
	c := newClient(&fakeSearcher{}, 10)
	for _, name := range []string{"Gebeya Inc.", "gebeya", " GEBEYA ", "Gebeya Inc"} {
		if _, err := c.ListJobs(context.Background(), name); err != nil {
			t.Errorf("ListJobs(%q) error = %v, want it to resolve to the Gebeya entry", name, err)
		}
	}
}

func TestListJobs_BudgetIsEnforcedAndCounted(t *testing.T) {
	s := &fakeSearcher{}
	c := newClient(s, 2)

	for i, name := range []string{"Chapa Financial Technologies", "Kifiya Financial Technology"} {
		if _, err := c.ListJobs(context.Background(), name); err != nil {
			t.Fatalf("ListJobs() #%d error = %v", i+1, err)
		}
		if c.budget.Used() != i+1 {
			t.Fatalf("budget used = %d after %d companies, want %d (one query each)", c.budget.Used(), i+1, i+1)
		}
	}
	// Fully spent: the next company fails and no query reaches the network.
	_, err := c.ListJobs(context.Background(), "Gebeya Inc.")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("third ListJobs() error = %v, want ErrBudgetExhausted", err)
	}
	if c.budget.Used() != 2 || len(s.queries) != 2 {
		t.Errorf("budget used = %d, queries sent = %d; want exactly the limit (2), never more", c.budget.Used(), len(s.queries))
	}
}

func TestBudget_ConcurrentTakesNeverExceedTheLimit(t *testing.T) {
	b := NewBudget(50)
	var wg sync.WaitGroup
	var ok, refused int64
	var mu sync.Mutex
	for range 200 {
		wg.Go(func() {
			err := b.Take()
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				refused++
			}
		})
	}
	wg.Wait()
	if ok != 50 || refused != 150 || b.Used() != 50 {
		t.Errorf("ok=%d refused=%d used=%d, want 50/150/50", ok, refused, b.Used())
	}
}

func TestListJobs_SearchErrorIsReturned(t *testing.T) {
	s := &fakeSearcher{err: search.ErrUnauthorized}
	_, err := newClient(s, 10).ListJobs(context.Background(), "Chapa Financial Technologies")
	if !errors.Is(err, search.ErrUnauthorized) {
		t.Errorf("error = %v, want it to wrap search.ErrUnauthorized", err)
	}
}

func TestListJobs_TransientSearchErrorIsRetriedOnceAndEveryAttemptSpendsBudget(t *testing.T) {
	s := &fakeSearcher{failTimes: 1}
	c := newClient(s, 10)

	if _, err := c.ListJobs(context.Background(), "EthSwitch"); err != nil {
		t.Fatalf("ListJobs() error = %v, want the single transient failure retried", err)
	}
	// One failed attempt plus its retry: 2 requests, 2 budget units.
	if len(s.queries) != 2 || c.budget.Used() != 2 {
		t.Errorf("queries = %d, budget used = %d; want 2 and 2 (a failed request may still be billed)", len(s.queries), c.budget.Used())
	}
}

func TestListJobs_PersistentSearchErrorGivesUpAfterOneRetry(t *testing.T) {
	s := &fakeSearcher{failTimes: 100}
	c := newClient(s, 10)
	if _, err := c.ListJobs(context.Background(), "EthSwitch"); err == nil {
		t.Fatal("ListJobs() succeeded although every request failed")
	}
	if len(s.queries) != 2 {
		t.Errorf("made %d requests, want exactly 2 (first try + one retry)", len(s.queries))
	}
}

func TestListJobs_UnauthorizedIsNeverRetried(t *testing.T) {
	s := &fakeSearcher{err: search.ErrUnauthorized}
	c := newClient(s, 10)
	if _, err := c.ListJobs(context.Background(), "EthSwitch"); !errors.Is(err, search.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if len(s.queries) != 1 {
		t.Errorf("made %d requests after a 401/403, want 1 (a bad key will not fix itself)", len(s.queries))
	}
}

func TestListJobs_RetryDoesNotOutliveTheBudget(t *testing.T) {
	s := &fakeSearcher{failTimes: 100}
	c := newClient(s, 1)
	_, err := c.ListJobs(context.Background(), "EthSwitch")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("error = %v, want ErrBudgetExhausted (the retry must not exceed the cap)", err)
	}
	if len(s.queries) != 1 {
		t.Errorf("made %d requests with a budget of 1", len(s.queries))
	}
}

func TestListJobs_SerializesSearchRequestsAcrossCompanies(t *testing.T) {
	s := &fakeSearcher{}
	c := newClient(s, 100)
	var wg sync.WaitGroup
	for _, name := range []string{"Chapa Financial Technologies", "Gebeya Inc.", "Kifiya Financial Technology", "EthSwitch"} {
		wg.Go(func() { _, _ = c.ListJobs(context.Background(), name) })
	}
	wg.Wait()
	if s.maxSeen != 1 {
		t.Errorf("up to %d Serper requests were in flight at once, want 1", s.maxSeen)
	}
	if len(s.queries) != 4 {
		t.Errorf("made %d requests, want 4 (1 per company)", len(s.queries))
	}
}

func TestListJobs_ContextCancellationIsReturnedAndNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &fakeSearcher{err: context.Canceled}
	_, err := newClient(s, 10).ListJobs(ctx, "Chapa Financial Technologies")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if len(s.queries) != 1 {
		t.Errorf("made %d requests after cancellation, want 1 (no retry)", len(s.queries))
	}
}

func TestStaleAfterIsDeclared(t *testing.T) {
	if got := newClient(nil, 1).StaleAfter(); got != DefaultStaleAfter || got <= 0 {
		t.Errorf("StaleAfter() = %v, want %v", got, DefaultStaleAfter)
	}
}

func TestDedupeOpenings(t *testing.T) {
	jobs := []ats.Job{
		{ExternalID: "ethiojobs:1", Title: "Senior Engineer", LocationRaw: "Addis Ababa"},
		{ExternalID: "linkedin:1", Title: "senior engineer", LocationRaw: "Ethiopia"},        // same opening on the other site
		{ExternalID: "linkedin:2", Title: "Senior Engineer", LocationRaw: ""},                // location unknown: treated as Ethiopian, same opening
		{ExternalID: "linkedin:3", Title: "Senior Engineer", LocationRaw: "Nairobi, Kenya"},  // a different opening: another country
		{ExternalID: "linkedin:4", Title: "Senior Engineer", LocationRaw: "nairobi, kenya "}, // duplicate of the Nairobi one
		{ExternalID: "linkedin:5", Title: "Senior Engineer", LocationRaw: "Lagos, Nigeria"},  // a third opening
		{ExternalID: "ethiojobs:gone", Closed: true},                                         // closure markers are never dropped
		{ExternalID: "linkedin:gone", Closed: true},
		{ExternalID: "ethiojobs:2", Title: "Accountant", LocationRaw: "Addis Ababa"},
	}
	out := dedupeOpenings(jobs)
	if !slices.Equal(ids(out), ids(jobs)) {
		t.Fatalf("dedupeOpenings ids = %v, want every id kept (a dropped duplicate becomes a closure marker) in order %v", ids(out), ids(jobs))
	}
	wantClosed := map[string]bool{"linkedin:1": true, "linkedin:2": true, "linkedin:4": true, "ethiojobs:gone": true, "linkedin:gone": true}
	for _, j := range out {
		if j.Closed != wantClosed[j.ExternalID] {
			t.Errorf("%s: Closed = %t, want %t", j.ExternalID, j.Closed, wantClosed[j.ExternalID])
		}
		if j.Closed && (j.Title != "" || j.URL != "") {
			t.Errorf("%s: closure marker carries data: %+v", j.ExternalID, j)
		}
	}
}

func TestParseResultDate_OverflowingAgesAreRejectedNotWrapped(t *testing.T) {
	for _, in := range []string{"300 years ago", "5000 months ago", "999999 days ago", "101 years ago"} {
		if got := parseResultDate(in, fixedNow); !got.IsZero() {
			t.Errorf("parseResultDate(%q) = %v, want zero (older than a century)", in, got)
		}
	}
	if got := parseResultDate("99 years ago", fixedNow); got.IsZero() || got.After(fixedNow) {
		t.Errorf("parseResultDate(99 years ago) = %v, want a past date", got)
	}
}

// "Addis Software" has Addis in its own name, which every one of its results
// repeats. That must not count as an Ethiopia signal for a Nairobi job.
func TestLinkedInJob_ACompanyNamedAddisDoesNotVouchForAJobElsewhere(t *testing.T) {
	m := companyMatch{company: Company{Name: "Addis Software"}, keys: map[string]bool{nameKey("Addis Software"): true}}
	nairobi := li("https://www.linkedin.com/jobs/view/backend-engineer-at-addis-software-4470900012",
		"Addis Software hiring Backend Engineer in Nairobi, Kenya | LinkedIn",
		"Addis Software is hiring a Backend Engineer in Nairobi, Kenya.", "2 days ago")
	if job, reason := linkedInJob(nairobi, m, fixedNow); reason != "outside-ethiopia" {
		t.Errorf("Nairobi job accepted: reason %q, job %+v", reason, job)
	}
	addis := li("https://et.linkedin.com/jobs/view/backend-engineer-at-addis-software-4470900013",
		"Addis Software hiring Backend Engineer in Addis Ababa, Ethiopia | LinkedIn", "", "2 days ago")
	if _, reason := linkedInJob(addis, m, fixedNow); reason != "" {
		t.Errorf("Addis Ababa job rejected: %q", reason)
	}
}

// The same set of jobs must dedupe the same way whatever order search returns
// them in (SamePlace is not transitive: "Ethiopia" matches both cities).
func TestDedupeOpenings_DoesNotDependOnResultOrder(t *testing.T) {
	locs := []string{"Ethiopia", "Addis Ababa", "Hawassa"}
	perms := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, p := range perms {
		var in []ats.Job
		for _, i := range p {
			in = append(in, ats.Job{ExternalID: locs[i], Title: "Cashier", Employer: "Bank", LocationRaw: locs[i], URL: "u"})
		}
		open := map[string]bool{}
		for _, j := range dedupeOpenings(in) {
			if !j.Closed {
				open[j.ExternalID] = true
			}
		}
		if len(open) != 2 || !open["Addis Ababa"] || !open["Hawassa"] {
			t.Errorf("order %v: open = %v, want the two named cities kept and the city-less one dropped", p, open)
		}
	}
}
