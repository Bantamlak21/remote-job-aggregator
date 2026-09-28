package account

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const (
	linkTTL    = 10 * time.Minute
	sessionTTL = 14 * 24 * time.Hour
	sessionMax = 30 * 24 * time.Hour
	day        = 24 * time.Hour
)

func TestStoreLogin_LinkIsSingleUseUnderConcurrency(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	_, hash, _ := newToken()
	if err := s.CreateLoginToken(ctx, "racer@example.com", hash, linkTTL); err != nil {
		t.Fatal(err)
	}

	const callers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		invalid int
		other   []error
	)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, sessHash, _ := newToken()
			<-start
			_, err := s.Login(ctx, hash, sessHash, sessionTTL, nil, 0)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrInvalidToken):
				invalid++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 || invalid != callers-1 || len(other) != 0 {
		t.Errorf("wins=%d invalid=%d other=%v, want exactly 1 win and %d ErrInvalidToken", wins, invalid, other, callers-1)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("sessions = %d, want 1 (one link opens one session)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM users`); n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
}

func TestStoreLogin_ExpiredLinkIsRefused(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)
	ctx := context.Background()
	_, hash, _ := newToken()
	if err := s.CreateLoginToken(ctx, "late@example.com", hash, linkTTL); err != nil {
		t.Fatal(err)
	}

	clock.Advance(linkTTL) // exactly the expiry instant: no longer valid
	if _, err := s.PeekLoginToken(ctx, hash); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("PeekLoginToken at the expiry instant = %v, want ErrInvalidToken", err)
	}
	_, sessHash, _ := newToken()
	if _, err := s.Login(ctx, hash, sessHash, sessionTTL, nil, 0); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Login with an expired link = %v, want ErrInvalidToken", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM users`); n != 0 {
		t.Errorf("an expired link created %d users, want 0", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sessions`); n != 0 {
		t.Errorf("an expired link opened %d sessions, want 0", n)
	}
}

func TestStoreLogin_UnknownLinkIsRefused(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	_, hash, _ := newToken()
	_, sessHash, _ := newToken()
	if _, err := s.Login(context.Background(), hash, sessHash, sessionTTL, nil, 0); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Login with a link nobody issued = %v, want ErrInvalidToken", err)
	}
}

func TestStoreLogin_CreatesTheAccountOnceAndReusesIt(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)

	first, _, _ := mustLogin(t, s, "new@example.com")
	clock.Advance(time.Hour)
	second, _, _ := mustLogin(t, s, "new@example.com")
	if first.ID != second.ID {
		t.Errorf("second sign-in gave user %d, want the same account %d", second.ID, first.ID)
	}
	if first.Email != "new@example.com" {
		t.Errorf("Email = %q", first.Email)
	}
	var last time.Time
	if err := pool.QueryRow(context.Background(), `SELECT last_login_at FROM users WHERE id = $1`, first.ID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if !last.Equal(clock.Now()) {
		t.Errorf("last_login_at = %v, want %v", last, clock.Now())
	}
	if n := countRows(t, pool, `SELECT count(*) FROM users`); n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
}

func TestStoreLogin_SessionFailureDoesNotSpendTheLink(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	_, hash, _ := newToken()
	if err := s.CreateLoginToken(ctx, "keep@example.com", hash, linkTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, hash, []byte("too short"), sessionTTL, nil, 0); err == nil {
		t.Fatal("Login with an invalid session digest succeeded, want an error")
	}
	if _, err := s.PeekLoginToken(ctx, hash); err != nil {
		t.Errorf("the link was spent by a failed sign-in: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM users`); n != 0 {
		t.Errorf("a failed sign-in left %d users behind, want 0 (one transaction)", n)
	}
}

func TestStoreLogin_PeekDoesNotSpendAndSpentLinkIsGone(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	_, hash, _ := newToken()
	if err := s.CreateLoginToken(ctx, "peek@example.com", hash, linkTTL); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		email, err := s.PeekLoginToken(ctx, hash)
		if err != nil || email != "peek@example.com" {
			t.Fatalf("PeekLoginToken #%d = %q, %v", i+1, email, err)
		}
	}
	_, sessHash, _ := newToken()
	if _, err := s.Login(ctx, hash, sessHash, sessionTTL, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekLoginToken(ctx, hash); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("PeekLoginToken after use = %v, want ErrInvalidToken", err)
	}
}

