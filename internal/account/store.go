package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrInvalidToken is returned for a login link that is unknown, already used or expired.
	// The three are deliberately not told apart.
	ErrInvalidToken = errors.New("account: invalid or expired login token")
	// ErrNoSession is returned for a session token that is unknown or expired.
	ErrNoSession = errors.New("account: no such session")
	// ErrJobNotFound is returned when a job to save does not exist or is no longer publicly listed.
	ErrJobNotFound = errors.New("account: job not found")
	// ErrBookmarkLimit is returned when saving would exceed the per-account limit.
	ErrBookmarkLimit = errors.New("account: bookmark limit reached")
	// ErrNoUser is returned when the account no longer exists.
	ErrNoUser = errors.New("account: no such user")
)

// User is an account.
type User struct {
	ID        int64
	Email     string
	CreatedAt time.Time
	// SessionExpires is when the session the user authenticated with ends, and
	// SessionRenewed says this call just extended it: the cookie should be sent again
	// with a fresh Max-Age, or the browser drops it at the old time however often
	// the person visits. Both are zero for a user not read from a session.
	SessionExpires time.Time
	SessionRenewed bool
}

// Bookmark is one saved job.
type Bookmark struct {
	JobID   int64
	SavedAt time.Time
}

// LinkLimits are the per-address and site-wide limits on emailed links that the
// database enforces, atomically, so they hold across restarts and across servers.
type LinkLimits struct {
	PerAddress15Min, PerAddressDay, GlobalHour int
}

// Store is the persistence for accounts. Every method that depends on the time
// takes it from the store's clock, which tests replace.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewStore returns a Store on the pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, now: time.Now} }

// WithClock returns a copy of the store that reads the time from now.
func (s *Store) WithClock(now func() time.Time) *Store {
	c := *s
	c.now = now
	return &c
}

// lock takes a transaction-scoped advisory lock named by prefix and key: everything
// that must not interleave for one address (spending its links, opening and revoking
// its sessions, counting its requests) takes the lock for that address first, so
// there is one order and no deadlock. hashtextextended spreads the name over 64 bits;
// a collision only makes two names wait for each other.
func lock(ctx context.Context, tx pgx.Tx, prefix, key string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, prefix+key); err != nil {
		return fmt.Errorf("account: taking a lock: %w", err)
	}
	return nil
}

// CreateLoginToken records an emailed link's digest, valid for ttl, without checking
// any limit (see CreateLoginTokenLimited).
func (s *Store) CreateLoginToken(ctx context.Context, email string, hash []byte, ttl time.Duration) error {
	now := s.now()
	if _, err := s.pool.Exec(ctx, `INSERT INTO login_tokens (token_hash, email, throttle_key, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hash, email, throttleKey(email), now, now.Add(ttl)); err != nil {
		return fmt.Errorf("account: storing a login token: %w", err)
	}
	return nil
}

// CreateLoginTokenLimited records a link only if the address (with its aliases folded)
// and the site are within their limits, as one atomic step: the count and the insert
// happen under a lock per address and a lock for the site, so however many requests
// race, across however many servers, the limits are exact. When a limit is reached it
// returns a *RateLimitedError and stores nothing.
func (s *Store) CreateLoginTokenLimited(ctx context.Context, email string, hash []byte, ttl time.Duration, lim LinkLimits) error {
	now := s.now()
	key := throttleKey(email)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lock(ctx, tx, "throttle:", key); err != nil {
		return err
	}
	for _, w := range []struct {
		window time.Duration
		max    int
	}{{15 * time.Minute, lim.PerAddress15Min}, {24 * time.Hour, lim.PerAddressDay}} {
		wait, limited, err := s.windowFull(ctx, tx, `throttle_key = $1`, key, now, w.window, w.max)
		if err != nil {
			return err
		}
		if limited {
			return &RateLimitedError{RetryAfter: wait}
		}
	}
	if err := lock(ctx, tx, "site:", "links"); err != nil {
		return err
	}
	wait, limited, err := s.windowFull(ctx, tx, `TRUE`, "", now, time.Hour, lim.GlobalHour)
	if err != nil {
		return err
	}
	if limited {
		return &RateLimitedError{RetryAfter: wait, Global: true}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO login_tokens (token_hash, email, throttle_key, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hash, email, key, now, now.Add(ttl)); err != nil {
		return fmt.Errorf("account: storing a login token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("account: commit: %w", err)
	}
	return nil
}

// windowFull reports whether max links were already created in the window, and if so
// how long until the one that filled the last slot leaves it. cond is a fixed SQL
// condition (never request data); arg is its only parameter, or "" for none.
func (s *Store) windowFull(ctx context.Context, tx pgx.Tx, cond, arg string, now time.Time, window time.Duration, max int) (time.Duration, bool, error) {
	since := now.Add(-window)
	var n int
	var err error
	if arg == "" {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM login_tokens WHERE `+cond+` AND created_at >= $1`, since).Scan(&n)
	} else {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM login_tokens WHERE `+cond+` AND created_at >= $2`, arg, since).Scan(&n)
	}
	if err != nil {
		return 0, false, fmt.Errorf("account: counting login tokens: %w", err)
	}
	if n < max {
		return 0, false, nil
	}
	// The max-th newest link in the window: when it leaves, a slot is free.
	var edge time.Time
	if arg == "" {
		err = tx.QueryRow(ctx, `SELECT created_at FROM login_tokens WHERE `+cond+` AND created_at >= $1 ORDER BY created_at DESC OFFSET $2 LIMIT 1`, since, max-1).Scan(&edge)
	} else {
		err = tx.QueryRow(ctx, `SELECT created_at FROM login_tokens WHERE `+cond+` AND created_at >= $2 ORDER BY created_at DESC OFFSET $3 LIMIT 1`, arg, since, max-1).Scan(&edge)
	}
	if err != nil {
		return 0, false, fmt.Errorf("account: finding the oldest counted link: %w", err)
	}
	wait := edge.Add(window).Sub(now)
	if wait < time.Second {
		wait = time.Second
	}
	return wait, true, nil
}

// CountLoginTokensSince counts the links requested for an address (aliases folded)
// since a time.
func (s *Store) CountLoginTokensSince(ctx context.Context, email string, since time.Time) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM login_tokens WHERE throttle_key = $1 AND created_at >= $2`, throttleKey(email), since).Scan(&n); err != nil {
		return 0, fmt.Errorf("account: counting login tokens: %w", err)
	}
	return n, nil
}

