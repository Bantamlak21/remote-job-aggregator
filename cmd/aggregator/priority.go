package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/careers"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/ethiojobs"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/feed"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/greenhouse"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/jobsearch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/remoteboards"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/companymatch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ingestion"
	"github.com/Bantamlak12/remote-job-aggregator/internal/priority"
	"github.com/Bantamlak12/remote-job-aggregator/internal/robots"
	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
)

const (
	defaultPriorityFile        = "configs/ethiopian_companies.json"
	defaultLinkedInQueriesFile = "configs/linkedin_queries.json"
)

// The many-employer collectors, in the order they run: Ethiojobs first, so
// an opening both sites list is stored with Ethiojobs' full description
// rather than LinkedIn's search snippet; then the remote job boards, the ones
// with the fullest text first, so an opening several boards list is stored
// from the richest. Each name is also the ats_provider of the targets it
// creates.
const (
	collectorEthiojobs = "ethiojobs"
	collectorLinkedIn  = "linkedin"

	boardHimalayas     = "himalayas"
	boardRemotive      = "remotive"
	boardJobicy        = "jobicy"
	boardWWR           = "weworkremotely"
	boardWorkingNomads = "workingnomads"
	boardRemoteOK      = "remoteok"

	// remoteBoardsGroup names every remote job board in --providers.
	remoteBoardsGroup = "remote-boards"
)

var remoteBoards = []string{boardHimalayas, boardRemotive, boardJobicy, boardWWR, boardWorkingNomads, boardRemoteOK}

var collectorOrder = append([]string{collectorEthiojobs, collectorLinkedIn}, remoteBoards...)

// listingFetchPause is the delay between successive listing-page fetches of
// a job board.
const listingFetchPause = 500 * time.Millisecond

// detailFetchPause is the delay between successive job-page fetches on one
// company's careers site: small company sites should not see a burst.
const detailFetchPause = 500 * time.Millisecond

// productToken reduces a User-Agent ("remote-job-aggregator/1.0 (+https://...)")
// to the product token robots.txt groups are matched against
// ("remote-job-aggregator").
func productToken(userAgent string) string {
	token, _, _ := strings.Cut(strings.TrimSpace(userAgent), " ")
	token, _, _ = strings.Cut(token, "/")
	return token
}

// ingestSources is everything runIngest needs to fetch from every source:
// a client per ats_provider (one company's board each), a collector per
// many-employer source (see ingestion.Collector), the providers that run by
// default, and the search budget (nil when no search-backed source is
// available).
type ingestSources struct {
	clients          map[string]ingestion.ATSClient
	collectors       map[string]ingestion.Collector
	defaultProviders []string
	budget           *jobsearch.Budget
	// resolver maps an employer name from a job site to a priority company;
	// nil when the priority list could not be loaded.
	resolver ingestion.EmployerResolver
}

