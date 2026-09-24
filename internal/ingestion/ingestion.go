// Package ingestion fetches jobs from every active target's ATS board
// and persists them via internal/job, closing out jobs that disappeared
// from the board since the last run. It knows nothing about any
// specific provider's response format — that lives in internal/ats and
// its per-provider subpackages — only that something satisfying
// ATSClient can list a board's jobs given a token.
package ingestion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

// TargetLister is the dependency Ingester needs from internal/company
// to find which boards to fetch. Defined here, on the consumer side,
// matching internal/discovery's CompanyUpserter/TargetUpserter
// convention — *company.TargetStore satisfies this structurally.
type TargetLister interface {
	ListActive(ctx context.Context) ([]company.TargetCompany, error)
}

// OnlyProviders wraps a TargetLister so it lists only targets whose
// ats_provider is one of providers. It lets one "ingest" invocation cover
// a subset of sources (say, only the cheap ones daily and the
// search-backed one weekly) without the Ingester knowing anything about
// which sources exist.
func OnlyProviders(inner TargetLister, providers ...string) TargetLister {
	allow := make(map[string]bool, len(providers))
	for _, p := range providers {
		allow[p] = true
	}
	return providerFilter{inner: inner, allow: allow}
}

type providerFilter struct {
	inner TargetLister
	allow map[string]bool
}

func (f providerFilter) ListActive(ctx context.Context) ([]company.TargetCompany, error) {
	all, err := f.inner.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	kept := make([]company.TargetCompany, 0, len(all))
	for _, t := range all {
		if f.allow[t.ATSProvider] {
			kept = append(kept, t)
		}
	}
	return kept, nil
}

// TargetRecorder is what Ingester needs from internal/company to record
// per-target ingestion outcomes: a successful fetch's timestamp, and a
// board confirmed gone.
type TargetRecorder interface {
	MarkIngestionSucceeded(ctx context.Context, id int64) error
	SetActive(ctx context.Context, id int64, active bool) error
}

// ATSClient fetches one board's jobs. Every ats_provider a target might
// name must have a registered client (see New's clients parameter); a
// target whose provider has none is reported as a per-target error,
// never silently skipped.
type ATSClient interface {
	ListJobs(ctx context.Context, boardToken string) ([]ats.Job, error)
}

// PartialClient is an optional interface an ATSClient implements when its
// listing is a sample of the company's jobs rather than all of them (web
// search shows some of a company's postings, never a complete board).
// For such a client, "not in this run's results" proves nothing, so the
// ingester does not close jobs missing from the run; it closes only those
// no run has seen for StaleAfter. A full-board client (Greenhouse, an RSS
// feed, a careers page) does not implement it and keeps the exact
// "missing from the board means gone" rule.
type PartialClient interface {
	StaleAfter() time.Duration
}

// JobUpserter is the dependency Ingester needs from internal/job to
// persist what an ATSClient returns.
type JobUpserter interface {
	UpsertFromATS(ctx context.Context, r job.Record) (job.UpsertOutcome, error)
	MarkMissingAsRemoved(ctx context.Context, targetCompanyID int64, seenSourceJobIDs []string) (int, error)
	CloseStale(ctx context.Context, targetCompanyID int64, olderThan time.Duration) (int, error)
	CloseBySourceID(ctx context.Context, targetCompanyID int64, sourceJobIDs []string) (int, error)
}

// Result is the outcome of ingesting one target. Err is nil if and only
// if the fetch succeeded and no infrastructure error interrupted
// persistence; individual jobs the database refused (bad data) are
// counted in Skipped, not treated as a target failure. Counts are
// only meaningful when Err is nil.
type Result struct {
	Target    company.TargetCompany
	Inserted  int
	Changed   int
	Unchanged int
	Skipped   int
	Removed   int
	Err       error
}

// Ingester fetches and persists jobs for every active target, using a
// bounded pool of workers. Each worker owns one target at a time to
// completion — no two goroutines ever process the same target
// concurrently — which is what makes JobUpserter's upsert-outcome
// bookkeeping safe without any locking in this package: a race on one
// target's own jobs is structurally impossible, and two different
// targets never share a (source, source_job_id) identity since a real
// ATS's ids are provider-global (verified for Greenhouse; see
// internal/job.ErrJobTargetMismatch for the defensive guard in case a
// future provider ever violates this).
type Ingester struct {
	targets        TargetLister
	targetRecorder TargetRecorder
	jobs           JobUpserter
	clients        map[string]ATSClient
	workers        int
	logger         *slog.Logger
	now            func() time.Time
}

