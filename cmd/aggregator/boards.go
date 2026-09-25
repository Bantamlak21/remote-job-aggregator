package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/discovery"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

// defaultRemoteCompaniesFile lists remote-first companies by name. It is only
// a list of names: discover-boards finds each one's job board itself.
const defaultRemoteCompaniesFile = "configs/remote_companies.txt"

// maxBoardNameRunes: a longer "name" is a scraping error (a sentence, a URL),
// not a company, and would only waste probes.
const maxBoardNameRunes = 80

type discoverBoardsArgs struct {
	namesFile  string // "-" means no file
	fromBoards bool
	limit      int  // most names probed; 0 means no limit
	recheck    bool // re-verify boards already registered for the names in the file
	apply      bool // with recheck: deactivate the boards that no longer verify
}

// parseDiscoverBoardsArgs reads: [names-file|-] [--from-boards] [--limit=N].
func parseDiscoverBoardsArgs(args []string) (discoverBoardsArgs, error) {
	usage := errors.New("discover-boards: usage: discover-boards [names-file|-] [--from-boards] [--limit=N] [--recheck [--apply]]")
	out := discoverBoardsArgs{namesFile: defaultRemoteCompaniesFile}
	fileGiven := false
	for _, a := range args {
		switch {
		case a == "--from-boards":
			out.fromBoards = true
		case a == "--recheck":
			out.recheck = true
		case a == "--apply":
			out.apply = true
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil || n < 1 {
				return out, fmt.Errorf("discover-boards: --limit must be a positive integer, got %q", strings.TrimPrefix(a, "--limit="))
			}
			out.limit = n
		case strings.HasPrefix(a, "--"):
			return out, usage
		default:
			if fileGiven {
				return out, usage
			}
			out.namesFile, fileGiven = a, true
		}
	}
	if out.apply && !out.recheck {
		return out, errors.New("discover-boards: --apply only makes sense with --recheck")
	}
	return out, nil
}

// atsBoardProviders are the ATSs discover-boards looks for boards on.
var atsBoardProviders = []string{string(ats.ProviderGreenhouse), string(ats.ProviderLever), string(ats.ProviderAshby)}

// cleanBoardEntries drops blank, overlong and URL-like names and repeats
// (case-insensitively), keeping the order.
func cleanBoardEntries(in []discovery.Entry) []discovery.Entry {
	var out []discovery.Entry
	seen := map[string]bool{}
	for _, e := range in {
		n := strings.Join(strings.Fields(e.Name), " ")
		k := strings.ToLower(n)
		if n == "" || len([]rune(n)) > maxBoardNameRunes || strings.Contains(n, "://") || seen[k] {
			continue
		}
		seen[k] = true
		e.Name = n
		out = append(out, e)
	}
	return out
}

// withoutBoards drops the entries whose company already has an ATS board, so
// a rerun looks only at what is new.
func withoutBoards(in []discovery.Entry, have []string) []discovery.Entry {
	skip := make(map[string]bool, len(have))
	for _, n := range have {
		skip[strings.ToLower(strings.TrimSpace(n))] = true
	}
	var out []discovery.Entry
	for _, e := range in {
		if !skip[strings.ToLower(e.Name)] {
			out = append(out, e)
		}
	}
	return out
}

