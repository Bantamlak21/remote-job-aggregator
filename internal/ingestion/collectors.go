package ingestion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/companymatch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

// Collector gathers jobs from a source that spans many employers: a job
// board's newest listings, a keyword search. Unlike an ATSClient it is not
// asked about one company's board; it returns whatever it found, every job
// naming its Employer, and the Ingester works out which company and target
// each belongs to (creating them on first sight).
//
// A collector's listing is always a sample (the newest N jobs, the first
// pages of a search), so absence from one run proves nothing: StaleAfter
// says how long a job may go unseen before it is closed.
type Collector interface {
	Collect(ctx context.Context) ([]ats.Job, error)
	StaleAfter() time.Duration
}

// PriorityRouter is an optional interface for a Collector whose jobs for a
// priority company should be stored under a different provider than the
// collector's own. The LinkedIn keyword collector uses it to file a priority
// company's jobs under "search", the provider the per-company search source
// already uses for that company, so one LinkedIn job found by both sources
// is one row, not two.
type PriorityRouter interface {
	PriorityProvider() string
}

// IntervalCollector is an optional interface for a Collector whose source
// limits how often it may be called. The ingester skips such a collector when
// it ran less than MinInterval ago (see RunLog), unless forced.
type IntervalCollector interface {
	MinInterval() time.Duration
}

// RunLog remembers when each collector last ran, so MinInterval holds across
// separate ingest processes.
type RunLog interface {
	// LastRun returns when the named collector last ran; ok is false if never.
	LastRun(ctx context.Context, name string) (at time.Time, ok bool, err error)
	// RecordRun records that the named collector ran at at.
	RecordRun(ctx context.Context, name string, at time.Time) error
}

// EmployerResolver maps an employer name from a job site to a known
// company. canonical is the name the company row should carry; priority
// reports whether the company is on the priority list. A nil resolver
// treats every employer as unknown.
type EmployerResolver interface {
	Resolve(employer string) (canonical string, priority bool)
}

// TargetRegistrar finds or creates the company and target a collector's
// jobs for one employer belong to. *company.Registrar satisfies it.
type TargetRegistrar interface {
	EnsureTarget(ctx context.Context, provider, employer string, priority bool, mk market.Market) (company.TargetCompany, error)
	// FindTarget returns the existing target without creating one.
	FindTarget(ctx context.Context, provider, employer string) (company.TargetCompany, bool, error)
}

// CollectorConfig wires collectors into an Ingester.
type CollectorConfig struct {
	// Collectors maps a collector's name, which is also the ats_provider
	// its own targets carry, to the collector.
	Collectors map[string]Collector
	Registrar  TargetRegistrar
	// Resolver maps employer names from Ethiopian sources to priority
	// companies; WorldwideResolver does the same for worldwide sources and
	// should know only the companies that hire outside Ethiopia. A name that
	// matches an Ethiopian-only priority company on a worldwide board is some
	// other company that happens to share the name. Nil means no resolution.
	Resolver          EmployerResolver
	WorldwideResolver EmployerResolver
	// RunLog, when set, enforces IntervalCollector limits across processes.
	RunLog RunLog
	// Targets lists active targets without any provider filter. After a
	// run it is used to age out the jobs of employers that did not appear
	// in the run at all.
	Targets TargetLister
}

// WithCollectors registers collectors and returns the Ingester.
func (in *Ingester) WithCollectors(cfg CollectorConfig) *Ingester {
	in.collectors = cfg
	return in
}

// WithForce makes RunCollectors ignore IntervalCollector limits. Use it only
// when you know the source can take the extra request.
func (in *Ingester) WithForce(force bool) *Ingester {
	in.forceCollectors = force
	return in
}

// CollectorNames returns the names of the registered collectors.
func (in *Ingester) CollectorNames() []string {
	names := make([]string, 0, len(in.collectors.Collectors))
	for n := range in.collectors.Collectors {
		names = append(names, n)
	}
	return names
}

// RunCollectors runs the named collectors in the order given, one after
// another (the order matters: an opening a later collector also found is
// skipped, so name the richer source first). It returns one Result per
// employer target touched, plus one failed Result for a collector that
// itself failed; a failing collector or employer never stops the others.
//
// Jobs are grouped by employer. Each employer gets its own company and
// target, because a job's company must be the company of the target that
// stores it. The same opening (same employer, title and place) found by an
// earlier collector in this run is not stored again.
func (in *Ingester) RunCollectors(ctx context.Context, names []string) ([]Result, error) {
	cfg := in.collectors
	for _, n := range names {
		if _, ok := cfg.Collectors[n]; !ok {
			return nil, fmt.Errorf("ingestion: no collector registered as %q", n)
		}
	}
	if cfg.Registrar == nil {
		return nil, errors.New("ingestion: collectors need a TargetRegistrar")
	}

	var results []Result
	// employer key + "|" + title key -> locations of the openings stored so
	// far this run.
	stored := map[string][]string{}

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		col := cfg.Collectors[name]
		res := in.runCollector(ctx, name, col, stored)
		results = append(results, res...)
	}
	return results, nil
}