// PeekLoginToken returns the address a live link is for without spending it.
func (s *Store) PeekLoginToken(ctx context.Context, hash []byte) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx, `SELECT email FROM login_tokens WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2`, hash, s.now()).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidToken
	}
	if err != nil {
		return "", fmt.Errorf("account: reading a login token: %w", err)
	}
	return email, nil
}

// Login spends a login link and opens a session, in one transaction: the link is
// single-use (the UPDATE succeeds for exactly one caller, however many race), the
// account is created on first sign-in, and if opening the session fails the link
// is not spent.
//
// The transaction runs under the address's lock, taken before the link is spent, the
// same lock "sign out everywhere" and account deletion take: so they run entirely
// before or entirely after a sign-in for that address, and a link cancelled by sign
// out everywhere can never still open a session. previous, if not empty, is the
// session the caller already holds: it is ended in the same transaction (a fresh
// sign-in replaces the old session rather than adding to it). maxSessions, if
// positive, caps the live sessions of the account: the oldest beyond it are ended.
func (s *Store) Login(ctx context.Context, tokenHash, sessionHash []byte, sessionTTL time.Duration, previous []byte, maxSessions int) (User, error) {
	now := s.now()
	// Which address the link is for is needed first, to take its lock. A link that
	// does not exist stops here; the UPDATE below is still the only thing that spends one.
	if _, err := s.PeekLoginToken(ctx, tokenHash); err != nil {
		return User{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var email string
	if err := tx.QueryRow(ctx, `SELECT email FROM login_tokens WHERE token_hash = $1`, tokenHash).Scan(&email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrInvalidToken
		}
		return User{}, fmt.Errorf("account: reading a login token: %w", err)
	}
	if err := lock(ctx, tx, "login:", email); err != nil {
		return User{}, err
	}
	err = tx.QueryRow(ctx, `UPDATE login_tokens SET used_at = $2 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 RETURNING email`,
		tokenHash, now).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidToken
	}
	if err != nil {
		return User{}, fmt.Errorf("account: spending a login token: %w", err)
	}

	var u User
	err = tx.QueryRow(ctx, `INSERT INTO users (email, created_at, last_login_at) VALUES ($1, $2, $2)
		ON CONFLICT (email) DO UPDATE SET last_login_at = EXCLUDED.last_login_at
		RETURNING id, email, created_at`, email, now).Scan(&u.ID, &u.Email, &u.CreatedAt)
	if err != nil {
		return User{}, fmt.Errorf("account: upserting the user: %w", err)
	}
	if len(previous) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, previous); err != nil {
			return User{}, fmt.Errorf("account: ending the previous session: %w", err)
		}
	}
	expires := now.Add(sessionTTL)
	if _, err := tx.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at) VALUES ($1, $2, $3, $4, $3)`,
		sessionHash, u.ID, now, expires); err != nil {
		return User{}, fmt.Errorf("account: opening a session: %w", err)
	}
	if maxSessions > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND token_hash NOT IN
			(SELECT token_hash FROM sessions WHERE user_id = $1 ORDER BY created_at DESC, token_hash LIMIT $2)`, u.ID, maxSessions); err != nil {
			return User{}, fmt.Errorf("account: capping sessions: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("account: commit: %w", err)
	}
	u.SessionExpires = expires
	return u, nil
}

