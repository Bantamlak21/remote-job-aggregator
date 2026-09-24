package priority

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

// Same guard as internal/company's tests: these tests drop every table, so
// the database name must end in "_test". Run with `go test -p 1`.
func newDB(t *testing.T) *database.DB {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; run `docker compose up -d db` and export TEST_DATABASE_URL to run this test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL does not parse: %v", err)
	}
	if name := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(name, "_test") {
		t.Fatalf("TEST_DATABASE_URL points at %q; these tests drop tables and need a database ending in \"_test\"", name)
	}
	ctx := context.Background()
	if err := database.MigrateDown(ctx, raw, migrations.FS); err != nil {
		t.Fatalf("MigrateDown() failed: %v", err)
	}
	if err := database.MigrateUp(ctx, raw, migrations.FS); err != nil {
		t.Fatalf("MigrateUp() failed: %v", err)
	}
	t.Cleanup(func() {
		if err := database.MigrateDown(context.Background(), raw, migrations.FS); err != nil {
			t.Errorf("cleanup MigrateDown() failed: %v", err)
		}
	})
	db, err := database.New(ctx, config.DatabaseConfig{
		URL: raw, MaxConns: 5, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("database.New() failed: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestSeedAndCoverage_AgainstRealPostgres(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	companies, targets, jobs := company.NewStore(db.Pool), company.NewTargetStore(db.Pool), job.NewStore(db.Pool)

	cfg := Config{Companies: []Entry{
		{Name: "Kifiya Financial Technology", Website: "https://kifiya.com", CareerPages: []string{"https://kifiya.com/jobs/"}},
		{Name: "EthSwitch", Feeds: []string{"https://ethswitch.com/jobs/feed/"}},
		{Name: "Apposit"},
	}}

	// A regular company that must never appear in the report.
	if _, err := companies.Upsert(ctx, company.UpsertParams{Name: "Airbnb"}); err != nil {
		t.Fatalf("seeding a regular company: %v", err)
	}

	// Seeding twice is a no-op the second time.
	for range 2 {
		res, err := Seed(ctx, cfg, companies, targets)
		if err != nil {
			t.Fatalf("Seed() error = %v", err)
		}
		if res.Companies != 3 || res.Targets != 5 {
			t.Fatalf("Seed() = %+v, want 3 companies and 5 targets (search x3, careers page, feed)", res)
		}
	}
	var nCompanies, nTargets int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM companies), (SELECT count(*) FROM target_companies)`).Scan(&nCompanies, &nTargets); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if nCompanies != 4 || nTargets != 5 {
		t.Errorf("after two seeds: companies=%d targets=%d, want 4 and 5", nCompanies, nTargets)
	}

	// Two Kifiya jobs (one per source), one EthSwitch job, one job Kifiya
	// had that has since been closed, and a job for the regular company.
	kifiya, err := companies.GetByName(ctx, "Kifiya Financial Technology")
	if err != nil {
		t.Fatalf("GetByName() error = %v", err)
	}
	kifiyaSearch, err := targets.GetByProviderAndBoard(ctx, "search", "Kifiya Financial Technology")
	if err != nil {
		t.Fatalf("GetByProviderAndBoard(search) error = %v", err)
	}
	kifiyaPage, err := targets.GetByProviderAndBoard(ctx, "careers-site", "https://kifiya.com/jobs/")
	if err != nil {
		t.Fatalf("GetByProviderAndBoard(careers-site) error = %v", err)
	}
	ethswitch, err := companies.GetByName(ctx, "EthSwitch")
	if err != nil {
		t.Fatalf("GetByName() error = %v", err)
	}
	ethswitchSearch, err := targets.GetByProviderAndBoard(ctx, "search", "EthSwitch")
	if err != nil {
		t.Fatalf("GetByProviderAndBoard() error = %v", err)
	}
	airbnb, err := companies.GetByName(ctx, "Airbnb")
	if err != nil {
		t.Fatalf("GetByName() error = %v", err)
	}
	airbnbTarget, err := targets.Upsert(ctx, company.TargetUpsertParams{CompanyID: airbnb.ID, ATSProvider: "greenhouse", ExternalBoardID: "airbnb"})
	if err != nil {
		t.Fatalf("seeding airbnb target: %v", err)
	}

	rec := func(c *company.Company, tgt *company.TargetCompany, source, id string) job.Record {
		return job.Record{CompanyID: c.ID, TargetCompanyID: tgt.ID, Source: source, SourceJobID: id,
			Title: "Role " + id, ApplicationURL: "https://x.example/" + id}
	}
	expired := rec(ethswitch, ethswitchSearch, "search", "ethiojobs:expired")
	expired.ExpiresAt = time.Now().Add(-48 * time.Hour) // open in the table, but past its deadline
	for _, r := range []job.Record{
		expired,
		rec(kifiya, kifiyaSearch, "search", "linkedin:1"),
		rec(kifiya, kifiyaPage, "careers-site", "kifiya.com/jobs/a"),
		rec(kifiya, kifiyaPage, "careers-site", "kifiya.com/jobs/closed"),
		rec(ethswitch, ethswitchSearch, "search", "ethiojobs:9"),
		rec(airbnb, airbnbTarget, "greenhouse", "1"),
	} {
		if _, err := jobs.UpsertFromATS(ctx, r); err != nil {
			t.Fatalf("UpsertFromATS(%s) error = %v", r.SourceJobID, err)
		}
	}
	if _, err := jobs.MarkMissingAsRemoved(ctx, kifiyaPage.ID, []string{"kifiya.com/jobs/a"}); err != nil {
		t.Fatalf("MarkMissingAsRemoved() error = %v", err)
	}
	if err := targets.MarkIngestionSucceeded(ctx, kifiyaPage.ID); err != nil {
		t.Fatalf("MarkIngestionSucceeded() error = %v", err)
	}
	if err := targets.SetActive(ctx, ethswitchSearch.ID, false); err != nil {
		t.Fatalf("SetActive() error = %v", err)
	}

	cov, err := NewStore(db.Pool).Coverage(ctx)
	if err != nil {
		t.Fatalf("Coverage() error = %v", err)
	}

	if len(cov.Companies) != 3 {
		t.Fatalf("report has %d companies %+v, want the 3 priority ones (Airbnb excluded)", len(cov.Companies), cov.Companies)
	}
	byName := map[string]CompanyCoverage{}
	for _, c := range cov.Companies {
		byName[c.Name] = c
	}
	if _, ok := byName["Airbnb"]; ok {
		t.Errorf("a non-priority company is in the report")
	}
	if got := byName["Kifiya Financial Technology"]; got.OpenJobs != 2 || len(got.Sources) != 2 {
		t.Errorf("Kifiya = %+v, want 2 open jobs over 2 sources (the closed job is not counted)", got)
	}
	if got := byName["EthSwitch"]; got.OpenJobs != 1 || len(got.Targets) != 2 {
		t.Errorf("EthSwitch = %+v, want 1 open job (the one past its deadline is not counted) and 2 targets", got)
	}
	if got := byName["Apposit"]; got.OpenJobs != 0 || len(got.Targets) != 1 {
		t.Errorf("Apposit = %+v, want 0 open jobs and its search target", got)
	}
	if cov.WithJobs() != 2 || cov.TotalOpen() != 3 {
		t.Errorf("WithJobs=%d TotalOpen=%d, want 2 and 3", cov.WithJobs(), cov.TotalOpen())
	}

	var buf bytes.Buffer
	if err := cov.Write(&buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"COMPANY", "Kifiya Financial Technology", "careers-site=1 search=1",
		"search(inactive)",       // EthSwitch's deactivated search target
		"search(never ingested)", // Apposit
		"2 of 3 priority companies have at least one open job (3 open jobs in total)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	// Deterministic: the same data prints the same bytes.
	var again bytes.Buffer
	if err := cov.Write(&again); err != nil || again.String() != out {
		t.Errorf("Write() is not deterministic")
	}
}

func TestSeed_NeverReassignsASourceOwnedByAnotherCompany(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	companies, targets := company.NewStore(db.Pool), company.NewTargetStore(db.Pool)

	other, err := companies.Upsert(ctx, company.UpsertParams{Name: "Someone Else"})
	if err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if _, err := targets.Upsert(ctx, company.TargetUpsertParams{
		CompanyID: other.ID, ATSProvider: "feed", ExternalBoardID: "https://shared.et/feed/",
	}); err != nil {
		t.Fatalf("seeding the other company's feed: %v", err)
	}

	_, err = Seed(ctx, Config{Companies: []Entry{{Name: "Acme", Feeds: []string{"https://shared.et/feed/"}}}}, companies, targets)
	if err == nil {
		t.Fatal("Seed() succeeded although the feed belongs to another company")
	}
	stored, err := targets.GetByProviderAndBoard(ctx, "feed", "https://shared.et/feed/")
	if err != nil {
		t.Fatalf("GetByProviderAndBoard() error = %v", err)
	}
	if stored.CompanyID != other.ID {
		t.Errorf("feed now belongs to company %d, want it left with %d", stored.CompanyID, other.ID)
	}
}

func TestSeed_TheShippedConfigSeedsCleanlyIntoARealDatabase(t *testing.T) {
	db := newDB(t)
	cfg, err := Load(filepath.Join("..", "..", "configs", "ethiopian_companies.json"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	res, err := Seed(context.Background(), cfg, company.NewStore(db.Pool), company.NewTargetStore(db.Pool))
	if err != nil {
		t.Fatalf("Seed() of the shipped config failed: %v", err)
	}
	if res.Companies != 25 {
		t.Errorf("seeded %d companies, want 25", res.Companies)
	}
	var n int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM companies WHERE is_priority`).Scan(&n); err != nil || n != 25 {
		t.Errorf("priority companies = %d (err %v), want 25", n, err)
	}
}
