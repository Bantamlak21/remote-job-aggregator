package jobsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
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

type fakePages struct {
	mu      sync.Mutex
	html    map[string]string
	errs    map[string]error
	fetched []string
}

func (f *fakePages) Fetch(_ context.Context, rawURL string) (*page.Page, error) {
	f.mu.Lock()
	f.fetched = append(f.fetched, rawURL)
	f.mu.Unlock()
	if err := f.errs[rawURL]; err != nil {
		return nil, err
	}
	body, ok := f.html[rawURL]
	if !ok {
		return nil, fmt.Errorf("fake: %s: %w", rawURL, page.ErrNotFound)
	}
	return page.Parse(rawURL, []byte(body))
}

func newClient(s Searcher, p Pages, budget int) *Client {
	c := New(s, p, []Company{
		{Name: "Chapa Financial Technologies", Aliases: []string{"Chapa"}},
		{Name: "Gebeya Inc.", HiresOutsideEthiopia: true},
		{Name: "DreamTech"},
		{Name: "Kifiya Financial Technology", Aliases: []string{"Kifiya Financial Technologies", "Kifiya"}},
		{Name: "EthSwitch"},
	}, NewBudget(budget), discardLogger())
	c.now = func() time.Time { return fixedNow }
	c.retryDelay = 0
	c.pagePause = 0
	return c
}

// matchFor returns the fixture client's match set for a primary company
// name. It panics on an unknown name: a typo here would hand back an empty
// match set, and every "must be rejected" test would then pass vacuously.
func matchFor(name string) companyMatch {
	m, ok := newClient(nil, nil, 1).companies[nameKey(name)]
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

func TestNameKey(t *testing.T) {
	cases := map[string]string{
		"Ethswitch S.C.":                  "ethswitch",
		"EthSwitch":                       "ethswitch",
		"Kifiya Financial Technology PLC": "kifiya-financial-technology",
		"Gebeya Inc.":                     "gebeya",
		"Gebeya":                          "gebeya",
		"Chaka Gebeya":                    "chaka-gebeya",
		"Chapa De Indian Health":          "chapa-de-indian-health",
		"Chapa":                           "chapa",
		"4Africa Systems":                 "4africa-systems",
		"251 Technologies":                "251-technologies",
		"Ethiopia":                        "ethiopia", // never strip down to nothing
		"PLC":                             "plc",
		"":                                "",
		"  Zare  Innovations  ":           "zare-innovations",
	}
	for in, want := range cases {
		if got := nameKey(in); got != want {
			t.Errorf("nameKey(%q) = %q, want %q", in, got, want)
		}
	}
}

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
			li("https://et.linkedin.com/jobs/view/product-manager-payment-solutions-at-chapa-3945687335",
				"Product Manager - Payment Solutions at Chapa - LinkedIn Ethiopia",
				"Apply for Product Manager - Payment Solutions at Chapa in Ethiopia. Full-time Not Applicable role. See responsibilities...",
				"5 days ago"),
			"linkedin:3945687335", "Product Manager - Payment Solutions", "Ethiopia",
		},
		{
			"hiring-style title keeps punctuation the slug loses",
			li("https://et.linkedin.com/jobs/view/ui-ux-designer-at-chapa-3945476980",
				"Chapa hiring UI/UX Designer in Ethiopia | LinkedIn",
				"Chapa Ethiopia. 2 weeks ago Be among the first 25 applicants.", "2 weeks ago"),
			"linkedin:3945476980", "UI/UX Designer", "Ethiopia",
		},
		{
			"www host and a tracking query string",
			li("https://www.linkedin.com/jobs/view/senior-accountant-at-chapa-4166950109?trk=public_jobs&position=1#x",
				"Senior Accountant at Chapa - LinkedIn Ethiopia",
				"Apply for Senior Accountant at Chapa in Addis Ababa, Addis Ababa, Ethiopia. Full-time Mid-Senior level role.", "Sep 14, 2026"),
			"linkedin:4166950109", "Senior Accountant", "Addis Ababa, Addis Ababa, Ethiopia",
		},
		{
			"truncated result title falls back to the slug",
			li("https://et.linkedin.com/jobs/view/data-analyst-at-chapa-financial-technologies-s-c-3945684459",
				"...", "", ""),
			"linkedin:3945684459", "Data Analyst", "",
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
	kenya := li("https://ke.linkedin.com/jobs/view/talent-specialist-job-matching-at-gebeya-inc-3633345749",
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
	job, reason := linkedInJob(li("https://et.linkedin.com/jobs/view/engineer-at-scale-at-chapa-1234567",
		"Engineer at Scale at Chapa - LinkedIn", "", "1 day ago"), m, fixedNow)
	if reason != "" || job.Title != "Engineer at Scale" {
		t.Errorf("job = %+v, reason = %q; want title %q", job, reason, "Engineer at Scale")
	}
}

func TestLinkedInJob_FreshnessRules(t *testing.T) {
	m := matchFor("Chapa Financial Technologies")
	const u = "https://et.linkedin.com/jobs/view/data-analyst-at-chapa-3945684459"
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
			if tc.want == "closed" {
				// Not a rejection: the result reports the job as ended, so it
				// is returned as a closure for ingestion to act on.
				if reason != "" || !job.Closed || job.ExternalID != "linkedin:3945684459" || job.Title != "" {
					t.Errorf("job = %+v, reason = %q; want a bare closure marker for linkedin:3945684459", job, reason)
				}
				return
			}
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
			if reason == "" && job.Closed {
				t.Errorf("a live job was marked closed: %+v", job)
			}
			if tc.want == "" && tc.date == "3 days ago" && !job.PublishedAt.Equal(fixedNow.Add(-72*time.Hour)) {
				t.Errorf("PublishedAt = %v, want the parsed result date", job.PublishedAt)
			}
		})
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

