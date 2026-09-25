package job

import (
	"context"
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
	first.RemoteType = "hybrid"
	if _, err := store.UpsertFromATS(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	silent := baseRecord(target, comp, "j1") // RemoteType ""
	if _, err := store.UpsertFromATS(ctx, silent); err != nil {
		t.Fatalf("silent upsert: %v", err)
	}
	got, _ := store.Get(ctx, mustJobID(t, ctx, db, "j1"))
	if got.RemoteType != RemoteTypeHybrid {
		t.Errorf("remote type after a silent upsert = %q, want hybrid kept", got.RemoteType)
	}

	answer := baseRecord(target, comp, "j1")
	answer.RemoteType = "remote"
	out, err := store.UpsertFromATS(ctx, answer)
	if err != nil || !out.Changed {
		t.Fatalf("answering upsert: %+v, %v; want a change", out, err)
	}
	got, _ = store.Get(ctx, mustJobID(t, ctx, db, "j1"))
	if got.RemoteType != RemoteTypeRemote {
		t.Errorf("remote type = %q, want remote", got.RemoteType)
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
