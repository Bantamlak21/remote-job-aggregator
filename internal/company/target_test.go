package company_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
)

// seedCompany creates the parent row a target needs: target_companies
// has a real foreign key to companies, so there is no way to test this
// store without one.
func seedCompany(t *testing.T, ctx context.Context, db *database.DB, name string) *company.Company {
	t.Helper()
	c, err := company.NewStore(db.Pool).Upsert(ctx, company.UpsertParams{Name: name})
	if err != nil {
		t.Fatalf("seeding company %q: %v", name, err)
	}
	return c
}

func countTargets(t *testing.T, ctx context.Context, db *database.DB) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM target_companies`).Scan(&n); err != nil {
		t.Fatalf("counting target companies: %v", err)
	}
	return n
}

func TestTargetStoreUpsert_CreatesTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	target, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:         c.ID,
		ATSProvider:       "greenhouse",
		ExternalBoardID:   "acme",
		BoardURL:          "https://boards.greenhouse.io/acme",
		DiscoveryMetadata: map[string]any{"source": "seed-list", "probe_status": float64(200)},
	})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	if target.ID <= 0 {
		t.Errorf("ID = %d, want a positive generated identity", target.ID)
	}
	if target.CompanyID != c.ID {
		t.Errorf("CompanyID = %d, want %d", target.CompanyID, c.ID)
	}
	if target.ATSProvider != "greenhouse" || target.ExternalBoardID != "acme" {
		t.Errorf("provider/board = %q/%q, want greenhouse/acme", target.ATSProvider, target.ExternalBoardID)
	}
	if target.BoardURL != "https://boards.greenhouse.io/acme" {
		t.Errorf("BoardURL = %q, want the supplied URL", target.BoardURL)
	}
	if !target.IsActive {
		t.Error("IsActive = false, want true (the column default)")
	}
	if target.LastSuccessfulIngestionAt != nil {
		t.Errorf("LastSuccessfulIngestionAt = %v, want nil for a never-ingested target", target.LastSuccessfulIngestionAt)
	}
	if target.CreatedAt.IsZero() || target.UpdatedAt.IsZero() {
		t.Errorf("timestamps not populated: created_at=%s updated_at=%s", target.CreatedAt, target.UpdatedAt)
	}
	// JSONB round-trip: numbers come back as float64 through map[string]any.
	if got := target.DiscoveryMetadata["source"]; got != "seed-list" {
		t.Errorf("DiscoveryMetadata[source] = %v, want \"seed-list\"", got)
	}
	if got := target.DiscoveryMetadata["probe_status"]; got != float64(200) {
		t.Errorf("DiscoveryMetadata[probe_status] = %v (%T), want float64(200)", got, got)
	}
}

// A target with no board_url must store NULL, and the empty metadata map
// must come back as the column's '{}' default rather than nil-with-an-error.
func TestTargetStoreUpsert_StoresOmittedFieldsAsNullAndEmptyMetadata(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	target, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}
	if target.BoardURL != "" {
		t.Errorf("BoardURL = %q, want empty", target.BoardURL)
	}
	if len(target.DiscoveryMetadata) != 0 {
		t.Errorf("DiscoveryMetadata = %v, want empty", target.DiscoveryMetadata)
	}

	var boardURLNull bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT board_url IS NULL FROM target_companies WHERE id = $1`, target.ID,
	).Scan(&boardURLNull); err != nil {
		t.Fatalf("checking null-ness: %v", err)
	}
	if !boardURLNull {
		t.Error("board_url IS NULL = false, want an omitted board URL stored as NULL")
	}
}

// The unique index is (ats_provider, lower(btrim(external_board_id))) —
// a plain column plus an expression. This proves that composite target
// is inferred correctly and resolves a real conflict.
func TestTargetStoreUpsert_MatchesExistingTargetIgnoringCaseAndWhitespace(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	first, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	})
	if err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	second, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "  ACME  ",
	})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}

	if second.ID != first.ID {
		t.Errorf("second Upsert() returned id %d, want the existing %d — ON CONFLICT did not match", second.ID, first.ID)
	}
	if n := countTargets(t, ctx, db); n != 1 {
		t.Errorf("target_companies row count = %d, want 1 (a duplicate was created)", n)
	}
	if second.ExternalBoardID != "acme" {
		t.Errorf("ExternalBoardID = %q, want the originally stored %q", second.ExternalBoardID, "acme")
	}
}

