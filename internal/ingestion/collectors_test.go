package ingestion

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

type fakeCollector struct {
	jobs       []ats.Job
	err        error
	staleAfter time.Duration
	priority   string // PriorityProvider, "" = not a PriorityRouter
	calls      int
}

func (f *fakeCollector) Collect(context.Context) ([]ats.Job, error) {
	f.calls++
	return f.jobs, f.err
}
func (f *fakeCollector) StaleAfter() time.Duration { return f.staleAfter }

// routedCollector adds the optional PriorityRouter behavior.
type routedCollector struct{ *fakeCollector }

func (r routedCollector) PriorityProvider() string { return r.priority }

type fakeRegistrar struct {
	mu      sync.Mutex
	nextID  int64
	targets map[string]company.TargetCompany
	calls   []registrarCall  // EnsureTarget
	finds   []registrarCall  // FindTarget
	failFor map[string]error // employer -> error
}

type registrarCall struct {
	provider, employer string
	priority           bool
	market             market.Market
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{targets: map[string]company.TargetCompany{}, failFor: map[string]error{}}
}

func (f *fakeRegistrar) EnsureTarget(_ context.Context, provider, employer string, priority bool, mk market.Market) (company.TargetCompany, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, registrarCall{provider, employer, priority, mk})
	if err := f.failFor[employer]; err != nil {
		return company.TargetCompany{}, err
	}
	key := provider + "/" + strings.ToLower(employer)
	if t, ok := f.targets[key]; ok {
		return t, nil
	}
	f.nextID++
	t := company.TargetCompany{ID: f.nextID, CompanyID: 1000 + f.nextID, ATSProvider: provider, ExternalBoardID: strings.ToLower(employer), IsActive: true}
	f.targets[key] = t
	return t, nil
}

func (f *fakeRegistrar) FindTarget(_ context.Context, provider, employer string) (company.TargetCompany, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds = append(f.finds, registrarCall{provider: provider, employer: employer})
	t, ok := f.targets[provider+"/"+strings.ToLower(employer)]
	return t, ok, nil
}

type fakeResolver map[string]struct {
	canonical string
	priority  bool
}

func (f fakeResolver) Resolve(employer string) (string, bool) {
	r, ok := f[strings.ToLower(employer)]
	if !ok {
		return "", false
	}
	return r.canonical, r.priority
}

func mkJob(id, title, employer string) ats.Job {
	return ats.Job{ExternalID: id, Title: title, URL: "https://x.example/" + id, Employer: employer}
}

func newCollectorIngester(jobs *fakeJobUpserter, reg *fakeRegistrar, resolver EmployerResolver, sweep TargetLister, cols map[string]Collector) (*Ingester, *fakeTargetRecorder) {
	rec := &fakeTargetRecorder{}
	in := New(&fakeTargetLister{}, rec, jobs, map[string]ATSClient{}, 1, testLogger())
	in.WithCollectors(CollectorConfig{Collectors: cols, Registrar: reg, Resolver: resolver, Targets: sweep})
	return in, rec
}

func TestRunCollectors_GroupsJobsByEmployerAndRegistersATargetForEach(t *testing.T) {
	col := &fakeCollector{staleAfter: 21 * 24 * time.Hour, jobs: []ats.Job{
		mkJob("1", "Accountant", "Acme PLC"),
		mkJob("2", "Driver", "Beta Ltd"),
		mkJob("3", "Cashier", "acme plc"), // same employer, different case/spacing
		mkJob("4", "Clerk", "  Acme   PLC "),
	}}
	reg, jobs := newFakeRegistrar(), &fakeJobUpserter{}
	in, rec := newCollectorIngester(jobs, reg, nil, nil, map[string]Collector{"ethiojobs": col})

	results, err := in.RunCollectors(context.Background(), []string{"ethiojobs"})
	if err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}

	if len(reg.calls) != 2 {
		t.Fatalf("registrar calls = %+v, want 2 (one per distinct employer)", reg.calls)
	}
	if reg.calls[0].provider != "ethiojobs" || reg.calls[0].employer != "Acme PLC" || reg.calls[0].priority {
		t.Errorf("first registration = %+v", reg.calls[0])
	}
	if len(results) != 2 || results[0].Inserted != 3 || results[1].Inserted != 1 {
		t.Errorf("results = %+v, want Acme with 3 jobs then Beta with 1", results)
	}
	if len(jobs.upserts) != 4 {
		t.Fatalf("upserts = %d, want 4", len(jobs.upserts))
	}
	for _, u := range jobs.upserts {
		if u.Source != "ethiojobs" || u.CompanyID == 0 || u.TargetCompanyID == 0 {
			t.Errorf("record %+v: source/company/target not taken from the registered target", u)
		}
	}
	if len(rec.succeededIDs) != 2 {
		t.Errorf("recorded %d successful ingestions, want 2", len(rec.succeededIDs))
	}
}

