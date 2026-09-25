package ingestion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

// Fakes let this package's own orchestration (per-target error
// isolation, the board-not-found deactivation rule, the never-mark-
// removed-on-fetch-failure invariant, the worker bound) be tested
// without a database or a real ATS endpoint — internal/job's and
// internal/company's own packages already prove the real behavior of
// UpsertFromATS/MarkMissingAsRemoved/SetActive against real Postgres.

type fakeTargetLister struct {
	targets []company.TargetCompany
	err     error
}

func (f *fakeTargetLister) ListActive(context.Context) ([]company.TargetCompany, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.targets, nil
}

type fakeTargetRecorder struct {
	mu             sync.Mutex
	succeededIDs   []int64
	deactivatedIDs []int64
}

func (f *fakeTargetRecorder) MarkIngestionSucceeded(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.succeededIDs = append(f.succeededIDs, id)
	return nil
}

func (f *fakeTargetRecorder) SetActive(_ context.Context, id int64, active bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !active {
		f.deactivatedIDs = append(f.deactivatedIDs, id)
	}
	return nil
}

func (f *fakeTargetRecorder) wasDeactivated(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.deactivatedIDs, id)
}

type fakeATSClient struct {
	mu    sync.Mutex
	jobs  map[string][]ats.Job
	err   map[string]error
	delay time.Duration
	calls []string

	current, maxSeen int32 // for concurrency-bound assertions
}

func (f *fakeATSClient) ListJobs(_ context.Context, boardToken string) ([]ats.Job, error) {
	n := atomic.AddInt32(&f.current, 1)
	for {
		max := atomic.LoadInt32(&f.maxSeen)
		if n <= max || atomic.CompareAndSwapInt32(&f.maxSeen, max, n) {
			break
		}
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	atomic.AddInt32(&f.current, -1)

	f.mu.Lock()
	f.calls = append(f.calls, boardToken)
	err := f.err[boardToken]
	jobs := f.jobs[boardToken]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

type removedCall struct {
	targetID int64
	seen     []string
}

type staleCall struct {
	targetID  int64
	olderThan time.Duration
}

type endedCall struct {
	targetID int64
	ids      []string
}

type fakeJobUpserter struct {
	endedCalls  []endedCall
	endedClosed int
	endedErr    error

	mu           sync.Mutex
	upserts      []job.Record
	outcomeFn    func(job.Record) (job.UpsertOutcome, error)
	removedCalls []removedCall
	staleCalls   []staleCall
	staleClosed  int
	staleErr     error
	upsertErr    error
	moves        []moveCall
	moveErr      error
	openings     []job.Opening // what OpenOpenings reports as stored by other sources
	openingsErr  error
	openingCalls []openingsCall
}

type openingsCall struct {
	companyID     int64
	market, avoid string
}

func (f *fakeJobUpserter) OpenOpenings(_ context.Context, companyID int64, mk, excludeSource string) ([]job.Opening, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openingCalls = append(f.openingCalls, openingsCall{companyID, mk, excludeSource})
	return f.openings, f.openingsErr
}

type moveCall struct {
	source, id          string
	targetID, companyID int64
}

func (f *fakeJobUpserter) MoveJob(_ context.Context, source, id string, targetID, companyID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = append(f.moves, moveCall{source, id, targetID, companyID})
	return f.moveErr
}

func (f *fakeJobUpserter) UpsertFromATS(_ context.Context, r job.Record) (job.UpsertOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return job.UpsertOutcome{}, f.upsertErr
	}
	f.upserts = append(f.upserts, r)
	if f.outcomeFn != nil {
		return f.outcomeFn(r)
	}
	return job.UpsertOutcome{Inserted: true, Changed: true}, nil
}

func (f *fakeJobUpserter) MarkMissingAsRemoved(_ context.Context, targetID int64, seen []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedCalls = append(f.removedCalls, removedCall{targetID, seen})
	return 0, nil
}

func (f *fakeJobUpserter) CloseBySourceID(_ context.Context, targetID int64, ids []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endedCalls = append(f.endedCalls, endedCall{targetID, append([]string(nil), ids...)})
	return f.endedClosed, f.endedErr
}

func (f *fakeJobUpserter) CloseStale(_ context.Context, targetID int64, olderThan time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staleCalls = append(f.staleCalls, staleCall{targetID, olderThan})
	return f.staleClosed, f.staleErr
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testTarget(id, companyID int64, provider, board string) company.TargetCompany {
	return company.TargetCompany{ID: id, CompanyID: companyID, ATSProvider: provider, ExternalBoardID: board, IsActive: true}
}

func TestRun_FetchesAndUpsertsEveryJobForEachActiveTarget(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "acme")
	atsClient := &fakeATSClient{jobs: map[string][]ats.Job{
		"acme": {{ExternalID: "1", Title: "A", URL: "https://x.test/1"}, {ExternalID: "2", Title: "B", URL: "https://x.test/2"}},
	}}
	jobs := &fakeJobUpserter{}
	recorder := &fakeTargetRecorder{}

	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, recorder, jobs,
		map[string]ATSClient{"greenhouse": atsClient}, 2, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v, want one successful result", results)
	}
	if results[0].Inserted != 2 {
		t.Errorf("Inserted = %d, want 2", results[0].Inserted)
	}
	if len(jobs.upserts) != 2 {
		t.Fatalf("got %d upserts, want 2", len(jobs.upserts))
	}
	if jobs.upserts[0].TargetCompanyID != 1 || jobs.upserts[0].CompanyID != 10 || jobs.upserts[0].Source != "greenhouse" {
		t.Errorf("upsert record = %+v, unexpected target/company/source linkage", jobs.upserts[0])
	}
	if len(jobs.removedCalls) != 1 || jobs.removedCalls[0].targetID != 1 {
		t.Fatalf("removedCalls = %+v, want one call for target 1", jobs.removedCalls)
	}
	if len(jobs.removedCalls[0].seen) != 2 {
		t.Errorf("seen ids = %v, want both fetched job ids", jobs.removedCalls[0].seen)
	}
	if len(recorder.succeededIDs) != 1 || recorder.succeededIDs[0] != 1 {
		t.Errorf("succeededIDs = %v, want [1]", recorder.succeededIDs)
	}
}