// renewAfter is how much extension makes a session worth a write: a session used
// more often than this is not written on every request.
const renewAfter = time.Hour

// Authenticate returns the user of a live session and slides its idle expiry: a
// session is extended to a full ttl from now, but never past maxAge from when it was
// opened (the absolute limit), and only when that would add at least renewAfter (so
// a busy session is not written on every request). The idle window is therefore ttl
// from the last use, to within renewAfter, and a session used regularly lives to
// maxAge. The returned user says when the session now ends and whether it was just
// extended.
func (s *Store) Authenticate(ctx context.Context, hash []byte, ttl, maxAge time.Duration) (User, error) {
	now := s.now()
	var (
		u       User
		expires time.Time
		created time.Time
	)
	err := s.pool.QueryRow(ctx, `SELECT u.id, u.email, u.created_at, s.expires_at, s.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > $2`, hash, now).Scan(&u.ID, &u.Email, &u.CreatedAt, &expires, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, fmt.Errorf("account: reading a session: %w", err)
	}
	next := now.Add(ttl)
	if limit := created.Add(maxAge); next.After(limit) {
		next = limit
	}
	u.SessionExpires = expires
	if next.Sub(expires) >= renewAfter {
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET expires_at = $2, last_seen_at = $3 WHERE token_hash = $1`, hash, next, now); err != nil {
			return User{}, fmt.Errorf("account: extending a session: %w", err)
		}
		u.SessionExpires = next
		u.SessionRenewed = true
	}
	return u, nil
}

// DeleteSession ends one session (idempotent).
func (s *Store) DeleteSession(ctx context.Context, hash []byte) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash); err != nil {
		return fmt.Errorf("account: deleting a session: %w", err)
	}
	return nil
}

// DeleteSessionsOfUser ends every session of an account and cancels the sign-in
// links still unused for its address (sign out everywhere). It runs under the
// address's lock, so a link being spent at the same moment either finishes first
// (and its new session is ended here) or finds itself cancelled.
func (s *Store) DeleteSessionsOfUser(ctx context.Context, userID int64) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var email string
	if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("account: reading the user: %w", err)
	}
	if err := lock(ctx, tx, "login:", email); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	if err != nil {
		return 0, fmt.Errorf("account: deleting sessions: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM login_tokens WHERE used_at IS NULL AND email = $1`, email); err != nil {
		return 0, fmt.Errorf("account: cancelling login links: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("account: commit: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteUser removes the account with its sessions and bookmarks (cascade) and
// any login links still outstanding for its address, under the address's lock.
func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var email string
	if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoUser
		}
		return fmt.Errorf("account: reading the user: %w", err)
	}
	if err := lock(ctx, tx, "login:", email); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("account: deleting the user: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM login_tokens WHERE email = $1`, email); err != nil {
		return fmt.Errorf("account: deleting login tokens: %w", err)
	}
	return tx.Commit(ctx)
}

// AddBookmark saves an open job (idempotent). The per-account limit is checked
// under a lock on the user's row, so concurrent saves cannot exceed it.
func (s *Store) AddBookmark(ctx context.Context, userID, jobID int64, max int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoUser
		}
		return fmt.Errorf("account: locking the user: %w", err)
	}
	var already bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bookmarks WHERE user_id = $1 AND job_id = $2)`, userID, jobID).Scan(&already); err != nil {
		return fmt.Errorf("account: checking a bookmark: %w", err)
	}
	if already {
		return nil
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1 AND status = 'open')`, jobID).Scan(&open); err != nil {
		return fmt.Errorf("account: checking the job: %w", err)
	}
	if !open {
		return ErrJobNotFound
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM bookmarks WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return fmt.Errorf("account: counting bookmarks: %w", err)
	}
	if n >= max {
		return ErrBookmarkLimit
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bookmarks (user_id, job_id, created_at) VALUES ($1, $2, $3)`, userID, jobID, s.now()); err != nil {
		return fmt.Errorf("account: saving a bookmark: %w", err)
	}
	return tx.Commit(ctx)
}

// RemoveBookmark unsaves a job (idempotent).
func (s *Store) RemoveBookmark(ctx context.Context, userID, jobID int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM bookmarks WHERE user_id = $1 AND job_id = $2`, userID, jobID); err != nil {
		return fmt.Errorf("account: removing a bookmark: %w", err)
	}
	return nil
}