// ---- Ethiojobs ----

func ejPage(status, published, expiry, company, title string) string {
	data := map[string]any{"props": map[string]any{"pageProps": map[string]any{"data": map[string]any{
		"title": title, "description": "<p>Build <b>things</b>.</p>", "requirement": "<ul><li>Go</li></ul>",
		"how_to_apply": "<p>Email hr@example.et</p>", "status": status, "date_published": published,
		"date_expiry": expiry, "city": "Addis Ababa", "state": "Addis Ababa",
		"company": map[string]any{"name": company},
	}}}}
	b, _ := json.Marshal(data)
	return `<html><head><title>x</title></head><body><script id="__NEXT_DATA__" type="application/json">` + string(b) + `</script></body></html>`
}

func TestEthiojobsJob_CurrentPostingIsAccepted(t *testing.T) {
	m := matchFor("EthSwitch")
	p, _ := page.Parse("https://ethiojobs.net/job/Ubd2cAJy92-senior-compliance-officer",
		[]byte(ejPage("active", "2026-09-23T10:22:18.000000Z", "2026-09-30T23:59:59.000000Z", "Ethswitch S.C.", "Senior Compliance Officer Re- Advertised")))

	job, reason := ethiojobsJob(p, "Ubd2cAJy92", p.URL, m, fixedNow)
	if reason != "" {
		t.Fatalf("rejected (%s), want accepted", reason)
	}
	if job.ExternalID != "ethiojobs:Ubd2cAJy92" || job.Title != "Senior Compliance Officer Re- Advertised" {
		t.Errorf("job = %+v", job)
	}
	if job.LocationRaw != "Addis Ababa" {
		t.Errorf("LocationRaw = %q, want the city/state without duplication", job.LocationRaw)
	}
	if !job.PublishedAt.Equal(time.Date(2026, 9, 23, 10, 22, 18, 0, time.UTC)) {
		t.Errorf("PublishedAt = %v", job.PublishedAt)
	}
	for _, want := range []string{"Build things.", "Go", "hr@example.et"} {
		if !strings.Contains(job.Description, want) {
			t.Errorf("Description = %q, missing %q", job.Description, want)
		}
	}
	if strings.Contains(job.Description, "<") {
		t.Errorf("Description still has HTML: %q", job.Description)
	}
}

