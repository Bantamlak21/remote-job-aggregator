package company

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

// TargetCompany is one row of target_companies: a single ATS board
// belonging to a company. A company can have several (one per provider,
// or several boards on one provider), which is why this is its own table
// and not columns on companies.
type TargetCompany struct {
	ID              int64
	CompanyID       int64
	ATSProvider     string
	ExternalBoardID string
	// BoardURL is "" when the column is NULL, matching Company.Website.
	BoardURL string
	// DiscoveryMetadata is the decoded discovery_metadata JSONB column:
	// whatever the discovery pass wants to remember about how this board
	// was found. The column is NOT NULL DEFAULT '{}', so this is an empty
	// map, never nil, for a row that has never had metadata written.
	DiscoveryMetadata         map[string]any
	IsActive                  bool
	LastSuccessfulIngestionAt *time.Time
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	// Market is the list this board's jobs belong to (see internal/market).
	Market market.Market
}

// TargetUpsertParams is the input to TargetStore.Upsert.
//
// DiscoveryMetadata is tri-state on purpose: a nil map means "leave
// whatever is stored alone", while a non-nil map (including an empty
// one) replaces the stored value outright. Discovery passes that learn
// nothing new about a board should not have to re-send its metadata to
// avoid clobbering it.
type TargetUpsertParams struct {
	CompanyID         int64
	ATSProvider       string
	ExternalBoardID   string
	BoardURL          string
	DiscoveryMetadata map[string]any
	// Market is "" to leave the stored market alone (a new row gets the
	// database default, worldwide), or the market to set. Discovery never
	// sets it, so re-discovering a board cannot move it between lists.
	Market market.Market
}

// TargetStore is the repository for the target_companies table.
type TargetStore struct {
	pool *pgxpool.Pool
}

// NewTargetStore returns a TargetStore backed by pool. The pool is owned
// by the caller and is not closed here.
func NewTargetStore(pool *pgxpool.Pool) *TargetStore {
	return &TargetStore{pool: pool}
}

// targetColumns is the column list every query in this file selects, in
// the order scanTarget expects.
const targetColumns = `id, company_id, ats_provider, external_board_id, board_url,
	discovery_metadata, is_active, last_successful_ingestion_at, created_at, updated_at, market`

// upsertTargetQuery is atomic for the same reason upsertCompanyQuery is:
// two discovery workers can hit the same provider+board at once, and
// PostgreSQL is the only participant that can order them.
//
// The conflict target matches target_companies_provider_board_idx
// exactly — a plain column plus an expression — because index inference
// compares the whole target, not each element loosely.
//
// Three deliberate differences from the company upsert:
//
//   - is_active is forced back to true. Re-discovering a board means it
//     responded, and there is no operator UI that could have deactivated
//     it on purpose yet, so "discovery found it again" wins over a
//     previous deactivation (which today only comes from ingestion
//     failures).
//   - board_url takes EXCLUDED first, so a freshly discovered URL
//     replaces a stale one; a blank parameter becomes NULL via NULLIF and
//     the COALESCE then keeps what is already stored. This is the
//     opposite precedence from companies.website, where a second opinion
//     about an employer's site is not evidence the first one was wrong —
//     here the board URL comes from the same probe that just proved the
//     board exists.
//   - discovery_metadata is replaced wholesale from $5 rather than from
//     EXCLUDED.discovery_metadata: the VALUES clause has already turned a
//     nil parameter into '{}' to satisfy the NOT NULL column, so EXCLUDED
//     could no longer tell "no metadata supplied" from "empty metadata
//     supplied". $5 still can.
//
// The WHERE guard is the fix for a data-integrity gap found by
// adversarial review: without it, a caller that (through a bug
// upstream — a URL-parsing mistake, a search result mismatch — this
// project has already had one) submits the same (ats_provider,
// external_board_id) under a DIFFERENT company_id would silently
// reassign an existing board's row to that other company, corrupting
// the original company's data with no error. The WHERE guard makes
// Postgres skip the UPDATE entirely when company_id doesn't match the
// existing row; Upsert below detects that (verified directly against a
// real database: ON CONFLICT DO UPDATE ... WHERE, when the guard
// excludes the row, makes RETURNING produce zero rows, not the existing
// row) and returns ErrTargetCompanyMismatch instead of silently
// reporting success for a write that didn't happen.
const upsertTargetQuery = `
	INSERT INTO target_companies (company_id, ats_provider, external_board_id, board_url, discovery_metadata, market)
	VALUES ($1, $2, $3, NULLIF($4, ''), COALESCE($5::jsonb, '{}'::jsonb), COALESCE(NULLIF($6, ''), 'worldwide'))
	ON CONFLICT (ats_provider, lower(btrim(external_board_id))) DO UPDATE SET
		is_active          = true,
		board_url          = COALESCE(EXCLUDED.board_url, target_companies.board_url),
		discovery_metadata = COALESCE($5::jsonb, target_companies.discovery_metadata),
		market             = COALESCE(NULLIF($6, ''), target_companies.market)
	WHERE target_companies.company_id = EXCLUDED.company_id
	RETURNING ` + targetColumns

