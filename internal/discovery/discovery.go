// Package discovery turns a list of candidate ATS boards into persisted
// companies and target_companies rows. It knows nothing about how to read
// job postings from a board (that is internal/ats and internal/ingestion,
// in a later phase) — its only question is "does this URL respond?", and
// its only side effect is persisting a company + target when the answer
// is yes.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

// Candidate is one board to attempt to discover: a company name paired
// with a specific ATS provider's board id and the URL to probe to confirm
// it's real. Candidates normally come from LoadSeedCandidates, but the
// type has no dependency on that loader — a future discovery mechanism
// (crawling, an admin API) can produce the same type.
type Candidate struct {
	CompanyName     string `json:"company_name"`
	Website         string `json:"website,omitempty"`
	ATSProvider     string `json:"ats_provider"`
	ExternalBoardID string `json:"external_board_id"`
	BoardURL        string `json:"board_url"`
	// Verified says the caller has already proven the board exists and is the
	// company's (through the ATS's own API), so the page probe, which only
	// checks that a public page answers, is skipped: it can add failures (a
	// page that blocks HEAD and GET) but no proof.
	Verified bool `json:"-"`
}

// validate checks the fields Discoverer itself relies on being present
// and well-formed. It does not know which ATS providers are real — that
// enforcement belongs to the internal/ats package once it exists; here,
// ats_provider is just a non-blank label.
func (c Candidate) validate() error {
	if strings.TrimSpace(c.CompanyName) == "" {
		return errors.New("company_name is required")
	}
	if strings.TrimSpace(c.ATSProvider) == "" {
		return errors.New("ats_provider is required")
	}
	if strings.TrimSpace(c.ExternalBoardID) == "" {
		return errors.New("external_board_id is required")
	}
	u, err := url.Parse(c.BoardURL)
	if err != nil {
		return fmt.Errorf("board_url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("board_url must be http or https, got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("board_url must include a host")
	}
	return nil
}

// Result is the outcome of attempting to discover one Candidate. Err is
// nil if and only if Validated is true and Company/Target are both set —
// callers should check Err, not Validated, to decide whether to trust
// Company/Target.
type Result struct {
	Candidate Candidate
	Company   *company.Company
	Target    *company.TargetCompany
	Validated bool
	Err       error
}

// CompanyUpserter is the persistence dependency Discoverer needs from
// internal/company for the companies table. Defined here, on the
// consumer side, per Go convention — *company.Store satisfies it without
// either package importing an interface type from the other.
type CompanyUpserter interface {
	Upsert(ctx context.Context, params company.UpsertParams) (*company.Company, error)
}

// TargetUpserter is the persistence dependency Discoverer needs from
// internal/company for the target_companies table.
type TargetUpserter interface {
	Upsert(ctx context.Context, params company.TargetUpsertParams) (*company.TargetCompany, error)
}

// Discoverer validates candidates and persists the ones that check out.
type Discoverer struct {
	companies CompanyUpserter
	targets   TargetUpserter
	http      *httpclient.Client
	workers   int
	logger    *slog.Logger
}

// New returns a Discoverer. workers bounds how many candidates are
// probed concurrently — CLAUDE.md's rule that discovery concurrency must
// be explicit and bounded, independent of the database pool or any
// future ingestion worker pool. The CLI's caller gets this value from
// config.DiscoveryConfig.Workers, already validated to be at least 1 —
// but New is exported, so a workers <= 0 from some other caller is
// clamped to 1 rather than trusted: with zero workers, Run's feeder
// goroutine has nothing to ever receive from the unbounded-wait side of
// its work channel and leaks forever on any non-empty candidate list
// (found by adversarial review, reproduced with a goroutine dump).
func New(companies CompanyUpserter, targets TargetUpserter, httpClient *httpclient.Client, workers int, logger *slog.Logger) *Discoverer {
	if workers < 1 {
		workers = 1
	}
	return &Discoverer{
		companies: companies,
		targets:   targets,
		http:      httpClient,
		workers:   workers,
		logger:    logger,
	}
}

// Run probes and persists every candidate, using a bounded pool of
// d.workers goroutines, and returns one Result per candidate in
// unspecified order (a channel-fed pool cannot preserve input order
// without extra bookkeeping this project has no use for yet).
//
// If ctx is canceled before every candidate has been dispatched to a
// worker, Run returns early with fewer results than len(candidates) —
// the candidates that were never dispatched simply aren't in the
// returned slice. A candidate already in flight when ctx is canceled
// still gets a Result (typically an error from the underlying HTTP call
// observing the same cancellation), since dropping partially-done work
// silently would be worse than reporting it as failed.
func (d *Discoverer) Run(ctx context.Context, candidates []Candidate) []Result {
	work := make(chan Candidate)
	results := make(chan Result)

	var wg sync.WaitGroup
	for i := 0; i < d.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				results <- d.process(ctx, c)
			}
		}()
	}

	go func() {
		defer close(work)
		for _, c := range candidates {
			select {
			case work <- c:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]Result, 0, len(candidates))
	for r := range results {
		out = append(out, r)
	}
	return out
}

