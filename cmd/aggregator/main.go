// Command aggregator is the composition root for the remote job
// aggregator. It wires configuration, logging, and the database pool
// together, and dispatches to the subcommand named on the command line.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/api"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/discovery"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ingestion"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/internal/observability"
	"github.com/Bantamlak12/remote-job-aggregator/internal/search"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

const usage = "usage: aggregator <run|migrate-up|migrate-down --yes [--all]|migrate-force <version>|discover [seed-file]|search-discover [company-names-file]|seed-priority [company-list-file]|priority-report|ingest [--providers=a,b]|serve>"

const (
	defaultSeedFile         = "configs/seed_companies.json"
	defaultCompanyNamesFile = "configs/company_names.txt"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		os.Exit(1)
	}
}

// bootstrapLogger is used only to report a config load failure, i.e.
// before we know the operator's chosen LOG_FORMAT/LOG_LEVEL. Every other
// error in this file is logged through the properly configured logger
// built right after config.Load succeeds.
func bootstrapLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// installSignalHandling returns a context canceled on the first
// SIGINT/SIGTERM, and forces an immediate os.Exit(1) on a second one.
//
// signal.NotifyContext alone does not give "second signal forces exit"
// behavior: it keeps re-delivering every signal as the same cancellation
// for as long as its stop func hasn't been called, and stop can't be
// called while the caller is itself blocked waiting on something that
// doesn't check ctx (e.g. golang-migrate's own Lock(), which runs its
// query with context.Background() regardless of the context passed to
// Up()/Down()/Steps() — verified live: holding an ACCESS EXCLUSIVE lock
// on schema_migrations and sending SIGTERM to a blocked migrate-down
// leaves it running indefinitely no matter how many SIGTERMs follow).
// This gives every command — including one stuck inside a library call
// we can't interrupt — a way out via a second signal, the same "ask
// nicely, then insist" pattern graceful shutdown implementations
// conventionally offer, without relying on OS default disposition (which
// signal.NotifyContext suppresses for as long as it's installed).
func installSignalHandling(parent context.Context) (ctx context.Context, stop func()) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-done:
			return
		}
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "received a second interrupt signal, forcing immediate exit")
			os.Exit(1)
		case <-done:
		}
	}()

	// Wrapped in sync.Once: this mimics signal.NotifyContext's stop
	// signature, and that stop is safe to call more than once, so callers
	// reasonably expect the same here. Without it, a second call would
	// panic on the already-closed done channel.
	var once sync.Once
	stop = func() {
		once.Do(func() {
			signal.Stop(sigCh)
			cancel()
			close(done)
		})
	}
	return ctx, stop
}

func run(ctx context.Context, args []string) error {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, usage)
		return errors.New(usage)
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run", "migrate-up", "migrate-down", "migrate-force", "discover", "search-discover", "seed-priority", "priority-report", "ingest", "serve":
	default:
		fmt.Fprintln(os.Stderr, usage)
		return fmt.Errorf("unknown command %q: %s", cmd, usage)
	}

	cfg, err := config.Load()
	if err != nil {
		bootstrapLogger().Error("failed to load configuration", "error", err)
		return err
	}

	logger := observability.NewLogger(cfg.Log, os.Stdout)
	slog.SetDefault(logger)

	// Every command gets a context that's canceled on the first
	// Ctrl-C/SIGTERM, with a second signal forcing immediate exit — see
	// installSignalHandling's comment for why that needs its own logic
	// rather than relying on signal.NotifyContext alone. migrate-up and
	// migrate-down additionally use that cancellation to stop gracefully
	// between migration files, via watchGracefulStop, instead of the
	// process dying mid-migration with schema_migrations left dirty.
	// migrate-force takes no context: it's a single fast metadata update
	// with nothing for graceful-stop to do.
	ctx, stop := installSignalHandling(ctx)
	defer stop()

	switch cmd {
	case "migrate-up":
		return runMigrateUp(ctx, cfg, logger)
	case "migrate-down":
		return runMigrateDown(ctx, cfg, logger, rest)
	case "migrate-force":
		return runMigrateForce(cfg, logger, rest)
	case "run":
		return runApp(ctx, cfg, logger)
	case "discover":
		return runDiscover(ctx, cfg, logger, rest)
	case "search-discover":
		return runSearchDiscover(ctx, cfg, logger, rest)
	case "seed-priority":
		return runSeedPriority(ctx, cfg, logger, rest)
	case "priority-report":
		return runPriorityReport(ctx, cfg, logger, rest)
	case "ingest":
		return runIngest(ctx, cfg, logger, rest)
	case "serve":
		return runServe(ctx, cfg, logger, rest)
	}
	return nil // unreachable: cmd was validated above
}

