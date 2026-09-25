package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// fakeCompanyUpserter and fakeTargetUpserter let discovery's own
// orchestration logic (probe fallback, concurrency bound, error
// propagation) be tested without a real Postgres — internal/company's
// own package already proves the real Upsert behavior against a real
// database; these fakes exist so a bug in that wiring never depends on
// a database being up to catch.

type fakeCompanyUpserter struct {
	mu     sync.Mutex
	calls  []company.UpsertParams
	err    error
	nextID int64
}

func (f *fakeCompanyUpserter) Upsert(_ context.Context, params company.UpsertParams) (*company.Company, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, params)
	f.nextID++
	return &company.Company{ID: f.nextID, Name: params.Name, Website: params.Website}, nil
}

func (f *fakeCompanyUpserter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeTargetUpserter struct {
	mu     sync.Mutex
	calls  []company.TargetUpsertParams
	err    error
	nextID int64
}

func (f *fakeTargetUpserter) Upsert(_ context.Context, params company.TargetUpsertParams) (*company.TargetCompany, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, params)
	f.nextID++
	return &company.TargetCompany{
		ID:              f.nextID,
		CompanyID:       params.CompanyID,
		ATSProvider:     params.ATSProvider,
		ExternalBoardID: params.ExternalBoardID,
		BoardURL:        params.BoardURL,
		IsActive:        true,
	}, nil
}

func (f *fakeTargetUpserter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testClient() *httpclient.Client {
	cfg := httpclient.DefaultConfig()
	cfg.Timeout = 5 * time.Second
	cfg.MaxRetries = 0 // tests want deterministic single attempts, not backoff delays
	return httpclient.New(cfg)
}

func validCandidate(boardURL string) Candidate {
	return Candidate{
		CompanyName:     "Acme",
		ATSProvider:     "greenhouse",
		ExternalBoardID: "acme",
		BoardURL:        boardURL,
	}
}

func TestRun_SuccessfulCandidateIsPersisted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	companies := &fakeCompanyUpserter{}
	targets := &fakeTargetUpserter{}
	d := New(companies, targets, testClient(), 2, testLogger())

	results := d.Run(context.Background(), []Candidate{validCandidate(srv.URL)})

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Err != nil {
		t.Fatalf("Result.Err = %v, want nil", r.Err)
	}
	if !r.Validated {
		t.Error("Result.Validated = false, want true")
	}
	if r.Company == nil || r.Target == nil {
		t.Fatal("Result.Company/Target must both be set on success")
	}
	if companies.callCount() != 1 || targets.callCount() != 1 {
		t.Errorf("companies.callCount()=%d targets.callCount()=%d, want 1 and 1", companies.callCount(), targets.callCount())
	}
}

func TestRun_HeadFailsGetSucceeds_FallsBackToGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	companies := &fakeCompanyUpserter{}
	targets := &fakeTargetUpserter{}
	d := New(companies, targets, testClient(), 1, testLogger())

	results := d.Run(context.Background(), []Candidate{validCandidate(srv.URL)})

	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v, want one successful result", results)
	}
	if len(targets.calls) != 1 {
		t.Fatalf("expected exactly one target upsert call, got %d", len(targets.calls))
	}
	method, _ := targets.calls[0].DiscoveryMetadata["validation_method"].(string)
	if method != http.MethodGet {
		t.Errorf("DiscoveryMetadata[validation_method] = %q, want %q (HEAD failed, GET must be the one that succeeded)", method, http.MethodGet)
	}
}

// Only a board registered on the strength of a name look-up carries the marker
// that lets a later re-check deactivate it.
func TestRun_OnlyAVerifiedCandidateCarriesTheDiscoverBoardsMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	targets := &fakeTargetUpserter{}
	d := New(&fakeCompanyUpserter{}, targets, testClient(), 1, testLogger())
	verified := validCandidate(srv.URL)
	verified.Verified = true
	d.Run(context.Background(), []Candidate{verified})
	seeded := validCandidate(srv.URL)
	seeded.ExternalBoardID = "acme-2"
	d.Run(context.Background(), []Candidate{seeded})

	if len(targets.calls) != 2 {
		t.Fatalf("%d target upserts, want 2", len(targets.calls))
	}
	if got := targets.calls[0].DiscoveryMetadata["source"]; got != SourceDiscoverBoards {
		t.Errorf("verified candidate: metadata source = %v, want %q", got, SourceDiscoverBoards)
	}
	if got, ok := targets.calls[1].DiscoveryMetadata["source"]; ok {
		t.Errorf("probed candidate: metadata source = %v, want none", got)
	}
}

func TestRun_BothHeadAndGetFail_NotPersisted(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	companies := &fakeCompanyUpserter{}
	targets := &fakeTargetUpserter{}
	d := New(companies, targets, testClient(), 1, testLogger())

	results := d.Run(context.Background(), []Candidate{validCandidate(srv.URL)})

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Err == nil {
		t.Fatal("Result.Err = nil, want an error (both HEAD and GET failed)")
	}
	if r.Validated {
		t.Error("Result.Validated = true, want false")
	}
	if companies.callCount() != 0 || targets.callCount() != 0 {
		t.Errorf("a failed probe must never reach persistence: companies=%d targets=%d", companies.callCount(), targets.callCount())
	}
	if hits != 2 {
		t.Errorf("server hit %d times, want exactly 2 (one HEAD, one GET fallback)", hits)
	}
}