func TestRun_CountsInsertedChangedAndUnchangedSeparately(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "acme")
	atsClient := &fakeATSClient{jobs: map[string][]ats.Job{
		"acme": {
			{ExternalID: "new", Title: "T", URL: "https://x.test/new"},
			{ExternalID: "changed", Title: "T", URL: "https://x.test/changed"},
			{ExternalID: "same", Title: "T", URL: "https://x.test/same"},
		},
	}}
	jobs := &fakeJobUpserter{outcomeFn: func(r job.Record) (job.UpsertOutcome, error) {
		switch r.SourceJobID {
		case "new":
			return job.UpsertOutcome{Inserted: true, Changed: true}, nil
		case "changed":
			return job.UpsertOutcome{Inserted: false, Changed: true}, nil
		default:
			return job.UpsertOutcome{Inserted: false, Changed: false}, nil
		}
	}}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"greenhouse": atsClient}, 1, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	r := results[0]
	if r.Inserted != 1 || r.Changed != 1 || r.Unchanged != 1 {
		t.Errorf("counts = inserted=%d changed=%d unchanged=%d, want 1/1/1", r.Inserted, r.Changed, r.Unchanged)
	}
}

func TestRun_UnregisteredProviderIsAPerTargetErrorNotPanic(t *testing.T) {
	target := testTarget(1, 10, "lever", "acme") // no "lever" client registered below
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, &fakeJobUpserter{},
		map[string]ATSClient{"greenhouse": &fakeATSClient{}}, 1, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want one result with a non-nil error", results)
	}
}

func TestRun_BoardNotFoundDeactivatesTarget(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "gone")
	atsClient := &fakeATSClient{err: map[string]error{"gone": fmt.Errorf("greenhouse: %q: %w", "gone", ats.ErrBoardNotFound)}}
	recorder := &fakeTargetRecorder{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, recorder, &fakeJobUpserter{},
		map[string]ATSClient{"greenhouse": atsClient}, 1, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if results[0].Err == nil {
		t.Fatal("want a non-nil error for the failed fetch")
	}
	if !recorder.wasDeactivated(1) {
		t.Error("target 1 was not deactivated after a board-not-found error")
	}
}

// Regression (adversarial review, S3): a definitively gone board must
// also close its open jobs, or they stay visible with dead links forever
// (the target is no longer fetched, so nothing else would ever close them).
func TestRun_BoardNotFoundAlsoClosesTheTargetsJobs(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "gone")
	atsClient := &fakeATSClient{err: map[string]error{"gone": fmt.Errorf("greenhouse: %w", ats.ErrBoardNotFound)}}
	jobs := &fakeJobUpserter{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"greenhouse": atsClient}, 1, testLogger())

	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(jobs.removedCalls) != 1 || jobs.removedCalls[0].targetID != 1 || len(jobs.removedCalls[0].seen) != 0 {
		t.Errorf("removedCalls = %+v, want exactly one call closing every job of target 1", jobs.removedCalls)
	}
}