func (in *Ingester) runCollector(ctx context.Context, name string, col Collector, stored map[string][]string) []Result {
	cfg := in.collectors
	failed := func(err error) []Result {
		return []Result{{Target: company.TargetCompany{ATSProvider: name, ExternalBoardID: "(collector)"},
			Err: fmt.Errorf("ingestion: collector %s: %w", name, err)}}
	}

	skip, err := in.tooSoon(ctx, name, col)
	if err != nil {
		return failed(err)
	}
	if skip {
		return nil
	}

	jobs, err := col.Collect(ctx)
	var partial []Result
	if err != nil {
		if !errors.Is(err, ats.ErrPartialResult) || len(jobs) == 0 {
			return failed(err)
		}
		// The collector read part of its source and then failed: store what it
		// has, and still report the failure.
		partial = failed(err)
	}
	staleAfter := col.StaleAfter()
	if staleAfter <= 0 {
		// A sample must never be treated as a full board: that would close
		// every job the run did not happen to return.
		return failed(errors.New("StaleAfter must be positive"))
	}
	priorityProvider := ""
	if pr, ok := col.(PriorityRouter); ok {
		priorityProvider = pr.PriorityProvider()
	}
	// The collector's own market ("" when it does not say: targets keep
	// whatever market they have, new ones are worldwide).
	var colMarket market.Market
	if mp, ok := col.(market.Provider); ok {
		colMarket = mp.Market()
	}

	type group struct {
		employer string // canonical
		priority bool
		jobs     []ats.Job
	}
	var order []string
	groups := map[string]*group{}
	var noEmployer int
	for _, aj := range jobs {
		employer := strings.Join(strings.Fields(aj.Employer), " ")
		if employer == "" {
			noEmployer++
			continue
		}
		canonical, priority := employer, false
		resolver := cfg.Resolver
		if colMarket == market.Worldwide {
			resolver = cfg.WorldwideResolver
		}
		if resolver != nil {
			if c, p := resolver.Resolve(employer); c != "" {
				canonical, priority = c, p
			}
		}
		key := strings.ToLower(canonical)
		g, ok := groups[key]
		if !ok {
			g = &group{employer: canonical, priority: priority}
			groups[key] = g
			order = append(order, key)
		}
		g.jobs = append(g.jobs, aj)
	}
	if noEmployer > 0 {
		in.logger.Warn("ingestion: collector jobs without an employer name were skipped", "collector", name, "jobs", noEmployer)
	}

	var results []Result
	touched := map[int64]bool{}
	for _, key := range order {
		if err := ctx.Err(); err != nil {
			return append(results, Result{Target: company.TargetCompany{ATSProvider: name, ExternalBoardID: "(collector)"}, Err: err})
		}
		g := groups[key]
		// Openings are compared within one market: an Ethiopian and a worldwide
		// listing of the same title at the same employer are different jobs
		// in different lists.
		dedupeMarket := colMarket
		if dedupeMarket == "" {
			dedupeMarket = market.Worldwide
		}
		provider, mk := name, colMarket
		if g.priority && priorityProvider != "" {
			// The priority provider's targets are the Ethiopian priority list's.
			provider, mk = priorityProvider, market.Ethiopia
			dedupeMarket = market.Ethiopia
		}

		// Drop openings an earlier collector already stored this run.
		fresh := make([]ats.Job, 0, len(g.jobs))
		for _, aj := range g.jobs {
			if aj.Closed {
				fresh = append(fresh, aj)
				continue
			}
			if openingStored(stored, dedupeMarket, key, aj) {
				continue
			}
			fresh = append(fresh, aj)
		}

		// An employer seen only through ended postings must not become a
		// company: find its target if it has one, and close what is stored.
		var target company.TargetCompany
		var err error
		if allClosed(fresh) {
			var found bool
			target, found, err = cfg.Registrar.FindTarget(ctx, provider, g.employer)
			if err == nil && !found {
				continue
			}
		} else {
			target, err = cfg.Registrar.EnsureTarget(ctx, provider, g.employer, g.priority, mk)
		}
		if err != nil {
			in.logger.Warn("ingestion: could not register an employer", "collector", name, "employer", g.employer, "error", err)
			results = append(results, Result{Target: company.TargetCompany{ATSProvider: provider, ExternalBoardID: g.employer},
				Err: fmt.Errorf("ingestion: registering %s/%s: %w", provider, g.employer, err)})
			continue
		}
		touched[target.ID] = true

		// Skip an opening another source already stores for this company in
		// this market, even if it was stored by an earlier invocation.
		if existing, oerr := in.jobs.OpenOpenings(ctx, target.CompanyID, string(dedupeMarket), provider); oerr != nil {
			in.logger.Warn("ingestion: could not check for duplicates of other sources; storing without the check",
				"collector", name, "employer", g.employer, "error", oerr)
		} else if len(existing) > 0 {
			fresh = dropOpenings(fresh, existing, dedupeMarket)
		}

		r := in.persist(ctx, target, fresh, staleAfter, true)
		results = append(results, r)
		if r.Err == nil {
			for _, aj := range fresh {
				if !aj.Closed && r.stored[aj.ExternalID] {
					k := openingKey(dedupeMarket, key, aj)
					stored[k] = append(stored[k], aj.LocationRaw)
				}
			}
		}
	}

	// Employers that did not appear in this run keep their open jobs until
	// those age out: a sample cannot say they are gone, only that they have
	// not been seen for staleAfter.
	if cfg.Targets != nil {
		results = append(results, in.sweepAbsent(ctx, name, staleAfter, touched)...)
	}
	return append(partial, results...)
}