func runMigrateUp(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("applying migrations")
	if err := database.MigrateUp(ctx, cfg.Database.URL, migrations.FS); err != nil {
		logger.Error("applying migrations failed", "error", err)
		return err
	}
	logger.Info("migrations applied")
	return nil
}

// runMigrateDown always requires --yes: rolling back a migration is a
// DROP-TABLE-class operation (this repo currently has exactly one
// migration, so even the "single step" default drops every table), and
// CLAUDE.md's Safety rules require explicit confirmation for that, not a
// bare CLI verb. --all additionally rolls back every applied migration
// instead of just the most recent one.
func runMigrateDown(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	var all, yes bool
	for _, arg := range args {
		switch arg {
		case "--all":
			all = true
		case "--yes":
			yes = true
		default:
			err := fmt.Errorf("migrate-down: unknown flag %q (usage: migrate-down --yes [--all])", arg)
			logger.Error(err.Error())
			return err
		}
	}

	if !yes {
		err := errors.New("migrate-down requires --yes to confirm: rolls back one migration by default, or every migration with --all (usage: migrate-down --yes [--all])")
		logger.Error(err.Error())
		return err
	}

	if all {
		logger.Warn("rolling back ALL migrations, dropping every table")
		if err := database.MigrateDown(ctx, cfg.Database.URL, migrations.FS); err != nil {
			logger.Error("rolling back migrations failed", "error", err)
			return err
		}
		logger.Info("all migrations rolled back")
		return nil
	}

	logger.Info("rolling back one migration")
	if err := database.MigrateDownStep(ctx, cfg.Database.URL, migrations.FS); err != nil {
		logger.Error("rolling back migration failed", "error", err)
		return err
	}
	logger.Info("migration rolled back")
	return nil
}

// runMigrateForce clears a "dirty" schema_migrations state left behind by
// an interrupted migrate-up/migrate-down, without running any migration.
func runMigrateForce(cfg *config.Config, logger *slog.Logger, args []string) error {
	if len(args) != 1 {
		err := errors.New("migrate-force: usage: migrate-force <version>")
		logger.Error(err.Error())
		return err
	}
	version, err := strconv.Atoi(args[0])
	if err != nil {
		err := fmt.Errorf("migrate-force: version must be an integer, got %q", args[0])
		logger.Error(err.Error())
		return err
	}

	logger.Warn("forcing migration version, no migration will run", "version", version)
	if err := database.MigrateForce(cfg.Database.URL, migrations.FS, version); err != nil {
		logger.Error("forcing migration version failed", "error", err)
		return err
	}
	logger.Info("migration version forced", "version", version)
	return nil
}

// newHTTPClient builds the one pooled HTTP client a command run uses —
// shared across discovery's own HEAD/GET probing and (for
// search-discover) the Serper search calls that produce candidates for
// it, rather than each constructing its own. CLAUDE.md's rule: reuse
// configured clients, don't create one per caller.
func newHTTPClient(cfg *config.Config) *httpclient.Client {
	httpCfg := httpclient.DefaultConfig()
	httpCfg.Timeout = cfg.HTTP.Timeout
	httpCfg.MaxResponseBytes = cfg.HTTP.MaxResponseBytes
	httpCfg.UserAgent = cfg.HTTP.UserAgent
	return httpclient.New(httpCfg)
}

