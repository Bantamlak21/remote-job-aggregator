package company_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
)

func newRegistrar(t *testing.T) (*company.Registrar, *company.Store, *company.TargetStore) {
	t.Helper()
	db := newDB(t)
	cs, ts := company.NewStore(db.Pool), company.NewTargetStore(db.Pool)
	return company.NewRegistrar(cs, ts), cs, ts
}

func TestRegistrar_CreatesCompanyAndTargetOnFirstSightAndFindsThemAfter(t *testing.T) {
	reg, cs, ts := newRegistrar(t)
	ctx := context.Background()

	first, err := reg.EnsureTarget(ctx, "ethiojobs", "  New   Employer PLC ", false)
	if err != nil {
		t.Fatalf("EnsureTarget() failed: %v", err)
	}
	if first.ATSProvider != "ethiojobs" || first.ExternalBoardID != "new employer plc" {
		t.Errorf("target = %+v, want provider ethiojobs and the lower-cased, whitespace-collapsed name as board id", first)
	}
	c, err := cs.GetByID(ctx, first.CompanyID)
	if err != nil {
		t.Fatalf("GetByID() failed: %v", err)
	}
	if c.Name != "New Employer PLC" || c.IsPriority {
		t.Errorf("company = %+v, want name 'New Employer PLC' (whitespace collapsed, case kept) and not priority", c)
	}

	// The same employer spelled differently resolves to the same rows.
	again, err := reg.EnsureTarget(ctx, "ethiojobs", "NEW EMPLOYER PLC", false)
	if err != nil {
		t.Fatalf("second EnsureTarget() failed: %v", err)
	}
	if again.ID != first.ID || again.CompanyID != first.CompanyID {
		t.Errorf("second call created new rows: %+v vs %+v", again, first)
	}
	got, err := ts.GetByProviderAndBoard(ctx, "ethiojobs", "new employer plc")
	if err != nil || got.ID != first.ID {
		t.Errorf("GetByProviderAndBoard() = %+v, %v", got, err)
	}
}

func TestRegistrar_SameEmployerUnderTwoProvidersSharesTheCompany(t *testing.T) {
	reg, _, _ := newRegistrar(t)
	ctx := context.Background()

	a, err := reg.EnsureTarget(ctx, "ethiojobs", "Acme", false)
	if err != nil {
		t.Fatalf("EnsureTarget(ethiojobs) failed: %v", err)
	}
	b, err := reg.EnsureTarget(ctx, "linkedin", "Acme", false)
	if err != nil {
		t.Fatalf("EnsureTarget(linkedin) failed: %v", err)
	}
	if a.CompanyID != b.CompanyID || a.ID == b.ID {
		t.Errorf("ethiojobs %+v and linkedin %+v: want one company and two targets", a, b)
	}
}

func TestRegistrar_PriorityFlagsButNeverUnflags(t *testing.T) {
	reg, cs, _ := newRegistrar(t)
	ctx := context.Background()

	tgt, err := reg.EnsureTarget(ctx, "ethiojobs", "EthSwitch", true)
	if err != nil {
		t.Fatalf("EnsureTarget(priority) failed: %v", err)
	}
	if c, _ := cs.GetByID(ctx, tgt.CompanyID); !c.IsPriority {
		t.Errorf("company not flagged priority")
	}
	// A later source that does not know the company is priority must not clear it.
	if _, err := reg.EnsureTarget(ctx, "linkedin", "ethswitch", false); err != nil {
		t.Fatalf("EnsureTarget(non-priority) failed: %v", err)
	}
	if c, _ := cs.GetByID(ctx, tgt.CompanyID); !c.IsPriority {
		t.Errorf("a non-priority sighting cleared the priority flag")
	}
}