// Regression (adversarial review, S1): a job the database refuses is
// skipped and counted, later jobs still ingest, and the skipped job
// still counts as "seen" so it is never closed out as if it vanished.
func TestRun_RejectedJobIsSkippedNotFatalAndStillCountsAsSeen(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "acme")
	atsClient := &fakeATSClient{jobs: map[string][]ats.Job{"acme": {
		{ExternalID: "bad", Title: "T", URL: "https://x.test/bad"},
		{ExternalID: "good", Title: "T", URL: "https://x.test/good"},
	}}}
	jobs := &fakeJobUpserter{outcomeFn: func(r job.Record) (job.UpsertOutcome, error) {
		if r.SourceJobID == "bad" {
			return job.UpsertOutcome{}, fmt.Errorf("wrapped: %w", job.ErrJobTargetMismatch)
		}
		return job.UpsertOutcome{Inserted: true, Changed: true}, nil
	}}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"greenhouse": atsClient}, 1, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	r := results[0]
	if r.Err != nil {
		t.Fatalf("Err = %v, want nil: one refused job must not fail the target", r.Err)
	}
	if r.Skipped != 1 || r.Inserted != 1 {
		t.Errorf("Skipped/Inserted = %d/%d, want 1/1", r.Skipped, r.Inserted)
	}
	if len(jobs.removedCalls) != 1 || len(jobs.removedCalls[0].seen) != 2 {
		t.Errorf("removedCalls = %+v, want one call whose seen list includes BOTH jobs", jobs.removedCalls)
	}
}

func TestRun_JobsMissingIDTitleOrURLAreSkippedWithoutBeingUpserted(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "acme")
	atsClient := &fakeATSClient{jobs: map[string][]ats.Job{"acme": {
		{ExternalID: "1", Title: "", URL: "https://x.test/1"},
		{ExternalID: "2", Title: "T", URL: ""},
		{ExternalID: "", Title: "T", URL: "https://x.test/3"},
	}}}
	jobs := &fakeJobUpserter{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"greenhouse": atsClient}, 1, testLogger())

	results, _ := in.Run(context.Background())
	if results[0].Skipped != 3 || len(jobs.upserts) != 0 {
		t.Errorf("Skipped = %d, upserts = %d, want 3 skipped and none upserted", results[0].Skipped, len(jobs.upserts))
	}
}

// An infrastructure error (not a data rejection) must still abort the
// target — and never reach MarkMissingAsRemoved with a partial seen list.
func TestRun_InfrastructureUpsertErrorAbortsAndNeverMarksRemoved(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "acme")
	atsClient := &fakeATSClient{jobs: map[string][]ats.Job{"acme": {
		{ExternalID: "1", Title: "T", URL: "https://x.test/1"},
		{ExternalID: "2", Title: "T", URL: "https://x.test/2"},
	}}}
	jobs := &fakeJobUpserter{upsertErr: errors.New("connection reset by peer")}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"greenhouse": atsClient}, 1, testLogger())

	results, _ := in.Run(context.Background())
	if results[0].Err == nil {
		t.Error("want a non-nil error for an infrastructure failure")
	}
	if len(jobs.removedCalls) != 0 {
		t.Errorf("MarkMissingAsRemoved called %d times after an aborted run, want 0", len(jobs.removedCalls))
	}
}

// Regression (adversarial review, S2): canonical_url is a global unique
// index; ingestion must not populate it from the job URL.
func TestRun_NeverSetsCanonicalURL(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "acme")
	atsClient := &fakeATSClient{jobs: map[string][]ats.Job{"acme": {{ExternalID: "1", Title: "T", URL: "https://x.test/1"}}}}
	jobs := &fakeJobUpserter{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"greenhouse": atsClient}, 1, testLogger())

	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(jobs.upserts) != 1 || jobs.upserts[0].CanonicalURL != "" || jobs.upserts[0].ApplicationURL != "https://x.test/1" {
		t.Errorf("upsert = %+v, want empty CanonicalURL and the job URL as ApplicationURL", jobs.upserts)
	}
}

func TestRun_TransientFetchErrorDoesNotDeactivateTarget(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "flaky")
	atsClient := &fakeATSClient{err: map[string]error{"flaky": errors.New("network timeout")}}
	recorder := &fakeTargetRecorder{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, recorder, &fakeJobUpserter{},
		map[string]ATSClient{"greenhouse": atsClient}, 1, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if results[0].Err == nil {
		t.Fatal("want a non-nil error for the failed fetch")
	}
	if recorder.wasDeactivated(1) {
		t.Error("target 1 was deactivated after a transient error, want it left active")
	}
}

