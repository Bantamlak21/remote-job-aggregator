package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record is the full persistence-level shape of one ingested job —
// richer than Job (the public API's read model): it carries the
// identity/lifecycle fields ingestion needs (company/target linkage,
// ATS source+id, the raw location string) that the public API has no
// business exposing. Store.UpsertFromATS takes this; Store.List/Get
// (satisfying Repository) return the narrower Job.
type Record struct {
	CompanyID       int64
	TargetCompanyID int64
	Source          string // ats_provider, e.g. "greenhouse" — matches target_companies.ats_provider
	SourceJobID     string
	CanonicalURL    string
	Title           string
	Description     string
	ApplicationURL  string
	LocationRaw     string
	PublishedAt     time.Time // zero means unknown
}

// UpsertOutcome reports what UpsertFromATS actually did, for ingestion's
// own per-run counters/logging — never used for control flow, since
// the row is written either way.
type UpsertOutcome struct {
	Inserted bool // true: this source_job_id has never been seen before
	Changed  bool // true: content_hash differs from what was already stored (always true when Inserted)
}

// ErrJobTargetMismatch is returned by UpsertFromATS when a job's
// (source, source_job_id) already exists under a *different*
// target_company_id/company_id than the one this call supplied. Mirrors
// company.ErrTargetCompanyMismatch's reasoning exactly: an ATS provider
// is trusted to hand out globally unique ids (Greenhouse's numeric id
// is documented as one), but if that assumption is ever wrong for some
// future provider, this refuses the silent reassignment instead of
// corrupting which company a job is attributed to.
var ErrJobTargetMismatch = errors.New("job: already registered to a different target company")

