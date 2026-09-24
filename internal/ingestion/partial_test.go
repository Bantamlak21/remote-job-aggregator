package ingestion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
)

// partialClient is a fakeATSClient that declares its listing a sample.
type partialClient struct {
	*fakeATSClient
	staleAfter time.Duration
}

func (p partialClient) StaleAfter() time.Duration { return p.staleAfter }

func TestRun_PartialClientClosesOnlyStaleJobsNeverMissingOnes(t *testing.T) {
	target := testTarget(7, 70, "search", "Kifiya Financial Technology")
	client := partialClient{
		fakeATSClient: &fakeATSClient{jobs: map[string][]ats.Job{
			"Kifiya Financial Technology": {{ExternalID: "linkedin:1", Title: "Dev", URL: "https://x/1"}},
		}},
		staleAfter: 21 * 24 * time.Hour,
	}
	jobs := &fakeJobUpserter{staleClosed: 3}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"search": client}, 1, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(jobs.removedCalls) != 0 {
		t.Errorf("MarkMissingAsRemoved called %d times for a partial source; absence from one search proves nothing", len(jobs.removedCalls))
	}
	if len(jobs.staleCalls) != 1 || jobs.staleCalls[0] != (staleCall{7, 21 * 24 * time.Hour}) {
		t.Fatalf("CloseStale calls = %+v, want one for target 7 with the client's window", jobs.staleCalls)
	}
	if results[0].Err != nil || results[0].Removed != 3 || results[0].Inserted != 1 {
		t.Errorf("result = %+v, want no error, Removed=3 (from CloseStale), Inserted=1", results[0])
	}
}

func TestRun_FullBoardClientStillClosesMissingJobs(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "acme")
	jobs := &fakeJobUpserter{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"greenhouse": &fakeATSClient{jobs: map[string][]ats.Job{
			"acme": {{ExternalID: "1", Title: "Dev", URL: "https://x/1"}},
		}}}, 1, testLogger())

	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(jobs.removedCalls) != 1 || len(jobs.staleCalls) != 0 {
		t.Errorf("removedCalls=%d staleCalls=%d, want 1 and 0 for a full-board client", len(jobs.removedCalls), len(jobs.staleCalls))
	}
}

// A window of zero would make CloseStale reject the call (and, if it did
// not, close every job). A client returning it is treated as a full board
// rather than as "close everything".
func TestRun_PartialClientWithNonPositiveWindowFallsBackToBoardSemantics(t *testing.T) {
	target := testTarget(1, 10, "search", "x")
	jobs := &fakeJobUpserter{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"search": partialClient{fakeATSClient: &fakeATSClient{}, staleAfter: 0}}, 1, testLogger())

	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(jobs.staleCalls) != 0 || len(jobs.removedCalls) != 1 {
		t.Errorf("staleCalls=%d removedCalls=%d, want 0 and 1", len(jobs.staleCalls), len(jobs.removedCalls))
	}
}

func TestRun_PartialClientFetchFailureClosesNothing(t *testing.T) {
	target := testTarget(1, 10, "search", "x")
	jobs := &fakeJobUpserter{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"search": partialClient{
			fakeATSClient: &fakeATSClient{err: map[string]error{"x": errors.New("serper down")}},
			staleAfter:    time.Hour,
		}}, 1, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if results[0].Err == nil {
		t.Errorf("result has no error for a failed fetch")
	}
	if len(jobs.staleCalls) != 0 || len(jobs.removedCalls) != 0 {
		t.Errorf("a failed fetch closed jobs (stale=%d removed=%d)", len(jobs.staleCalls), len(jobs.removedCalls))
	}
}

func TestRun_CloseStaleErrorIsReportedOnTheTarget(t *testing.T) {
	target := testTarget(1, 10, "search", "x")
	jobs := &fakeJobUpserter{staleErr: errors.New("db gone")}
	recorder := &fakeTargetRecorder{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, recorder, jobs,
		map[string]ATSClient{"search": partialClient{fakeATSClient: &fakeATSClient{}, staleAfter: time.Hour}}, 1, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if results[0].Err == nil {
		t.Fatal("CloseStale failure was swallowed")
	}
	if len(recorder.succeededIDs) != 0 {
		t.Errorf("recorded a successful ingestion despite the CloseStale failure")
	}
}