func TestStoreAuthenticate_UnknownAndExpired(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)
	ctx := context.Background()

	if _, err := s.Authenticate(ctx, hashToken("nobody"), sessionTTL, sessionMax); !errors.Is(err, ErrNoSession) {
		t.Errorf("unknown session = %v, want ErrNoSession", err)
	}
	u, _, sessHash := mustLogin(t, s, "who@example.com")
	got, err := s.Authenticate(ctx, sessHash, sessionTTL, sessionMax)
	if err != nil || got.ID != u.ID || got.Email != "who@example.com" {
		t.Fatalf("Authenticate = %+v, %v, want the signed-in user", got, err)
	}
	clock.Advance(sessionTTL) // idle expiry reached
	if _, err := s.Authenticate(ctx, sessHash, sessionTTL, sessionMax); !errors.Is(err, ErrNoSession) {
		t.Errorf("expired session = %v, want ErrNoSession", err)
	}
}

func sessionExpiry(t *testing.T, s *Store, hash []byte) time.Time {
	t.Helper()
	var e time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT expires_at FROM sessions WHERE token_hash = $1`, hash).Scan(&e); err != nil {
		t.Fatalf("reading the session expiry: %v", err)
	}
	return e
}

func TestStoreAuthenticate_SlidesButNeverPastTheAbsoluteLimit(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)
	ctx := context.Background()
	start := clock.Now()
	_, _, hash := mustLogin(t, s, "slide@example.com")

	use := func(after time.Duration) User {
		t.Helper()
		clock.t = start.Add(after) // same package: move the clock directly
		u, err := s.Authenticate(ctx, hash, sessionTTL, sessionMax)
		if err != nil {
			t.Fatalf("Authenticate at +%v: %v", after, err)
		}
		return u
	}
	want := func(label string, at time.Duration) {
		t.Helper()
		if got := sessionExpiry(t, s, hash); !got.Equal(start.Add(at)) {
			t.Errorf("%s: expires_at = +%v, want +%v", label, got.Sub(start), at)
		}
	}

	want("after login", 14*day)
	if u := use(30 * time.Minute); u.SessionRenewed { // an extension under an hour is not worth a write
		t.Error("a use 30 minutes after sign-in rewrote the session")
	}
	want("used 30 minutes in", 14*day)

	u := use(6 * day) // the idle window is 14 days from THIS use, not from sign-in
	want("used on day 6", 20*day)
	if !u.SessionRenewed || !u.SessionExpires.Equal(start.Add(20*day)) {
		t.Errorf("used on day 6: renewed=%v expires=+%v, want renewed and +%v (the cookie is re-sent with this)", u.SessionRenewed, u.SessionExpires.Sub(start), 20*day)
	}
	if u := use(6*day + 10*time.Minute); u.SessionRenewed || !u.SessionExpires.Equal(start.Add(20*day)) {
		t.Errorf("a second use 10 minutes later: renewed=%v expires=+%v, want not renewed and unchanged", u.SessionRenewed, u.SessionExpires.Sub(start))
	}
	use(16 * day) // 16 + 14 = 30 days, exactly the absolute limit
	want("extended up to the absolute limit", 30*day)
	u = use(25 * day) // would be +39 days: capped, so nothing to extend
	want("never past 30 days from sign-in", 30*day)
	if u.SessionRenewed {
		t.Error("a session at its absolute limit reported a renewal")
	}

	clock.t = start.Add(30 * day)
	if _, err := s.Authenticate(ctx, hash, sessionTTL, sessionMax); !errors.Is(err, ErrNoSession) {
		t.Errorf("session at its absolute limit = %v, want ErrNoSession however often it was used", err)
	}
}

func TestStoreAuthenticate_ADailyUserKeepsTheSessionUntilTheAbsoluteLimit(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)
	ctx := context.Background()
	start := clock.Now()
	_, _, hash := mustLogin(t, s, "daily@example.com")
	for d := 1; d < 30; d++ {
		clock.t = start.Add(time.Duration(d) * day)
		if _, err := s.Authenticate(ctx, hash, sessionTTL, sessionMax); err != nil {
			t.Fatalf("a session used every day died on day %d: %v", d, err)
		}
	}
	clock.t = start.Add(30 * day)
	if _, err := s.Authenticate(ctx, hash, sessionTTL, sessionMax); !errors.Is(err, ErrNoSession) {
		t.Errorf("day 30 = %v, want the session gone at the absolute limit", err)
	}
}

func TestStoreDeleteSession_EndsOnlyThatSession(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	_, _, h1 := mustLogin(t, s, "two@example.com")
	_, _, h2 := mustLogin(t, s, "two@example.com")

	if err := s.DeleteSession(ctx, h1); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, h1); err != nil {
		t.Errorf("deleting an already deleted session = %v, want nil (idempotent)", err)
	}
	if _, err := s.Authenticate(ctx, h1, sessionTTL, sessionMax); !errors.Is(err, ErrNoSession) {
		t.Errorf("signed-out session still authenticates: %v", err)
	}
	if _, err := s.Authenticate(ctx, h2, sessionTTL, sessionMax); err != nil {
		t.Errorf("the other session was ended too: %v", err)
	}
}

func TestStoreDeleteSessionsOfUser_EndsSessionsAndCancelsUnusedLinks(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	u, _, h1 := mustLogin(t, s, "all@example.com")
	_, _, h2 := mustLogin(t, s, "all@example.com")
	other, _, hOther := mustLogin(t, s, "other@example.com")

	_, unusedHash, _ := newToken()
	if err := s.CreateLoginToken(ctx, "all@example.com", unusedHash, linkTTL); err != nil {
		t.Fatal(err)
	}
	_, otherHash, _ := newToken()
	if err := s.CreateLoginToken(ctx, "other@example.com", otherHash, linkTTL); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteSessionsOfUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("sessions ended = %d, want 2", n)
	}
	for _, h := range [][]byte{h1, h2} {
		if _, err := s.Authenticate(ctx, h, sessionTTL, sessionMax); !errors.Is(err, ErrNoSession) {
			t.Errorf("a session survived sign-out-everywhere: %v", err)
		}
	}
	if _, err := s.PeekLoginToken(ctx, unusedHash); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("an unused link for the address survived sign-out-everywhere: %v", err)
	}
	if _, err := s.PeekLoginToken(ctx, otherHash); err != nil {
		t.Errorf("another address's link was cancelled: %v", err)
	}
	if got, err := s.Authenticate(ctx, hOther, sessionTTL, sessionMax); err != nil || got.ID != other.ID {
		t.Errorf("another user's session was affected: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM users WHERE id = $1`, u.ID); n != 1 {
		t.Error("sign-out-everywhere deleted the account")
	}
}

