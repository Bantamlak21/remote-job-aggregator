package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestUpsert_StoresWhatTheSourceSaysAboutRemoteAndEmploymentType(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	store := NewStore(db.Pool)
	target, comp := seedTarget(t, ctx, db, "Acme", "acme")

	rec := baseRecord(target, comp, "j1")
	rec.RemoteType, rec.EmploymentType = "remote", "contract"
	if _, err := store.UpsertFromATS(ctx, rec); err != nil {
		t.Fatalf("UpsertFromATS() failed: %v", err)
	}
	res, err := store.List(ctx, Filter{RemoteType: RemoteTypeRemote, EmploymentType: EmploymentTypeContract})
	if err != nil || res.Total != 1 {
		t.Fatalf("List(remote, contract) = %+v, %v; want the job", res, err)
	}
	if got := res.Jobs[0]; got.RemoteType != RemoteTypeRemote || got.EmploymentType != EmploymentTypeContract || got.Source != "greenhouse" {
		t.Errorf("job = %+v, want remote/contract from source greenhouse", got)
	}
}

// A source that says nothing must not reset what is stored (a later
// classification, or an earlier source's answer); a source that says
// something replaces it and counts as a content change.
func TestUpsert_SilenceKeepsTheStoredValueAndAnAnswerReplacesIt(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	store := NewStore(db.Pool)
	target, comp := seedTarget(t, ctx, db, "Acme", "acme")

	first := baseRecord(target, comp, "j1")
	first.RemoteType, first.EmploymentType = "hybrid", "part_time"
	if _, err := store.UpsertFromATS(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	silent := baseRecord(target, comp, "j1") // RemoteType ""
	if _, err := store.UpsertFromATS(ctx, silent); err != nil {
		t.Fatalf("silent upsert: %v", err)
	}
	got, _ := store.Get(ctx, mustJobID(t, ctx, db, "j1"))
	if got.RemoteType != RemoteTypeHybrid || got.EmploymentType != EmploymentTypePartTime {
		t.Errorf("after a silent upsert: remote %q, employment %q; want hybrid and part_time kept", got.RemoteType, got.EmploymentType)
	}

	answer := baseRecord(target, comp, "j1")
	answer.RemoteType, answer.EmploymentType = "remote", "internship"
	out, err := store.UpsertFromATS(ctx, answer)
	if err != nil || !out.Changed {
		t.Fatalf("answering upsert: %+v, %v; want a change", out, err)
	}
	got, _ = store.Get(ctx, mustJobID(t, ctx, db, "j1"))
	if got.RemoteType != RemoteTypeRemote || got.EmploymentType != EmploymentTypeInternship {
		t.Errorf("remote %q, employment %q; want remote and internship", got.RemoteType, got.EmploymentType)
	}
}

func TestUpsert_ADefaultedJobIsUnknown_AndAnInvalidValueIsARejectedRecord(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	store := NewStore(db.Pool)
	target, comp := seedTarget(t, ctx, db, "Acme", "acme")

	if _, err := store.UpsertFromATS(ctx, baseRecord(target, comp, "plain")); err != nil {
		t.Fatalf("plain upsert: %v", err)
	}
	got, _ := store.Get(ctx, mustJobID(t, ctx, db, "plain"))
	if got.RemoteType != RemoteTypeUnknown || got.EmploymentType != EmploymentTypeUnknown {
		t.Errorf("defaults = %q/%q, want unknown/unknown", got.RemoteType, got.EmploymentType)
	}

	bad := baseRecord(target, comp, "bad")
	bad.RemoteType = "Remote" // wrong case: not in the vocabulary
	_, err := store.UpsertFromATS(ctx, bad)
	if err == nil || !IsRejectedRecord(err) {
		t.Errorf("invalid remote type: err = %v, want a rejected record (skipped, not fatal)", err)
	}
}

// A record that never fills remote or employment type must hash exactly as it
// did before those fields existed, so existing rows are not all reported as
// changed once.
func TestContentHash_IgnoresRemoteAndEmploymentTypeWhenTheSourceSaysNothing(t *testing.T) {
	base := baseRecord(1, 1, "j")
	silent := contentHash(base)
	said := base
	said.RemoteType = "remote"
	if contentHash(said) == silent {
		t.Errorf("saying remote did not change the hash")
	}
	said = base
	said.EmploymentType = "contract"
	if contentHash(said) == silent {
		t.Errorf("saying contract did not change the hash")
	}
	// The value the hash had before RemoteType and EmploymentType existed.
	legacy := sha256.New()
	for _, f := range []string{base.Title, base.Description, base.ApplicationURL, base.CanonicalURL, base.LocationRaw} {
		legacy.Write([]byte(f))
		legacy.Write([]byte{0})
	}
	if want := hex.EncodeToString(legacy.Sum(nil)); silent != want {
		t.Errorf("hash of a silent record = %s, want the pre-existing hash %s", silent, want)
	}
}