// maxFuturePublishedAt is how far ahead of now a publish date may be
// (clock skew and time zones) before it is treated as a source error.
const maxFuturePublishedAt = 24 * time.Hour

// maxTitleRunes bounds a stored job title.
const maxTitleRunes = 300

// New returns an Ingester. clients maps an ats_provider value (matching
// target_companies.ats_provider, e.g. "greenhouse") to the client that
// knows how to fetch that provider's boards. workers <= 0 is clamped to
// 1, matching internal/discovery.New's reasoning: a zero-worker pool's
// feeder goroutine would leak forever on any non-empty target list.
func New(targets TargetLister, targetRecorder TargetRecorder, jobs JobUpserter, clients map[string]ATSClient, workers int, logger *slog.Logger) *Ingester {
	if workers < 1 {
		workers = 1
	}
	return &Ingester{
		targets:        targets,
		targetRecorder: targetRecorder,
		jobs:           jobs,
		clients:        clients,
		workers:        workers,
		logger:         logger,
		now:            time.Now,
	}
}

// Run fetches and persists jobs for every currently-active target,
// using a bounded pool of in.workers goroutines, and returns one Result
// per target in unspecified order — the same channel-fed-pool tradeoff
// internal/discovery.Run makes, for the same reason (this project has
// no use yet for preserving input order).
//
// If ctx is canceled before every target has been dispatched, Run
// returns early with fewer results than the target list's length — see
// internal/discovery.Run's identical documented behavior.
func (in *Ingester) Run(ctx context.Context) ([]Result, error) {
	targets, err := in.targets.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingestion: listing active targets: %w", err)
	}

	work := make(chan company.TargetCompany)
	results := make(chan Result)

	var wg sync.WaitGroup
	for i := 0; i < in.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				results <- in.processTarget(ctx, t)
			}
		}()
	}

	go func() {
		defer close(work)
		for _, t := range targets {
			select {
			case work <- t:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]Result, 0, len(targets))
	for r := range results {
		out = append(out, r)
	}
	return out, nil
}