func TestRegistrar_ExistingSeededCompanyIsReusedNotDuplicated(t *testing.T) {
	reg, cs, _ := newRegistrar(t)
	ctx := context.Background()

	seeded, err := cs.Upsert(ctx, company.UpsertParams{Name: "Kifiya Financial Technology", Website: "https://kifiya.com"})
	if err != nil {
		t.Fatalf("seeding failed: %v", err)
	}
	tgt, err := reg.EnsureTarget(ctx, "ethiojobs", "Kifiya Financial Technology", true)
	if err != nil {
		t.Fatalf("EnsureTarget() failed: %v", err)
	}
	if tgt.CompanyID != seeded.ID {
		t.Errorf("target attached to company %d, want the seeded %d", tgt.CompanyID, seeded.ID)
	}
	c, _ := cs.GetByID(ctx, seeded.ID)
	if c.Website != "https://kifiya.com" {
		t.Errorf("website = %q, want the seeded one kept", c.Website)
	}
}

func TestRegistrar_RejectsUnusableInput(t *testing.T) {
	db := newDB(t)
	reg := company.NewRegistrar(company.NewStore(db.Pool), company.NewTargetStore(db.Pool))
	ctx := context.Background()

	cases := []struct {
		name, provider, employer string
	}{
		{"blank employer", "ethiojobs", "   "},
		{"empty employer", "ethiojobs", ""},
		{"blank provider", " ", "Acme"},
		{"absurdly long employer", "ethiojobs", strings.Repeat("x", 201)},
	}
	for _, tc := range cases {
		if _, err := reg.EnsureTarget(ctx, tc.provider, tc.employer, false); err == nil {
			t.Errorf("%s: succeeded, want an error", tc.name)
		}
	}
	if n := countCompanies(t, ctx, db); n != 0 {
		t.Errorf("%d companies created by rejected calls, want 0", n)
	}
}

func TestRegistrar_FindTargetNeverCreatesAnything(t *testing.T) {
	reg, cs, _ := newRegistrar(t)
	ctx := context.Background()

	if _, ok, err := reg.FindTarget(ctx, "ethiojobs", "Ghost Co"); ok || err != nil {
		t.Fatalf("FindTarget(unknown) = ok %v, err %v; want not found and no error", ok, err)
	}
	for _, bad := range [][2]string{{"ethiojobs", "  "}, {" ", "Acme"}, {"ethiojobs", strings.Repeat("x", 201)}} {
		if _, ok, err := reg.FindTarget(ctx, bad[0], bad[1]); ok || err != nil {
			t.Errorf("FindTarget(%q, %q) = ok %v, err %v; want not found and no error", bad[0], bad[1], ok, err)
		}
	}
	if _, err := cs.GetByName(ctx, "Ghost Co"); err == nil {
		t.Errorf("FindTarget created a company")
	}

	made, err := reg.EnsureTarget(ctx, "ethiojobs", "Real Co", false)
	if err != nil {
		t.Fatalf("EnsureTarget() failed: %v", err)
	}
	got, ok, err := reg.FindTarget(ctx, "ethiojobs", "  REAL   co ")
	if err != nil || !ok || got.ID != made.ID {
		t.Errorf("FindTarget() = %+v, %v, %v; want the created target %d", got, ok, err, made.ID)
	}
	if _, ok, _ := reg.FindTarget(ctx, "linkedin", "Real Co"); ok {
		t.Errorf("FindTarget found a target under another provider")
	}
}

// The target's identity is (provider, lower(name)); a different company can
// never be attached to an existing target.
func TestRegistrar_ATargetOwnedByAnotherCompanyIsAnError(t *testing.T) {
	reg, cs, ts := newRegistrar(t)
	ctx := context.Background()

	other, err := cs.Upsert(ctx, company.UpsertParams{Name: "Other Co"})
	if err != nil {
		t.Fatalf("seeding failed: %v", err)
	}
	// Someone registered the board id "acme" for a different company.
	if _, err := ts.Upsert(ctx, company.TargetUpsertParams{CompanyID: other.ID, ATSProvider: "ethiojobs", ExternalBoardID: "acme"}); err != nil {
		t.Fatalf("seeding target failed: %v", err)
	}
	_, err = reg.EnsureTarget(ctx, "ethiojobs", "Acme", false)
	if !errors.Is(err, company.ErrTargetCompanyMismatch) {
		t.Fatalf("error = %v, want ErrTargetCompanyMismatch", err)
	}
}
