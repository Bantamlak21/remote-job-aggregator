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
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/feed"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/greenhouse"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/jobsearch"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ats/page"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ingestion"
	"github.com/Bantamlak12/remote-job-aggregator/internal/priority"
	"github.com/Bantamlak12/remote-job-aggregator/internal/robots"
	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
)

const defaultPriorityFile = "configs/ethiopian_companies.json"

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
// a client per ats_provider, the providers that run by default, and the
// search budget (nil when the search source is unavailable).
type ingestSources struct {
	clients          map[string]ingestion.ATSClient
	defaultProviders []string
	budget           *jobsearch.Budget
}

// newIngestSources builds one client per source, all sharing one pooled
// HTTP client and one robots.txt checker. The search-backed source needs
// two things the others do not: a Serper key and the priority company list
// (its names and aliases are how a search result is proven to belong to
// the company). Without either, that source is left out and its targets are
// skipped rather than reported as failures.
func newIngestSources(cfg *config.Config, httpClient *httpclient.Client, priorityFile string, logger *slog.Logger) ingestSources {
	robotsChecker := robots.New(httpClient, productToken(cfg.HTTP.UserAgent))
	pages := page.NewFetcher(httpClient, robotsChecker)

	s := ingestSources{clients: map[string]ingestion.ATSClient{
		string(ats.ProviderGreenhouse):  greenhouse.New(httpClient),
		string(ats.ProviderFeed):        feed.New(httpClient, robotsChecker, 0),
		string(ats.ProviderCareersSite): careers.New(pages, 0, 0, detailFetchPause),
	}}

	switch {
	case !cfg.Search.Configured():
		logger.Info("search-backed job source disabled: SERPER_API_KEY is not set")
	default:
		list, err := priority.Load(priorityFile)
		if err != nil {
			logger.Warn("search-backed job source disabled: cannot load the priority company list", "error", err)
			break
		}
		companies := make([]jobsearch.Company, len(list.Companies))
		for i, e := range list.Companies {
			companies[i] = jobsearch.Company{Name: e.Name, Aliases: e.Aliases, HiresOutsideEthiopia: e.HiresOutsideEthiopia}
		}
		s.budget = jobsearch.NewBudget(cfg.Search.MaxQueriesPerRun)
		searchClient := search.New(httpClient, search.Config{APIKey: cfg.Search.SerperAPIKey})
		s.clients[string(ats.ProviderSearch)] = jobsearch.New(searchClient, pages, companies, s.budget, logger)
	}

	// The search source spends a fixed, non-renewing Serper allowance (50
	// queries a run), so it never runs by default: a plain daily "ingest"
	// would exhaust the free 2,500 in about 50 days. It runs only when
	// asked for by name (--providers=search).
	for p := range s.clients {
		if p != string(ats.ProviderSearch) {
			s.defaultProviders = append(s.defaultProviders, p)
		}
	}
	slices.Sort(s.defaultProviders)
	return s
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
	return nil, errors.New("ingest: usage: ingest [--providers=greenhouse,feed,careers-site,search]")
}

// chooseProviders resolves the requested providers against the ones that
// have a client. Asking for one that has none (search without a key, a
// typo) is an error, not a silent no-op: a cron job that thinks it is
// ingesting a source it is not would go unnoticed for weeks.
func chooseProviders(requested []string, s ingestSources) ([]string, error) {
	if requested == nil {
		return s.defaultProviders, nil
	}
	var unavailable []string
	for _, p := range requested {
		if _, ok := s.clients[p]; !ok {
			unavailable = append(unavailable, p)
		}
	}
	if len(unavailable) > 0 {
		available := make([]string, 0, len(s.clients))
		for p := range s.clients {
			available = append(available, p)
		}
		slices.Sort(available)
		return nil, fmt.Errorf("ingest: no client for provider(s) %s (available: %s; search needs SERPER_API_KEY and %s)",
			strings.Join(unavailable, ", "), strings.Join(available, ", "), defaultPriorityFile)
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