func TestEthiojobsJob_Rejections(t *testing.T) {
	m := matchFor("Kifiya Financial Technology")
	const good, exp = "2026-09-20T00:00:00.000000Z", "2026-10-20T00:00:00.000000Z"
	cases := []struct {
		name string
		html string
		want string
	}{
		{"closed (real Kifiya posting from 2025)", ejPage("closed", "2025-08-13T12:31:42.000000Z", "2025-08-20T23:59:59.000000Z", "Kifiya Financial Technologies", "Credit Risk"), "CLOSED"},
		{"closed status even with a future expiry", ejPage("closed", good, exp, "Kifiya Financial Technologies", "X"), "CLOSED"},
		{"unknown status is not trusted", ejPage("draft", good, exp, "Kifiya Financial Technologies", "X"), "not-active"},
		{"empty status", ejPage("", good, exp, "Kifiya Financial Technologies", "X"), "not-active"},
		{"active but expired yesterday", ejPage("active", "2026-09-10T00:00:00Z", "2026-09-23T23:59:59Z", "Kifiya Financial Technologies", "X"), "CLOSED"},
		{"active, expires exactly now", ejPage("active", good, "2026-09-24T12:00:00Z", "Kifiya Financial Technologies", "X"), "CLOSED"},
		{"no expiry and old", ejPage("active", "2026-01-01T00:00:00Z", "", "Kifiya Financial Technologies", "X"), "no-expiry-and-old"},
		{"no expiry and no publish date", ejPage("active", "", "", "Kifiya Financial Technologies", "X"), "no-expiry-and-old"},
		{"another company on the same site", ejPage("active", good, exp, "Qena Software Design & Development PLC", "X"), "other-company"},
		{"company name only similar", ejPage("active", good, exp, "Kifiya Bank", "X"), "other-company"},
		{"blank title", ejPage("active", good, exp, "Kifiya Financial Technologies", "   "), "no-title"},
		{"no embedded data", `<html><body>Hello</body></html>`, "no-data"},
		{"embedded data is not JSON", `<script id="__NEXT_DATA__">{oops</script>`, "bad-data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := page.Parse("https://ethiojobs.net/job/abcdef1234-x", []byte(tc.html))
			job, reason := ethiojobsJob(p, "abcdef1234", p.URL, m, fixedNow)
			if tc.want == "CLOSED" {
				if reason != "" || !job.Closed || job.ExternalID != "ethiojobs:abcdef1234" {
					t.Errorf("job = %+v, reason = %q; want a closure marker for ethiojobs:abcdef1234", job, reason)
				}
				return
			}
			if reason != tc.want {
				t.Errorf("reason = %q (job %+v), want %q", reason, job, tc.want)
			}
		})
	}
}

func TestEthiojobsJob_AliasCompanyNameMatches(t *testing.T) {
	m := matchFor("Kifiya Financial Technology")
	p, _ := page.Parse("https://ethiojobs.net/job/abcdef1234-x",
		[]byte(ejPage("active", "2026-09-20T00:00:00Z", "2026-10-20T00:00:00Z", "Kifiya Financial Technologies", "Manager")))
	if _, reason := ethiojobsJob(p, "abcdef1234", p.URL, m, fixedNow); reason != "" {
		t.Errorf("alias 'Kifiya Financial Technologies' rejected: %s", reason)
	}
}

// ---- ListJobs end to end (fakes) ----