// newIngestionHTTPClient is newHTTPClient with the response-size cap and
// timeout replaced by ingestion's own (config.IngestionConfig): a real
// ATS board fetched with content=true is far larger than a discovery
// probe, and the shared 5 MiB default would fail big boards forever.
func newIngestionHTTPClient(cfg *config.Config) *httpclient.Client {
	httpCfg := httpclient.DefaultConfig()
	httpCfg.Timeout = cfg.Ingestion.Timeout
	httpCfg.MaxResponseBytes = cfg.Ingestion.MaxResponseBytes
	httpCfg.UserAgent = cfg.HTTP.UserAgent
	return httpclient.New(httpCfg)
}

func newDiscoverer(cfg *config.Config, db *database.DB, httpClient *httpclient.Client, logger *slog.Logger) *discovery.Discoverer {
	return discovery.New(
		company.NewStore(db.Pool),
		company.NewTargetStore(db.Pool),
		httpClient,
		cfg.Discovery.Workers,
		logger,
	)
}

// reportDiscoveryResults logs a per-candidate outcome and a summary, and
// decides the command's exit status: non-zero only when every candidate
// failed. A mix of successes and failures is discovery's normal steady
// state (a board can go away, or search can miss one company) and must
// not fail a cron-style invocation of either discover or
// search-discover.
func reportDiscoveryResults(logger *slog.Logger, results []discovery.Result) error {
	var succeeded, failed int
	for _, r := range results {
		if r.Err != nil {
			failed++
			logger.Warn("candidate not discovered", "company", r.Candidate.CompanyName, "board_url", r.Candidate.BoardURL, "error", r.Err)
			continue
		}
		succeeded++
	}
	logger.Info("discovery run complete", "total", len(results), "succeeded", succeeded, "failed", failed)

	if succeeded == 0 {
		return fmt.Errorf("all %d candidate(s) failed", len(results))
	}
	return nil
}

// runDiscover loads candidates from a seed file (configs/seed_companies.json
// by default) and runs discovery against them, persisting the companies
// and target boards that validate.
func runDiscover(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	if len(args) > 1 {
		err := errors.New("discover: usage: discover [seed-file]")
		logger.Error(err.Error())
		return err
	}
	seedPath := defaultSeedFile
	if len(args) == 1 {
		seedPath = args[0]
	}

	candidates, err := discovery.LoadSeedCandidates(seedPath)
	if err != nil {
		logger.Error("loading seed candidates failed", "error", err)
		return err
	}
	logger.Info("loaded seed candidates", "count", len(candidates), "seed_file", seedPath)
	if len(candidates) == 0 {
		logger.Info("no candidates to discover")
		return nil
	}

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		logger.Error("connecting to database failed", "error", err)
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	d := newDiscoverer(cfg, db, newHTTPClient(cfg), logger)
	results := d.Run(ctx, candidates)
	return reportDiscoveryResults(logger, results)
}