// newIngestSources builds one client per source, all sharing one pooled
// HTTP client and one robots.txt checker. The search-backed sources need
// a Serper key, and the per-company one also needs the priority company
// list (its names and aliases are how a search result is proven to belong to
// the company). Without them, those sources are left out and their targets
// are skipped rather than reported as failures.
func newIngestSources(cfg *config.Config, httpClient *httpclient.Client, priorityFile, queriesFile string, logger *slog.Logger) ingestSources {
	robotsChecker := robots.New(httpClient, productToken(cfg.HTTP.UserAgent))
	pages := page.NewFetcher(httpClient, robotsChecker)

	s := ingestSources{
		clients: map[string]ingestion.ATSClient{
			string(ats.ProviderGreenhouse):  greenhouse.New(httpClient),
			string(ats.ProviderFeed):        feed.New(httpClient, robotsChecker, 0),
			string(ats.ProviderCareersSite): careers.New(pages, 0, 0, detailFetchPause),
		},
		collectors: map[string]ingestion.Collector{
			collectorEthiojobs: ethiojobs.New(pages, cfg.Ingestion.EthiojobsMaxPages, 0, listingFetchPause, logger),
			boardHimalayas:     remoteboards.NewHimalayas(httpClient, cfg.Ingestion.HimalayasMaxPages, listingFetchPause, logger),
			boardRemotive:      remoteboards.NewRemotive(httpClient, logger),
			boardJobicy:        remoteboards.NewJobicy(httpClient, logger),
			boardWWR:           remoteboards.NewWeWorkRemotely(httpClient, logger),
			boardWorkingNomads: remoteboards.NewWorkingNomads(httpClient, logger),
			boardRemoteOK:      remoteboards.NewRemoteOK(httpClient, logger),
		},
	}

	list, listErr := priority.Load(priorityFile)
	if listErr != nil {
		logger.Warn("priority company list unavailable: priority companies are not recognized in collected jobs and the per-company search source is disabled", "error", listErr)
	} else {
		entries := make([]companymatch.Entry, len(list.Companies))
		for i, e := range list.Companies {
			entries[i] = companymatch.Entry{Name: e.Name, Aliases: e.Aliases}
		}
		s.resolver = companymatch.NewMatcher(entries)
	}

	if !cfg.Search.Configured() {
		logger.Info("search-backed job sources disabled: SERPER_API_KEY is not set")
	} else {
		s.budget = jobsearch.NewBudget(cfg.Search.MaxQueriesPerRun)
		searchClient := search.New(httpClient, search.Config{APIKey: cfg.Search.SerperAPIKey})

		if listErr == nil {
			companies := make([]jobsearch.Company, len(list.Companies))
			for i, e := range list.Companies {
				companies[i] = jobsearch.Company{Name: e.Name, Aliases: e.Aliases, HiresOutsideEthiopia: e.HiresOutsideEthiopia}
			}
			s.clients[string(ats.ProviderSearch)] = jobsearch.New(searchClient, companies, s.budget, logger)
		}

		fresh, err := jobsearch.LoadFreshConfig(queriesFile)
		if err != nil {
			logger.Warn("LinkedIn keyword source disabled: cannot load its keyword list", "error", err)
		} else {
			if fresh.Queries() > s.budget.Max() {
				logger.Warn("the LinkedIn keyword list needs more queries than SEARCH_MAX_QUERIES_PER_RUN allows; the last keywords will not run",
					"queries_needed", fresh.Queries(), "limit", s.budget.Max())
			}
			priorityProvider := ""
			if listErr == nil {
				priorityProvider = string(ats.ProviderSearch)
			}
			s.collectors[collectorLinkedIn] = jobsearch.NewFresh(searchClient, fresh, s.budget, priorityProvider, logger)
		}
	}

	// The Serper-backed sources spend a fixed, non-renewing allowance, so they
	// never run by default: a plain daily "ingest" would exhaust the free
	// 2,500 queries in weeks. They run only when asked for by name
	// (--providers=search, --providers=linkedin). Ethiojobs is free and runs
	// by default.
	for p := range s.clients {
		if p != string(ats.ProviderSearch) {
			s.defaultProviders = append(s.defaultProviders, p)
		}
	}
	// Ethiojobs and the remote job boards are free and run by default. The
	// boards ask for few requests (Remotive: at most 4 a day; Himalayas
	// refreshes daily), so ingest is meant to run about daily.
	s.defaultProviders = append(s.defaultProviders, collectorEthiojobs)
	s.defaultProviders = append(s.defaultProviders, remoteBoards...)
	slices.Sort(s.defaultProviders)
	return s
}

// expandGroups replaces the group name "remote-boards" with the individual
// boards (in place, without duplicates), so --providers=remote-boards runs
// all of them.
func expandGroups(requested []string) []string {
	if !slices.Contains(requested, remoteBoardsGroup) {
		return requested
	}
	var out []string
	add := func(p string) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, p := range requested {
		if p == remoteBoardsGroup {
			for _, b := range remoteBoards {
				add(b)
			}
			continue
		}
		add(p)
	}
	return out
}

// hasProvider reports whether name is an ATS provider or a collector.
func (s ingestSources) hasProvider(name string) bool {
	_, isClient := s.clients[name]
	_, isCollector := s.collectors[name]
	return isClient || isCollector
}

// splitProviders separates chosen provider names into ATS providers (run
// per target by Ingester.Run) and collector names (run by
// Ingester.RunCollectors, in collectorOrder).
func splitProviders(chosen []string, s ingestSources) (atsProviders, collectorNames []string) {
	for _, p := range chosen {
		if _, ok := s.collectors[p]; !ok {
			atsProviders = append(atsProviders, p)
		}
	}
	for _, c := range collectorOrder {
		if slices.Contains(chosen, c) {
			collectorNames = append(collectorNames, c)
		}
	}
	return atsProviders, collectorNames
}

