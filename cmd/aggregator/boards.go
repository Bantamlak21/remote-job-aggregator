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
	limit      int // most names probed; 0 means no limit
}

// parseDiscoverBoardsArgs reads: [names-file|-] [--from-boards] [--limit=N].
func parseDiscoverBoardsArgs(args []string) (discoverBoardsArgs, error) {
	usage := errors.New("discover-boards: usage: discover-boards [names-file|-] [--from-boards] [--limit=N]")
	out := discoverBoardsArgs{namesFile: defaultRemoteCompaniesFile}
	fileGiven := false
	for _, a := range args {
		switch {
		case a == "--from-boards":
			out.fromBoards = true
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
	return out, nil
}

// cleanBoardNames drops blank, overlong and URL-like names and repeats
// (case-insensitively), keeping the order.
func cleanBoardNames(names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.Join(strings.Fields(n), " ")
		k := strings.ToLower(n)
		if n == "" || len([]rune(n)) > maxBoardNameRunes || strings.Contains(n, "://") || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
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

	var names []string
	if a.namesFile != "-" {
		fromFile, err := discovery.LoadCompanyNames(a.namesFile)
		if err != nil {
			logger.Error("loading company names failed", "error", err)
			return err
		}
		names = append(names, fromFile...)
	}

	db, err := database.New(ctx, cfg.Database)
	if err != nil {
		logger.Error("connecting to database failed", "error", err)
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	if a.fromBoards {
		fromDB, err := company.NewStore(db.Pool).NamesWithoutBoard(ctx, remoteBoards,
			[]string{string(ats.ProviderGreenhouse), string(ats.ProviderLever), string(ats.ProviderAshby)}, 100000)
		if err != nil {
			logger.Error("listing employers from the job boards failed", "error", err)
			return err
		}
		logger.Info("employers shown by job boards that have no ATS board yet", "count", len(fromDB))
		names = append(names, fromDB...)
	}
	names = cleanBoardNames(names)
	if a.limit > 0 && len(names) > a.limit {
		names = names[:a.limit]
	}
	if len(names) == 0 {
		logger.Info("no company names to look up")
		return nil
	}

	guesser := discovery.NewGuesser(newIngestionHTTPClient(cfg), 0, boardProbePause, logger)
	logger.Info("looking for job boards", "companies", len(names))
	candidates, report := guesser.Guess(ctx, names)
	logger.Info("job board search complete",
		"companies", report.Names, "with_a_board", report.Found, "boards", report.Boards,
		"none_found", len(report.NotFound), "unconfirmed", len(report.Unconfirmed))
	for _, u := range report.Unconfirmed {
		logger.Info("a board exists but does not show it is the company's; not registered", "board", u)
	}
	if len(candidates) == 0 {
		return nil
	}

	d := newDiscoverer(cfg, db, newHTTPClient(cfg), logger)
	return reportDiscoveryResults(logger, d.Run(ctx, candidates))
}