func TestListJobs_CombinesBothSitesAndDedupesByTitle(t *testing.T) {
	s := &fakeSearcher{bySite: map[string][]search.Result{
		"linkedin.com/jobs/view": {
			li("https://et.linkedin.com/jobs/view/senior-compliance-officer-at-ethswitch-4400000001", "Senior Compliance Officer at EthSwitch - LinkedIn", "", "2 days ago"),
			li("https://et.linkedin.com/jobs/view/driver-at-ethswitch-4400000002", "Driver at EthSwitch - LinkedIn", "", "3 days ago"),
			li("https://et.linkedin.com/jobs/view/analyst-at-some-other-bank-4400000003", "Analyst at Some Other Bank", "", "1 day ago"),
		},
		"ethiojobs.net/job": {
			li("https://ethiojobs.net/job/Ubd2cAJy92-senior-compliance-officer", "Senior Compliance Officer", "", ""),
			li("https://ethiojobs.net/companies/ethswitch-sc", "Ethswitch S.C. Jobs and Vacancies", "", ""),
			li("https://ethiojobs.net/job/xBLzjWk2fj-network-manager", "Network Manager", "", ""),
		},
	}}
	p := &fakePages{html: map[string]string{
		"https://ethiojobs.net/job/Ubd2cAJy92-senior-compliance-officer": ejPage("active", "2026-09-23T10:22:18Z", "2026-09-30T23:59:59Z", "Ethswitch S.C.", "Senior Compliance Officer"),
		"https://ethiojobs.net/job/xBLzjWk2fj-network-manager":           ejPage("closed", "2026-09-16T10:44:57Z", "2026-09-23T23:59:59Z", "Ethswitch S.C.", "Network Manager"),
	}}
	c := newClient(s, p, 10)

	jobs, err := c.ListJobs(context.Background(), "ethswitch")
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}

	got := map[string]ats.Job{}
	for _, j := range jobs {
		got[j.ExternalID] = j
	}
	if len(jobs) != 4 {
		t.Fatalf("got %d jobs %v, want 4: the Ethiojobs compliance officer, the LinkedIn driver, a closure "+
			"marker for the ended network-manager posting and a closure marker for the LinkedIn duplicate of "+
			"the compliance officer (the other bank and the company page are rejected)",
			len(jobs), ids(jobs))
	}
	if dup := got["linkedin:4400000001"]; !dup.Closed {
		t.Errorf("the LinkedIn duplicate = %+v, want a closure marker so an earlier-stored copy does not show twice", dup)
	}
	if ended := got["ethiojobs:xBLzjWk2fj"]; !ended.Closed {
		t.Errorf("the closed Ethiojobs posting = %+v, want a closure marker so a stored copy is closed at once", ended)
	}
	for _, id := range []string{"ethiojobs:Ubd2cAJy92", "linkedin:4400000002"} {
		if got[id].Closed || got[id].Title == "" {
			t.Errorf("%s = %+v, want a live job", id, got[id])
		}
	}
	if _, ok := got["ethiojobs:Ubd2cAJy92"]; !ok {
		t.Errorf("missing the Ethiojobs job; got %v (Ethiojobs' richer record must win the duplicate)", ids(jobs))
	}
	if _, ok := got["linkedin:4400000002"]; !ok {
		t.Errorf("missing the LinkedIn driver job; got %v", ids(jobs))
	}

	// Two queries, month-limited, one per site, quoted company name.
	if len(s.queries) != 2 {
		t.Fatalf("queries = %v, want exactly 2", s.queries)
	}
	for i, q := range s.queries {
		if !strings.Contains(q, `"EthSwitch"`) {
			t.Errorf("query %d = %q, want the quoted company name", i, q)
		}
		if s.recency[i] != search.RecencyMonth {
			t.Errorf("query %d recency = %q, want month", i, s.recency[i])
		}
	}
	// Only the two job-shaped Ethiojobs URLs were fetched; never LinkedIn,
	// never the company page.
	for _, u := range p.fetched {
		if strings.Contains(u, "linkedin") || strings.Contains(u, "/companies/") {
			t.Errorf("fetched %s; LinkedIn and non-job pages must never be fetched", u)
		}
	}
	if len(p.fetched) != 2 {
		t.Errorf("fetched %v, want the 2 job pages", p.fetched)
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
	c := newClient(s, &fakePages{}, 10)
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
	c := newClient(&fakeSearcher{}, &fakePages{}, 10)
	for _, name := range []string{"Gebeya Inc.", "gebeya", " GEBEYA ", "Gebeya Inc"} {
		if _, err := c.ListJobs(context.Background(), name); err != nil {
			t.Errorf("ListJobs(%q) error = %v, want it to resolve to the Gebeya entry", name, err)
		}
	}
}