// runSearchDiscover reads a plain-text list of company names (default
// configs/company_names.txt), finds each one's ATS board via the Serper
// search API, and runs the same discovery pipeline runDiscover does —
// HEAD/GET validation, then persistence — against the resulting
// candidates. This is the "real search instead of hand-curated seed
// data" mechanism: a company name is enough, search finds the board.
func runSearchDiscover(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	if len(args) > 1 {
		err := errors.New("search-discover: usage: search-discover [company-names-file]")
		logger.Error(err.Error())
		return err
	}
	if !cfg.Search.Configured() {
		err := errors.New("search-discover requires SERPER_API_KEY to be set")
		logger.Error(err.Error())
		return err
	}
	logger.Info("starting search-discover", "search", cfg.Search)

	namesPath := defaultCompanyNamesFile
	if len(args) == 1 {
		namesPath = args[0]
	}

	names, err := discovery.LoadCompanyNames(namesPath)
	if err != nil {
		logger.Error("loading company names failed", "error", err)
		return err
	}
	logger.Info("loaded company names", "count", len(names), "names_file", namesPath)

	httpClient := newHTTPClient(cfg)
	searchClient := search.New(httpClient, search.Config{
		APIKey: cfg.Search.SerperAPIKey,
	})

	candidates, searchErrs := discovery.CandidatesFromSearch(ctx, searchClient, names)
	for _, searchErr := range searchErrs {
		logger.Warn("company search failed", "error", searchErr)
	}
	logger.Info("search complete",
		"companies_searched", len(names), "candidates_found", len(candidates), "search_failures", len(searchErrs))

	if len(candidates) == 0 {
		return fmt.Errorf("search-discover: no ATS board found for any of %d company name(s)", len(names))
	}

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		logger.Error("connecting to database failed", "error", err)
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	d := newDiscoverer(cfg, db, httpClient, logger)
	results := d.Run(ctx, candidates)
	return reportDiscoveryResults(logger, results)
}

// newJobRepository constructs the job.Repository serve hands to
// internal/api, per JOB_REPOSITORY: the real Postgres-backed job.Store
// (default, Phase 3) or job.MockRepository's fixture data (no database
// needed, for frontend development). The returned cleanup closes
// whatever this call opened (the database pool, for postgres; a no-op
// for mock). internal/api depends only on job.Repository, so this
// function is the only place that knows which implementation is in use.
func newJobRepository(ctx context.Context, cfg *config.Config, logger *slog.Logger) (job.Repository, func(), error) {
	if cfg.API.JobRepository == config.JobRepositoryMock {
		logger.Info("serving mock job data", "job_repository", cfg.API.JobRepository)
		return job.NewMockRepository(), func() {}, nil
	}

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to database: %w", err)
	}
	logger.Info("serving jobs from postgres", "job_repository", cfg.API.JobRepository, "job_max_age", cfg.API.JobMaxAge)
	return job.NewStore(db.Pool).WithMaxAge(cfg.API.JobMaxAge), db.Close, nil
}

// runServe starts the public, read-only job API (see docs/api.md).
func runServe(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	if len(args) != 0 {
		err := errors.New("serve: usage: serve")
		logger.Error(err.Error())
		return err
	}

	repo, closeRepo, err := newJobRepository(ctx, cfg, logger)
	if err != nil {
		logger.Error("constructing job repository failed", "error", err)
		return err
	}
	defer closeRepo()

	server := api.NewServer(repo, api.Config{
		Addr:              cfg.API.Addr,
		CORSAllowedOrigin: cfg.API.CORSAllowedOrigin,
	}, logger)

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	logger.Info("starting API server", "addr", cfg.API.Addr, "cors_allowed_origin", cfg.API.CORSAllowedOrigin)

	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("API server failed", "error", err)
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received, closing API server")
		// http.Server.Shutdown only force-closes connections it considers
		// idle immediately; a keep-alive connection whose client hasn't
		// fully drained the previous response body can look "active" to
		// the server for longer than expected, so a shutdown can take up
		// to the full timeout below in that case rather than returning
		// instantly — bounded, not a hang, but worth knowing if a deploy
		// ever seems to wait the full SHUTDOWN_TIMEOUT on this command.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("API server shutdown did not complete cleanly", "error", err)
			return err
		}
		logger.Info("shutdown complete")
		return nil
	}
}