// A collector's listing is a sample: it must age jobs out, never close
// "whatever this run did not return".
func TestRunCollectors_UsesTheStaleRuleNeverMarkMissingAsRemoved(t *testing.T) {
	col := &fakeCollector{staleAfter: 7 * 24 * time.Hour, jobs: []ats.Job{mkJob("1", "Accountant", "Acme")}}
	jobs := &fakeJobUpserter{}
	in, _ := newCollectorIngester(jobs, newFakeRegistrar(), nil, nil, map[string]Collector{"ethiojobs": col})

	if _, err := in.RunCollectors(context.Background(), []string{"ethiojobs"}); err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	if len(jobs.removedCalls) != 0 {
		t.Errorf("MarkMissingAsRemoved called %d times for a sample source", len(jobs.removedCalls))
	}
	if len(jobs.staleCalls) != 1 || jobs.staleCalls[0].olderThan != 7*24*time.Hour {
		t.Errorf("CloseStale calls = %+v, want one with the collector's window", jobs.staleCalls)
	}
}

func TestRunCollectors_APriorityEmployerKeepsItsCanonicalNameAndIsFlagged(t *testing.T) {
	col := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{mkJob("1", "Compliance Officer", "Ethswitch S.C.")}}
	reg := newFakeRegistrar()
	resolver := fakeResolver{"ethswitch s.c.": {canonical: "EthSwitch", priority: true}}
	in, _ := newCollectorIngester(&fakeJobUpserter{}, reg, resolver, nil, map[string]Collector{"ethiojobs": col})

	if _, err := in.RunCollectors(context.Background(), []string{"ethiojobs"}); err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	if len(reg.calls) != 1 || reg.calls[0] != (registrarCall{"ethiojobs", "EthSwitch", true, ""}) {
		t.Errorf("registration = %+v, want the canonical name EthSwitch flagged priority", reg.calls)
	}
}

// The LinkedIn collector files a priority company's jobs under "search",
// the provider that company's own per-company target already uses.
func TestRunCollectors_PriorityRouterSendsPriorityEmployersToTheOtherProvider(t *testing.T) {
	inner := &fakeCollector{staleAfter: time.Hour, priority: "search", jobs: []ats.Job{
		mkJob("linkedin:1", "Compliance Officer", "EthSwitch S.C."),
		mkJob("linkedin:2", "Driver", "Some Other Company"),
	}}
	reg := newFakeRegistrar()
	resolver := fakeResolver{"ethswitch s.c.": {canonical: "EthSwitch", priority: true}}
	in, _ := newCollectorIngester(&fakeJobUpserter{}, reg, resolver, nil, map[string]Collector{"linkedin": routedCollector{inner}})

	if _, err := in.RunCollectors(context.Background(), []string{"linkedin"}); err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	want := []registrarCall{{"search", "EthSwitch", true, market.Ethiopia}, {"linkedin", "Some Other Company", false, ""}}
	if !slices.Equal(reg.calls, want) {
		t.Errorf("registrations = %+v, want %+v", reg.calls, want)
	}
}

func TestRunCollectors_AnOpeningAnEarlierCollectorStoredIsNotStoredAgain(t *testing.T) {
	first := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{mkJob("e1", "Senior Accountant", "Acme")}}
	second := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{
		mkJob("l1", "senior  accountant!", "ACME"), // same opening on the other site
		mkJob("l2", "Driver", "Acme"),              // a different opening
		mkJob("l3", "Senior Accountant", "Beta"),   // same title, different employer
	}}
	jobs := &fakeJobUpserter{}
	in, _ := newCollectorIngester(jobs, newFakeRegistrar(), nil, nil, map[string]Collector{"ethiojobs": first, "linkedin": second})

	if _, err := in.RunCollectors(context.Background(), []string{"ethiojobs", "linkedin"}); err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	var stored []string
	for _, u := range jobs.upserts {
		stored = append(stored, u.SourceJobID)
	}
	slices.Sort(stored)
	if !slices.Equal(stored, []string{"e1", "l2", "l3"}) {
		t.Errorf("stored = %v, want e1, l2, l3 (l1 duplicates e1)", stored)
	}
}

