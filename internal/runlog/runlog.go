// Package runlog remembers when each many-employer collector last ran, in
// PostgreSQL, so a source's minimum interval holds across separate ingest
// processes (see ingestion.IntervalCollector).
package runlog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store reads and writes collector_runs.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// LastRun returns when the named collector last ran; ok is false if it never
// has.
func (s *Store) LastRun(ctx context.Context, name string) (time.Time, bool, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT last_run_at FROM collector_runs WHERE name = $1`, strings.TrimSpace(name)).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("runlog: reading last run of %q: %w", name, err)
	}
	return at, true, nil
}

// RecordRun records that the named collector ran at at. The stored time only
// moves forward: two overlapping runs cannot make the newest one look older.
func (s *Store) RecordRun(ctx context.Context, name string, at time.Time) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("runlog: collector name is required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO collector_runs (name, last_run_at) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET last_run_at = GREATEST(collector_runs.last_run_at, EXCLUDED.last_run_at)`,
		name, at)
	if err != nil {
		return fmt.Errorf("runlog: recording a run of %q: %w", name, err)
	}
	return nil
}
