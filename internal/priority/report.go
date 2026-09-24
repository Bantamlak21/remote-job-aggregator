package priority

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SourceCount is how many open jobs one source contributes.
type SourceCount struct {
	Source string
	Open   int
}

// TargetStatus is the state of one registered source.
type TargetStatus struct {
	Provider       string
	Board          string
	Active         bool
	LastIngestedAt *time.Time
}

// CompanyCoverage is one priority company's row in the report.
type CompanyCoverage struct {
	Name     string
	OpenJobs int
	Sources  []SourceCount
	Targets  []TargetStatus
}

// Coverage is the report: per-company open jobs, and the honest headline
// number, how many of the priority companies have at least one open job.
type Coverage struct {
	Companies []CompanyCoverage
	// Missing lists companies the configured list names but the database
	// does not flag as priority (never seeded, or a seed that failed). They
	// count in the headline's denominator: "4 of 22" after a failed seed
	// would hide three companies that were never even tried.
	Missing []string
}

// NoteMissing records which of the expected (configured) company names have
// no priority row in this report.
func (c *Coverage) NoteMissing(expected []string) {
	have := make(map[string]bool, len(c.Companies))
	for _, co := range c.Companies {
		have[strings.ToLower(strings.TrimSpace(co.Name))] = true
	}
	c.Missing = nil
	for _, name := range expected {
		if !have[strings.ToLower(strings.TrimSpace(name))] {
			c.Missing = append(c.Missing, name)
		}
	}
}

// Total is the number of priority companies the report accounts for: the
// ones found plus the ones missing.
func (c Coverage) Total() int { return len(c.Companies) + len(c.Missing) }

// WithJobs is the number of companies with at least one open job.
func (c Coverage) WithJobs() int {
	n := 0
	for _, co := range c.Companies {
		if co.OpenJobs > 0 {
			n++
		}
	}
	return n
}

// TotalOpen is the number of open priority jobs across all companies.
func (c Coverage) TotalOpen() int {
	n := 0
	for _, co := range c.Companies {
		n += co.OpenJobs
	}
	return n
}

// Store reads coverage from PostgreSQL. It is read-only.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const (
	coverageCompaniesQuery = `SELECT id, name FROM companies WHERE is_priority ORDER BY lower(name)`

	coverageJobsQuery = `
		SELECT j.company_id, j.source, count(*)
		FROM jobs j JOIN companies c ON c.id = j.company_id
		WHERE c.is_priority AND j.status = 'open'
			AND (j.expires_at IS NULL OR j.expires_at > now())
		GROUP BY j.company_id, j.source
		ORDER BY j.company_id, j.source`

	coverageTargetsQuery = `
		SELECT t.company_id, t.ats_provider, t.external_board_id, t.is_active, t.last_successful_ingestion_at
		FROM target_companies t JOIN companies c ON c.id = t.company_id
		WHERE c.is_priority
		ORDER BY t.company_id, t.ats_provider, t.external_board_id`
)

// Coverage reports, for every priority company, its open jobs by source
// and the state of each registered source. Three cheap queries assembled
// in Go rather than one wide join, so a company with many jobs and many
// targets does not multiply rows.
func (s *Store) Coverage(ctx context.Context) (Coverage, error) {
	rows, err := s.pool.Query(ctx, coverageCompaniesQuery)
	if err != nil {
		return Coverage{}, fmt.Errorf("priority: listing companies: %w", err)
	}
	byID := map[int64]*CompanyCoverage{}
	var order []int64
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return Coverage{}, fmt.Errorf("priority: listing companies: %w", err)
		}
		byID[id] = &CompanyCoverage{Name: name}
		order = append(order, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Coverage{}, fmt.Errorf("priority: listing companies: %w", err)
	}

	jobRows, err := s.pool.Query(ctx, coverageJobsQuery)
	if err != nil {
		return Coverage{}, fmt.Errorf("priority: counting jobs: %w", err)
	}
	for jobRows.Next() {
		var (
			companyID int64
			source    string
			n         int
		)
		if err := jobRows.Scan(&companyID, &source, &n); err != nil {
			jobRows.Close()
			return Coverage{}, fmt.Errorf("priority: counting jobs: %w", err)
		}
		if c := byID[companyID]; c != nil {
			c.Sources = append(c.Sources, SourceCount{Source: source, Open: n})
			c.OpenJobs += n
		}
	}
	jobRows.Close()
	if err := jobRows.Err(); err != nil {
		return Coverage{}, fmt.Errorf("priority: counting jobs: %w", err)
	}

	targetRows, err := s.pool.Query(ctx, coverageTargetsQuery)
	if err != nil {
		return Coverage{}, fmt.Errorf("priority: listing targets: %w", err)
	}
	for targetRows.Next() {
		var (
			companyID int64
			t         TargetStatus
		)
		if err := targetRows.Scan(&companyID, &t.Provider, &t.Board, &t.Active, &t.LastIngestedAt); err != nil {
			targetRows.Close()
			return Coverage{}, fmt.Errorf("priority: listing targets: %w", err)
		}
		if c := byID[companyID]; c != nil {
			c.Targets = append(c.Targets, t)
		}
	}
	targetRows.Close()
	if err := targetRows.Err(); err != nil {
		return Coverage{}, fmt.Errorf("priority: listing targets: %w", err)
	}

	out := Coverage{Companies: make([]CompanyCoverage, 0, len(order))}
	for _, id := range order {
		out.Companies = append(out.Companies, *byID[id])
	}
	return out, nil
}

// Write renders the report as a plain-text table followed by the headline
// numbers. Deterministic (companies alphabetical, sources by name) so two
// runs over the same data print the same bytes.
func (c Coverage) Write(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "COMPANY\tOPEN\tBY SOURCE\tSOURCES REGISTERED")
	for _, co := range c.Companies {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", co.Name, co.OpenJobs, formatSources(co.Sources), formatTargets(co.Targets))
	}
	for _, name := range c.Missing {
		fmt.Fprintf(tw, "%s\t0\t-\tNOT SEEDED (run seed-priority)\n", name)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\n%d of %d priority companies have at least one open job (%d open jobs in total)\n",
		c.WithJobs(), c.Total(), c.TotalOpen())
	return err
}

func formatSources(sources []SourceCount) string {
	if len(sources) == 0 {
		return "-"
	}
	sorted := append([]SourceCount(nil), sources...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Source < sorted[j].Source })
	parts := make([]string, len(sorted))
	for i, s := range sorted {
		parts[i] = fmt.Sprintf("%s=%d", s.Source, s.Open)
	}
	return strings.Join(parts, " ")
}

// formatTargets lists each provider once with its state: "feed" (working),
// "careers-site(inactive)" (the board 404ed and was deactivated), or
// "search(never ingested)".
func formatTargets(targets []TargetStatus) string {
	if len(targets) == 0 {
		return "none"
	}
	parts := make([]string, len(targets))
	for i, t := range targets {
		switch {
		case !t.Active:
			parts[i] = t.Provider + "(inactive)"
		case t.LastIngestedAt == nil:
			parts[i] = t.Provider + "(never ingested)"
		default:
			parts[i] = t.Provider
		}
	}
	return strings.Join(parts, " ")
}