// runDiscoverBoards finds the Greenhouse, Lever and Ashby job boards of the
// companies in a names file (and, with --from-boards, of every employer a
// remote job board has shown that has no ATS board yet), and registers each
// verified board as a worldwide target. A board is registered only when it
// proves it belongs to the company; see internal/discovery/guess.go.
func runDiscoverBoards(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	a, err := parseDiscoverBoardsArgs(args)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	var entries []discovery.Entry
	if a.namesFile != "-" {
		lines, err := discovery.LoadCompanyNames(a.namesFile)
		if err != nil {
			logger.Error("loading company names failed", "error", err)
			return err
		}
		for _, l := range lines {
			entries = append(entries, discovery.ParseEntry(l))
		}
	}

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		logger.Error("connecting to database failed", "error", err)
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	if a.recheck {
		if a.fromBoards {
			// The boards found from employer names: companies a job board showed
			// that now have an ATS board.
			fromDB, err := company.NewStore(db.Pool).NamesWithSourceAndBoard(ctx, remoteBoards, atsBoardProviders)
			if err != nil {
				logger.Error("listing employers with a board failed", "error", err)
				return err
			}
			for _, n := range fromDB {
				entries = append(entries, discovery.Entry{Name: n})
			}
		}
		return runRecheck(ctx, cfg, logger, db, entries, a)
	}

	if a.fromBoards {
		fromDB, err := company.NewStore(db.Pool).NamesWithoutBoard(ctx, remoteBoards, atsBoardProviders, 100000)
		if err != nil {
			logger.Error("listing employers from the job boards failed", "error", err)
			return err
		}
		logger.Info("employers shown by job boards that have no ATS board yet", "count", len(fromDB))
		for _, n := range fromDB {
			entries = append(entries, discovery.Entry{Name: n})
		}
	}
	// Leave companies that already have a board alone: a rerun looks at what is new.
	have, err := company.NewStore(db.Pool).NamesWithBoard(ctx, atsBoardProviders)
	if err != nil {
		logger.Error("listing companies that already have a board failed", "error", err)
		return err
	}
	entries = withoutBoards(cleanBoardEntries(entries), have)
	if a.limit > 0 && len(entries) > a.limit {
		entries = entries[:a.limit]
	}
	if len(entries) == 0 {
		logger.Info("no company names to look up")
		return nil
	}

	guesser := discovery.NewGuesser(newIngestionHTTPClient(cfg), 0, boardProbePause, logger)
	logger.Info("looking for job boards", "companies", len(entries))
	candidates, report := guesser.Guess(ctx, entries)
	logger.Info("job board search complete",
		"companies", report.Names, "with_a_board", report.Found, "boards", report.Boards,
		"none_found", len(report.NotFound), "unconfirmed", len(report.Unconfirmed), "ambiguous", len(report.Ambiguous),
		"need_a_domain", len(report.NeedsDomain), "failed_probes", len(report.Failed), "not_reached", len(report.Unreached))
	for _, u := range report.Unconfirmed {
		logger.Info("a board exists but does not show it is the company's; not registered", "board", u)
	}
	for _, n := range report.Ambiguous {
		logger.Info("the name verified on more than one board; not registered (add 'Name | domain' to the names file to settle it)", "company", n)
	}
	for _, n := range report.NeedsDomain {
		logger.Info("the name is an everyday word and cannot be told from another company's by name; add 'Name | domain' to the names file", "company", n)
	}
	for _, f := range report.Failed {
		logger.Warn("a probe failed; the name was not fully checked", "probe", f)
	}
	if report.Aborted {
		return fmt.Errorf("discover-boards: stopped after too many failed probes (rate limited?); %d of %d names not reached", len(report.Unreached), report.Names)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("discover-boards: interrupted; nothing from this run was registered: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}

	d := newDiscoverer(cfg, db, newHTTPClient(cfg), logger)
	return reportDiscoveryResults(logger, d.Run(ctx, candidates))
}

// staleBoards returns the registered boards that the fresh look-up did not
// verify: a board whose company was fully checked (no failed or unreached
// probe) and whose (provider, slug) is not among the verified candidates. A
// company that was not fully checked is never judged.
func staleBoards(existing []company.NamedTarget, verified []discovery.Candidate, unchecked map[string]bool) []company.NamedTarget {
	ok := map[string]bool{}
	for _, c := range verified {
		ok[strings.ToLower(c.CompanyName)+"|"+c.ATSProvider+"|"+strings.ToLower(c.ExternalBoardID)] = true
	}
	var out []company.NamedTarget
	for _, t := range existing {
		name := strings.ToLower(t.CompanyName)
		if unchecked[name] || ok[name+"|"+t.ATSProvider+"|"+strings.ToLower(t.ExternalBoardID)] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// uncheckedNames lists (lower-cased) the companies a Guess run did not fully
// check: failed probes or names not reached.
func uncheckedNames(report discovery.GuessReport) map[string]bool {
	out := map[string]bool{}
	for _, n := range report.Unreached {
		out[strings.ToLower(n)] = true
	}
	for _, f := range report.Failed {
		// "Name provider/slug: error": the name is everything before the last " provider/slug".
		if i := strings.Index(f, ": "); i > 0 {
			head := f[:i]
			if j := strings.LastIndex(head, " "); j > 0 {
				out[strings.ToLower(head[:j])] = true
			}
		}
	}
	return out
}

// runRecheck re-verifies, under the current rules, the boards registered for
// the companies in the names file. Boards that no longer verify are listed;
// with --apply they are deactivated and their jobs closed.
func runRecheck(ctx context.Context, cfg *config.Config, logger *slog.Logger, db *database.DB, entries []discovery.Entry, a discoverBoardsArgs) error {
	entries = cleanBoardEntries(entries)
	if a.limit > 0 && len(entries) > a.limit {
		entries = entries[:a.limit]
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	targets := company.NewTargetStore(db.Pool)
	existing, err := targets.ListByCompanyNames(ctx, names, atsBoardProviders)
	if err != nil {
		logger.Error("listing registered boards failed", "error", err)
		return err
	}
	// Only companies that have a board need looking at.
	have := map[string]bool{}
	for _, t := range existing {
		have[strings.ToLower(t.CompanyName)] = true
	}
	var toCheck []discovery.Entry
	for _, e := range entries {
		if have[strings.ToLower(e.Name)] {
			toCheck = append(toCheck, e)
		}
	}
	guesser := discovery.NewGuesser(newIngestionHTTPClient(cfg), 0, boardProbePause, logger)
	logger.Info("re-checking registered boards", "companies", len(toCheck), "boards", len(existing))
	verified, report := guesser.Guess(ctx, toCheck)
	if report.Aborted {
		return fmt.Errorf("discover-boards: stopped after too many failed probes (rate limited?); nothing was changed")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("discover-boards: interrupted; nothing was changed: %w", err)
	}
	stale := staleBoards(existing, verified, uncheckedNames(report))
	jobs := job.NewStore(db.Pool)
	for _, t := range stale {
		logger.Warn("a registered board does not verify as the company's",
			"company", t.CompanyName, "provider", t.ATSProvider, "board", t.ExternalBoardID, "active", t.IsActive, "apply", a.apply)
		if !a.apply || !t.IsActive {
			continue
		}
		if err := targets.SetActive(ctx, t.ID, false); err != nil {
			logger.Error("deactivating a board failed", "board", t.ExternalBoardID, "error", err)
			continue
		}
		closed, err := jobs.MarkMissingAsRemoved(ctx, t.ID, nil)
		if err != nil {
			logger.Error("closing the jobs of a deactivated board failed", "board", t.ExternalBoardID, "error", err)
			continue
		}
		logger.Info("board deactivated and its jobs closed", "company", t.CompanyName, "provider", t.ATSProvider, "board", t.ExternalBoardID, "jobs_closed", closed)
	}
	logger.Info("re-check complete", "boards_checked", len(existing), "not_verified", len(stale), "applied", a.apply)
	return nil
}