// tooSoon reports whether the collector must be skipped because its source
// limits how often it may be called and it ran too recently. It records the
// run when it lets one through (an attempt counts: the request was made
// whether or not it succeeded).
func (in *Ingester) tooSoon(ctx context.Context, name string, col Collector) (skip bool, err error) {
	ic, ok := col.(IntervalCollector)
	rl := in.collectors.RunLog
	if !ok || rl == nil || ic.MinInterval() <= 0 {
		return false, nil
	}
	now := in.now()
	if !in.forceCollectors {
		last, seen, err := rl.LastRun(ctx, name)
		if err != nil {
			// Not knowing is not permission: this source asks to be called
			// rarely, so it is not called. It is also not a quiet skip: the run
			// log being unreadable (a missing migration, a database problem) is
			// a failure the operator must see.
			return false, fmt.Errorf("reading the run log to check the rate limit: %w", err)
		}
		// A scheduled run lands a little earlier or later than the last one (the
		// check happens some minutes into the process), so a gap of up to
		// intervalTolerance short of the minimum still counts as the interval.
		if seen && now.Sub(last) < ic.MinInterval()-intervalTolerance {
			in.logger.Info("ingestion: skipping a collector that ran recently (use --force to override)",
				"collector", name, "last_run", last, "min_interval", ic.MinInterval())
			return true, nil
		}
	}
	if err := rl.RecordRun(ctx, name, now); err != nil {
		return false, fmt.Errorf("recording the run for the rate limit: %w", err)
	}
	return false, nil
}

// intervalTolerance is how much earlier than MinInterval a scheduled run may
// arrive and still go ahead.
const intervalTolerance = 10 * time.Minute

// dropOpenings removes the jobs that duplicate an opening already stored by
// another source: the same title, and in the Ethiopian market the same place
// (see openingStored). Closure markers stay.
func dropOpenings(jobs []ats.Job, existing []job.Opening, mk market.Market) []ats.Job {
	out := jobs[:0:0]
	for _, aj := range jobs {
		dup := false
		if !aj.Closed {
			for _, o := range existing {
				if companymatch.TitleKey(o.Title) == companymatch.TitleKey(aj.Title) &&
					(mk == market.Worldwide || companymatch.SamePlace(o.Location, aj.LocationRaw)) {
					dup = true
					break
				}
			}
		}
		if !dup {
			out = append(out, aj)
		}
	}
	return out
}

// openingKey identifies an opening within a market: employer and title.
func openingKey(mk market.Market, employerKey string, aj ats.Job) string {
	return string(mk) + "|" + employerKey + "|" + companymatch.TitleKey(aj.Title)
}

// openingStored reports whether an opening at the same employer and with the
// same title was already written this run in the same market. In the
// Ethiopian market the place must match too (the same title in Addis Ababa and
// Hawassa is two jobs); in the worldwide market every listing is remote and
// each board words the region its own way ("USA", "United States"), so the
// place is ignored.
func openingStored(stored map[string][]string, mk market.Market, employerKey string, aj ats.Job) bool {
	for _, loc := range stored[openingKey(mk, employerKey, aj)] {
		if mk == market.Worldwide || companymatch.SamePlace(loc, aj.LocationRaw) {
			return true
		}
	}
	return false
}

func allClosed(jobs []ats.Job) bool {
	for _, j := range jobs {
		if !j.Closed {
			return false
		}
	}
	return true
}

// sweepAbsent closes the jobs of this collector's own targets that were not
// part of the run and have gone unseen for staleAfter.
func (in *Ingester) sweepAbsent(ctx context.Context, provider string, staleAfter time.Duration, touched map[int64]bool) []Result {
	all, err := in.collectors.Targets.ListActive(ctx)
	if err != nil {
		return []Result{{Target: company.TargetCompany{ATSProvider: provider, ExternalBoardID: "(sweep)"},
			Err: fmt.Errorf("ingestion: listing targets to age out %s jobs: %w", provider, err)}}
	}
	var out []Result
	for _, t := range all {
		if t.ATSProvider != provider || touched[t.ID] {
			continue
		}
		closed, err := in.jobs.CloseStale(ctx, t.ID, staleAfter)
		if err != nil {
			out = append(out, Result{Target: t, Err: fmt.Errorf("ingestion: aging out %s/%s: %w", t.ATSProvider, t.ExternalBoardID, err)})
			continue
		}
		if closed > 0 {
			out = append(out, Result{Target: t, Removed: closed})
		}
	}
	return out
}