// runIngest fetches jobs from every active target's ATS board and
// persists them (see internal/ingestion). Like discover, it exits
// non-zero only if every target failed — a mix of successes and
// failures is ingestion's normal steady state (a board can be down or
// gone at any time) and must not fail a cron-style invocation.
func runIngest(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	requested, err := parseIngestArgs(args)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	sources := newIngestSources(cfg, newIngestionHTTPClient(cfg), defaultPriorityFile, defaultLinkedInQueriesFile, logger)
	providers, err := chooseProviders(requested, sources)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		logger.Error("connecting to database failed", "error", err)
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	targetStore := company.NewTargetStore(db.Pool)
	atsProviders, collectorNames := splitProviders(providers, sources)
	in := ingestion.New(ingestion.OnlyProviders(targetStore, atsProviders...), targetStore, job.NewStore(db.Pool),
		sources.clients, cfg.Ingestion.Workers, logger)
	in.WithCollectors(ingestion.CollectorConfig{
		Collectors: sources.collectors,
		Registrar:  company.NewRegistrar(company.NewStore(db.Pool), targetStore),
		Resolver:   sources.resolver,
		Targets:    targetStore,
	})
	logger.Info("starting ingestion", "providers", providers)

	var results []ingestion.Result
	if len(atsProviders) > 0 {
		results, err = in.Run(ctx)
		if err != nil {
			logger.Error("ingestion failed", "error", err)
			return err
		}
	}
	if len(collectorNames) > 0 {
		collected, err := in.RunCollectors(ctx, collectorNames)
		results = append(results, collected...)
		if err != nil {
			logger.Error("collectors failed", "error", err)
			return err
		}
	}
	if sources.budget != nil {
		logger.Info("search queries spent", "used", sources.budget.Used(), "limit", sources.budget.Max())
	}
	reportErr := reportIngestionResults(logger, results)
	logPriorityCoverage(ctx, logger, db)
	return reportErr
}

func reportIngestionResults(logger *slog.Logger, results []ingestion.Result) error {
	var succeeded, failed, inserted, changed, unchanged, skipped, removed int
	for _, r := range results {
		if r.Err != nil {
			failed++
			logger.Warn("target not ingested",
				"target_id", r.Target.ID, "provider", r.Target.ATSProvider, "board", r.Target.ExternalBoardID, "error", r.Err)
			continue
		}
		succeeded++
		inserted += r.Inserted
		changed += r.Changed
		unchanged += r.Unchanged
		skipped += r.Skipped
		removed += r.Removed
		logger.Info("target ingested",
			"target_id", r.Target.ID, "provider", r.Target.ATSProvider, "board", r.Target.ExternalBoardID,
			"inserted", r.Inserted, "changed", r.Changed, "unchanged", r.Unchanged, "skipped", r.Skipped, "removed", r.Removed)
	}
	logger.Info("ingestion run complete",
		"targets", len(results), "succeeded", succeeded, "failed", failed,
		"jobs_inserted", inserted, "jobs_changed", changed, "jobs_unchanged", unchanged, "jobs_skipped", skipped, "jobs_removed", removed)

	if len(results) > 0 && succeeded == 0 {
		return fmt.Errorf("all %d target(s) failed", len(results))
	}
	return nil
}

// runApp starts the long-running process: connect to the database, then
// block until a shutdown signal arrives, then release resources within
// cfg.Shutdown.Timeout. There is no ingestion pipeline yet (that lands in
// a later phase) — this establishes the connect/shutdown lifecycle the
// rest of the application will run inside.
func runApp(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("starting aggregator", "app_env", cfg.AppEnv, "database", cfg.Database)

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		logger.Error("connecting to database failed", "error", err)
		return fmt.Errorf("connecting to database: %w", err)
	}
	logger.Info("connected to database")

	<-ctx.Done()
	logger.Info("shutdown signal received, closing resources")

	if err := closeWithTimeout(cfg.Shutdown.Timeout, db.Close); err != nil {
		logger.Error("shutdown did not complete cleanly", "error", err)
		return err
	}
	logger.Info("shutdown complete")
	return nil
}

// closeWithTimeout runs closeFn in the background and waits up to timeout
// for it to finish, returning an error if it doesn't. Extracted from
// runApp so the timeout-vs-clean-exit behavior is testable without a real
// database or OS signals.
func closeWithTimeout(timeout time.Duration, closeFn func()) error {
	done := make(chan struct{})
	go func() {
		closeFn()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("shutdown timed out after %s", timeout)
	}
}
