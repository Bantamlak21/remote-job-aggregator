package eligibility

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"

	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering"
	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/relevance"
)

// Verdicts is what the service needs of the store.
type Verdicts interface {
	Pending(ctx context.Context, afterID int64, force bool, version int, target string, limit int) ([]Pending, error)
	Save(ctx context.Context, rows []Row) error
}

// EligibilityRules decides eligibility for one job.
type EligibilityRules interface {
	Classify(in filtering.Input) filtering.Verdict
}

// RoleRules puts a title in a role family.
type RoleRules interface {
	Classify(title string) relevance.Result
}

// Stats is what one Run did.
type Stats struct {
	Classified int
	Eligible   int
	Ineligible int
	Uncertain  int
	// Failed counts jobs the rules panicked on; each is stored as uncertain
	// with basis no_signal, never dropped and never marked eligible.
	Failed int
}

// Service classifies the jobs that need it.
type Service struct {
	store   Verdicts
	rules   EligibilityRules
	roles   RoleRules
	target  string
	batch   int
	workers int
	logger  *slog.Logger
}

// NewService returns a Service. batch is how many jobs are read, classified
// and written at a time (memory is bounded by it); workers is the size of the
// classification pool (0 means the number of CPUs, at most 8).
func NewService(store Verdicts, rules EligibilityRules, roles RoleRules, target string, batch, workers int, logger *slog.Logger) *Service {
	if batch <= 0 {
		batch = 200
	}
	if workers <= 0 {
		workers = min(runtime.NumCPU(), 8)
	}
	return &Service{store: store, rules: rules, roles: roles, target: target, batch: batch, workers: workers, logger: logger}
}

// Run classifies every open job that has no verdict, or has one made by other
// rules, for another country or from an older version of the job. With force
// every open job is classified again. It stops on ctx cancellation; what was
// written stays.
func (s *Service) Run(ctx context.Context, force bool) (Stats, error) {
	var st Stats
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		pending, err := s.store.Pending(ctx, after, force, filtering.Version, s.target, s.batch)
		if err != nil {
			return st, err
		}
		if len(pending) == 0 {
			return st, nil
		}
		rows := s.classify(pending, &st)
		if err := s.store.Save(ctx, rows); err != nil {
			return st, err
		}
		after = pending[len(pending)-1].ID
		st.Classified += len(rows)
		s.logger.Info("eligibility: batch classified", "jobs", len(rows), "total", st.Classified, "last_job_id", after)
	}
}

// classify runs the rules over a batch with a bounded pool. The rows come back
// in the batch's order.
func (s *Service) classify(pending []Pending, st *Stats) []Row {
	rows := make([]Row, len(pending))
	failed := make([]bool, len(pending))
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(s.workers, len(pending)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				rows[i], failed[i] = s.one(pending[i])
			}
		}()
	}
	for i := range pending {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, r := range rows {
		if failed[i] {
			st.Failed++
		}
		switch r.Verdict.Status {
		case filtering.Eligible:
			st.Eligible++
		case filtering.Ineligible:
			st.Ineligible++
		default:
			st.Uncertain++
		}
	}
	return rows
}

// one classifies one job. A panic in the rules is contained: the job gets an
// "uncertain, no signal" verdict so it stays visible and is retried on the
// next version of the rules.
func (s *Service) one(p Pending) (row Row, failed bool) {
	row = Row{JobID: p.ID, Target: s.target, Version: filtering.Version, ContentHash: p.ContentHash}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("eligibility: the rules panicked on a job; storing it as uncertain", "job_id", p.ID, "panic", fmt.Sprint(r))
			row.Verdict = filtering.Verdict{Status: filtering.Uncertain, Confidence: 0, Basis: filtering.BasisNoSignal,
				Reasons: []string{"the rules failed on this job"}, Evidence: []filtering.Evidence{}, Locations: []string{}, Restrictions: []string{}}
			row.Role = s.roles.Classify("") // the profile's default family, for a title with nothing in it
			failed = true
		}
	}()
	row.Verdict = s.rules.Classify(p.Input)
	row.Role = s.roles.Classify(p.Input.Title)
	return row, false
}
