package ingestion

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
)

func runOne(t *testing.T, provider string, client ATSClient, jobs *fakeJobUpserter, board string) Result {
	t.Helper()
	target := testTarget(5, 50, provider, board)
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{provider: client}, 1, testLogger())
	in.now = func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }
	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return results[0]
}

func TestRun_JobsTheSourceReportsEndedAreClosedNotStoredNorSeen(t *testing.T) {
	client := partialClient{
		fakeATSClient: &fakeATSClient{jobs: map[string][]ats.Job{"x": {
			{ExternalID: "ethiojobs:live", Title: "Live", URL: "https://x/live"},
			{ExternalID: "ethiojobs:expired", Closed: true},
			{ExternalID: "", Closed: true}, // nothing to close
		}}},
		staleAfter: time.Hour,
	}
	jobs := &fakeJobUpserter{endedClosed: 1, staleClosed: 2}

	res := runOne(t, "search", client, jobs, "x")

	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if len(jobs.upserts) != 1 || jobs.upserts[0].SourceJobID != "ethiojobs:live" {
		t.Errorf("upserts = %+v, want only the live job (an ended job must never be stored)", jobs.upserts)
	}
	if len(jobs.endedCalls) != 1 || !slices.Equal(jobs.endedCalls[0].ids, []string{"ethiojobs:expired"}) || jobs.endedCalls[0].targetID != 5 {
		t.Errorf("CloseBySourceID calls = %+v, want one for target 5 with exactly the expired id", jobs.endedCalls)
	}
	if res.Removed != 3 {
		t.Errorf("Removed = %d, want 3 (1 ended + 2 stale)", res.Removed)
	}
	if res.Skipped != 0 {
		t.Errorf("Skipped = %d; an ended job is not a skipped/bad one", res.Skipped)
	}
}

func TestRun_EndedJobsOnAFullBoardAreNotCountedAsSeen(t *testing.T) {
	client := &fakeATSClient{jobs: map[string][]ats.Job{"x": {
		{ExternalID: "keep", Title: "Keep", URL: "https://x/keep"},
		{ExternalID: "ended", Closed: true},
	}}}
	jobs := &fakeJobUpserter{}
	runOne(t, "greenhouse", client, jobs, "x")

	if len(jobs.removedCalls) != 1 || !slices.Equal(jobs.removedCalls[0].seen, []string{"keep"}) {
		t.Errorf("MarkMissingAsRemoved seen = %+v, want only [keep] so the ended id is closed", jobs.removedCalls)
	}
}

func TestRun_NoEndedJobsMeansNoCloseBySourceIDCall(t *testing.T) {
	client := &fakeATSClient{jobs: map[string][]ats.Job{"x": {{ExternalID: "a", Title: "A", URL: "https://x/a"}}}}
	jobs := &fakeJobUpserter{}
	runOne(t, "greenhouse", client, jobs, "x")
	if len(jobs.endedCalls) != 0 {
		t.Errorf("CloseBySourceID called %d times with nothing ended", len(jobs.endedCalls))
	}
}

func TestRun_CloseBySourceIDFailureIsATargetError(t *testing.T) {
	client := &fakeATSClient{jobs: map[string][]ats.Job{"x": {{ExternalID: "e", Closed: true}}}}
	jobs := &fakeJobUpserter{endedErr: errors.New("db gone")}
	res := runOne(t, "greenhouse", client, jobs, "x")
	if res.Err == nil {
		t.Fatal("a failed close was swallowed")
	}
	if len(jobs.removedCalls) != 0 {
		t.Errorf("continued to MarkMissingAsRemoved after the close failed")
	}
}

func TestRun_OverlongTitlesAreTruncatedOnARuneBoundary(t *testing.T) {
	long := ""
	for range 400 {
		long += "é"
	}
	client := &fakeATSClient{jobs: map[string][]ats.Job{"x": {{ExternalID: "a", Title: long, URL: "https://x/a"}}}}
	jobs := &fakeJobUpserter{}
	runOne(t, "greenhouse", client, jobs, "x")

	if len(jobs.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(jobs.upserts))
	}
	if got := []rune(jobs.upserts[0].Title); len(got) != maxTitleRunes {
		t.Errorf("stored title has %d runes, want %d", len(got), maxTitleRunes)
	}
}

func TestRun_FuturePublishDatesAreDroppedNotTrusted(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	client := &fakeATSClient{jobs: map[string][]ats.Job{"x": {
		{ExternalID: "far", Title: "Far", URL: "https://x/1", PublishedAt: now.AddDate(5, 0, 0)},
		{ExternalID: "tomorrow-ish", Title: "T", URL: "https://x/2", PublishedAt: now.Add(23 * time.Hour)},
		{ExternalID: "past", Title: "P", URL: "https://x/3", PublishedAt: now.Add(-48 * time.Hour)},
		{ExternalID: "none", Title: "N", URL: "https://x/4"},
	}}}
	jobs := &fakeJobUpserter{}
	runOne(t, "greenhouse", client, jobs, "x")

	got := map[string]time.Time{}
	for _, r := range jobs.upserts {
		got[r.SourceJobID] = r.PublishedAt
	}
	if !got["far"].IsZero() {
		t.Errorf("a date 5 years ahead was kept: %v", got["far"])
	}
	if got["tomorrow-ish"].IsZero() {
		t.Errorf("a date 23h ahead (clock skew) was dropped")
	}
	if got["past"].IsZero() || !got["none"].IsZero() {
		t.Errorf("past = %v, none = %v; a past date must be kept and a missing one stay zero", got["past"], got["none"])
	}
}