func TestRun_InvalidCandidateIsNeverProbed(t *testing.T) {
	hitServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server was hit; an invalid candidate must be rejected before any HTTP request")
		w.WriteHeader(http.StatusOK)
	}))
	defer hitServer.Close()

	companies := &fakeCompanyUpserter{}
	targets := &fakeTargetUpserter{}
	d := New(companies, targets, testClient(), 1, testLogger())

	invalid := validCandidate(hitServer.URL)
	invalid.CompanyName = "" // now invalid

	results := d.Run(context.Background(), []Candidate{invalid})

	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want one result with a validation error", results)
	}
}

func TestRun_CompanyUpsertErrorIsPropagatedAndTargetIsNeverCalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	companies := &fakeCompanyUpserter{err: errors.New("boom")}
	targets := &fakeTargetUpserter{}
	d := New(companies, targets, testClient(), 1, testLogger())

	results := d.Run(context.Background(), []Candidate{validCandidate(srv.URL)})

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "boom") {
		t.Errorf("Result.Err = %v, want it to wrap the company upsert error", r.Err)
	}
	if targets.callCount() != 0 {
		t.Errorf("target upsert must not be attempted after a company upsert failure, got %d calls", targets.callCount())
	}
}

func TestRun_TargetUpsertErrorIsPropagatedButCompanyIsStillSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	companies := &fakeCompanyUpserter{}
	targets := &fakeTargetUpserter{err: errors.New("target boom")}
	d := New(companies, targets, testClient(), 1, testLogger())

	results := d.Run(context.Background(), []Candidate{validCandidate(srv.URL)})

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "target boom") {
		t.Errorf("Result.Err = %v, want it to wrap the target upsert error", r.Err)
	}
	if r.Company == nil {
		t.Error("Result.Company = nil, want it set: the company upsert succeeded before the target upsert failed")
	}
}

func TestRun_ReturnsOneResultPerCandidate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var candidates []Candidate
	for i := 0; i < 10; i++ {
		c := validCandidate(srv.URL)
		c.ExternalBoardID = fmt.Sprintf("acme-%d", i)
		candidates = append(candidates, c)
	}

	d := New(&fakeCompanyUpserter{}, &fakeTargetUpserter{}, testClient(), 3, testLogger())
	results := d.Run(context.Background(), candidates)

	if len(results) != len(candidates) {
		t.Fatalf("got %d results, want %d", len(results), len(candidates))
	}
	seen := make(map[string]bool)
	for _, r := range results {
		seen[r.Candidate.ExternalBoardID] = true
	}
	if len(seen) != len(candidates) {
		t.Errorf("results cover %d distinct candidates, want %d", len(seen), len(candidates))
	}
}

func TestRun_RespectsWorkerBound(t *testing.T) {
	const workers = 3
	var (
		current int32
		maxSeen int32
		mu      sync.Mutex
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&current, 1)
		mu.Lock()
		if n > maxSeen {
			maxSeen = n
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var candidates []Candidate
	for i := 0; i < 12; i++ {
		c := validCandidate(srv.URL)
		c.ExternalBoardID = fmt.Sprintf("acme-%d", i)
		candidates = append(candidates, c)
	}

	d := New(&fakeCompanyUpserter{}, &fakeTargetUpserter{}, testClient(), workers, testLogger())
	results := d.Run(context.Background(), candidates)

	if len(results) != len(candidates) {
		t.Fatalf("got %d results, want %d", len(results), len(candidates))
	}
	mu.Lock()
	got := maxSeen
	mu.Unlock()
	if got > int32(workers) {
		t.Errorf("observed %d concurrent probes, want at most %d (the configured worker bound)", got, workers)
	}
}

func TestRun_AlreadyCanceledContextReturnsPromptlyWithoutHanging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // long enough that a real bug would time out this test
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var candidates []Candidate
	for i := 0; i < 20; i++ {
		c := validCandidate(srv.URL)
		c.ExternalBoardID = fmt.Sprintf("acme-%d", i)
		candidates = append(candidates, c)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := New(&fakeCompanyUpserter{}, &fakeTargetUpserter{}, testClient(), 2, testLogger())

	done := make(chan []Result, 1)
	go func() { done <- d.Run(ctx, candidates) }()

	select {
	case results := <-done:
		if len(results) > len(candidates) {
			t.Errorf("got %d results, want at most %d", len(results), len(candidates))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return promptly for an already-canceled context")
	}
}

// Regression test for a goroutine leak found by adversarial review: with
// workers <= 0, the pre-fix New() spawned zero worker goroutines, so
// Run's feeder goroutine had nothing to ever receive from its work
// channel and blocked forever on any non-empty candidate list (a real
// goroutine leak, reproduced with a stack dump during review). New must
// clamp workers to at least 1 rather than trusting the caller, since New
// is exported and config's own >=1 validation doesn't protect a
// hypothetical future caller.
func TestNew_ClampsNonPositiveWorkersToOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, workers := range []int{0, -1, -100} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			d := New(&fakeCompanyUpserter{}, &fakeTargetUpserter{}, testClient(), workers, testLogger())

			done := make(chan []Result, 1)
			go func() { done <- d.Run(context.Background(), []Candidate{validCandidate(srv.URL)}) }()

			select {
			case results := <-done:
				// The bug this guards against does not make Run itself
				// hang: with zero workers, wg.Wait() over zero counters
				// returns immediately, so Run returns an empty slice
				// right away — it is the feeder goroutine, left forever
				// blocked on an unbuffered send nothing will ever drain,
				// that leaks. This is the branch that actually catches
				// an unclamped workers count.
				if len(results) != 1 || results[0].Err != nil {
					t.Errorf("results = %+v, want one successful result (an unclamped "+
						"workers<=0 returns zero results immediately instead, without erroring)", results)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Run() did not return within 3s")
			}
		})
	}
}
