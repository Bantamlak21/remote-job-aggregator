package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/eligibility"
	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering"
	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/relevance"
)

// classifyBatch is how many jobs are read, classified and written at a time.
const classifyBatch = 200

// parseClassifyArgs reads: [--reclassify].
func parseClassifyArgs(args []string) (reclassify bool, err error) {
	for _, a := range args {
		switch a {
		case "--reclassify":
			reclassify = true
		default:
			return false, errors.New("classify: usage: classify [--reclassify]")
		}
	}
	return reclassify, nil
}

// relevanceProfile loads the configured role profile, or the built-in one.
func relevanceProfile(cfg *config.Config) (*relevance.Profile, error) {
	if cfg.Filtering.RelevanceProfile == "" {
		return relevance.Default(), nil
	}
	return relevance.Load(cfg.Filtering.RelevanceProfile)
}

// classifyJobs gives every open job that needs one a verdict (see
// internal/eligibility): the jobs with none, or with one made by older rules
// or from an older version of the job. force redoes every open job.
func classifyJobs(ctx context.Context, cfg *config.Config, logger *slog.Logger, db *database.DB, force bool) (eligibility.Stats, error) {
	profile, err := relevanceProfile(cfg)
	if err != nil {
		return eligibility.Stats{}, err
	}
	svc := eligibility.NewService(eligibility.NewStore(db.Pool), filtering.NewClassifier(), profile, "ET", classifyBatch, 0, logger)
	return svc.Run(ctx, force)
}

// runClassify is `aggregator classify [--reclassify]`.
func runClassify(ctx context.Context, cfg *config.Config, logger *slog.Logger, args []string) error {
	force, err := parseClassifyArgs(args)
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

	st, err := classifyJobs(ctx, cfg, logger, db, force)
	logger.Info("classification complete", "classified", st.Classified, "eligible", st.Eligible, "ineligible", st.Ineligible,
		"uncertain", st.Uncertain, "rule_failures", st.Failed, "reclassify", force)
	if err != nil {
		logger.Error("classification stopped", "error", err)
		return err
	}
	counts, cerr := eligibility.NewStore(db.Pool).Counts(ctx)
	if cerr != nil {
		logger.Warn("counting classified jobs failed", "error", cerr)
	} else {
		logger.Info("open jobs by eligibility", "eligible", counts["eligible"], "ineligible", counts["ineligible"],
			"uncertain", counts["uncertain"], "unclassified", counts["unclassified"])
	}
	return nil
}