// Upsert registers an ATS board for a company, or updates the existing
// row for that provider+board id (case- and whitespace-insensitively),
// returning the full row either way.
//
// CompanyID is validated but not checked for existence: the foreign key
// does that, and a SELECT here would be both a wasted round trip and a
// TOCTOU window (the company could be deleted between the check and the
// insert).
func (s *TargetStore) Upsert(ctx context.Context, params TargetUpsertParams) (*TargetCompany, error) {
	if params.CompanyID <= 0 {
		return nil, fmt.Errorf("company: target upsert: company_id must be positive, got %d", params.CompanyID)
	}
	provider := strings.TrimSpace(params.ATSProvider)
	if provider == "" {
		return nil, errors.New("company: target upsert: ats_provider is required")
	}
	boardID := strings.TrimSpace(params.ExternalBoardID)
	if boardID == "" {
		return nil, errors.New("company: target upsert: external_board_id is required")
	}

	if params.Market != "" && !params.Market.Valid() {
		return nil, fmt.Errorf("company: target upsert: market must be %q or %q, got %q", market.Ethiopia, market.Worldwide, params.Market)
	}

	// nil stays nil so pgx sends SQL NULL for $5, which is what both
	// COALESCEs in the query test for. Marshaling a nil map instead would
	// send the four bytes "null", a perfectly valid jsonb value that is
	// not NULL and would therefore overwrite the stored metadata.
	var metadata []byte
	if params.DiscoveryMetadata != nil {
		b, err := json.Marshal(params.DiscoveryMetadata)
		if err != nil {
			return nil, fmt.Errorf("company: target upsert: encoding discovery metadata: %w", err)
		}
		metadata = b
	}

	t, err := scanTarget(s.pool.QueryRow(ctx, upsertTargetQuery,
		params.CompanyID,
		provider,
		boardID,
		strings.TrimSpace(params.BoardURL),
		metadata,
		string(params.Market),
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The WHERE guard blocked this: a row already exists for
			// (provider, boardID) under a different company_id. Re-fetch
			// purely to name which company it actually belongs to in the
			// error — this has no bearing on correctness, since Postgres
			// already decided atomically not to write; a stale read here
			// could only produce a slightly outdated error message, never
			// a wrong write.
			if existing, getErr := s.GetByProviderAndBoard(ctx, provider, boardID); getErr == nil {
				return nil, fmt.Errorf("company: upserting target %s/%s: already registered to company %d, refusing to reassign to company %d: %w",
					provider, boardID, existing.CompanyID, params.CompanyID, ErrTargetCompanyMismatch)
			}
			return nil, fmt.Errorf("company: upserting target %s/%s: %w", provider, boardID, ErrTargetCompanyMismatch)
		}
		return nil, fmt.Errorf("company: upserting target %s/%s: %w", provider, boardID, err)
	}
	return t, nil
}

// ErrTargetCompanyMismatch is returned by Upsert when (ats_provider,
// external_board_id) already belongs to a different company than the
// one in params — Upsert never reassigns an existing board to a new
// company silently.
var ErrTargetCompanyMismatch = errors.New("company: target already registered to a different company")

const getTargetByIDQuery = `SELECT ` + targetColumns + ` FROM target_companies WHERE id = $1`

