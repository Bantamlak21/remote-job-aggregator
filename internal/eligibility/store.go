// Package eligibility keeps the filtering package's verdicts about jobs in the
// database: which jobs still need one (new, changed, or classified by older
// rules), the batch write, and the service that runs it. The rules themselves
// live in internal/filtering; this package is the application layer around
// them.
package eligibility

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering"
	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/relevance"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

// Pending is a job that needs a verdict, with everything the rules read.
type Pending struct {
	ID          int64
	Input       filtering.Input
	ContentHash string
}

// Row is one verdict to store.
type Row struct {
	JobID       int64
	Target      string
	Verdict     filtering.Verdict
	Role        relevance.Result
	Version     int
	ContentHash string
}

// Store reads pending jobs and writes verdicts.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store on the pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// pendingQuery selects open jobs after a job id (keyset pagination, so the
// scan never holds the whole table) that have no verdict, or whose verdict was
// made by other rules, for another country, or from an older version of the
// job. force selects every open job.
const pendingQuery = `
	SELECT j.id, j.source, j.title, COALESCE(j.location_raw, ''), j.remote_type, COALESCE(j.description, ''),
	       j.content_hash, ` + job.MarketExpr + `
	FROM jobs j
	JOIN companies c ON c.id = j.company_id
	JOIN target_companies t ON t.id = j.target_company_id
	LEFT JOIN job_eligibility e ON e.job_id = j.id
	WHERE j.status = 'open' AND j.id > $1
	  AND ($2 OR e.job_id IS NULL OR e.classifier_version <> $3 OR e.target_country <> $4 OR e.job_content_hash <> j.content_hash)
	ORDER BY j.id
	LIMIT $5`

// Pending returns up to limit jobs with id > afterID that need a verdict.
func (s *Store) Pending(ctx context.Context, afterID int64, force bool, version int, target string, limit int) ([]Pending, error) {
	rows, err := s.pool.Query(ctx, pendingQuery, afterID, force, version, target, limit)
	if err != nil {
		return nil, fmt.Errorf("eligibility: listing pending jobs: %w", err)
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.ID, &p.Input.Source, &p.Input.Title, &p.Input.Location, &p.Input.RemoteType,
			&p.Input.Description, &p.ContentHash, &p.Input.Market); err != nil {
			return nil, fmt.Errorf("eligibility: scanning a pending job: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const upsertQuery = `
	INSERT INTO job_eligibility (job_id, target_country, status, confidence, basis, reasons, evidence, detected_locations,
		restrictions, hours_constraint, role_family, relevant, relevance_matched, classifier_version, job_content_hash, classified_at)
	SELECT $1::bigint, $2::text, $3::text, $4::numeric, $5::text, $6::jsonb, $7::jsonb, $8::jsonb, $9::jsonb, NULLIF($10::text, ''), $11::text, $12::boolean,
	       NULLIF($13::text, ''), $14::integer, $15::text, now()
	WHERE EXISTS (SELECT 1 FROM jobs WHERE id = $1::bigint)
	ON CONFLICT (job_id) DO UPDATE SET
		target_country = EXCLUDED.target_country, status = EXCLUDED.status, confidence = EXCLUDED.confidence, basis = EXCLUDED.basis,
		reasons = EXCLUDED.reasons, evidence = EXCLUDED.evidence, detected_locations = EXCLUDED.detected_locations,
		restrictions = EXCLUDED.restrictions, hours_constraint = EXCLUDED.hours_constraint, role_family = EXCLUDED.role_family,
		relevant = EXCLUDED.relevant, relevance_matched = EXCLUDED.relevance_matched,
		classifier_version = EXCLUDED.classifier_version, job_content_hash = EXCLUDED.job_content_hash, classified_at = now()`

// Save writes the verdicts in one transaction. A job deleted since it was read
// (the foreign key fails) is skipped, not an error for the whole batch.
func (s *Store) Save(ctx context.Context, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("eligibility: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(upsertQuery, r.JobID, r.Target, string(r.Verdict.Status), r.Verdict.Confidence, string(r.Verdict.Basis),
			mustJSON(r.Verdict.Reasons), mustJSON(r.Verdict.Evidence), mustJSON(r.Verdict.Locations), mustJSON(r.Verdict.Restrictions),
			r.Verdict.HoursConstraint, r.Role.Family, r.Role.Relevant, r.Role.Matched, r.Version, r.ContentHash)
	}
	res := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := res.Exec(); err != nil {
			_ = res.Close()
			return fmt.Errorf("eligibility: saving verdicts: %w", err)
		}
	}
	if err := res.Close(); err != nil {
		return fmt.Errorf("eligibility: saving verdicts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("eligibility: commit: %w", err)
	}
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// Counts returns how many open jobs have each eligibility status, plus the
// number with no verdict yet under the key "unclassified".
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(e.status, 'unclassified'), count(*)
		FROM jobs j LEFT JOIN job_eligibility e ON e.job_id = j.id
		WHERE j.status = 'open' GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("eligibility: counting: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, fmt.Errorf("eligibility: scanning counts: %w", err)
		}
		out[k] = n
	}
	return out, rows.Err()
}