func TestListJobs_BudgetIsEnforcedAndCounted(t *testing.T) {
	s := &fakeSearcher{}
	c := newClient(s, &fakePages{}, 3)

	if _, err := c.ListJobs(context.Background(), "Chapa Financial Technologies"); err != nil {
		t.Fatalf("first ListJobs() error = %v", err)
	}
	if c.budget.Used() != 2 {
		t.Fatalf("budget used = %d after one company, want 2", c.budget.Used())
	}
	// Needs 2 more, only 1 left: the LinkedIn query spends it, the second
	// query is refused, and the whole company fails rather than returning half.
	_, err := c.ListJobs(context.Background(), "Kifiya Financial Technology")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second ListJobs() error = %v, want ErrBudgetExhausted", err)
	}
	if c.budget.Used() != 3 || len(s.queries) != 3 {
		t.Errorf("budget used = %d, queries sent = %d; want exactly the limit (3), never more", c.budget.Used(), len(s.queries))
	}
	// Fully spent: no query at all reaches the network.
	if _, err := c.ListJobs(context.Background(), "Gebeya Inc."); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("third ListJobs() error = %v, want ErrBudgetExhausted", err)
	}
	if len(s.queries) != 3 {
		t.Errorf("queries sent = %d after exhaustion, want still 3", len(s.queries))
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
	_, err := newClient(s, &fakePages{}, 10).ListJobs(context.Background(), "Chapa Financial Technologies")
	if !errors.Is(err, search.ErrUnauthorized) {
		t.Errorf("error = %v, want it to wrap search.ErrUnauthorized", err)
	}
}

func TestListJobs_TransientSearchErrorIsRetriedOnceAndEveryAttemptSpendsBudget(t *testing.T) {
	s := &fakeSearcher{failTimes: 1}
	c := newClient(s, &fakePages{}, 10)

	if _, err := c.ListJobs(context.Background(), "EthSwitch"); err != nil {
		t.Fatalf("ListJobs() error = %v, want the single transient failure retried", err)
	}
	// LinkedIn: fail + retry, Ethiojobs: 1 = 3 requests, 3 budget units.
	if len(s.queries) != 3 || c.budget.Used() != 3 {
		t.Errorf("queries = %d, budget used = %d; want 3 and 3 (a failed request may still be billed)", len(s.queries), c.budget.Used())
	}
}

func TestListJobs_PersistentSearchErrorGivesUpAfterOneRetry(t *testing.T) {
	s := &fakeSearcher{failTimes: 100}
	c := newClient(s, &fakePages{}, 10)
	if _, err := c.ListJobs(context.Background(), "EthSwitch"); err == nil {
		t.Fatal("ListJobs() succeeded although every request failed")
	}
	if len(s.queries) != 2 {
		t.Errorf("made %d requests, want exactly 2 (first try + one retry)", len(s.queries))
	}
}

func TestListJobs_UnauthorizedIsNeverRetried(t *testing.T) {
	s := &fakeSearcher{err: search.ErrUnauthorized}
	c := newClient(s, &fakePages{}, 10)
	if _, err := c.ListJobs(context.Background(), "EthSwitch"); !errors.Is(err, search.ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if len(s.queries) != 1 {
		t.Errorf("made %d requests after a 401/403, want 1 (a bad key will not fix itself)", len(s.queries))
	}
}

func TestListJobs_RetryDoesNotOutliveTheBudget(t *testing.T) {
	s := &fakeSearcher{failTimes: 100}
	c := newClient(s, &fakePages{}, 1)
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
	c := newClient(s, &fakePages{}, 100)
	var wg sync.WaitGroup
	for _, name := range []string{"Chapa Financial Technologies", "Gebeya Inc.", "Kifiya Financial Technology", "EthSwitch"} {
		wg.Go(func() { _, _ = c.ListJobs(context.Background(), name) })
	}
	wg.Wait()
	if s.maxSeen != 1 {
		t.Errorf("up to %d Serper requests were in flight at once, want 1", s.maxSeen)
	}
	if len(s.queries) != 8 {
		t.Errorf("made %d requests, want 8 (2 per company)", len(s.queries))
	}
}

func TestListJobs_EthiojobsFetchFailuresSkipTheResultNotTheRun(t *testing.T) {
	s := &fakeSearcher{bySite: map[string][]search.Result{
		"ethiojobs.net/job": {
			li("https://ethiojobs.net/job/aaaaaaaaaa-one", "One", "", ""),
			li("https://ethiojobs.net/job/bbbbbbbbbb-two", "Two", "", ""),
			li("https://ethiojobs.net/job/cccccccccc-three", "Three", "", ""),
		},
	}}
	p := &fakePages{
		errs: map[string]error{
			"https://ethiojobs.net/job/aaaaaaaaaa-one": page.ErrDisallowed,
			"https://ethiojobs.net/job/bbbbbbbbbb-two": errors.New("timeout"),
		},
		html: map[string]string{
			"https://ethiojobs.net/job/cccccccccc-three": ejPage("active", "2026-09-20T00:00:00Z", "2026-10-20T00:00:00Z", "Gebeya Developer As A Service", "Three"),
		},
	}
	// "Gebeya Developer As A Service" is not "Gebeya": also proves the
	// page-side company check runs on the fetched page.
	jobs, err := newClient(s, p, 10).ListJobs(context.Background(), "Gebeya Inc.")
	if err != nil {
		t.Fatalf("ListJobs() error = %v, want the per-page failures skipped", err)
	}
	if len(jobs) != 0 {
		t.Errorf("jobs = %v, want none", ids(jobs))
	}
}

func TestListJobs_CapsEthiojobsPageFetches(t *testing.T) {
	var results []search.Result
	for i := range 25 {
		results = append(results, li(fmt.Sprintf("https://ethiojobs.net/job/id%08d-role-%d", i, i), "r", "", ""))
	}
	// The real Serper client returns at most 10 results, but the cap must hold
	// whatever a Searcher hands back.
	s := &fakeSearcher{bySite: map[string][]search.Result{"ethiojobs.net/job": results}}
	p := &fakePages{}
	c := newClient(s, p, 10)
	c.ListJobs(context.Background(), "Chapa Financial Technologies") //nolint:errcheck
	if len(p.fetched) > maxEthiojobsFetches {
		t.Errorf("fetched %d pages, cap is %d", len(p.fetched), maxEthiojobsFetches)
	}
}

func TestListJobs_ContextCancellationIsReturned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakeSearcher{bySite: map[string][]search.Result{
		"ethiojobs.net/job": {li("https://ethiojobs.net/job/aaaaaaaaaa-one", "One", "", "")},
	}}
	p := &fakePages{errs: map[string]error{"https://ethiojobs.net/job/aaaaaaaaaa-one": context.Canceled}}
	cancel()
	if _, err := newClient(s, p, 10).ListJobs(ctx, "Chapa Financial Technologies"); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled (not silently skipped)", err)
	}
}

