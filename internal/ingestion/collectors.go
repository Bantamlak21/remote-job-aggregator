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
	EnsureTarget(ctx context.Context, provider, employer string, priority bool) (company.TargetCompany, error)
	// FindTarget returns the existing target without creating one.
	FindTarget(ctx context.Context, provider, employer string) (company.TargetCompany, bool, error)
}

// CollectorConfig wires collectors into an Ingester.
type CollectorConfig struct {
	// Collectors maps a collector's name, which is also the ats_provider
	// its own targets carry, to the collector.
	Collectors map[string]Collector
	Registrar  TargetRegistrar
	Resolver   EmployerResolver
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

	jobs, err := col.Collect(ctx)
	if err != nil {
		return failed(err)
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
		if cfg.Resolver != nil {
			if c, p := cfg.Resolver.Resolve(employer); c != "" {
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
		provider := name
		if g.priority && priorityProvider != "" {
			provider = priorityProvider
		}

		// Drop openings an earlier collector already stored this run.
		fresh := make([]ats.Job, 0, len(g.jobs))
		for _, aj := range g.jobs {
			if aj.Closed {
				fresh = append(fresh, aj)
				continue
			}
			if openingStored(stored, key, aj) {
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
			target, err = cfg.Registrar.EnsureTarget(ctx, provider, g.employer, g.priority)
		}
		if err != nil {
			in.logger.Warn("ingestion: could not register an employer", "collector", name, "employer", g.employer, "error", err)
			results = append(results, Result{Target: company.TargetCompany{ATSProvider: provider, ExternalBoardID: g.employer},
				Err: fmt.Errorf("ingestion: registering %s/%s: %w", provider, g.employer, err)})
			continue
		}
		touched[target.ID] = true

		r := in.persist(ctx, target, fresh, staleAfter)
		results = append(results, r)
		if r.Err == nil {
			for _, aj := range fresh {
				if !aj.Closed && r.stored[aj.ExternalID] {
					k := key + "|" + companymatch.TitleKey(aj.Title)
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
	return results
}

// openingStored reports whether an opening at the same employer, with the
// same title and place, was already written this run.
func openingStored(stored map[string][]string, employerKey string, aj ats.Job) bool {
	for _, loc := range stored[employerKey+"|"+companymatch.TitleKey(aj.Title)] {
		if companymatch.SamePlace(loc, aj.LocationRaw) {
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