// Bookmarks returns one page of an account's saved jobs, newest first, and how
// many there are.
func (s *Store) Bookmarks(ctx context.Context, userID int64, limit, offset int) ([]Bookmark, int, error) {
	rows, err := s.pool.Query(ctx, `SELECT job_id, created_at, count(*) OVER() FROM bookmarks WHERE user_id = $1
		ORDER BY created_at DESC, job_id DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("account: listing bookmarks: %w", err)
	}
	defer rows.Close()
	var out []Bookmark
	total := 0
	for rows.Next() {
		var b Bookmark
		if err := rows.Scan(&b.JobID, &b.SavedAt, &total); err != nil {
			return nil, 0, fmt.Errorf("account: scanning a bookmark: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(out) == 0 && offset > 0 {
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM bookmarks WHERE user_id = $1`, userID).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("account: counting bookmarks: %w", err)
		}
	}
	return out, total, nil
}

// BookmarkIDs returns the ids of every saved job (at most max), so a page can
// mark the jobs already saved.
func (s *Store) BookmarkIDs(ctx context.Context, userID int64, max int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT job_id FROM bookmarks WHERE user_id = $1 ORDER BY created_at DESC, job_id DESC LIMIT $2`, userID, max)
	if err != nil {
		return nil, fmt.Errorf("account: listing bookmark ids: %w", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("account: scanning a bookmark id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// HasBookmark reports whether an account has saved a job.
func (s *Store) HasBookmark(ctx context.Context, userID, jobID int64) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bookmarks WHERE user_id = $1 AND job_id = $2)`, userID, jobID).Scan(&ok); err != nil {
		return false, fmt.Errorf("account: checking a bookmark: %w", err)
	}
	return ok, nil
}

// purgeBatch bounds one DELETE, so a large backlog is removed in small
// transactions that never hold a long lock or bloat the WAL in one burst.
const purgeBatch = 1000

// Purge deletes login links that expired or were used before cutoff and
// sessions that expired before cutoff, in batches, and returns how many rows went.
func (s *Store) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for _, q := range []string{
		`DELETE FROM login_tokens WHERE ctid IN (SELECT ctid FROM login_tokens WHERE expires_at < $1 OR used_at < $1 LIMIT $2)`,
		`DELETE FROM sessions WHERE ctid IN (SELECT ctid FROM sessions WHERE expires_at < $1 LIMIT $2)`,
	} {
		for {
			tag, err := s.pool.Exec(ctx, q, cutoff, purgeBatch)
			if err != nil {
				return total, fmt.Errorf("account: purging: %w", err)
			}
			total += tag.RowsAffected()
			if tag.RowsAffected() < purgeBatch {
				break
			}
		}
	}
	return total, nil
}

// ErrTelegramChatAlreadyLinked is returned when a Telegram chat that is
// already linked to a different account tries to confirm a link.
var ErrTelegramChatAlreadyLinked = errors.New("account: this telegram chat is already linked to another account")

