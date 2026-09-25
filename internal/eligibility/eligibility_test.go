package eligibility_test

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/eligibility"
	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering"
	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/relevance"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimPrefix(u.Path, "/"), "_test") {
		t.Fatalf("TEST_DATABASE_URL must point at a database whose name ends in _test")
	}
	return raw
}

func newDB(t *testing.T) *database.DB {
	t.Helper()
	u := testDatabaseURL(t)
	if err := database.MigrateDown(context.Background(), u, migrations.FS); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}
	if err := database.MigrateUp(context.Background(), u, migrations.FS); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	t.Cleanup(func() { _ = database.MigrateDown(context.Background(), u, migrations.FS) })
	db, err := database.New(context.Background(), config.DatabaseConfig{URL: u, MaxConns: 5, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	db     *database.DB
	jobs   *job.Store
	target int64
	comp   int64
}

func newFixture(t *testing.T, name string, priority bool) fixture {
	t.Helper()
	db := newDB(t)
	ctx := context.Background()
	c, err := company.NewStore(db.Pool).Upsert(ctx, company.UpsertParams{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if priority {
		if _, err := db.Pool.Exec(ctx, `UPDATE companies SET is_priority = true WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	tg, err := company.NewTargetStore(db.Pool).Upsert(ctx, company.TargetUpsertParams{CompanyID: c.ID, ATSProvider: "greenhouse", ExternalBoardID: strings.ToLower(name)})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{db: db, jobs: job.NewStore(db.Pool), target: tg.ID, comp: c.ID}
}

func (f fixture) add(t *testing.T, id, title, location, remoteType, description string) {
	t.Helper()
	_, err := f.jobs.UpsertFromATS(context.Background(), job.Record{
		CompanyID: f.comp, TargetCompanyID: f.target, Source: "greenhouse", SourceJobID: id,
		Title: title, Description: description, ApplicationURL: "https://boards.greenhouse.io/x/jobs/" + id,
		LocationRaw: location, RemoteType: remoteType, PublishedAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("adding job %s: %v", id, err)
	}
}

func (f fixture) service(batch int) *eligibility.Service {
	return eligibility.NewService(eligibility.NewStore(f.db.Pool), filtering.NewClassifier(), relevance.Default(), "ET", batch, 2, quiet)
}

func (f fixture) counts(t *testing.T) map[string]int {
	t.Helper()
	m, err := eligibility.NewStore(f.db.Pool).Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRun_ClassifiesNewJobsOnceAndStoresWhatDecidedThem(t *testing.T) {
	f := newFixture(t, "Acme", false)
	f.add(t, "1", "Backend Engineer", "Worldwide", "remote", "Build things.")
	f.add(t, "2", "Sales Manager", "London, UK", "onsite", "Sell things.")
	f.add(t, "3", "Designer", "Remote", "remote", "Design things.")
	ctx := context.Background()

	st, err := f.service(200).Run(ctx, false)
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if st.Classified != 3 || st.Eligible != 1 || st.Ineligible != 1 || st.Uncertain != 1 || st.Failed != 0 {
		t.Errorf("stats = %+v, want 3 classified: 1 eligible, 1 ineligible, 1 uncertain", st)
	}

	var status, basis, family string
	var relevant bool
	var conf float64
	var evidence string
	if err := f.db.Pool.QueryRow(ctx, `SELECT e.status, e.basis, e.role_family, e.relevant, e.confidence::float8, e.evidence::text
		FROM job_eligibility e JOIN jobs j ON j.id = e.job_id WHERE j.source_job_id = '1'`).Scan(&status, &basis, &family, &relevant, &conf, &evidence); err != nil {
		t.Fatalf("reading the verdict: %v", err)
	}
	if status != "eligible" || basis != "worldwide" || family != "software_engineering" || !relevant || conf <= 0 || !strings.Contains(evidence, "Worldwide") {
		t.Errorf("verdict = %s/%s family %s relevant %t conf %.2f evidence %s; want eligible/worldwide, a relevant software role, with the phrase quoted", status, basis, family, relevant, conf, evidence)
	}

	// A second run finds nothing to do.
	st, err = f.service(200).Run(ctx, false)
	if err != nil || st.Classified != 0 {
		t.Errorf("second Run() = %+v, %v; want nothing to classify", st, err)
	}
}

func TestRun_ReclassifiesAJobWhoseTextChanged_AndOnlyThat(t *testing.T) {
	f := newFixture(t, "Acme", false)
	f.add(t, "1", "Backend Engineer", "Remote", "remote", "Build things.")
	f.add(t, "2", "Data Engineer", "Remote", "remote", "Build pipelines.")
	ctx := context.Background()
	if _, err := f.service(200).Run(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := f.counts(t); got["uncertain"] != 2 {
		t.Fatalf("counts = %v, want both uncertain (a bare Remote)", got)
	}

	f.add(t, "1", "Backend Engineer", "Remote", "remote", "Build things. This role can be remote anywhere in the world; candidates may be based anywhere.")
	st, err := f.service(200).Run(ctx, false)
	if err != nil || st.Classified != 1 {
		t.Fatalf("Run() = %+v, %v; want exactly the changed job classified", st, err)
	}
	if got := f.counts(t); got["eligible"] != 1 || got["uncertain"] != 1 {
		t.Errorf("counts = %v, want one eligible and one uncertain", got)
	}
}

func TestRun_ReclassifiesWhenTheRulesVersionOrTargetChanged_AndForce(t *testing.T) {
	f := newFixture(t, "Acme", false)
	f.add(t, "1", "Backend Engineer", "Worldwide", "remote", "x")
	ctx := context.Background()
	svc := f.service(200)
	if _, err := svc.Run(ctx, false); err != nil {
		t.Fatal(err)
	}

	if _, err := f.db.Pool.Exec(ctx, `UPDATE job_eligibility SET classifier_version = classifier_version - 1`); err != nil {
		t.Fatal(err)
	}
	if st, _ := svc.Run(ctx, false); st.Classified != 1 {
		t.Errorf("after a version change Classified = %d, want 1", st.Classified)
	}
	if _, err := f.db.Pool.Exec(ctx, `UPDATE job_eligibility SET target_country = 'KE'`); err != nil {
		t.Fatal(err)
	}
	if st, _ := svc.Run(ctx, false); st.Classified != 1 {
		t.Errorf("after a target change Classified = %d, want 1", st.Classified)
	}
	if st, _ := svc.Run(ctx, false); st.Classified != 0 {
		t.Errorf("nothing changed but Classified = %d", st.Classified)
	}
	if st, _ := svc.Run(ctx, true); st.Classified != 1 {
		t.Errorf("force: Classified = %d, want every open job (1)", st.Classified)
	}
}

func TestRun_WorksThroughBatchesWithoutRepeatingOrSkipping(t *testing.T) {
	f := newFixture(t, "Acme", false)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		f.add(t, id, "Backend Engineer", "Worldwide", "remote", "x")
	}
	st, err := f.service(2).Run(context.Background(), false) // batches of 2, 2, 1
	if err != nil || st.Classified != 5 || st.Eligible != 5 {
		t.Errorf("Run() = %+v, %v; want 5 classified", st, err)
	}
	if got := f.counts(t); got["eligible"] != 5 {
		t.Errorf("counts = %v", got)
	}
}

func TestRun_OnlyOpenJobsAreClassified(t *testing.T) {
	f := newFixture(t, "Acme", false)
	f.add(t, "1", "Backend Engineer", "Worldwide", "remote", "x")
	f.add(t, "2", "Backend Engineer", "Worldwide", "remote", "x")
	if _, err := f.db.Pool.Exec(context.Background(), `UPDATE jobs SET status = 'removed', closed_at = now() WHERE source_job_id = '2'`); err != nil {
		t.Fatal(err)
	}
	st, _ := f.service(200).Run(context.Background(), false)
	if st.Classified != 1 {
		t.Errorf("Classified = %d, want only the open job", st.Classified)
	}
}

func TestRun_APriorityCompanysJobIsClassifiedInTheEthiopianMarket(t *testing.T) {
	f := newFixture(t, "Kifiya", true)
	f.add(t, "1", "Backend Engineer", "", "unknown", "x") // no location, worldwide target: the priority company makes it Ethiopian
	if _, err := f.service(200).Run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	var status, basis string
	if err := f.db.Pool.QueryRow(context.Background(), `SELECT status, basis FROM job_eligibility`).Scan(&status, &basis); err != nil {
		t.Fatal(err)
	}
	if status != "eligible" || basis != "local_ethiopia" {
		t.Errorf("verdict = %s/%s, want eligible/local_ethiopia", status, basis)
	}
}

type panicRules struct{}

func (panicRules) Classify(filtering.Input) filtering.Verdict { panic("boom") }

func TestRun_ARulesPanicIsContainedAndTheJobStaysVisibleAsUncertain(t *testing.T) {
	f := newFixture(t, "Acme", false)
	f.add(t, "1", "Backend Engineer", "Worldwide", "remote", "x")
	svc := eligibility.NewService(eligibility.NewStore(f.db.Pool), panicRules{}, relevance.Default(), "ET", 10, 2, quiet)
	st, err := svc.Run(context.Background(), false)
	if err != nil || st.Failed != 1 || st.Uncertain != 1 || st.Eligible != 0 {
		t.Fatalf("Run() = %+v, %v; want the panic contained and the job stored uncertain", st, err)
	}
	var status, basis string
	if err := f.db.Pool.QueryRow(context.Background(), `SELECT status, basis FROM job_eligibility`).Scan(&status, &basis); err != nil {
		t.Fatal(err)
	}
	if status != "uncertain" || basis != "no_signal" {
		t.Errorf("verdict = %s/%s, want uncertain/no_signal", status, basis)
	}
}

func TestRun_StopsWhenCanceled(t *testing.T) {
	f := newFixture(t, "Acme", false)
	f.add(t, "1", "Backend Engineer", "Worldwide", "remote", "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.service(200).Run(ctx, false); err == nil {
		t.Errorf("Run() on a canceled context returned no error")
	}
}

func TestSave_SkipsAJobDeletedSinceItWasRead(t *testing.T) {
	f := newFixture(t, "Acme", false)
	store := eligibility.NewStore(f.db.Pool)
	err := store.Save(context.Background(), []eligibility.Row{{JobID: 999999, Target: "ET", Version: filtering.Version, ContentHash: "h",
		Verdict: filtering.Verdict{Status: filtering.Eligible, Confidence: 0.9, Basis: filtering.BasisWorldwide}, Role: relevance.Result{Family: "non_tech"}}})
	if err != nil {
		t.Errorf("Save() of a missing job = %v, want it skipped without error", err)
	}
	var n int
	_ = f.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM job_eligibility`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows stored for a job that does not exist", n)
	}
}

func TestJobEligibility_CascadesWithItsJobAndRejectsBadValues(t *testing.T) {
	f := newFixture(t, "Acme", false)
	f.add(t, "1", "Backend Engineer", "Worldwide", "remote", "x")
	ctx := context.Background()
	if _, err := f.service(200).Run(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx, `UPDATE job_eligibility SET status = 'maybe'`); err == nil {
		t.Errorf("a status outside eligible/ineligible/uncertain was accepted")
	}
	if _, err := f.db.Pool.Exec(ctx, `UPDATE job_eligibility SET confidence = 1.5`); err == nil {
		t.Errorf("a confidence above 1 was accepted")
	}
	if _, err := f.db.Pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.db.Pool.QueryRow(ctx, `SELECT count(*) FROM job_eligibility`).Scan(&n)
	if n != 0 {
		t.Errorf("%d verdicts left after their job was deleted", n)
	}
}

func TestSave_ReportsAWriteThatFailsInsteadOfPretendingItSucceeded(t *testing.T) {
	f := newFixture(t, "Acme", false)
	f.add(t, "1", "Backend Engineer", "Worldwide", "remote", "x")
	var id int64
	if err := f.db.Pool.QueryRow(context.Background(), `SELECT id FROM jobs`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// A confidence above 1 breaks the table's CHECK: the batch must fail, and nothing of it may be stored.
	err := eligibility.NewStore(f.db.Pool).Save(context.Background(), []eligibility.Row{
		{JobID: id, Target: "ET", Version: filtering.Version, ContentHash: "h", Verdict: filtering.Verdict{Status: filtering.Eligible, Confidence: 1.5, Basis: filtering.BasisWorldwide}, Role: relevance.Result{Family: "non_tech"}},
	})
	if err == nil {
		t.Fatal("Save() of a row that violates the table's constraints returned no error")
	}
	var n int
	_ = f.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM job_eligibility`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows stored from a failed batch", n)
	}
}
