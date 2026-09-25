package database_test

import (
	"context"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

// 000004 puts every pre-existing non-ATS target in the Ethiopian market; ATS
// boards stay worldwide, even those of a priority company (the market follows
// the source, not the company).
func TestMigration000004_AssignsTheMarketOfExistingTargets(t *testing.T) {
	url := testDatabaseURL(t)
	ctx := context.Background()
	resetSchema(t, url)

	// Back to the schema as of 000003.
	if err := database.MigrateDownStep(ctx, url, migrations.FS); err != nil {
		t.Fatalf("MigrateDownStep() failed: %v", err)
	}
	db, err := database.New(ctx, dbConfig(url))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer db.Close()

	company := func(name string, priority bool) int64 {
		t.Helper()
		var id int64
		if err := db.Pool.QueryRow(ctx, `INSERT INTO companies (name, is_priority) VALUES ($1, $2) RETURNING id`, name, priority).Scan(&id); err != nil {
			t.Fatalf("inserting company %s: %v", name, err)
		}
		return id
	}
	target := func(companyID int64, provider, board string) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO target_companies (company_id, ats_provider, external_board_id) VALUES ($1, $2, $3)`, companyID, provider, board); err != nil {
			t.Fatalf("inserting target %s/%s: %v", provider, board, err)
		}
	}
	airbnb, eth := company("Airbnb", false), company("EthSwitch", true)
	target(airbnb, "greenhouse", "airbnb")
	target(eth, "greenhouse", "ethswitch") // a priority company's ATS board: still worldwide
	target(eth, "search", "EthSwitch")
	target(eth, "feed", "https://ethswitch.example/feed")
	target(eth, "careers-site", "https://ethswitch.example/jobs")
	target(airbnb, "ethiojobs", "airbnb ethiopia")
	target(airbnb, "linkedin", "airbnb")

	if err := database.MigrateUp(ctx, url, migrations.FS); err != nil {
		t.Fatalf("MigrateUp() failed: %v", err)
	}

	want := map[string]string{
		"greenhouse/airbnb":                           "worldwide",
		"greenhouse/ethswitch":                        "worldwide",
		"search/EthSwitch":                            "ethiopia",
		"feed/https://ethswitch.example/feed":         "ethiopia",
		"careers-site/https://ethswitch.example/jobs": "ethiopia",
		"ethiojobs/airbnb ethiopia":                   "ethiopia",
		"linkedin/airbnb":                             "ethiopia",
	}
	rows, err := db.Pool.Query(ctx, `SELECT ats_provider || '/' || external_board_id, market FROM target_companies`)
	if err != nil {
		t.Fatalf("reading targets: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var k, m string
		if err := rows.Scan(&k, &m); err != nil {
			t.Fatal(err)
		}
		got[k] = m
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("target %s: market = %q, want %q", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("read %d targets, want %d", len(got), len(want))
	}

	// The column rejects anything but the two markets.
	if _, err := db.Pool.Exec(ctx, `UPDATE target_companies SET market = 'mars'`); err == nil {
		t.Errorf("market = 'mars' was accepted by the CHECK constraint")
	}
}