// parseIngestArgs reads ingest's only option, --providers=a,b,c. nil
// means "every available provider".
func parseIngestArgs(args []string) ([]string, error) {
	switch len(args) {
	case 0:
		return nil, nil
	case 1:
		list, ok := strings.CutPrefix(args[0], "--providers=")
		if !ok {
			break
		}
		var out []string
		for p := range strings.SplitSeq(list, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			return nil, errors.New("ingest: --providers needs at least one provider name")
		}
		return out, nil
	}
	return nil, errors.New("ingest: usage: ingest [--providers=greenhouse,feed,careers-site,ethiojobs,remote-boards,search,linkedin]")
}

// chooseProviders resolves the requested providers against the ones that
// have a client. Asking for one that has none (search without a key, a
// typo) is an error, not a silent no-op: a cron job that thinks it is
// ingesting a source it is not would go unnoticed for weeks.
func chooseProviders(requested []string, s ingestSources) ([]string, error) {
	if requested == nil {
		return s.defaultProviders, nil
	}
	requested = expandGroups(requested)
	var unavailable []string
	for _, p := range requested {
		if !s.hasProvider(p) {
			unavailable = append(unavailable, p)
		}
	}
	if len(unavailable) > 0 {
		var available []string
		for p := range s.clients {
			available = append(available, p)
		}
		for p := range s.collectors {
			available = append(available, p)
		}
		slices.Sort(available)
		return nil, fmt.Errorf("ingest: no client for provider(s) %s (available: %s; search needs SERPER_API_KEY and %s, linkedin needs SERPER_API_KEY and %s)",
			strings.Join(unavailable, ", "), strings.Join(available, ", "), defaultPriorityFile, defaultLinkedInQueriesFile)
	}
	return requested, nil
}

// runSeedPriority registers the priority companies and their sources.
func runSeedPriority(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	if len(args) > 1 {
		err := errors.New("seed-priority: usage: seed-priority [company-list-file]")
		logger.Error(err.Error())
		return err
	}
	path := defaultPriorityFile
	if len(args) == 1 {
		path = args[0]
	}

	list, err := priority.Load(path)
	if err != nil {
		logger.Error("loading the priority company list failed", "error", err)
		return err
	}

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		logger.Error("connecting to database failed", "error", err)
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	res, err := priority.Seed(ctx, list, company.NewStore(db.Pool), company.NewTargetStore(db.Pool))
	logger.Info("priority companies seeded", "list_file", path, "companies_listed", len(list.Companies),
		"companies_registered", res.Companies, "targets_registered", res.Targets)
	if err != nil {
		logger.Error("some priority companies could not be fully seeded", "error", err)
		return err
	}
	return nil
}

// runPriorityReport prints how well the sources cover the priority
// companies. Read-only.
func runPriorityReport(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	if len(args) != 0 {
		err := errors.New("priority-report: usage: priority-report")
		logger.Error(err.Error())
		return err
	}
	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		logger.Error("connecting to database failed", "error", err)
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	cov, err := priority.NewStore(db.Pool).Coverage(ctx)
	if err != nil {
		logger.Error("building the coverage report failed", "error", err)
		return err
	}
	// Account for configured companies that are not in the database, so the
	// headline denominator is the list, not whatever happened to be seeded.
	if list, err := priority.Load(defaultPriorityFile); err != nil {
		logger.Warn("company list not loaded; the report covers only companies already in the database", "error", err)
	} else {
		names := make([]string, len(list.Companies))
		for i, e := range list.Companies {
			names[i] = e.Name
		}
		cov.NoteMissing(names)
	}
	return cov.Write(os.Stdout)
}

// logPriorityCoverage logs the run's headline outcome: how many priority
// companies have an open job. Best effort; a failure is logged, not fatal,
// because the ingestion it follows already succeeded.
func logPriorityCoverage(ctx context.Context, logger *slog.Logger, db *database.DB) {
	cov, err := priority.NewStore(db.Pool).Coverage(ctx)
	if err != nil {
		logger.Warn("priority coverage unavailable", "error", err)
		return
	}
	if len(cov.Companies) == 0 {
		return
	}
	logger.Info("priority coverage",
		"companies_with_open_jobs", cov.WithJobs(), "priority_companies", len(cov.Companies), "open_priority_jobs", cov.TotalOpen())
}
