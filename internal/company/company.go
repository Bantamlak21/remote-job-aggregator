// Package company owns the domain types and persistence for the
// companies and target_companies tables. It is the only place that
// writes to either table: discovery and ingestion go through Store and
// TargetStore rather than issuing their own SQL, so the normalization
// rules the schema's unique indexes encode (case-fold + trim on name and
// external_board_id, case-fold + trailing-slash-strip on website) live
// in exactly one place.
package company

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status mirrors the companies.status CHECK constraint. It is a named
// string type rather than an int enum so a value read out of the
// database always round-trips to the same text the column holds.
type Status string

const (
	StatusActive   Status = "active"
	StatusInactive Status = "inactive"
)

// Company is one row of the companies table.
//
// Website and Description are plain strings rather than *string: the
// column is nullable, but "absent" and "empty" are the same thing for
// both fields at the domain level, and the schema's CHECK constraint
// already forbids a blank-but-non-null website. The empty string means
// the column is NULL; Store is responsible for the translation in both
// directions so no caller has to remember it.
type Company struct {
	ID          int64
	Name        string
	Website     string
	Description string
	Status      Status
	// IsPriority marks an employer whose jobs the board badges and can
	// filter to (the curated Ethiopian companies list); it does not affect
	// sort order, which is by recency only. Set only through
	// SetPriority, never through Upsert: discovery re-finding a company
	// by name must not be able to flip it.
	IsPriority bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// UpsertParams is the input to Store.Upsert. Website and Description are
// optional; an empty (or whitespace-only) value means "not provided" and
// is stored as NULL, never as an empty string, because an empty website
// would collide with every other website-less company under the unique
// index on normalize_website(website).
type UpsertParams struct {
	Name        string
	Website     string
	Description string
}

// ErrNotFound is returned (wrapped) by the Get* methods when no row
// matches. Callers test for it with errors.Is; pgx.ErrNoRows is never
// allowed to escape this package as the not-found signal, so callers
// don't have to import pgx to handle an empty lookup.
var ErrNotFound = errors.New("company: not found")

// Store is the repository for the companies table. It is a concrete
// type, not an interface: nothing else implements it, and inventing an
// interface here would only add indirection for a fake nobody needs —
// these methods are tested against a real PostgreSQL instance.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by pool. The pool is owned by the
// caller (internal/database) and is not closed here.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// companyColumns is the column list every query in this file selects, in
// the order scanCompany expects.
const companyColumns = `id, name, website, description, status, is_priority, created_at, updated_at`

// upsertCompanyQuery is a single atomic statement on purpose: a
// SELECT-then-INSERT would let two concurrent discovery workers both
// observe "no such company" and both insert, and the loser would get a
// unique-violation instead of the existing row. ON CONFLICT hands the
// race to PostgreSQL, which is the only participant that can settle it.
//
// The conflict target is the *expression* the unique index is built on
// (lower(btrim(name))), not the name column, because companies has no
// unique constraint on name itself — index inference only matches an
// expression index when the ON CONFLICT expression is written the same
// way.
//
// DO UPDATE fills gaps and never overwrites: COALESCE puts the existing
// row's value first, so a second discovery pass that reports a different
// website for a company we already have a website for leaves the stored
// one alone. NULLIF turns an empty parameter into a real NULL so that
// COALESCE sees "missing" rather than an empty string, which would
// otherwise be treated as a perfectly good value and win.
//
// Note this only targets the name index. A conflict on the *other*
// unique index (normalize_website(website), for a different company
// name) is not inferred here and surfaces as a raw 23505 from the
// driver, wrapped by Upsert. That is deliberate: two different names
// claiming the same website is a data-quality problem for the caller to
// resolve, not something this layer can silently merge.
const upsertCompanyQuery = `
	INSERT INTO companies (name, website, description)
	VALUES ($1, NULLIF($2, ''), NULLIF($3, ''))
	ON CONFLICT (lower(btrim(name))) DO UPDATE SET
		website     = COALESCE(companies.website, EXCLUDED.website),
		description = COALESCE(companies.description, EXCLUDED.description)
	RETURNING ` + companyColumns

// Upsert inserts a company or, if one already exists with the same name
// (case- and whitespace-insensitively), fills in any website or
// description the existing row is missing. It returns the full row
// either way, so a caller that just wants "the company for this name"
// gets it in one round trip without a second SELECT.
//
// Values are trimmed before they are stored: the unique index compares
// btrim(name), so storing the untrimmed form would mean the first
// spelling seen wins the display name and later lookups return a name
// with stray whitespace in it.
func (s *Store) Upsert(ctx context.Context, params UpsertParams) (*Company, error) {
	name := strings.TrimSpace(params.Name)
	if name == "" {
		return nil, errors.New("company: upsert: name is required")
	}

	c, err := scanCompany(s.pool.QueryRow(ctx, upsertCompanyQuery,
		name,
		strings.TrimSpace(params.Website),
		strings.TrimSpace(params.Description),
	))
	if err != nil {
		return nil, fmt.Errorf("company: upserting %q: %w", name, err)
	}
	return c, nil
}

const getCompanyByIDQuery = `SELECT ` + companyColumns + ` FROM companies WHERE id = $1`

// GetByID returns the company with the given id, or an error wrapping
// ErrNotFound if there is none.
func (s *Store) GetByID(ctx context.Context, id int64) (*Company, error) {
	c, err := scanCompany(s.pool.QueryRow(ctx, getCompanyByIDQuery, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("company: get by id %d: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("company: get by id %d: %w", id, err)
	}
	return c, nil
}

// getCompanyByNameQuery normalizes both sides with the same expression
// the unique index is built on, so the lookup is both case/whitespace
// insensitive and index-backed rather than a sequential scan.
const getCompanyByNameQuery = `SELECT ` + companyColumns + `
	FROM companies WHERE lower(btrim(name)) = lower(btrim($1))`

const setPriorityQuery = `UPDATE companies SET is_priority = $2 WHERE id = $1`

// SetPriority flips a company's priority flag. Idempotent: setting the
// value a row already has succeeds (RowsAffected counts matched rows,
// not changed ones). Returns an error wrapping ErrNotFound for an
// unknown id.
func (s *Store) SetPriority(ctx context.Context, id int64, priority bool) error {
	tag, err := s.pool.Exec(ctx, setPriorityQuery, id, priority)
	if err != nil {
		return fmt.Errorf("company: setting priority=%t on company %d: %w", priority, id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("company: setting priority=%t on company %d: %w", priority, id, ErrNotFound)
	}
	return nil
}

// GetByName returns the company whose name matches the given one
// ignoring case and surrounding whitespace — the same identity rule
// Upsert's ON CONFLICT uses — or an error wrapping ErrNotFound.
func (s *Store) GetByName(ctx context.Context, name string) (*Company, error) {
	c, err := scanCompany(s.pool.QueryRow(ctx, getCompanyByNameQuery, name))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("company: get by name %q: %w", name, ErrNotFound)
		}
		return nil, fmt.Errorf("company: get by name %q: %w", name, err)
	}
	return c, nil
}

// scanCompany reads one row in companyColumns order. The nullable text
// columns are scanned through pointers and flattened to "" so Company
// stays free of pointer fields; status is scanned as a plain string and
// converted rather than scanned straight into Status, which would rely
// on pgx's reflection fallback for named string types.
func scanCompany(row pgx.Row) (*Company, error) {
	var (
		c           Company
		website     *string
		description *string
		status      string
	)
	if err := row.Scan(&c.ID, &c.Name, &website, &description, &status, &c.IsPriority, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if website != nil {
		c.Website = *website
	}
	if description != nil {
		c.Description = *description
	}
	c.Status = Status(status)
	return &c, nil
}

const namesWithoutBoardQuery = `
	SELECT DISTINCT c.name
	FROM companies c
	JOIN target_companies t ON t.company_id = c.id AND t.ats_provider = ANY($1)
	WHERE NOT EXISTS (
		SELECT 1 FROM target_companies b WHERE b.company_id = c.id AND b.ats_provider = ANY($2)
	)
	ORDER BY c.name
	LIMIT $3`

// NamesWithoutBoard lists the names of companies that have a target from one
// of sourceProviders (a job board that names its employers) and none from any
// of boardProviders (an ATS that would list all of the company's jobs
// itself). Discovery uses it to look for the ATS board of every employer a
// job board has shown. limit bounds the result.
func (s *Store) NamesWithoutBoard(ctx context.Context, sourceProviders, boardProviders []string, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, namesWithoutBoardQuery, sourceProviders, boardProviders, limit)
	if err != nil {
		return nil, fmt.Errorf("company: listing names without a board: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("company: scanning a name: %w", err)
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