func (d *Discoverer) process(ctx context.Context, c Candidate) Result {
	if err := c.validate(); err != nil {
		return Result{Candidate: c, Err: fmt.Errorf("discovery: invalid candidate %q: %w", c.CompanyName, err)}
	}

	statusCode, method := 0, "verified-by-api"
	if !c.Verified {
		var err error
		statusCode, method, err = d.probe(ctx, c.BoardURL)
		if err != nil {
			d.logger.Warn("discovery: candidate failed validation",
				"company", c.CompanyName, "ats_provider", c.ATSProvider, "board_url", c.BoardURL, "error", err)
			return Result{Candidate: c, Err: fmt.Errorf("discovery: validating %s: %w", c.BoardURL, err)}
		}
	}

	comp, err := d.companies.Upsert(ctx, company.UpsertParams{
		Name:    c.CompanyName,
		Website: c.Website,
	})
	if err != nil {
		return Result{Candidate: c, Err: fmt.Errorf("discovery: upserting company %q: %w", c.CompanyName, err)}
	}

	target, err := d.targets.Upsert(ctx, company.TargetUpsertParams{
		CompanyID:       comp.ID,
		ATSProvider:     c.ATSProvider,
		ExternalBoardID: c.ExternalBoardID,
		BoardURL:        c.BoardURL,
		DiscoveryMetadata: map[string]any{
			"validated_at":      time.Now().UTC().Format(time.RFC3339),
			"validation_method": method,
			"status_code":       statusCode,
		},
	})
	if err != nil {
		return Result{Candidate: c, Company: comp, Err: fmt.Errorf("discovery: upserting target %s/%s: %w", c.ATSProvider, c.ExternalBoardID, err)}
	}

	d.logger.Info("discovery: target discovered",
		"company", comp.Name, "ats_provider", c.ATSProvider, "board_url", c.BoardURL, "method", method)
	return Result{Candidate: c, Company: comp, Target: target, Validated: true}
}

// probe confirms boardURL responds, trying HEAD first and falling back
// to GET — CLAUDE.md's explicit rule: "HEAD requests may be used where
// useful, but GET must remain a fallback because some servers do not
// handle HEAD reliably." The mechanism is exercised by
// TestRun_HeadFailsGetSucceeds_FallsBackToGet against a controlled fake
// server; live spot-checks against roughly a dozen real Greenhouse/
// Lever/Ashby-hosted boards while building this never actually hit a
// case where HEAD and GET disagreed (both consistently redirect or
// succeed together on every board tried) — so this fallback is
// defensive per the stated project requirement, not something observed
// to be load-bearing against real traffic yet. Keep it regardless: the
// requirement is explicit, the cost of the second request only on
// HEAD's failure is negligible, and "never seen it happen yet" is not
// the same claim as "cannot happen."
//
// A non-2xx (or network error) from HEAD is not itself treated as
// failure — GET still gets a chance regardless of what specifically went
// wrong with HEAD. Only a non-2xx (or error) from GET is reported.
func (d *Discoverer) probe(ctx context.Context, boardURL string) (statusCode int, method string, err error) {
	var lastErr error
	for _, m := range []string{http.MethodHead, http.MethodGet} {
		req, reqErr := http.NewRequestWithContext(ctx, m, boardURL, nil)
		if reqErr != nil {
			return 0, "", fmt.Errorf("building %s request: %w", m, reqErr)
		}

		resp, doErr := d.http.Do(req)
		if doErr != nil {
			lastErr = doErr
			if m == http.MethodHead {
				continue
			}
			return 0, "", lastErr
		}

		// Drain a small prefix so the connection can be reused, then
		// close — probing only needs the status, not the body; reading
		// job content is internal/ats's job in a later phase.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, m, nil
		}
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		if m == http.MethodHead {
			continue
		}
		return resp.StatusCode, m, lastErr
	}
	return 0, "", lastErr
}