// Store is the repository for the jobs table: both the public,
// read-only Repository interface (List/Get) and the write path
// ingestion needs (UpsertFromATS, MarkMissingAsRemoved). Concrete, not
// an interface, matching internal/company's Store/TargetStore — nothing
// else implements this, and every method is tested against a real
// PostgreSQL instance.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by pool. The pool is owned by the
// caller (internal/database) and is not closed here.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// contentHash is a deterministic hash over the fields that count as
// "this job's content changed" — title, description, application URL,
// canonical URL, and location. UpsertFromATS only advances
// last_changed_at (and the content columns) when this differs from what
// is already stored; last_seen_at advances on every successful upsert
// regardless, which is how "still on the board, unchanged" is
// distinguished from "content changed" without a second round trip.
func contentHash(r Record) string {
	h := sha256.New()
	for _, field := range []string{r.Title, r.Description, r.ApplicationURL, r.CanonicalURL, r.LocationRaw} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// upsertJobQuery captures the previous content_hash (if any) in a CTE
// before the INSERT runs, in the same statement — not a separate round
// trip, so there is no race window between "check" and "write" the way
// there would be if this were two queries. jobs.<col> on the right-hand
// side of a SET assignment refers to the row's value *before* this
// statement's update (Postgres evaluates every SET expression against
// the pre-update row in one pass), so last_changed_at's CASE correctly
// compares old vs. new content_hash.
//
// status/closed_at are unconditionally reset to open/NULL on every
// upsert: if a job is being upserted at all, it was just seen on the
// board, so it cannot still be removed/closed, regardless of whether
// its content also happened to change.
//
// The WHERE guard mirrors target_companies' own fix for the identical
// class of bug (see ErrJobTargetMismatch): verified against this same
// database that RETURNING produces zero rows, not the existing row's
// values, when the guard blocks the update — Upsert detects that via
// pgx.ErrNoRows, exactly like TargetStore.Upsert does.
const upsertJobQuery = `
	WITH previous AS (
		SELECT content_hash, status FROM jobs WHERE source = $3 AND source_job_id = $4
	)
	INSERT INTO jobs (
		company_id, target_company_id, source, source_job_id, canonical_url,
		title, description, application_url, location_raw, published_at, content_hash
	) VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, NULLIF($9, ''), $10, $11)
	ON CONFLICT (source, source_job_id) DO UPDATE SET
		title              = EXCLUDED.title,
		description        = EXCLUDED.description,
		application_url    = EXCLUDED.application_url,
		location_raw       = EXCLUDED.location_raw,
		canonical_url      = EXCLUDED.canonical_url,
		published_at       = COALESCE(EXCLUDED.published_at, jobs.published_at),
		content_hash       = EXCLUDED.content_hash,
		last_seen_at       = now(),
		last_changed_at    = CASE WHEN jobs.content_hash IS DISTINCT FROM EXCLUDED.content_hash OR jobs.status <> 'open'
		                          THEN now() ELSE jobs.last_changed_at END,
		status             = 'open',
		closed_at          = NULL
	WHERE jobs.target_company_id = EXCLUDED.target_company_id AND jobs.company_id = EXCLUDED.company_id
	RETURNING id, (xmax = 0) AS inserted,
		(SELECT content_hash FROM previous) AS previous_content_hash,
		(SELECT status FROM previous) AS previous_status`

// UpsertFromATS inserts a job or updates the existing row for its
// (source, source_job_id) identity, returning what actually happened.
// The row's company_id/target_company_id are trusted as supplied — the
// composite foreign keys jobs_target_company_fk/jobs_target_source_fk
// enforce that they actually match a real target_companies row, so a
// caller passing a target_company_id that does not belong to
// company_id, or whose ats_provider does not equal Source, gets a
// foreign-key violation rather than a silently-accepted mismatch.
func (s *Store) UpsertFromATS(ctx context.Context, r Record) (UpsertOutcome, error) {
	source := strings.TrimSpace(r.Source)
	sourceJobID := strings.TrimSpace(r.SourceJobID)
	if source == "" || sourceJobID == "" {
		return UpsertOutcome{}, fmt.Errorf("job: upsert: source and source_job_id are required")
	}

	newHash := contentHash(r)
	var publishedAt *time.Time
	if !r.PublishedAt.IsZero() {
		t := r.PublishedAt
		publishedAt = &t
	}

	var (
		id                  int64
		inserted            bool
		previousContentHash *string
		previousStatus      *string
	)
	err := s.pool.QueryRow(ctx, upsertJobQuery,
		r.CompanyID, r.TargetCompanyID, source, sourceJobID, strings.TrimSpace(r.CanonicalURL),
		r.Title, r.Description, r.ApplicationURL, strings.TrimSpace(r.LocationRaw), publishedAt, newHash,
	).Scan(&id, &inserted, &previousContentHash, &previousStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UpsertOutcome{}, fmt.Errorf("job: upserting %s/%s: %w", source, sourceJobID, ErrJobTargetMismatch)
		}
		return UpsertOutcome{}, fmt.Errorf("job: upserting %s/%s: %w", source, sourceJobID, err)
	}

	// A job that was removed/closed and is now back counts as changed
	// even when its content is byte-identical: its visible state changed.
	reopened := previousStatus != nil && *previousStatus != "open"
	changed := inserted || reopened || previousContentHash == nil || *previousContentHash != newHash
	return UpsertOutcome{Inserted: inserted, Changed: changed}, nil
}

// IsRejectedRecord reports whether err from UpsertFromATS means this one
// job's data was refused (a Postgres data-exception or
// integrity-constraint violation — SQLSTATE classes 22 and 23 — or the
// cross-target guard), as opposed to an infrastructure failure
// (connection loss, cancellation) that should abort the whole run.
// Ingestion skips and logs the former per job: one bad job must never
// freeze its entire board's ingestion forever.
func IsRejectedRecord(err error) bool {
	if errors.Is(err, ErrJobTargetMismatch) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 23503 (foreign-key violation) is deliberately NOT a per-job
		// rejection: it means the target/company linkage itself is wrong
		// (e.g. the target row was deleted mid-run), which affects every
		// job on the board. Skipping each one would report a "successful"
		// run that stored nothing; aborting surfaces it.
		if pgErr.Code == "23503" {
			return false
		}
		return strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23")
	}
	return false
}

