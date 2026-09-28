package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/account"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ingestion"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/internal/notification"
	"github.com/Bantamlak12/remote-job-aggregator/internal/runlog"
)

// schedulerLookbackFactor widens RecentlyOpened's "since" window past one
// Interval, so a tick that runs late or a scheduler restart never leaves a
// gap of un-checked jobs. Safe to overlap: NotifyNewJobs is idempotent
// (notified_jobs), so a job seen again in the widened window is just
// skipped the second time, never double-sent.
const schedulerLookbackFactor = 2

// newNotifier builds the sender for job notifications from the
// notification settings: Telegram when TELEGRAM_BOT_TOKEN is set, else a
// notifier that only logs -- mirrors newMailer's AUTH_MAILER=log fallback.
func newNotifier(cfg config.NotificationConfig, httpClient *httpclient.Client, logger *slog.Logger) notification.Notifier {
	if cfg.Configured() {
		return notification.Telegram{BotToken: cfg.TelegramBotToken, HTTPClient: httpClient}
	}
	logger.Warn("TELEGRAM_BOT_TOKEN is not set: job notifications are written to the log only")
	return notification.Log{Logger: logger}
}

// runSchedulerTick is the scheduler's per-interval unit of work: ingest
// every default provider, classify what was stored, then notify accounts
// whose keywords match anything newly open. Unlike runIngest (its CLI
// counterpart), it takes no flags -- a scheduler tick always runs every
// default provider with no --force, matching Phase 6's "periodic,
// unattended execution" (PROJECT_STATUS.md).
func runSchedulerTick(ctx context.Context, cfg *config.Config, logger *slog.Logger, db *database.DB, jobStore *job.Store, acctSvc *account.Service, notifier notification.Notifier) error {
	sources := newIngestSources(cfg, newIngestionHTTPClient(cfg), defaultPriorityFile, defaultLinkedInQueriesFile, logger)
	providers, err := chooseProviders(nil, sources)
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}

	targetStore := company.NewTargetStore(db.Pool)
	atsProviders, collectorNames := splitProviders(providers, sources)
	in := ingestion.New(ingestion.OnlyProviders(targetStore, atsProviders...), targetStore, job.NewStore(db.Pool),
		sources.clients, cfg.Ingestion.Workers, logger)
	in.WithCollectors(ingestion.CollectorConfig{
		Collectors:        sources.collectors,
		Registrar:         company.NewRegistrar(company.NewStore(db.Pool), targetStore),
		Resolver:          sources.resolver,
		WorldwideResolver: sources.worldwideResolver,
		RunLog:            runlog.NewStore(db.Pool),
		Targets:           targetStore,
	})
	logger.Info("scheduler: starting ingestion", "providers", providers)

	var results []ingestion.Result
	if len(atsProviders) > 0 {
		if results, err = in.Run(ctx); err != nil {
			// Logged, not returned: a target board being down is ingestion's
			// normal steady state, and one bad tick must not stop the next one
			// any more than one bad cron-triggered `aggregator ingest` should.
			logger.Warn("scheduler: ingestion had failures", "error", err)
		}
	}
	if len(collectorNames) > 0 {
		collected, cerr := in.RunCollectors(ctx, collectorNames)
		results = append(results, collected...)
		if cerr != nil {
			logger.Warn("scheduler: collectors had failures", "error", cerr)
		}
	}
	if sources.budget != nil {
		logger.Info("scheduler: search queries spent", "used", sources.budget.Used(), "limit", sources.budget.Max())
	}
	if err := reportIngestionResults(logger, results); err != nil {
		logger.Warn("scheduler: every target failed this tick", "error", err)
	}

	st, err := classifyJobs(ctx, cfg, logger, db, false)
	if err != nil {
		logger.Warn("scheduler: classifying failed; the next tick retries", "error", err)
	} else if st.Classified > 0 {
		logger.Info("scheduler: classified the ingested jobs", "classified", st.Classified, "eligible", st.Eligible, "ineligible", st.Ineligible, "uncertain", st.Uncertain)
	}

	if acctSvc == nil {
		return nil // sign-in is off, so there is no one to notify
	}
	since := time.Now().Add(-schedulerLookbackFactor * cfg.Scheduler.Interval)
	candidates, err := jobStore.RecentlyOpened(ctx, since)
	if err != nil {
		return fmt.Errorf("scheduler: listing recently opened jobs: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}
	notifyCandidates := make([]notification.Candidate, len(candidates))
	for i, c := range candidates {
		notifyCandidates[i] = notification.Candidate{JobID: c.JobID, Title: c.Title}
	}
	if err := acctSvc.NotifyNewJobs(ctx, notifyCandidates, notifier); err != nil {
		return fmt.Errorf("scheduler: notifying subscribers: %w", err)
	}
	logger.Info("scheduler: notification check complete", "candidates", len(notifyCandidates))
	return nil
}
