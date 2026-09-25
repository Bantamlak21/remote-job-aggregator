package database_test

import (
	"context"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

// 000003 closes the rows the per-company search source stored for Ethiojobs
// ("ethiojobs:<id>" under provider "search"), which the new Ethiojobs source
// supersedes. It must touch nothing else.
func TestMigration000003_ClosesOnlyLegacySearchEthiojobsRows(t *testing.T) {
	url := testDatabaseURL(t)
	ctx := context.Background()
	resetSchema(t, url)

	// Back to the schema as of 000002 (undo 000005, 000004 and 000003), then insert
	// rows the way the old code did.
	for range 3 {
		if err := database.MigrateDownStep(ctx, url, migrations.FS); err != nil {
			t.Fatalf("MigrateDownStep() failed: %v", err)
		}
	}
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	var companyID, searchTarget, ghTarget int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO companies (name) VALUES ('EthSwitch') RETURNING id`).Scan(&companyID); err != nil {
		t.Fatalf("inserting company: %v", err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO target_companies (company_id, ats_provider, external_board_id) VALUES ($1, 'search', 'EthSwitch') RETURNING id`, companyID).Scan(&searchTarget); err != nil {
		t.Fatalf("inserting search target: %v", err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO target_companies (company_id, ats_provider, external_board_id) VALUES ($1, 'greenhouse', 'ethswitch') RETURNING id`, companyID).Scan(&ghTarget); err != nil {
		t.Fatalf("inserting greenhouse target: %v", err)
	}
	insert := func(target int64, source, id string) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO jobs (company_id, target_company_id, source, source_job_id, title, application_url, content_hash)
			 VALUES ($1, $2, $3, $4, 'T', 'https://x.example', 'h')`, companyID, target, source, id); err != nil {
			t.Fatalf("inserting job %s/%s: %v", source, id, err)
		}
	}
	insert(searchTarget, "search", "ethiojobs:abc123")        // legacy: must be closed
	insert(searchTarget, "search", "linkedin:4470000001")     // LinkedIn via search: must stay open
	insert(ghTarget, "greenhouse", "ethiojobs:looks-similar") // another provider: must stay open

	if err := database.MigrateUp(ctx, url, migrations.FS); err != nil {
		t.Fatalf("MigrateUp() failed: %v", err)
	}

	status := func(source, id string) (s string, closed bool) {
		t.Helper()
		if err := db.Pool.QueryRow(ctx,
			`SELECT status, closed_at IS NOT NULL FROM jobs WHERE source = $1 AND source_job_id = $2`, source, id).Scan(&s, &closed); err != nil {
			t.Fatalf("reading %s/%s: %v", source, id, err)
		}
		return s, closed
	}
	if s, c := status("search", "ethiojobs:abc123"); s != "removed" || !c {
		t.Errorf("legacy ethiojobs row: status=%s closed_at set=%t, want removed with closed_at", s, c)
	}
	if s, _ := status("search", "linkedin:4470000001"); s != "open" {
		t.Errorf("LinkedIn row status = %s, want open", s)
	}
	if s, _ := status("greenhouse", "ethiojobs:looks-similar"); s != "open" {
		t.Errorf("greenhouse row status = %s, want open (only provider 'search' rows are legacy)", s)
	}
}