// maxPageIndex bounds (page-1)*pageSize well inside int64/Postgres
// OFFSET range; pages beyond it are treated as past the end.
const maxPageIndex = 1 << 40

// likeEscaper escapes the LIKE/ILIKE metacharacters so a user's search
// text is matched literally as a substring (Postgres's default LIKE
// escape character is the backslash).
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// markMissingAsRemovedQuery marks every job of this target that is
// currently 'open' but was not seen in the latest fetch as 'removed'.
// A single atomic statement, not an application-level loop over
// individual UPDATEs: Postgres decides which rows qualify from the
// array parameter directly, so this scales with one round trip
// regardless of how many jobs a board has.
const markMissingAsRemovedQuery = `
	UPDATE jobs SET status = 'removed', closed_at = now()
	WHERE target_company_id = $1 AND status = 'open' AND source_job_id <> ALL($2::text[])`

// MarkMissingAsRemoved closes out every job of targetCompanyID that is
// currently open but absent from seenSourceJobIDs — the ids UpsertFromATS
// was just called with for this same fetch. Ingestion must only call
// this after a *successful* ListJobs call: an empty seenSourceJobIDs
// from a genuinely empty board (0 jobs posted) is a legitimate reason to
// remove everything, but an empty slice from a *failed* fetch would
// wrongly do the same — this method has no way to tell those two apart
// on its own, so that distinction is the caller's responsibility.
func (s *Store) MarkMissingAsRemoved(ctx context.Context, targetCompanyID int64, seenSourceJobIDs []string) (int, error) {
	// pgx encodes a nil []string as SQL NULL, never as an empty array —
	// and "x <> ALL(NULL)" evaluates to NULL (never TRUE) for every row,
	// which would silently remove nothing instead of "everything" for a
	// genuinely empty board. A real, non-nil empty slice encodes as '{}',
	// against which "<> ALL('{}')" is correctly TRUE for every row (found
	// by this package's own test, not assumed).
	if seenSourceJobIDs == nil {
		seenSourceJobIDs = []string{}
	}
	tag, err := s.pool.Exec(ctx, markMissingAsRemovedQuery, targetCompanyID, seenSourceJobIDs)
	if err != nil {
		return 0, fmt.Errorf("job: marking missing jobs removed for target %d: %w", targetCompanyID, err)
	}
	return int(tag.RowsAffected()), nil
}

// closeStaleQuery closes a target's open jobs that no fetch has seen for
// longer than $2 seconds. The cutoff is computed by Postgres from its own
// clock (last_seen_at is also written by now()), so app/db clock skew can
// never close a job that was just seen.
const closeStaleQuery = `
	UPDATE jobs SET status = 'removed', closed_at = now()
	WHERE target_company_id = $1 AND status = 'open'
	  AND last_seen_at < now() - make_interval(secs => $2)`

// CloseStale closes out targetCompanyID's open jobs whose last_seen_at is
// older than olderThan. It is MarkMissingAsRemoved's counterpart for
// sources whose listing is not exhaustive (a search result page shows a
// sample of a company's jobs, not all of them): absence from one fetch
// proves nothing there, so a job is only closed once it has gone unseen
// for a whole window of consecutive runs.
func (s *Store) CloseStale(ctx context.Context, targetCompanyID int64, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, fmt.Errorf("job: closing stale jobs for target %d: olderThan must be positive, got %s", targetCompanyID, olderThan)
	}
	tag, err := s.pool.Exec(ctx, closeStaleQuery, targetCompanyID, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("job: closing stale jobs for target %d: %w", targetCompanyID, err)
	}
	return int(tag.RowsAffected()), nil
}