func TestRunCollectors_ClosedJobsAreClosedNotStored(t *testing.T) {
	col := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{
		mkJob("live", "Accountant", "Acme"),
		{ExternalID: "gone", Closed: true, Employer: "Acme"},
	}}
	jobs := &fakeJobUpserter{endedClosed: 1}
	in, _ := newCollectorIngester(jobs, newFakeRegistrar(), nil, nil, map[string]Collector{"ethiojobs": col})

	results, err := in.RunCollectors(context.Background(), []string{"ethiojobs"})
	if err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	if len(jobs.upserts) != 1 || jobs.upserts[0].SourceJobID != "live" {
		t.Errorf("upserts = %+v, want only the live job", jobs.upserts)
	}
	if len(jobs.endedCalls) != 1 || !slices.Equal(jobs.endedCalls[0].ids, []string{"gone"}) {
		t.Errorf("CloseBySourceID calls = %+v, want one for 'gone'", jobs.endedCalls)
	}
	if results[0].Removed != 1 {
		t.Errorf("Removed = %d, want 1", results[0].Removed)
	}
}

func TestRunCollectors_AFailingCollectorDoesNotStopTheOthers(t *testing.T) {
	bad := &fakeCollector{staleAfter: time.Hour, err: errors.New("site down")}
	good := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{mkJob("1", "Accountant", "Acme")}}
	jobs := &fakeJobUpserter{}
	in, _ := newCollectorIngester(jobs, newFakeRegistrar(), nil, nil, map[string]Collector{"ethiojobs": bad, "linkedin": good})

	results, err := in.RunCollectors(context.Background(), []string{"ethiojobs", "linkedin"})
	if err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	if len(results) != 2 || results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "ethiojobs") || results[1].Err != nil {
		t.Errorf("results = %+v, want the first failed (naming the collector) and the second fine", results)
	}
	if len(jobs.upserts) != 1 {
		t.Errorf("upserts = %d, want the good collector's 1", len(jobs.upserts))
	}
	// A failed collection must not close anything.
	if len(jobs.staleCalls) != 1 || len(jobs.removedCalls) != 0 {
		t.Errorf("stale calls = %d, removed calls = %d; only the good collector's employer may be aged", len(jobs.staleCalls), len(jobs.removedCalls))
	}
}

func TestRunCollectors_ASampleThatDeclaresNoStaleWindowIsRefused(t *testing.T) {
	col := &fakeCollector{staleAfter: 0, jobs: []ats.Job{mkJob("1", "Accountant", "Acme")}}
	jobs := &fakeJobUpserter{}
	in, _ := newCollectorIngester(jobs, newFakeRegistrar(), nil, nil, map[string]Collector{"x": col})

	results, _ := in.RunCollectors(context.Background(), []string{"x"})
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want one failed result", results)
	}
	if len(jobs.upserts) != 0 || len(jobs.removedCalls) != 0 {
		t.Errorf("stored/closed jobs for a collector with no StaleAfter: upserts=%d removed=%d", len(jobs.upserts), len(jobs.removedCalls))
	}
}

func TestRunCollectors_AnEmployerThatCannotBeRegisteredFailsAloneAndJobsWithoutOneAreSkipped(t *testing.T) {
	col := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{
		mkJob("1", "Accountant", "Broken Co"),
		mkJob("2", "Driver", "Fine Co"),
		mkJob("3", "Nameless", ""),
		mkJob("4", "Blank", "   "),
	}}
	reg := newFakeRegistrar()
	reg.failFor["Broken Co"] = errors.New("target already registered to a different company")
	jobs := &fakeJobUpserter{}
	in, _ := newCollectorIngester(jobs, reg, nil, nil, map[string]Collector{"ethiojobs": col})

	results, err := in.RunCollectors(context.Background(), []string{"ethiojobs"})
	if err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	if len(results) != 2 || results[0].Err == nil || results[1].Err != nil {
		t.Fatalf("results = %+v, want Broken Co failed and Fine Co fine", results)
	}
	if len(jobs.upserts) != 1 || jobs.upserts[0].SourceJobID != "2" {
		t.Errorf("upserts = %+v, want only Fine Co's job", jobs.upserts)
	}
}

