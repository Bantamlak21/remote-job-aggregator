package priority

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

// The 25 companies exactly as Bantamlak listed them. The shipped config
// must contain every one of them, spelled the same way.
var requestedCompanies = []string{
	"Gebeya Inc.", "Chapa Financial Technologies", "Hisab Technologies", "EthSwitch", "Apposit",
	"Addis Software", "Wizard Labs", "4Africa Systems", "Belcash Technology Solutions",
	"Kifiya Financial Technology", "EagleLion System Technology", "Cybersoft PLC", "ERP Solutions PLC",
	"251 Technologies", "Zare Innovations", "ZEMEL IT SYSTEMS PLC", "iceaddis", "Parallel Solutions",
	"Ashewa Technology Solution", "Lelna AI", "Ahun", "Dalol Web Services", "360 Ground", "ZalaTech", "DreamTech",
}

func TestShippedConfig_ContainsEveryRequestedCompanyAndIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "ethiopian_companies.json"))
	if err != nil {
		t.Fatalf("Load() of the shipped config failed: %v", err)
	}
	var got []string
	for _, e := range cfg.Companies {
		got = append(got, e.Name)
	}
	if len(got) != len(requestedCompanies) {
		t.Errorf("config lists %d companies, want %d", len(got), len(requestedCompanies))
	}
	for _, want := range requestedCompanies {
		if !slices.Contains(got, want) {
			t.Errorf("config is missing %q", want)
		}
	}
	for _, name := range got {
		if !slices.Contains(requestedCompanies, name) {
			t.Errorf("config lists %q, which was not in the requested list", name)
		}
	}

	// Only Gebeya (verified: openings in Nairobi, Lagos, Dakar) is marked as
	// hiring outside Ethiopia; every other company keeps the strict default.
	for _, e := range cfg.Companies {
		if want := e.Name == "Gebeya Inc."; e.HiresOutsideEthiopia != want {
			t.Errorf("%s: hires_outside_ethiopia = %t, want %t", e.Name, e.HiresOutsideEthiopia, want)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return p
}

func TestLoad_Valid(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"companies":[
		{"name":"Acme","aliases":["Acme Inc"],"website":"https://acme.et","feeds":["https://acme.et/feed/"],"career_pages":["https://acme.et/careers"],"hires_outside_ethiopia":true},
		{"name":"Beta"}]}`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Companies) != 2 || cfg.Companies[0].Feeds[0] != "https://acme.et/feed/" || cfg.Companies[0].Aliases[0] != "Acme Inc" {
		t.Errorf("cfg = %+v", cfg)
	}
	if !cfg.Companies[0].HiresOutsideEthiopia || cfg.Companies[1].HiresOutsideEthiopia {
		t.Errorf("hires_outside_ethiopia = %t, %t; want true then false (the safe default)",
			cfg.Companies[0].HiresOutsideEthiopia, cfg.Companies[1].HiresOutsideEthiopia)
	}
}

func TestLoad_Rejections(t *testing.T) {
	cases := map[string]struct{ body, wantSubstr string }{
		"missing file is separate":   {"", ""},
		"not json":                   {`{oops`, "parsing"},
		"unknown field (typo)":       {`{"companies":[{"name":"A","carreer_pages":["https://a.et/c"]}]}`, "unknown field"},
		"unknown top-level field":    {`{"company":[]}`, "unknown field"},
		"no companies":               {`{"companies":[]}`, "no companies"},
		"blank name":                 {`{"companies":[{"name":"  "}]}`, "no name"},
		"duplicate name":             {`{"companies":[{"name":"Acme"},{"name":"ACME"}]}`, "listed twice"},
		"relative feed":              {`{"companies":[{"name":"A","feeds":["/feed"]}]}`, "not an absolute http(s) URL"},
		"non-http career page":       {`{"companies":[{"name":"A","career_pages":["ftp://a.et/c"]}]}`, "not an absolute http(s) URL"},
		"javascript website":         {`{"companies":[{"name":"A","website":"javascript:1"}]}`, "website"},
		"same feed under two":        {`{"companies":[{"name":"A","feeds":["https://x.et/f"]},{"name":"B","feeds":["https://X.et/f"]}]}`, "already used by"},
		"trailing garbage after doc": {`{"companies":[{"name":"A"}]} {"x":1}`, "unexpected data"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.body == "" {
				if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
					t.Fatal("Load() of a missing file succeeded")
				}
				return
			}
			_, err := Load(writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("Load() error = %v, want it to mention %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestValidate_ReportsEveryProblemNotJustTheFirst(t *testing.T) {
	err := Config{Companies: []Entry{
		{Name: ""},
		{Name: "A", Feeds: []string{"nope"}},
		{Name: "a"},
	}}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil")
	}
	for _, want := range []string{"no name", "not an absolute", "listed twice"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCoverage_MissingCompaniesCountInTheDenominator(t *testing.T) {
	cov := Coverage{Companies: []CompanyCoverage{
		{Name: "Kifiya", OpenJobs: 2},
		{Name: "EthSwitch", OpenJobs: 0},
	}}
	cov.NoteMissing([]string{"kifiya", " EthSwitch ", "Chapa", "Zare"})

	if !slices.Equal(cov.Missing, []string{"Chapa", "Zare"}) {
		t.Fatalf("Missing = %v, want [Chapa Zare] (matching is case- and space-insensitive)", cov.Missing)
	}
	if cov.Total() != 4 || cov.WithJobs() != 1 {
		t.Errorf("Total=%d WithJobs=%d, want 4 and 1", cov.Total(), cov.WithJobs())
	}
	var out strings.Builder
	if err := cov.Write(&out); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	for _, want := range []string{"Chapa", "NOT SEEDED", "1 of 4 priority companies"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}

	// Re-noting replaces, never accumulates.
	cov.NoteMissing([]string{"Kifiya"})
	if len(cov.Missing) != 0 {
		t.Errorf("Missing = %v after re-noting, want none", cov.Missing)
	}
}

// ---- Seed with fakes ----

type fakeCompanies struct {
	upserts   []company.UpsertParams
	priority  map[int64]bool
	failNames map[string]error
	nextID    int64
	ids       map[string]int64
}

func newFakeCompanies() *fakeCompanies {
	return &fakeCompanies{priority: map[int64]bool{}, failNames: map[string]error{}, ids: map[string]int64{}}
}

func (f *fakeCompanies) Upsert(_ context.Context, p company.UpsertParams) (*company.Company, error) {
	f.upserts = append(f.upserts, p)
	if err := f.failNames[p.Name]; err != nil {
		return nil, err
	}
	id, ok := f.ids[strings.ToLower(p.Name)]
	if !ok {
		f.nextID++
		id = f.nextID
		f.ids[strings.ToLower(p.Name)] = id
	}
	return &company.Company{ID: id, Name: p.Name}, nil
}

func (f *fakeCompanies) SetPriority(_ context.Context, id int64, priority bool) error {
	f.priority[id] = priority
	return nil
}

type fakeTargets struct {
	params  []company.TargetUpsertParams
	failFor map[string]error // key: provider/board
}

func (f *fakeTargets) Upsert(_ context.Context, p company.TargetUpsertParams) (*company.TargetCompany, error) {
	if err := f.failFor[p.ATSProvider+"/"+p.ExternalBoardID]; err != nil {
		return nil, err
	}
	f.params = append(f.params, p)
	return &company.TargetCompany{CompanyID: p.CompanyID, ATSProvider: p.ATSProvider, ExternalBoardID: p.ExternalBoardID}, nil
}

func TestSeed_RegistersCompanyPriorityAndEverySource(t *testing.T) {
	cfg := Config{Companies: []Entry{
		{Name: "Acme", Website: "https://acme.et", Feeds: []string{"https://acme.et/feed/"}, CareerPages: []string{"https://acme.et/careers", "https://acme.et/jobs"}},
		{Name: "Beta"},
	}}
	companies, targets := newFakeCompanies(), &fakeTargets{}

	res, err := Seed(context.Background(), cfg, companies, targets)
	if err != nil {
		t.Fatalf("Seed() error = %v", err)
	}
	if res.Companies != 2 || res.Targets != 5 {
		t.Errorf("result = %+v, want 2 companies and 5 targets (Acme: search+feed+2 pages, Beta: search)", res)
	}
	if !companies.priority[1] || !companies.priority[2] || len(companies.priority) != 2 {
		t.Errorf("priority flags = %v, want both companies flagged", companies.priority)
	}
	if companies.upserts[0].Website != "https://acme.et" {
		t.Errorf("website not passed through: %+v", companies.upserts[0])
	}

	type key struct {
		company  int64
		provider string
		board    string
		boardURL string
	}
	var got []key
	for _, p := range targets.params {
		got = append(got, key{p.CompanyID, p.ATSProvider, p.ExternalBoardID, p.BoardURL})
	}
	want := []key{
		{1, string(ats.ProviderSearch), "Acme", ""},
		{1, string(ats.ProviderFeed), "https://acme.et/feed/", "https://acme.et/feed/"},
		{1, string(ats.ProviderCareersSite), "https://acme.et/careers", "https://acme.et/careers"},
		{1, string(ats.ProviderCareersSite), "https://acme.et/jobs", "https://acme.et/jobs"},
		{2, string(ats.ProviderSearch), "Beta", ""},
	}
	if !slices.Equal(got, want) {
		t.Errorf("targets registered:\n got  %+v\n want %+v", got, want)
	}
	// Every source of an Ethiopian priority company belongs to the Ethiopian list.
	for _, p := range targets.params {
		if p.Market != market.Ethiopia {
			t.Errorf("target %s/%s seeded with market %q, want %q", p.ATSProvider, p.ExternalBoardID, p.Market, market.Ethiopia)
		}
	}
}

func TestSeed_IsIdempotent(t *testing.T) {
	cfg := Config{Companies: []Entry{{Name: "Acme"}}}
	companies, targets := newFakeCompanies(), &fakeTargets{}
	for range 3 {
		if _, err := Seed(context.Background(), cfg, companies, targets); err != nil {
			t.Fatalf("Seed() error = %v", err)
		}
	}
	if len(companies.ids) != 1 {
		t.Errorf("created %d distinct companies over three runs, want 1", len(companies.ids))
	}
}

func TestSeed_OneCompanysFailureDoesNotStopTheOthers(t *testing.T) {
	boom := errors.New("website already used by another company")
	cfg := Config{Companies: []Entry{{Name: "Bad"}, {Name: "Good"}, {Name: "AlsoGood"}}}
	companies := newFakeCompanies()
	companies.failNames["Bad"] = boom

	res, err := Seed(context.Background(), cfg, companies, &fakeTargets{})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "Bad") {
		t.Fatalf("Seed() error = %v, want it to wrap the failure and name the company", err)
	}
	if res.Companies != 2 {
		t.Errorf("Companies = %d, want the 2 healthy ones registered", res.Companies)
	}
}

func TestSeed_ATargetFailureIsReportedAndOtherTargetsStillRegister(t *testing.T) {
	boom := errors.New("target already registered to a different company")
	cfg := Config{Companies: []Entry{{Name: "Acme", Feeds: []string{"https://acme.et/feed/"}, CareerPages: []string{"https://acme.et/careers"}}}}
	targets := &fakeTargets{failFor: map[string]error{"feed/https://acme.et/feed/": boom}}

	res, err := Seed(context.Background(), cfg, newFakeCompanies(), targets)
	if !errors.Is(err, boom) {
		t.Fatalf("Seed() error = %v, want it to wrap the target failure", err)
	}
	if res.Targets != 2 || res.Companies != 0 {
		t.Errorf("result = %+v, want the other 2 targets registered and the company not counted as fully seeded", res)
	}
}

func TestSeed_StopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	companies := newFakeCompanies()
	_, err := Seed(ctx, Config{Companies: []Entry{{Name: "A"}, {Name: "B"}}}, companies, &fakeTargets{})
	if !errors.Is(err, context.Canceled) || len(companies.upserts) != 0 {
		t.Errorf("err = %v, upserts = %d; want context.Canceled before any write", err, len(companies.upserts))
	}
}