const closeBySourceIDQuery = `
	UPDATE jobs SET status = 'removed', closed_at = now()
	WHERE target_company_id = $1 AND status = 'open' AND source_job_id = ANY($2::text[])`

// CloseBySourceID closes targetCompanyID's open jobs with the given source
// ids, for sources that positively report a job as ended (an Ethiojobs
// posting past its expiry). Ids with no open row are ignored.
func (s *Store) CloseBySourceID(ctx context.Context, targetCompanyID int64, sourceJobIDs []string) (int, error) {
	if len(sourceJobIDs) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, closeBySourceIDQuery, targetCompanyID, sourceJobIDs)
	if err != nil {
		return 0, fmt.Errorf("job: closing ended jobs for target %d: %w", targetCompanyID, err)
	}
	return int(tag.RowsAffected()), nil
}

// jobColumnsForAPI is the column list List/Get select, in the order
// scanJobSummary expects. Only 'open' jobs are ever exposed through the
// public Repository interface — a removed/closed job is not something
// a job-seeker should be shown, even if it is still in the table for
// ingestion's own history/audit purposes.
const jobColumnsForAPI = `j.id, j.title, c.name, c.is_priority, j.remote_type, j.employment_type,
	j.location_raw, COALESCE(j.published_at, j.first_seen_at) AS posted_at, j.application_url`

// List returns open jobs matching filter, newest-first (ties broken by
// id descending), paginated. Tags is always an empty slice: the jobs
// table has no tags column, so nothing ingested through Phase 3 has
// any — a real, documented limitation (see docs/api.md), not a bug.
// CompanyLogoURL is always empty/null for the same reason: no logo_url
// column exists anywhere in this schema.
func (s *Store) List(ctx context.Context, filter Filter) (ListResult, error) {
	// No tags column exists yet, so no job can ever match a tag filter —
	// short-circuit rather than build a WHERE clause that could only ever
	// produce zero rows.
	if filter.Tag != "" {
		return ListResult{}, nil
	}

	page := filter.Page
	if page <= 0 {
		page = DefaultPage
	}
	pageSize := filter.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}

	const fromClause = ` FROM jobs j JOIN companies c ON c.id = j.company_id`
	where := ` WHERE j.status = 'open'`
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if filter.RemoteType != "" {
		where += " AND j.remote_type = " + arg(string(filter.RemoteType))
	}
	if filter.EmploymentType != "" {
		where += " AND j.employment_type = " + arg(string(filter.EmploymentType))
	}
	if filter.Company != "" {
		where += " AND lower(c.name) = lower(" + arg(filter.Company) + ")"
	}
	if filter.PriorityOnly {
		where += " AND c.is_priority"
	}
	if q := strings.TrimSpace(filter.Query); q != "" {
		// Escaped so "%" and "_" in the user's text match literally,
		// not as wildcards (docs/api.md promises a substring match).
		likeArg := arg("%" + likeEscaper.Replace(q) + "%")
		where += " AND (j.title ILIKE " + likeArg + " OR c.name ILIKE " + likeArg + ")"
	}
	filterArgs := len(args)

	// (page-1)*pageSize can overflow int for an absurd page number, and
	// Postgres rejects a negative OFFSET with a 500 from a public
	// endpoint. No real result set has anywhere near this many pages, so
	// such a page is simply "past the end": answer with the real total
	// and no jobs, exactly as any other out-of-range page.
	if page-1 > maxPageIndex/pageSize {
		total := 0
		if err := s.pool.QueryRow(ctx, `SELECT count(*)`+fromClause+where, args...).Scan(&total); err != nil {
			return ListResult{}, fmt.Errorf("job: list: counting matches: %w", err)
		}
		return ListResult{Total: total}, nil
	}

	// Priority companies' jobs are pinned above everything else, so the
	// flag is the first sort key; newest-first only orders within each
	// group. posted_at is the COALESCE(...) column alias from
	// jobColumnsForAPI; referencing it by alias in ORDER BY (rather than
	// repeating the COALESCE expression) is standard Postgres.
	query := `SELECT ` + jobColumnsForAPI + `, count(*) OVER() AS total` + fromClause + where +
		" ORDER BY c.is_priority DESC, posted_at DESC, j.id DESC" +
		" LIMIT " + arg(pageSize) + " OFFSET " + arg((page-1)*pageSize)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return ListResult{}, fmt.Errorf("job: list: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	total := 0
	for rows.Next() {
		j, t, err := scanJobSummaryWithTotal(rows)
		if err != nil {
			return ListResult{}, fmt.Errorf("job: list: %w", err)
		}
		jobs = append(jobs, j)
		total = t
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, fmt.Errorf("job: list: %w", err)
	}

	// count(*) OVER() is computed per returned row, so a page past the
	// last one returns no rows and therefore no total. Total must still
	// be the real match count (the API reports it and the frontend
	// self-corrects an out-of-range page from it), so ask directly.
	if len(jobs) == 0 && page > 1 {
		if err := s.pool.QueryRow(ctx, `SELECT count(*)`+fromClause+where, args[:filterArgs]...).Scan(&total); err != nil {
			return ListResult{}, fmt.Errorf("job: list: counting matches: %w", err)
		}
	}

	return ListResult{Jobs: jobs, Total: total}, nil
}