// Same board id under a different provider is a different target: the
// index is composite, so the provider column must actually participate.
func TestTargetStoreUpsert_SameBoardIDUnderDifferentProviderIsDistinct(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	greenhouse, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	})
	if err != nil {
		t.Fatalf("greenhouse Upsert() failed: %v", err)
	}
	lever, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "lever",
		ExternalBoardID: "acme",
	})
	if err != nil {
		t.Fatalf("lever Upsert() failed: %v", err)
	}

	if lever.ID == greenhouse.ID {
		t.Errorf("both providers returned id %d, want two distinct targets", lever.ID)
	}
	if n := countTargets(t, ctx, db); n != 2 {
		t.Errorf("target_companies row count = %d, want 2", n)
	}
}

// Re-discovery reactivates: there is no operator UI that deactivates a
// target on purpose yet, so a board that answers again is active again.
func TestTargetStoreUpsert_ReactivatesDeactivatedTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	first, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	})
	if err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	if _, err := db.Pool.Exec(ctx, `UPDATE target_companies SET is_active = false WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("deactivating target: %v", err)
	}

	second, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second Upsert() returned id %d, want %d", second.ID, first.ID)
	}
	if !second.IsActive {
		t.Error("IsActive = false after re-discovery, want true")
	}

	stored, err := store.GetByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if !stored.IsActive {
		t.Error("stored is_active = false after re-discovery, want true")
	}
}

func TestTargetStoreUpsert_NilMetadataLeavesStoredMetadata(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	first, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:         c.ID,
		ATSProvider:       "greenhouse",
		ExternalBoardID:   "acme",
		DiscoveryMetadata: map[string]any{"round": "A"},
	})
	if err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	second, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second Upsert() returned id %d, want %d", second.ID, first.ID)
	}
	if got := second.DiscoveryMetadata["round"]; got != "A" {
		t.Errorf("DiscoveryMetadata = %v, want the stored {\"round\": \"A\"} left alone", second.DiscoveryMetadata)
	}
}

func TestTargetStoreUpsert_NewMetadataReplacesStoredMetadata(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	first, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:         c.ID,
		ATSProvider:       "greenhouse",
		ExternalBoardID:   "acme",
		DiscoveryMetadata: map[string]any{"round": "A", "only_in_a": true},
	})
	if err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	second, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:         c.ID,
		ATSProvider:       "greenhouse",
		ExternalBoardID:   "acme",
		DiscoveryMetadata: map[string]any{"round": "B"},
	})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second Upsert() returned id %d, want %d", second.ID, first.ID)
	}
	if got := second.DiscoveryMetadata["round"]; got != "B" {
		t.Errorf("DiscoveryMetadata[round] = %v, want \"B\"", got)
	}
	// Replacement, not a merge: keys only present in A are gone.
	if _, ok := second.DiscoveryMetadata["only_in_a"]; ok {
		t.Errorf("DiscoveryMetadata = %v, want the previous value replaced outright", second.DiscoveryMetadata)
	}

	stored, err := store.GetByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if got := stored.DiscoveryMetadata["round"]; got != "B" {
		t.Errorf("stored DiscoveryMetadata[round] = %v, want \"B\"", got)
	}
}

// An explicitly empty (non-nil) map is a real value and clears the
// stored metadata — that is the difference nil is reserved for.
func TestTargetStoreUpsert_EmptyNonNilMetadataClearsStoredMetadata(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	if _, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:         c.ID,
		ATSProvider:       "greenhouse",
		ExternalBoardID:   "acme",
		DiscoveryMetadata: map[string]any{"round": "A"},
	}); err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	second, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:         c.ID,
		ATSProvider:       "greenhouse",
		ExternalBoardID:   "acme",
		DiscoveryMetadata: map[string]any{},
	})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}
	if len(second.DiscoveryMetadata) != 0 {
		t.Errorf("DiscoveryMetadata = %v, want it cleared by an explicit empty map", second.DiscoveryMetadata)
	}
}

// board_url takes the newly discovered value when one is supplied (the
// probe that just found the board is the better source), and keeps the
// stored one when the parameter is blank.
func TestTargetStoreUpsert_BoardURLPrefersNewValueAndKeepsStoredOneWhenBlank(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	if _, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	}); err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	filled, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
		BoardURL:        "https://boards.greenhouse.io/acme",
	})
	if err != nil {
		t.Fatalf("second Upsert() failed: %v", err)
	}
	if filled.BoardURL != "https://boards.greenhouse.io/acme" {
		t.Errorf("BoardURL = %q, want the newly supplied URL to fill the NULL", filled.BoardURL)
	}

	replaced, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
		BoardURL:        "https://job-boards.greenhouse.io/acme",
	})
	if err != nil {
		t.Fatalf("third Upsert() failed: %v", err)
	}
	if replaced.BoardURL != "https://job-boards.greenhouse.io/acme" {
		t.Errorf("BoardURL = %q, want the newly supplied URL to win", replaced.BoardURL)
	}

	kept, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	})
	if err != nil {
		t.Fatalf("fourth Upsert() failed: %v", err)
	}
	if kept.BoardURL != "https://job-boards.greenhouse.io/acme" {
		t.Errorf("BoardURL = %q, want the stored URL kept when none is supplied", kept.BoardURL)
	}
}

// Validation runs before any SQL: these three can only ever produce a
// foreign-key or CHECK violation, and the caller deserves to read which
// field it forgot.
func TestTargetStoreUpsert_RejectsInvalidParams(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	valid := company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	}

	cases := map[string]struct {
		mutate func(p *company.TargetUpsertParams)
		want   string
	}{
		"zero company id":     {func(p *company.TargetUpsertParams) { p.CompanyID = 0 }, "company_id must be positive"},
		"negative company id": {func(p *company.TargetUpsertParams) { p.CompanyID = -1 }, "company_id must be positive"},
		"blank provider":      {func(p *company.TargetUpsertParams) { p.ATSProvider = "" }, "ats_provider is required"},
		"whitespace provider": {func(p *company.TargetUpsertParams) { p.ATSProvider = "   " }, "ats_provider is required"},
		"blank board id":      {func(p *company.TargetUpsertParams) { p.ExternalBoardID = "" }, "external_board_id is required"},
		"whitespace board id": {func(p *company.TargetUpsertParams) { p.ExternalBoardID = "\t\n " }, "external_board_id is required"},
	}
	for name, tc := range cases {
		params := valid
		tc.mutate(&params)

		target, err := store.Upsert(ctx, params)
		if err == nil {
			t.Errorf("%s: Upsert() succeeded, want an error", name)
			continue
		}
		if target != nil {
			t.Errorf("%s: Upsert() returned a non-nil target alongside an error", name)
		}
		if !strings.HasPrefix(err.Error(), "company: ") {
			t.Errorf("%s: error = %q, want a \"company: \" prefix", name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want it to contain %q", name, err, tc.want)
		}
	}

	if n := countTargets(t, ctx, db); n != 0 {
		t.Errorf("target_companies row count = %d, want 0 — validation should have run before any SQL", n)
	}
}

// A company_id that passes validation but does not exist is the
// foreign key's job, not ours; it must still come back as a wrapped
// error rather than a panic.
func TestTargetStoreUpsert_RejectsUnknownCompanyID(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewTargetStore(db.Pool)

	target, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       424242,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	})
	if err == nil {
		t.Fatalf("Upsert() succeeded for a nonexistent company, want a foreign key violation (returned id %d)", target.ID)
	}
	if !strings.HasPrefix(err.Error(), "company: ") {
		t.Errorf("error = %q, want a \"company: \" prefix", err)
	}
}

func TestTargetStoreGetByID_ReturnsStoredTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	created, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:         c.ID,
		ATSProvider:       "greenhouse",
		ExternalBoardID:   "acme",
		BoardURL:          "https://boards.greenhouse.io/acme",
		DiscoveryMetadata: map[string]any{"source": "seed-list"},
	})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	got, err := store.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if got.ID != created.ID || got.CompanyID != c.ID {
		t.Errorf("GetByID() = id %d/company %d, want %d/%d", got.ID, got.CompanyID, created.ID, c.ID)
	}
	if got.BoardURL != created.BoardURL {
		t.Errorf("BoardURL = %q, want %q", got.BoardURL, created.BoardURL)
	}
	if got.DiscoveryMetadata["source"] != "seed-list" {
		t.Errorf("DiscoveryMetadata = %v, want {\"source\": \"seed-list\"}", got.DiscoveryMetadata)
	}
}

func TestTargetStoreGetByID_ReturnsErrNotFoundForMissingRow(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewTargetStore(db.Pool)

	target, err := store.GetByID(ctx, 424242)
	if !errors.Is(err, company.ErrNotFound) {
		t.Fatalf("GetByID() error = %v, want it to wrap company.ErrNotFound", err)
	}
	if target != nil {
		t.Errorf("GetByID() returned a non-nil target alongside ErrNotFound")
	}
}

// Regression test for a data-integrity gap found by adversarial review:
// a second company submitting the same (ats_provider, external_board_id)
// — reachable in practice through a discovery bug that mis-extracts a
// board id — must never silently reassign the board's row to itself.
// Before the fix, Upsert's ON CONFLICT DO UPDATE had no guard on
// company_id and would happily rewrite the row.
func TestTargetStoreUpsert_RejectsReassigningAnExistingBoardToADifferentCompany(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewTargetStore(db.Pool)

	first := seedCompany(t, ctx, db, "Acme")
	second := seedCompany(t, ctx, db, "Globex")

	original, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       first.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "shared-slug",
		BoardURL:        "https://boards.greenhouse.io/shared-slug",
	})
	if err != nil {
		t.Fatalf("Upsert() for the first company failed: %v", err)
	}

	_, err = store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       second.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "shared-slug",
		BoardURL:        "https://boards.greenhouse.io/hijacked",
	})
	if err == nil {
		t.Fatal("Upsert() for the second company succeeded, want it rejected: this would reassign the first company's board")
	}
	if !errors.Is(err, company.ErrTargetCompanyMismatch) {
		t.Errorf("error = %v, want it to wrap company.ErrTargetCompanyMismatch", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("company %d", first.ID)) {
		t.Errorf("error = %q, want it to name the company (%d) that actually owns the board", err, first.ID)
	}

	// The row must be completely untouched by the rejected attempt — not
	// just "still owned by the first company" but not even its board_url
	// leaked through.
	unchanged, err := store.GetByID(ctx, original.ID)
	if err != nil {
		t.Fatalf("GetByID() after the rejected upsert failed: %v", err)
	}
	if unchanged.CompanyID != first.ID {
		t.Errorf("CompanyID = %d after the rejected upsert, want it still %d", unchanged.CompanyID, first.ID)
	}
	if unchanged.BoardURL != "https://boards.greenhouse.io/shared-slug" {
		t.Errorf("BoardURL = %q after the rejected upsert, want the original URL unchanged, not the hijack attempt's", unchanged.BoardURL)
	}
}

func TestTargetStoreUpsert_SameCompanyCanStillUpdateItsOwnTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewTargetStore(db.Pool)
	c := seedCompany(t, ctx, db, "Acme")

	if _, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
		BoardURL:        "https://boards.greenhouse.io/acme",
	}); err != nil {
		t.Fatalf("first Upsert() failed: %v", err)
	}

	updated, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
		BoardURL:        "https://boards.greenhouse.io/acme-new",
	})
	if err != nil {
		t.Fatalf("second Upsert() by the SAME company failed: %v — the WHERE guard must not reject same-company updates", err)
	}
	if updated.BoardURL != "https://boards.greenhouse.io/acme-new" {
		t.Errorf("BoardURL = %q, want it updated to the new value", updated.BoardURL)
	}
}

func TestTargetStoreGetByProviderAndBoard_ReturnsStoredTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	created, err := store.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       c.ID,
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
	})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	got, err := store.GetByProviderAndBoard(ctx, "greenhouse", "  ACME  ")
	if err != nil {
		t.Fatalf("GetByProviderAndBoard() failed: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("GetByProviderAndBoard() id = %d, want %d", got.ID, created.ID)
	}
}

func TestTargetStoreGetByProviderAndBoard_ReturnsErrNotFoundForMissingRow(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewTargetStore(db.Pool)

	_, err := store.GetByProviderAndBoard(ctx, "greenhouse", "does-not-exist")
	if !errors.Is(err, company.ErrNotFound) {
		t.Fatalf("GetByProviderAndBoard() error = %v, want it to wrap company.ErrNotFound", err)
	}
}

func TestTargetStoreListActive_ReturnsOnlyActiveTargetsOrderedByID(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	active1, err := store.Upsert(ctx, company.TargetUpsertParams{CompanyID: c.ID, ATSProvider: "greenhouse", ExternalBoardID: "acme-a"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}
	active2, err := store.Upsert(ctx, company.TargetUpsertParams{CompanyID: c.ID, ATSProvider: "greenhouse", ExternalBoardID: "acme-b"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}
	inactive, err := store.Upsert(ctx, company.TargetUpsertParams{CompanyID: c.ID, ATSProvider: "greenhouse", ExternalBoardID: "acme-c"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}
	if err := store.SetActive(ctx, inactive.ID, false); err != nil {
		t.Fatalf("SetActive(false) failed: %v", err)
	}

	got, err := store.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive() failed: %v", err)
	}

	var gotIDs []int64
	for _, tgt := range got {
		if tgt.CompanyID == c.ID {
			gotIDs = append(gotIDs, tgt.ID)
		}
	}
	if len(gotIDs) != 2 || gotIDs[0] != active1.ID || gotIDs[1] != active2.ID {
		t.Errorf("ListActive() ids for this company = %v, want [%d %d] (active only, ordered by id)", gotIDs, active1.ID, active2.ID)
	}
	for _, tgt := range got {
		if tgt.ID == inactive.ID {
			t.Errorf("ListActive() included deactivated target %d", inactive.ID)
		}
	}
}

func TestTargetStoreMarkIngestionSucceeded_SetsTimestamp(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	target, err := store.Upsert(ctx, company.TargetUpsertParams{CompanyID: c.ID, ATSProvider: "greenhouse", ExternalBoardID: "acme"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}
	if target.LastSuccessfulIngestionAt != nil {
		t.Fatalf("LastSuccessfulIngestionAt = %v before any ingestion, want nil", target.LastSuccessfulIngestionAt)
	}

	if err := store.MarkIngestionSucceeded(ctx, target.ID); err != nil {
		t.Fatalf("MarkIngestionSucceeded() failed: %v", err)
	}

	got, err := store.GetByID(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if got.LastSuccessfulIngestionAt == nil {
		t.Fatal("LastSuccessfulIngestionAt = nil after MarkIngestionSucceeded, want a timestamp")
	}
}

func TestTargetStoreMarkIngestionSucceeded_ReturnsErrNotFoundForMissingTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewTargetStore(db.Pool)

	err := store.MarkIngestionSucceeded(ctx, -1)
	if !errors.Is(err, company.ErrNotFound) {
		t.Fatalf("MarkIngestionSucceeded() error = %v, want it to wrap company.ErrNotFound", err)
	}
}

func TestTargetStoreSetActive_TogglesFlag(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	c := seedCompany(t, ctx, db, "Acme")
	store := company.NewTargetStore(db.Pool)

	target, err := store.Upsert(ctx, company.TargetUpsertParams{CompanyID: c.ID, ATSProvider: "greenhouse", ExternalBoardID: "acme"})
	if err != nil {
		t.Fatalf("Upsert() failed: %v", err)
	}

	if err := store.SetActive(ctx, target.ID, false); err != nil {
		t.Fatalf("SetActive(false) failed: %v", err)
	}
	got, err := store.GetByID(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if got.IsActive {
		t.Error("IsActive = true after SetActive(false)")
	}

	if err := store.SetActive(ctx, target.ID, true); err != nil {
		t.Fatalf("SetActive(true) failed: %v", err)
	}
	got, err = store.GetByID(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if !got.IsActive {
		t.Error("IsActive = false after SetActive(true)")
	}
}

func TestTargetStoreSetActive_ReturnsErrNotFoundForMissingTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := company.NewTargetStore(db.Pool)

	err := store.SetActive(ctx, -1, false)
	if !errors.Is(err, company.ErrNotFound) {
		t.Fatalf("SetActive() error = %v, want it to wrap company.ErrNotFound", err)
	}
}