func TestRunCollectors_EmployersAbsentFromTheRunAreAgedOutButOnlyThisCollectorsOwn(t *testing.T) {
	col := &fakeCollector{staleAfter: 21 * 24 * time.Hour, jobs: []ats.Job{mkJob("1", "Accountant", "Acme")}}
	reg, jobs := newFakeRegistrar(), &fakeJobUpserter{staleClosed: 2}
	// Existing active targets: Acme (touched by the run), Gone Co and another
	// collector's target and a Greenhouse board (must not be aged by this run).
	acme, _ := reg.EnsureTarget(context.Background(), "ethiojobs", "Acme", false, "")
	reg.calls = nil
	sweep := &fakeTargetLister{targets: []company.TargetCompany{
		acme,
		{ID: 501, CompanyID: 1, ATSProvider: "ethiojobs", ExternalBoardID: "gone co"},
		{ID: 502, CompanyID: 2, ATSProvider: "linkedin", ExternalBoardID: "other collectors target"},
		{ID: 503, CompanyID: 3, ATSProvider: "greenhouse", ExternalBoardID: "board"},
	}}
	in, _ := newCollectorIngester(jobs, reg, nil, sweep, map[string]Collector{"ethiojobs": col})

	results, err := in.RunCollectors(context.Background(), []string{"ethiojobs"})
	if err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	var aged []int64
	for _, c := range jobs.staleCalls {
		aged = append(aged, c.targetID)
	}
	slices.Sort(aged)
	if !slices.Equal(aged, []int64{acme.ID, 501}) {
		t.Errorf("CloseStale targets = %v, want Acme (the run's own persist) and 501 (absent) only", aged)
	}
	found := false
	for _, r := range results {
		if r.Target.ID == 501 && r.Removed == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("results = %+v, want a result reporting 2 jobs aged out of target 501", results)
	}
}

func TestRunCollectors_Validation(t *testing.T) {
	col := &fakeCollector{staleAfter: time.Hour}
	in, _ := newCollectorIngester(&fakeJobUpserter{}, newFakeRegistrar(), nil, nil, map[string]Collector{"ethiojobs": col})

	if _, err := in.RunCollectors(context.Background(), []string{"ethiojobs", "nope"}); err == nil {
		t.Error("an unknown collector name was accepted")
	}
	if col.calls != 0 {
		t.Errorf("collector ran %d times although another requested name was unknown", col.calls)
	}

	noReg := New(&fakeTargetLister{}, &fakeTargetRecorder{}, &fakeJobUpserter{}, nil, 1, testLogger())
	noReg.WithCollectors(CollectorConfig{Collectors: map[string]Collector{"ethiojobs": col}})
	if _, err := noReg.RunCollectors(context.Background(), []string{"ethiojobs"}); err == nil {
		t.Error("collectors ran without a TargetRegistrar")
	}

	if names := in.CollectorNames(); !slices.Equal(names, []string{"ethiojobs"}) {
		t.Errorf("CollectorNames() = %v", names)
	}
}

func TestRunCollectors_StopsWhenTheContextIsCanceled(t *testing.T) {
	col := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{mkJob("1", "Accountant", "Acme")}}
	jobs := &fakeJobUpserter{}
	in, _ := newCollectorIngester(jobs, newFakeRegistrar(), nil, nil, map[string]Collector{"ethiojobs": col})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := in.RunCollectors(ctx, []string{"ethiojobs"}); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if col.calls != 0 || len(jobs.upserts) != 0 {
		t.Errorf("work done after cancellation: collect calls=%d upserts=%d", col.calls, len(jobs.upserts))
	}
}

// An employer seen only through ended postings must not become a company.
func TestRunCollectors_AnEmployerWithOnlyEndedPostingsIsNeverCreated(t *testing.T) {
	col := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{
		{ExternalID: "gone1", Closed: true, Employer: "Unknown Co"},
		{ExternalID: "gone2", Closed: true, Employer: "Known Co"},
		mkJob("live", "Accountant", "Live Co"),
	}}
	reg, jobs := newFakeRegistrar(), &fakeJobUpserter{endedClosed: 1}
	// Known Co already has a target from an earlier run.
	if _, err := reg.EnsureTarget(context.Background(), "ethiojobs", "Known Co", false, ""); err != nil {
		t.Fatal(err)
	}
	reg.calls = nil
	in, _ := newCollectorIngester(jobs, reg, nil, nil, map[string]Collector{"ethiojobs": col})

	if _, err := in.RunCollectors(context.Background(), []string{"ethiojobs"}); err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	var created []string
	for _, c := range reg.calls {
		created = append(created, c.employer)
	}
	if !slices.Equal(created, []string{"Live Co"}) {
		t.Errorf("EnsureTarget called for %v, want only Live Co", created)
	}
	if _, ok := reg.targets["ethiojobs/unknown co"]; ok {
		t.Errorf("a target was created for an employer with only ended postings")
	}
	closedFor := map[string]bool{}
	for _, c := range jobs.endedCalls {
		for _, id := range c.ids {
			closedFor[id] = true
		}
	}
	if !closedFor["gone2"] || closedFor["gone1"] {
		t.Errorf("CloseBySourceID ids = %v, want gone2 (known employer) and not gone1 (unknown)", closedFor)
	}
}