// GetByID returns the target company with the given id, or an error
// wrapping ErrNotFound if there is none.
func (s *TargetStore) GetByID(ctx context.Context, id int64) (*TargetCompany, error) {
	t, err := scanTarget(s.pool.QueryRow(ctx, getTargetByIDQuery, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("company: get target by id %d: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("company: get target by id %d: %w", id, err)
	}
	return t, nil
}

const getTargetByProviderAndBoardQuery = `SELECT ` + targetColumns + `
	FROM target_companies WHERE ats_provider = $1 AND lower(btrim(external_board_id)) = lower(btrim($2))`

// GetByProviderAndBoard returns the target company for the given
// provider+board id (case- and whitespace-insensitively, matching
// Upsert's own identity rule), or an error wrapping ErrNotFound.
func (s *TargetStore) GetByProviderAndBoard(ctx context.Context, provider, boardID string) (*TargetCompany, error) {
	t, err := scanTarget(s.pool.QueryRow(ctx, getTargetByProviderAndBoardQuery, provider, boardID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("company: get target %s/%s: %w", provider, boardID, ErrNotFound)
		}
		return nil, fmt.Errorf("company: get target %s/%s: %w", provider, boardID, err)
	}
	return t, nil
}

const listActiveTargetsQuery = `SELECT ` + targetColumns + `
	FROM target_companies WHERE is_active = true ORDER BY id`

// ListActive returns every target company ingestion should fetch —
// is_active = true, ordered by id for a stable, testable sequence.
// Ordering by last_successful_ingestion_at (staler targets first) would
// be a reasonable refinement once there are enough active targets for
// fetch order to matter; not needed yet.
func (s *TargetStore) ListActive(ctx context.Context) ([]TargetCompany, error) {
	rows, err := s.pool.Query(ctx, listActiveTargetsQuery)
	if err != nil {
		return nil, fmt.Errorf("company: list active targets: %w", err)
	}
	defer rows.Close()

	var targets []TargetCompany
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("company: list active targets: %w", err)
		}
		targets = append(targets, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("company: list active targets: %w", err)
	}
	return targets, nil
}

const markIngestionSucceededQuery = `
	UPDATE target_companies SET last_successful_ingestion_at = now() WHERE id = $1`

// MarkIngestionSucceeded records that ingestion just fetched this
// target's board successfully — independent of whether any individual
// job's content actually changed; "succeeded" means the fetch itself
// worked, not that new data resulted.
func (s *TargetStore) MarkIngestionSucceeded(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, markIngestionSucceededQuery, id)
	if err != nil {
		return fmt.Errorf("company: marking ingestion succeeded for target %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("company: marking ingestion succeeded for target %d: %w", id, ErrNotFound)
	}
	return nil
}

const setTargetActiveQuery = `UPDATE target_companies SET is_active = $2 WHERE id = $1`

// SetActive flips a target's is_active flag. Ingestion uses this to
// deactivate a target when its board returns a definitive "gone" signal
// (e.g. a 404 board-not-found from the ATS) — a clear, permanent signal,
// unlike a transient network/5xx error, which leaves is_active alone so
// a temporary outage doesn't silently stop future ingestion attempts.
func (s *TargetStore) SetActive(ctx context.Context, id int64, active bool) error {
	tag, err := s.pool.Exec(ctx, setTargetActiveQuery, id, active)
	if err != nil {
		return fmt.Errorf("company: setting target %d active=%t: %w", id, active, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("company: setting target %d active=%t: %w", id, active, ErrNotFound)
	}
	return nil
}

// scanTarget reads one row in targetColumns order. discovery_metadata is
// scanned as raw bytes and decoded here rather than letting pgx decode
// straight into the map, so a malformed value reports which column it
// came from instead of surfacing as a bare json error.
func scanTarget(row pgx.Row) (*TargetCompany, error) {
	var (
		t        TargetCompany
		boardURL *string
		metadata []byte
	)
	err := row.Scan(
		&t.ID,
		&t.CompanyID,
		&t.ATSProvider,
		&t.ExternalBoardID,
		&boardURL,
		&metadata,
		&t.IsActive,
		&t.LastSuccessfulIngestionAt,
		&t.CreatedAt,
		&t.UpdatedAt,
		&t.Market,
	)
	if err != nil {
		return nil, err
	}
	if boardURL != nil {
		t.BoardURL = *boardURL
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &t.DiscoveryMetadata); err != nil {
			return nil, fmt.Errorf("decoding discovery_metadata: %w", err)
		}
	}
	return &t, nil
}

// NamedTarget is a target with its company's name.
type NamedTarget struct {
	TargetCompany
	CompanyName string
}

const targetsByCompanyNamesQuery = `SELECT t.id, t.company_id, t.ats_provider, t.external_board_id, t.is_active, c.name
	FROM target_companies t JOIN companies c ON c.id = t.company_id
	WHERE lower(c.name) = ANY($1) AND t.ats_provider = ANY($2)
	ORDER BY c.name, t.ats_provider, t.external_board_id`

// ListByCompanyNames returns the targets of the named companies (matched
// case-insensitively) whose provider is one of providers. Discovery uses it to
// re-check boards it registered earlier.
func (s *TargetStore) ListByCompanyNames(ctx context.Context, names, providers []string) ([]NamedTarget, error) {
	lower := make([]string, len(names))
	for i, n := range names {
		lower[i] = strings.ToLower(strings.TrimSpace(n))
	}
	rows, err := s.pool.Query(ctx, targetsByCompanyNamesQuery, lower, providers)
	if err != nil {
		return nil, fmt.Errorf("company: listing targets by company names: %w", err)
	}
	defer rows.Close()
	var out []NamedTarget
	for rows.Next() {
		var t NamedTarget
		if err := rows.Scan(&t.ID, &t.CompanyID, &t.ATSProvider, &t.ExternalBoardID, &t.IsActive, &t.CompanyName); err != nil {
			return nil, fmt.Errorf("company: scanning a target: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