func TestStaleAfterIsDeclared(t *testing.T) {
	if got := newClient(nil, nil, 1).StaleAfter(); got != DefaultStaleAfter || got <= 0 {
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

func TestTitleKey(t *testing.T) {
	distinct := []string{"C# Developer", "C++ Developer", "C Developer", "Senior Accountant", "የሂሳብ ባለሙያ", "የሽያጭ ባለሙያ"}
	seen := map[string]string{}
	for _, title := range distinct {
		k := titleKey(title)
		if k == "" {
			t.Errorf("titleKey(%q) is empty", title)
		}
		if other, dup := seen[k]; dup {
			t.Errorf("titleKey(%q) == titleKey(%q) == %q; distinct titles collapsed", title, other, k)
		}
		seen[k] = title
	}
	same := [][2]string{
		{"Senior  Engineer", "senior engineer"},
		{"Senior Engineer!", "SENIOR ENGINEER"},
		{"Re- Advertised", "Re-Advertised"},
	}
	for _, p := range same {
		if titleKey(p[0]) != titleKey(p[1]) {
			t.Errorf("titleKey(%q) = %q != titleKey(%q) = %q", p[0], titleKey(p[0]), p[1], titleKey(p[1]))
		}
	}
}

func TestListJobs_PageFetchesAreSpaced(t *testing.T) {
	var results []search.Result
	for i := range 3 {
		results = append(results, li(fmt.Sprintf("https://ethiojobs.net/job/id%08d-role-%d", i, i), "r", "", ""))
	}
	s := &fakeSearcher{bySite: map[string][]search.Result{"ethiojobs.net/job": results}}
	c := newClient(s, &fakePages{}, 10)
	c.pagePause = 40 * time.Millisecond

	start := time.Now()
	if _, err := c.ListJobs(context.Background(), "EthSwitch"); err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	// 3 fetches => 2 pauses of 40ms.
	if elapsed := time.Since(start); elapsed < 75*time.Millisecond {
		t.Errorf("3 page fetches took %v, want at least 2 pauses of 40ms", elapsed)
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

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"UI/UX Designer":             "ui-ux-designer",
		"  Hello,   World!  ":        "hello-world",
		"Frontend Developer (React)": "frontend-developer-react",
		"Écrivain":                   "crivain",
		"":                           "",
		"---":                        "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