// SetNotificationKeywords replaces an account's keyword subscriptions
// wholesale (an empty slice clears them), in one transaction so a partial
// write is never observed.
func (s *Store) SetNotificationKeywords(ctx context.Context, userID int64, keywords []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM notification_keywords WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("account: clearing notification keywords: %w", err)
	}
	now := s.now()
	for _, kw := range keywords {
		if _, err := tx.Exec(ctx, `INSERT INTO notification_keywords (user_id, keyword, created_at) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, keyword) DO NOTHING`, userID, kw, now); err != nil {
			return fmt.Errorf("account: setting a notification keyword: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// NotificationKeywords returns an account's keyword subscriptions, sorted.
func (s *Store) NotificationKeywords(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT keyword FROM notification_keywords WHERE user_id = $1 ORDER BY keyword`, userID)
	if err != nil {
		return nil, fmt.Errorf("account: listing notification keywords: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var kw string
		if err := rows.Scan(&kw); err != nil {
			return nil, fmt.Errorf("account: scanning a notification keyword: %w", err)
		}
		out = append(out, kw)
	}
	return out, rows.Err()
}

// CreateTelegramLinkToken records a requested Telegram link, valid for ttl.
// A person may hold more than one outstanding code at once (e.g. they
// asked twice); each independently confirms the same account, the same
// tolerance CreateLoginToken already has for sign-in links.
func (s *Store) CreateTelegramLinkToken(ctx context.Context, userID int64, hash []byte, ttl time.Duration) error {
	now := s.now()
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_link_tokens (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		hash, userID, now, now.Add(ttl)); err != nil {
		return fmt.Errorf("account: storing a telegram link token: %w", err)
	}
	return nil
}

// ConfirmTelegramLink spends a single-use Telegram link code and links
// chatID to whichever account requested it, in one transaction. Every
// failure but a chat already linked elsewhere is ErrInvalidToken, the same
// "don't distinguish the reasons" treatment Verify gives sign-in links.
func (s *Store) ConfirmTelegramLink(ctx context.Context, hash []byte, chatID int64) (int64, error) {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID int64
	err = tx.QueryRow(ctx, `UPDATE telegram_link_tokens SET used_at = $2 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 RETURNING user_id`,
		hash, now).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInvalidToken
	}
	if err != nil {
		return 0, fmt.Errorf("account: spending a telegram link token: %w", err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO telegram_links (user_id, chat_id, linked_at) VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET chat_id = EXCLUDED.chat_id, linked_at = EXCLUDED.linked_at`,
		userID, chatID, now); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation on chat_id
			return 0, ErrTelegramChatAlreadyLinked
		}
		return 0, fmt.Errorf("account: linking a telegram chat: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("account: commit: %w", err)
	}
	return userID, nil
}

// TelegramChatID returns an account's linked chat id, if any.
func (s *Store) TelegramChatID(ctx context.Context, userID int64) (int64, bool, error) {
	var chatID int64
	err := s.pool.QueryRow(ctx, `SELECT chat_id FROM telegram_links WHERE user_id = $1`, userID).Scan(&chatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("account: reading a telegram link: %w", err)
	}
	return chatID, true, nil
}

// UnlinkTelegram removes an account's Telegram link (idempotent).
func (s *Store) UnlinkTelegram(ctx context.Context, userID int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM telegram_links WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("account: unlinking telegram: %w", err)
	}
	return nil
}

// notifiedJobIDs returns which of jobIDs this account has already been
// notified about, so NotifyNewJobs can skip them.
func (s *Store) notifiedJobIDs(ctx context.Context, userID int64, jobIDs []int64) (map[int64]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT job_id FROM notified_jobs WHERE user_id = $1 AND job_id = ANY($2)`, userID, jobIDs)
	if err != nil {
		return nil, fmt.Errorf("account: reading notified jobs: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("account: scanning a notified job id: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// recordNotified marks a job as delivered to an account, idempotent
// (ON CONFLICT DO NOTHING: a delivery already recorded is left alone).
func (s *Store) recordNotified(ctx context.Context, userID, jobID int64) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO notified_jobs (user_id, job_id, notified_at) VALUES ($1, $2, $3)
		ON CONFLICT (user_id, job_id) DO NOTHING`, userID, jobID, s.now()); err != nil {
		return fmt.Errorf("account: recording a notified job: %w", err)
	}
	return nil
}

// subscriber is an account with at least one notification keyword and a
// confirmed Telegram chat -- NotifyNewJobs' candidate list to check new
// jobs against.
type subscriber struct {
	userID   int64
	chatID   int64
	keywords []string
}

// subscribers returns every account eligible to be notified: it has a
// linked Telegram chat and at least one keyword.
func (s *Store) subscribers(ctx context.Context) ([]subscriber, error) {
	rows, err := s.pool.Query(ctx, `SELECT tl.user_id, tl.chat_id, array_agg(nk.keyword)
		FROM telegram_links tl
		JOIN notification_keywords nk ON nk.user_id = tl.user_id
		GROUP BY tl.user_id, tl.chat_id`)
	if err != nil {
		return nil, fmt.Errorf("account: listing notification subscribers: %w", err)
	}
	defer rows.Close()
	var out []subscriber
	for rows.Next() {
		var sub subscriber
		if err := rows.Scan(&sub.userID, &sub.chatID, &sub.keywords); err != nil {
			return nil, fmt.Errorf("account: scanning a notification subscriber: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}