func TestStoreDeleteUser_CascadesAndLeavesOthersAlone(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	u, _, hash := mustLogin(t, s, "gone@example.com")
	other, _, otherHash := mustLogin(t, s, "stays@example.com")
	job := seedJob(t, pool, "open")
	for _, id := range []int64{u.ID, other.ID} {
		if err := s.AddBookmark(ctx, id, job, 10); err != nil {
			t.Fatal(err)
		}
	}
	_, pendingHash, _ := newToken()
	if err := s.CreateLoginToken(ctx, "gone@example.com", pendingHash, linkTTL); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, hash, sessionTTL, sessionMax); !errors.Is(err, ErrNoSession) {
		t.Errorf("deleted account's session still works: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM bookmarks WHERE user_id = $1`, u.ID); n != 0 {
		t.Errorf("bookmarks left after account deletion = %d", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE email = 'gone@example.com'`); n != 0 {
		t.Errorf("login links left for the deleted address = %d (used and unused must both go)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM users WHERE email = 'gone@example.com'`); n != 0 {
		t.Error("the user row is still there")
	}
	if got, err := s.Authenticate(ctx, otherHash, sessionTTL, sessionMax); err != nil || got.ID != other.ID {
		t.Errorf("another user's session was affected: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM bookmarks WHERE user_id = $1`, other.ID); n != 1 {
		t.Errorf("another user's bookmarks = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM jobs`); n != 1 {
		t.Error("deleting an account deleted a job")
	}
	if err := s.DeleteUser(ctx, u.ID); !errors.Is(err, ErrNoUser) {
		t.Errorf("deleting twice = %v, want ErrNoUser", err)
	}
}

func TestStoreAddBookmark_CapHoldsUnderConcurrency(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	u, _, _ := mustLogin(t, s, "cap@example.com")

	const jobs, max = 30, 5
	ids := make([]int64, jobs)
	for i := range ids {
		ids[i] = seedJob(t, pool, "open")
	}
	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		saved, limit int
		other        []error
	)
	start := make(chan struct{})
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := s.AddBookmark(ctx, u.ID, id, max)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				saved++
			case errors.Is(err, ErrBookmarkLimit):
				limit++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if saved != max || limit != jobs-max || len(other) != 0 {
		t.Errorf("saved=%d limit=%d other=%v, want %d saved and %d ErrBookmarkLimit", saved, limit, other, max, jobs-max)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM bookmarks WHERE user_id = $1`, u.ID); n != max {
		t.Errorf("bookmarks in the database = %d, want %d", n, max)
	}
}

func TestStoreAddBookmark_CapIsExactAndSavingAgainIsIdempotent(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	u, _, _ := mustLogin(t, s, "exact@example.com")
	a, b, c := seedJob(t, pool, "open"), seedJob(t, pool, "open"), seedJob(t, pool, "open")

	if err := s.AddBookmark(ctx, u.ID, a, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBookmark(ctx, u.ID, a, 2); err != nil {
		t.Errorf("saving the same job again = %v, want nil", err)
	}
	if err := s.AddBookmark(ctx, u.ID, b, 2); err != nil {
		t.Fatalf("saving up to the cap: %v", err)
	}
	if err := s.AddBookmark(ctx, u.ID, c, 2); !errors.Is(err, ErrBookmarkLimit) {
		t.Errorf("saving past the cap = %v, want ErrBookmarkLimit", err)
	}
	if err := s.AddBookmark(ctx, u.ID, a, 2); err != nil {
		t.Errorf("re-saving a saved job at the cap = %v, want nil (it is not a new bookmark)", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM bookmarks WHERE user_id = $1`, u.ID); n != 2 {
		t.Errorf("bookmarks = %d, want 2", n)
	}
}

func TestStoreAddBookmark_OnlyOpenExistingJobsForExistingUsers(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	u, _, _ := mustLogin(t, s, "closed@example.com")

	for _, status := range []string{"closed", "removed"} {
		id := seedJob(t, pool, status)
		if err := s.AddBookmark(ctx, u.ID, id, 10); !errors.Is(err, ErrJobNotFound) {
			t.Errorf("saving a %s job = %v, want ErrJobNotFound", status, err)
		}
	}
	if err := s.AddBookmark(ctx, u.ID, 987654321, 10); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("saving a job that does not exist = %v, want ErrJobNotFound", err)
	}
	open := seedJob(t, pool, "open")
	if err := s.AddBookmark(ctx, 987654321, open, 10); !errors.Is(err, ErrNoUser) {
		t.Errorf("saving for a user that does not exist = %v, want ErrNoUser", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM bookmarks`); n != 0 {
		t.Errorf("bookmarks = %d, want 0 after every refused save", n)
	}
}

func TestStoreBookmarks_SurviveTheJobClosing(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	u, _, _ := mustLogin(t, s, "keeps@example.com")
	id := seedJob(t, pool, "open")
	if err := s.AddBookmark(ctx, u.ID, id, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'closed', closed_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	marks, total, err := s.Bookmarks(ctx, u.ID, 10, 0)
	if err != nil || total != 1 || len(marks) != 1 || marks[0].JobID != id {
		t.Errorf("Bookmarks after the job closed = %+v, %d, %v; want the saved job still listed", marks, total, err)
	}
	ok, err := s.HasBookmark(ctx, u.ID, id)
	if err != nil || !ok {
		t.Errorf("HasBookmark after the job closed = %v, %v, want true", ok, err)
	}
}

func TestStoreBookmarks_PagingOrderAndTotals(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)
	ctx := context.Background()
	u, _, _ := mustLogin(t, s, "pages@example.com")
	other, _, _ := mustLogin(t, s, "elsewhere@example.com")

	var ids []int64
	for i := 0; i < 7; i++ {
		id := seedJob(t, pool, "open")
		ids = append(ids, id)
		clock.Advance(time.Minute)
		if err := s.AddBookmark(ctx, u.ID, id, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddBookmark(ctx, other.ID, ids[0], 100); err != nil {
		t.Fatal(err)
	}

	var got []int64
	for _, page := range []struct{ offset, wantLen int }{{0, 3}, {3, 3}, {6, 1}} {
		marks, total, err := s.Bookmarks(ctx, u.ID, 3, page.offset)
		if err != nil {
			t.Fatal(err)
		}
		if total != 7 || len(marks) != page.wantLen {
			t.Errorf("offset %d: len=%d total=%d, want len=%d total=7", page.offset, len(marks), total, page.wantLen)
		}
		for _, m := range marks {
			got = append(got, m.JobID)
		}
	}
	for i, id := range got { // newest saved first
		if want := ids[len(ids)-1-i]; id != want {
			t.Errorf("position %d = job %d, want %d (newest first)", i, id, want)
		}
	}
	marks, total, err := s.Bookmarks(ctx, u.ID, 3, 30)
	if err != nil || len(marks) != 0 || total != 7 {
		t.Errorf("past the last page = %d marks, total %d, %v; want 0 marks and total 7", len(marks), total, err)
	}
	if _, total, _ := s.Bookmarks(ctx, other.ID, 10, 0); total != 1 {
		t.Errorf("another account's total = %d, want 1", total)
	}
}

func TestStoreBookmarkIDs_RemoveAndHas(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)
	ctx := context.Background()
	u, _, _ := mustLogin(t, s, "ids@example.com")
	a, b, c := seedJob(t, pool, "open"), seedJob(t, pool, "open"), seedJob(t, pool, "open")
	for _, id := range []int64{a, b, c} {
		clock.Advance(time.Minute)
		if err := s.AddBookmark(ctx, u.ID, id, 10); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.BookmarkIDs(ctx, u.ID, 10)
	if err != nil || len(ids) != 3 || ids[0] != c || ids[2] != a {
		t.Errorf("BookmarkIDs = %v, %v, want [c b a] newest first", ids, err)
	}
	if ids, _ := s.BookmarkIDs(ctx, u.ID, 2); len(ids) != 2 {
		t.Errorf("BookmarkIDs with max 2 returned %d ids", len(ids))
	}
	if err := s.RemoveBookmark(ctx, u.ID, b); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveBookmark(ctx, u.ID, b); err != nil {
		t.Errorf("removing twice = %v, want nil (idempotent)", err)
	}
	if ok, _ := s.HasBookmark(ctx, u.ID, b); ok {
		t.Error("HasBookmark is true after removal")
	}
	if ok, _ := s.HasBookmark(ctx, u.ID, a); !ok {
		t.Error("HasBookmark is false for a saved job")
	}
	empty, _, _ := mustLogin(t, s, "empty@example.com")
	if ids, err := s.BookmarkIDs(ctx, empty.ID, 10); err != nil || ids == nil || len(ids) != 0 {
		t.Errorf("BookmarkIDs for an account with none = %#v, %v, want an empty non-nil slice", ids, err)
	}
}

func TestStorePurge_RemovesOnlyDeadRows(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)
	ctx := context.Background()

	// An expired link, a used link, a live link, an expired session and a live session.
	_, expiredLink, _ := newToken()
	if err := s.CreateLoginToken(ctx, "purge@example.com", expiredLink, linkTTL); err != nil {
		t.Fatal(err)
	}
	_, usedLink, _ := newToken()
	if err := s.CreateLoginToken(ctx, "purge@example.com", usedLink, 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	_, sessHash, _ := newToken()
	if _, err := s.Login(ctx, usedLink, sessHash, time.Hour, nil, 0); err != nil { // a session that expires after an hour
		t.Fatal(err)
	}
	clock.Advance(3 * day)
	_, liveLink, _ := newToken()
	if err := s.CreateLoginToken(ctx, "purge@example.com", liveLink, linkTTL); err != nil {
		t.Fatal(err)
	}
	_, _, liveSession := mustLogin(t, s, "purge@example.com")

	n, err := s.Purge(ctx, clock.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// expired link, used link (used 3 days ago), the first login's link is `usedLink`; the mustLogin link is used just now.
	if n != 3 {
		t.Errorf("Purge removed %d rows, want 3 (expired link, old used link, expired session)", n)
	}
	if _, err := s.PeekLoginToken(ctx, liveLink); err != nil {
		t.Errorf("a live link was purged: %v", err)
	}
	if _, err := s.Authenticate(ctx, liveSession, sessionTTL, sessionMax); err != nil {
		t.Errorf("a live session was purged: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM users`); n != 1 {
		t.Errorf("purge touched users: %d rows", n)
	}
}

func TestStoreCountLoginTokensSince_IsPerAddressAndWindowed(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)
	ctx := context.Background()
	mk := func(email string) {
		t.Helper()
		_, h, _ := newToken()
		if err := s.CreateLoginToken(ctx, email, h, linkTTL); err != nil {
			t.Fatal(err)
		}
	}
	mk("count@example.com")
	clock.Advance(20 * time.Minute)
	mk("count@example.com")
	mk("count@example.com")
	mk("someone-else@example.com")

	got, err := s.CountLoginTokensSince(ctx, "count@example.com", clock.Now().Add(-15*time.Minute))
	if err != nil || got != 2 {
		t.Errorf("count in the last 15 minutes = %d, %v, want 2", got, err)
	}
	got, _ = s.CountLoginTokensSince(ctx, "count@example.com", clock.Now().Add(-time.Hour))
	if got != 3 {
		t.Errorf("count in the last hour = %d, want 3", got)
	}
}