func storedIDs(jobs *fakeJobUpserter) []string {
	var ids []string
	for _, u := range jobs.upserts {
		ids = append(ids, u.SourceJobID)
	}
	slices.Sort(ids)
	return ids
}

// The same title in two cities is two openings; "Ethiopia" alone could be
// either city, so it is the same opening.
func TestRunCollectors_TheSameTitleInAnotherCityIsAnotherOpening(t *testing.T) {
	at := func(id, place string) ats.Job {
		j := mkJob(id, "Customer Service Officer", "Bank")
		j.LocationRaw = place
		return j
	}
	first := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{at("e1", "Addis Ababa")}}
	second := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{
		at("l1", "Hawassa, Ethiopia"),        // another city: kept
		at("l2", "Ethiopia"),                 // no city, Ethiopian: the same opening as e1
		at("l3", "Addis Ababa, Addis Ababa"), // same city, other spelling: duplicate
		at("l4", "Nairobi, Kenya"),           // another country: kept
	}}
	jobs := &fakeJobUpserter{}
	in, _ := newCollectorIngester(jobs, newFakeRegistrar(), nil, nil, map[string]Collector{"ethiojobs": first, "linkedin": second})

	if _, err := in.RunCollectors(context.Background(), []string{"ethiojobs", "linkedin"}); err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	if got := storedIDs(jobs); !slices.Equal(got, []string{"e1", "l1", "l4"}) {
		t.Errorf("stored = %v, want e1, l1, l4", got)
	}
}

// An opening the first collector could not store (the database refused it)
// is not stored, so the second collector's copy must not be dropped as a
// duplicate of it.
func TestRunCollectors_AnOpeningThatFailedToStoreDoesNotSuppressTheNextCollectorsCopy(t *testing.T) {
	first := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{mkJob("e1", "Accountant", "Acme")}}
	second := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{mkJob("l1", "Accountant", "Acme")}}
	jobs := &fakeJobUpserter{outcomeFn: func(r job.Record) (job.UpsertOutcome, error) {
		if r.SourceJobID == "e1" {
			return job.UpsertOutcome{}, job.ErrJobTargetMismatch
		}
		return job.UpsertOutcome{Inserted: true, Changed: true}, nil
	}}
	in, _ := newCollectorIngester(jobs, newFakeRegistrar(), nil, nil, map[string]Collector{"ethiojobs": first, "linkedin": second})

	if _, err := in.RunCollectors(context.Background(), []string{"ethiojobs", "linkedin"}); err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	// The fake records every attempt: e1 (refused), then l1.
	if got := storedIDs(jobs); !slices.Equal(got, []string{"e1", "l1"}) {
		t.Errorf("upsert attempts = %v, want e1 (refused) then l1", got)
	}
}

// marketCollector adds the optional market.Provider behavior.
type marketCollector struct {
	*fakeCollector
	mk market.Market
}

func (m marketCollector) Market() market.Market { return m.mk }

// A collector that names its market has its employers' targets registered in
// it; one that does not leaves the market to the registrar (unset).
func TestRunCollectors_TargetsAreRegisteredInTheCollectorsMarket(t *testing.T) {
	ethiopian := marketCollector{&fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{mkJob("e1", "Cook", "Local Co")}}, market.Ethiopia}
	global := marketCollector{&fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{mkJob("g1", "Cook", "Global Co")}}, market.Worldwide}
	silent := &fakeCollector{staleAfter: time.Hour, jobs: []ats.Job{mkJob("s1", "Cook", "Silent Co")}}
	reg := newFakeRegistrar()
	in, _ := newCollectorIngester(&fakeJobUpserter{}, reg, nil, nil, map[string]Collector{"ethiojobs": ethiopian, "remotive": global, "other": silent})

	if _, err := in.RunCollectors(context.Background(), []string{"ethiojobs", "remotive", "other"}); err != nil {
		t.Fatalf("RunCollectors() error = %v", err)
	}
	want := []registrarCall{
		{"ethiojobs", "Local Co", false, market.Ethiopia},
		{"remotive", "Global Co", false, market.Worldwide},
		{"other", "Silent Co", false, ""},
	}
	if !slices.Equal(reg.calls, want) {
		t.Errorf("registrar calls = %+v, want %+v", reg.calls, want)
	}
}