const getJobByIDQuery = `SELECT ` + jobColumnsForAPI + `, j.description
	FROM jobs j JOIN companies c ON c.id = j.company_id
	WHERE j.status = 'open' AND j.id = $1`

// Get returns the open job with the given id, or ErrNotFound — both for
// a genuinely missing row and for a non-numeric id (this Store's ids
// are always the jobs table's bigint primary key formatted as a
// string; anything that doesn't parse as one cannot possibly match a
// real row, so there is nothing to distinguish by returning a
// different error).
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	numericID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return Job{}, fmt.Errorf("job: get %q: %w", id, ErrNotFound)
	}

	var (
		j              Job
		dbID           int64
		remoteType     string
		employmentType string
		locationRaw    *string
		description    *string
	)
	err = s.pool.QueryRow(ctx, getJobByIDQuery, numericID).Scan(
		&dbID, &j.Title, &j.CompanyName, &j.IsPriority, &remoteType, &employmentType,
		&locationRaw, &j.PostedAt, &j.ApplicationURL, &description,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Job{}, fmt.Errorf("job: get %q: %w", id, ErrNotFound)
		}
		return Job{}, fmt.Errorf("job: get %q: %w", id, err)
	}
	j.ID = strconv.FormatInt(dbID, 10)

	j.RemoteType = RemoteType(remoteType)
	j.EmploymentType = EmploymentType(employmentType)
	if locationRaw != nil {
		j.RegionNote = *locationRaw
	}
	if description != nil {
		j.Description = *description
	}
	j.Tags = []string{}
	return j, nil
}

// scanJobSummaryWithTotal reads one row in jobColumnsForAPI order plus
// the trailing count(*) OVER() column List's query adds.
func scanJobSummaryWithTotal(row pgx.Row) (Job, int, error) {
	var (
		j              Job
		dbID           int64
		remoteType     string
		employmentType string
		locationRaw    *string
		total          int
	)
	err := row.Scan(
		&dbID, &j.Title, &j.CompanyName, &j.IsPriority, &remoteType, &employmentType,
		&locationRaw, &j.PostedAt, &j.ApplicationURL, &total,
	)
	if err != nil {
		return Job{}, 0, err
	}
	j.ID = strconv.FormatInt(dbID, 10)
	j.RemoteType = RemoteType(remoteType)
	j.EmploymentType = EmploymentType(employmentType)
	if locationRaw != nil {
		j.RegionNote = *locationRaw
	}
	j.Tags = []string{}
	return j, total, nil
}