// The single most important safety invariant in this package: a failed
// fetch must never reach MarkMissingAsRemoved, since an empty
// seenSourceJobIDs from a *failure* (as opposed to a genuinely empty
// board) would wrongly close out every real, still-open job for that
// target.
func TestRun_FetchFailureNeverCallsMarkMissingAsRemoved(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "flaky")
	atsClient := &fakeATSClient{err: map[string]error{"flaky": errors.New("network timeout")}}
	jobs := &fakeJobUpserter{}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, jobs,
		map[string]ATSClient{"greenhouse": atsClient}, 1, testLogger())

	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(jobs.removedCalls) != 0 {
		t.Errorf("MarkMissingAsRemoved was called %d times after a fetch failure, want 0", len(jobs.removedCalls))
	}
}

func TestRun_OneTargetsFailureDoesNotStopOthers(t *testing.T) {
	failing := testTarget(1, 10, "greenhouse", "fails")
	succeeding := testTarget(2, 20, "greenhouse", "works")
	atsClient := &fakeATSClient{
		err:  map[string]error{"fails": errors.New("boom")},
		jobs: map[string][]ats.Job{"works": {{ExternalID: "1"}}},
	}
	in := New(&fakeTargetLister{targets: []company.TargetCompany{failing, succeeding}}, &fakeTargetRecorder{}, &fakeJobUpserter{},
		map[string]ATSClient{"greenhouse": atsClient}, 2, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	var sawFailure, sawSuccess bool
	for _, r := range results {
		if r.Target.ID == 1 && r.Err != nil {
			sawFailure = true
		}
		if r.Target.ID == 2 && r.Err == nil {
			sawSuccess = true
		}
	}
	if !sawFailure || !sawSuccess {
		t.Errorf("results = %+v, want target 1 failed and target 2 succeeded independently", results)
	}
}

func TestRun_ListActiveErrorIsPropagated(t *testing.T) {
	in := New(&fakeTargetLister{err: errors.New("db down")}, &fakeTargetRecorder{}, &fakeJobUpserter{},
		map[string]ATSClient{}, 1, testLogger())

	_, err := in.Run(context.Background())
	if err == nil {
		t.Fatal("want a non-nil error when ListActive fails")
	}
}

func TestRun_RespectsWorkerBound(t *testing.T) {
	const workers = 2
	var targets []company.TargetCompany
	jobsByBoard := map[string][]ats.Job{}
	for i := range 8 {
		board := fmt.Sprintf("board-%d", i)
		targets = append(targets, testTarget(int64(i+1), int64(i+1), "greenhouse", board))
		jobsByBoard[board] = nil
	}
	atsClient := &fakeATSClient{jobs: jobsByBoard, delay: 30 * time.Millisecond}

	in := New(&fakeTargetLister{targets: targets}, &fakeTargetRecorder{}, &fakeJobUpserter{},
		map[string]ATSClient{"greenhouse": atsClient}, workers, testLogger())

	results, err := in.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(results) != len(targets) {
		t.Fatalf("got %d results, want %d", len(results), len(targets))
	}
	if got := atomic.LoadInt32(&atsClient.maxSeen); got > int32(workers) {
		t.Errorf("observed %d concurrent fetches, want at most %d", got, workers)
	}
}

func TestRun_AlreadyCanceledContextReturnsPromptlyWithoutHanging(t *testing.T) {
	var targets []company.TargetCompany
	for i := range 20 {
		targets = append(targets, testTarget(int64(i+1), int64(i+1), "greenhouse", fmt.Sprintf("board-%d", i)))
	}
	atsClient := &fakeATSClient{delay: 2 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	in := New(&fakeTargetLister{targets: targets}, &fakeTargetRecorder{}, &fakeJobUpserter{},
		map[string]ATSClient{"greenhouse": atsClient}, 2, testLogger())

	done := make(chan []Result, 1)
	go func() {
		results, _ := in.Run(ctx)
		done <- results
	}()

	select {
	case results := <-done:
		if len(results) > len(targets) {
			t.Errorf("got %d results, want at most %d", len(results), len(targets))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return promptly for an already-canceled context")
	}
}

func TestNew_ClampsNonPositiveWorkersToOne(t *testing.T) {
	target := testTarget(1, 10, "greenhouse", "acme")
	atsClient := &fakeATSClient{jobs: map[string][]ats.Job{"acme": nil}}

	in := New(&fakeTargetLister{targets: []company.TargetCompany{target}}, &fakeTargetRecorder{}, &fakeJobUpserter{},
		map[string]ATSClient{"greenhouse": atsClient}, 0, testLogger())

	done := make(chan []Result, 1)
	go func() {
		results, _ := in.Run(context.Background())
		done <- results
	}()

	select {
	case results := <-done:
		if len(results) != 1 {
			t.Errorf("got %d results, want 1", len(results))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() with workers=0 did not return -- the feeder goroutine leaked (see internal/discovery's identical regression)")
	}
}