func (in *Ingester) processTarget(ctx context.Context, t company.TargetCompany) Result {
	client, ok := in.clients[t.ATSProvider]
	if !ok {
		return Result{Target: t, Err: fmt.Errorf("ingestion: no ATS client registered for provider %q", t.ATSProvider)}
	}

	atsJobs, err := client.ListJobs(ctx, t.ExternalBoardID)
	if err != nil {
		// A definitive "board is gone" signal is worth acting on: the
		// target is deactivated (no future run will keep re-fetching a
		// board that will never come back) and its open jobs are closed
		// (their links are dead — leaving them open would show job-seekers
		// listings that no longer exist, and no later run would ever close
		// them, since the target is no longer fetched). Both are
		// recoverable: discovery re-finding the board reactivates the
		// target (TargetStore.Upsert forces is_active back on), and the
		// next ingestion reopens any job that reappears. Any other error
		// (network, 5xx, timeout) is left alone: it might be transient, and
		// acting on a guess would hide or stop ingesting a healthy target.
		if errors.Is(err, ats.ErrBoardNotFound) {
			in.retireGoneBoard(ctx, t)
		}
		return Result{Target: t, Err: fmt.Errorf("ingestion: fetching %s/%s: %w", t.ATSProvider, t.ExternalBoardID, err)}
	}

	seenIDs := make([]string, 0, len(atsJobs))
	var endedIDs []string
	var inserted, changed, unchanged, skipped int
	latestPlausible := in.now().Add(maxFuturePublishedAt)
	for _, aj := range atsJobs {
		// A job the source itself reports as ended is not stored; any open
		// row for it is closed below. It is deliberately not "seen".
		if aj.Closed {
			if aj.ExternalID != "" {
				endedIDs = append(endedIDs, aj.ExternalID)
			}
			continue
		}
		// A publish date from the future is a source bug; trusted, it would
		// pin the job above every real one in its group. Falling back to the
		// first-seen time is the honest answer.
		if aj.PublishedAt.After(latestPlausible) {
			in.logger.Warn("ingestion: ignoring a publish date in the future",
				"target_id", t.ID, "external_id", aj.ExternalID, "published_at", aj.PublishedAt)
			aj.PublishedAt = time.Time{}
		}

		// Every job the board listed counts as "seen" even if it cannot
		// be stored below: a job that is on the board but has bad data
		// must not be closed out as if it had disappeared.
		seenIDs = append(seenIDs, aj.ExternalID)

		// An unbounded title (a whole page's <title>, a blob) is a source
		// bug; it would bloat every list response.
		if r := []rune(aj.Title); len(r) > maxTitleRunes {
			aj.Title = string(r[:maxTitleRunes])
		}
		if aj.ExternalID == "" || aj.Title == "" || aj.URL == "" {
			skipped++
			in.logger.Warn("ingestion: skipping a job missing id, title, or url",
				"target_id", t.ID, "board", t.ExternalBoardID, "external_id", aj.ExternalID)
			continue
		}

		outcome, err := in.jobs.UpsertFromATS(ctx, job.Record{
			CompanyID: t.CompanyID, TargetCompanyID: t.ID,
			Source: t.ATSProvider, SourceJobID: aj.ExternalID,
			// CanonicalURL is deliberately left empty: (source, source_job_id)
			// is the identity, and jobs.canonical_url is a *global* unique
			// index — setting it from the job URL let one reposted or
			// duplicated URL abort a whole board's ingestion forever.
			Title: aj.Title, Description: aj.Description,
			ApplicationURL: aj.URL, LocationRaw: aj.LocationRaw, PublishedAt: aj.PublishedAt,
		})
		if err != nil {
			// A job the database refused (bad data) is skipped and logged;
			// one bad job must never freeze its whole board's ingestion.
			// Anything else (connection loss, cancellation) aborts.
			if job.IsRejectedRecord(err) {
				skipped++
				in.logger.Warn("ingestion: skipping a job the database refused",
					"target_id", t.ID, "board", t.ExternalBoardID, "external_id", aj.ExternalID, "error", err)
				continue
			}
			return Result{Target: t, Inserted: inserted, Changed: changed, Unchanged: unchanged, Skipped: skipped,
				Err: fmt.Errorf("ingestion: upserting %s/%s job %s: %w", t.ATSProvider, t.ExternalBoardID, aj.ExternalID, err)}
		}
		switch {
		case outcome.Inserted:
			inserted++
		case outcome.Changed:
			changed++
		default:
			unchanged++
		}
	}

	var removed int
	if len(endedIDs) > 0 {
		closed, err := in.jobs.CloseBySourceID(ctx, t.ID, endedIDs)
		if err != nil {
			return Result{Target: t, Inserted: inserted, Changed: changed, Unchanged: unchanged, Skipped: skipped,
				Err: fmt.Errorf("ingestion: closing ended jobs for %s/%s: %w", t.ATSProvider, t.ExternalBoardID, err)}
		}
		removed += closed
	}
	var closedByRule int
	if pc, ok := client.(PartialClient); ok && pc.StaleAfter() > 0 {
		closedByRule, err = in.jobs.CloseStale(ctx, t.ID, pc.StaleAfter())
	} else {
		closedByRule, err = in.jobs.MarkMissingAsRemoved(ctx, t.ID, seenIDs)
	}
	removed += closedByRule
	if err != nil {
		return Result{Target: t, Inserted: inserted, Changed: changed, Unchanged: unchanged, Skipped: skipped,
			Err: fmt.Errorf("ingestion: closing jobs no longer listed for %s/%s: %w", t.ATSProvider, t.ExternalBoardID, err)}
	}

	if err := in.targetRecorder.MarkIngestionSucceeded(ctx, t.ID); err != nil {
		in.logger.Error("ingestion: recording successful ingestion failed",
			"target_id", t.ID, "error", err)
	}

	return Result{Target: t, Inserted: inserted, Changed: changed, Unchanged: unchanged, Skipped: skipped, Removed: removed}
}

// retireGoneBoard deactivates a target whose board the ATS reports as
// not found, and closes its open jobs. Failures are logged, not
// returned: the caller already returns the fetch error for this target.
func (in *Ingester) retireGoneBoard(ctx context.Context, t company.TargetCompany) {
	if err := in.targetRecorder.SetActive(ctx, t.ID, false); err != nil {
		in.logger.Error("ingestion: deactivating a not-found target failed",
			"target_id", t.ID, "company_id", t.CompanyID, "error", err)
		return
	}
	closed, err := in.jobs.MarkMissingAsRemoved(ctx, t.ID, nil)
	if err != nil {
		in.logger.Error("ingestion: closing a gone board's jobs failed", "target_id", t.ID, "error", err)
		return
	}
	in.logger.Warn("ingestion: board not found, target deactivated and its jobs closed",
		"target_id", t.ID, "company_id", t.CompanyID, "provider", t.ATSProvider,
		"board", t.ExternalBoardID, "jobs_closed", closed)
}
