package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestThrottleKey(t *testing.T) {
	cases := map[string]string{
		"alice@example.com":       "alice@example.com",
		"alice+jobs@example.com":  "alice@example.com",  // a +tag is the same mailbox
		"a.lice@example.com":      "a.lice@example.com", // dots count outside Gmail
		"a.b.c@gmail.com":         "abc@gmail.com",
		"a.b+x.y@gmail.com":       "ab@gmail.com",
		"ab@googlemail.com":       "ab@gmail.com",
		"a.b+1@googlemail.com":    "ab@gmail.com",
		"+odd@example.com":        "+odd@example.com", // a leading + is not a tag
		"x@sub.gmail.com":         "x@sub.gmail.com",
		"a.b@notgmail.com":        "a.b@notgmail.com",
		"first+a+b@example.co.uk": "first@example.co.uk",
	}
	for in, want := range cases {
		if got := throttleKey(in); got != want {
			t.Errorf("throttleKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClampToken(t *testing.T) {
	if got := clampToken("abc"); got != "abc" {
		t.Errorf("clampToken changed a short token: %q", got)
	}
	long := strings.Repeat("x", 5000)
	if got := clampToken(long); len(got) != maxLookupLen {
		t.Errorf("clampToken length = %d, want %d", len(got), maxLookupLen)
	}
}

// F3: sign out everywhere and a sign-in that spends a link minted earlier must never
// interleave so that the new session survives.
func TestSignOutEverywhereRacingAVerifyLeavesNoSession(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	u, _, _ := mustLogin(t, s, "race@example.com")

	survived, verified := 0, 0
	const rounds = 150
	for i := 0; i < rounds; i++ {
		_, linkHash, _ := newToken() // a link minted before sign out everywhere
		if err := s.CreateLoginToken(ctx, "race@example.com", linkHash, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		_, sessHash, _ := newToken()

		var wg sync.WaitGroup
		var loginErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, loginErr = s.Login(ctx, linkHash, sessHash, time.Hour, nil, 0) }()
		go func() { defer wg.Done(); <-start; _, _ = s.DeleteSessionsOfUser(ctx, u.ID) }()
		close(start)
		wg.Wait()

		if loginErr == nil {
			verified++
		} else if !errors.Is(loginErr, ErrInvalidToken) {
			t.Fatalf("Login = %v, want success or ErrInvalidToken", loginErr)
		}
		if n := countRows(t, pool, `SELECT count(*) FROM sessions WHERE user_id = $1`, u.ID); n != 0 {
			survived++
		}
	}
	if survived != 0 {
		t.Fatalf("%d of %d races left a live session after sign out everywhere (%d of the links were spent first)", survived, rounds, verified)
	}
	t.Logf("verify won %d of %d races, sign out everywhere the rest; no session ever survived", verified, rounds)
}

// F5: signing in ends the session the caller already holds.
func TestVerifyEndsThePreviousSession(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	ctx := context.Background()

	signIn := func(previous string) (string, User) {
		t.Helper()
		if err := s.RequestLogin(ctx, "prev@example.com", "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
		sess, u, err := s.Verify(ctx, tokenOf(t, take(t, s)), "192.0.2.1", previous)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		return sess, u
	}
	first, _ := signIn("")
	second, _ := signIn(first)
	if _, err := s.Authenticate(ctx, first); !errors.Is(err, ErrNoSession) {
		t.Errorf("the session held at sign-in still works: %v", err)
	}
	if _, err := s.Authenticate(ctx, second); err != nil {
		t.Errorf("the new session does not work: %v", err)
	}
	// A different account's session sent along is ended too (whatever the caller held is replaced).
	other, _ := func() (string, User) {
		if err := s.RequestLogin(ctx, "someone-else@example.com", "192.0.2.2"); err != nil {
			t.Fatal(err)
		}
		sess, u, err := s.Verify(ctx, tokenOf(t, take(t, s)), "192.0.2.2", "")
		if err != nil {
			t.Fatal(err)
		}
		return sess, u
	}()
	third, _ := signIn(other)
	if _, err := s.Authenticate(ctx, other); !errors.Is(err, ErrNoSession) {
		t.Errorf("another account's session sent with the sign-in was not ended: %v", err)
	}
	// A previous value that is garbage changes nothing and does not fail the sign-in.
	if _, u := signIn("not-a-session"); u.Email != "prev@example.com" {
		t.Errorf("sign-in with a garbage previous session = %+v", u)
	}
	if _, err := s.Authenticate(ctx, third); err != nil {
		t.Errorf("an unrelated live session was ended: %v", err)
	}
}

// F19: a sixth device ends the oldest session, not the newest.
func TestLoginCapsLiveSessions(t *testing.T) {
	pool := newPool(t)
	clock := newTestClock()
	s := NewStore(pool).WithClock(clock.Now)
	ctx := context.Background()

	var hashes [][]byte
	for i := 0; i < 5; i++ {
		clock.Advance(time.Minute)
		_, linkHash, _ := newToken()
		if err := s.CreateLoginToken(ctx, "cap@example.com", linkHash, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		_, sessHash, _ := newToken()
		if _, err := s.Login(ctx, linkHash, sessHash, sessionTTL, nil, 3); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, sessHash)
	}
	live := 0
	for i, h := range hashes {
		_, err := s.Authenticate(ctx, h, sessionTTL, sessionMax)
		wantLive := i >= 2 // the two oldest are gone
		if (err == nil) != wantLive {
			t.Errorf("session %d live = %v, want %v", i, err == nil, wantLive)
		}
		if err == nil {
			live++
		}
	}
	if live != 3 {
		t.Errorf("live sessions = %d, want the cap of 3", live)
	}
}

func TestDefaultsCapSessionsAtTen(t *testing.T) {
	if Defaults().MaxSessions != 10 || (Config{}).withDefaults().MaxSessions != 10 {
		t.Errorf("MaxSessions default = %d, want 10", Defaults().MaxSessions)
	}
}

// F18: aliases of one mailbox share one budget of 3 links per 15 minutes.
func TestAliasesShareOneLinkBudget(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginPerEmail15Min, cfg.LoginPerEmailDay = 3, 10
	s, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()

	aliases := []string{"a.b+1@gmail.com", "ab+2@gmail.com", "a.b@googlemail.com", "ab@gmail.com", "a.b+5@gmail.com"}
	sent, limited := 0, 0
	for _, a := range aliases {
		err := s.RequestLogin(ctx, a, "192.0.2.1")
		var rl *RateLimitedError
		switch {
		case err == nil:
			sent++
		case errors.As(err, &rl):
			limited++
		default:
			t.Fatalf("RequestLogin(%q) = %v", a, err)
		}
	}
	if sent != 3 || limited != 2 {
		t.Errorf("sent %d, limited %d, want 3 and 2: five spellings of one Gmail mailbox share one budget", sent, limited)
	}
	// The mail goes to the address exactly as typed (normalized), not to the folded key.
	first := take(t, s)
	if first.To != "a.b+1@gmail.com" {
		t.Errorf("first email went to %q, want the typed address", first.To)
	}
	// A different mailbox is unaffected.
	if err := s.RequestLogin(ctx, "someone.else@gmail.com", "192.0.2.1"); err != nil {
		t.Errorf("an unrelated address was limited: %v", err)
	}
}

// F9: however many servers race, the per-address limit is exact.
func TestPerAddressLimitIsExactAcrossTwoServices(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginPerEmail15Min, cfg.LoginPerEmailDay = 3, 10
	a, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	b, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()

	var wg sync.WaitGroup
	var ok, limited atomic.Int64
	start := make(chan struct{})
	for i := 0; i < 30; i++ {
		svc := a
		if i%2 == 1 {
			svc = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := svc.RequestLogin(ctx, "many@example.com", "192.0.2.1")
			var rl *RateLimitedError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &rl):
				limited.Add(1)
			default:
				t.Errorf("RequestLogin = %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 3 {
		t.Errorf("accepted %d requests, want exactly 3 (two services, 30 concurrent requests)", ok.Load())
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE email = 'many@example.com'`); n != 3 {
		t.Errorf("stored %d links, want 3", n)
	}
}

func TestSiteWideLinkCapIsExactAcrossServicesAndFlagsTheBreaker(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginGlobalHour = 5
	a, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	b, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()

	var wg sync.WaitGroup
	var ok, global atomic.Int64
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		svc := a
		if i%2 == 1 {
			svc = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := svc.RequestLogin(ctx, fmt.Sprintf("person%d@example.com", i), fmt.Sprintf("192.0.2.%d", i+1))
			var rl *RateLimitedError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &rl) && rl.Global:
				global.Add(1)
			default:
				t.Errorf("RequestLogin = %v, want nil or a global RateLimitedError", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 5 || global.Load() != 15 {
		t.Errorf("accepted %d, refused by the site cap %d; want 5 and 15", ok.Load(), global.Load())
	}
}

func TestSiteWideCapSurvivesARestart(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginGlobalHour = 2
	first, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := first.RequestLogin(ctx, fmt.Sprintf("p%d@example.com", i), "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	restarted, _, _ := newService(t, pool, newRecordingMailer(), cfg) // empty in-memory limiters
	err := restarted.RequestLogin(ctx, "p9@example.com", "192.0.2.2")
	var rl *RateLimitedError
	if !errors.As(err, &rl) || !rl.Global {
		t.Errorf("after a restart the site cap = %v, want the database to remember it", err)
	}
}

func TestRetryAfterIsUntilTheOldestCountedLinkLeavesTheWindow(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginPerEmail15Min, cfg.LoginPerEmailDay = 3, 100
	s, clock, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.RequestLogin(ctx, "wait@example.com", "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
	}
	// Bypass the in-memory limiter so the database answer is what is measured.
	s2, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	s2.store = NewStore(pool).WithClock(clock.Now)
	err := s2.RequestLogin(ctx, "wait@example.com", "192.0.2.1")
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("4th request = %v, want a RateLimitedError", err)
	}
	// The links were created at t0, t0+1m, t0+2m; now is t0+3m; the 3rd newest (t0) leaves at t0+15m.
	if want := 12 * time.Minute; rl.RetryAfter != want {
		t.Errorf("RetryAfter = %v, want %v", rl.RetryAfter, want)
	}
}

// F4: only what the public can see may be saved.
type fakeVisibility struct{ visible map[int64]bool }

func (f fakeVisibility) Visible(_ context.Context, id int64) (bool, error) { return f.visible[id], nil }

func TestSaveRequiresAPubliclyVisibleJob(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	ctx := context.Background()
	u, _, _ := mustLogin(t, s.store, "saver@example.com")
	visibleJob, hiddenJob, agedJob := seedJob(t, pool, "open"), seedJob(t, pool, "open"), seedJob(t, pool, "open")
	// hiddenJob and agedJob are open on their board but not visible to the public
	// (the store's own check cannot tell: this is what the visibility hook is for).
	s.WithJobs(fakeVisibility{visible: map[int64]bool{visibleJob: true}})

	if err := s.Save(ctx, u.ID, visibleJob); err != nil {
		t.Errorf("saving a visible job = %v", err)
	}
	for _, id := range []int64{hiddenJob, agedJob} {
		if err := s.Save(ctx, u.ID, id); !errors.Is(err, ErrJobNotFound) {
			t.Errorf("saving job %d that the public cannot see = %v, want ErrJobNotFound", id, err)
		}
	}
	// A job saved while it was visible stays saved, and saving it again is a no-op, after it leaves view.
	s.WithJobs(fakeVisibility{visible: map[int64]bool{}})
	if err := s.Save(ctx, u.ID, visibleJob); err != nil {
		t.Errorf("re-saving a job that left view after being saved = %v, want nil (idempotent)", err)
	}
	if ok, _ := s.HasSaved(ctx, u.ID, visibleJob); !ok {
		t.Error("the saved job vanished when it left view")
	}
	if ok, _ := s.HasSaved(ctx, u.ID, hiddenJob); ok {
		t.Error("a hidden job was saved")
	}
}

// queryCounter counts the statements that touch login_tokens.
type queryCounter struct{ n atomic.Int64 }

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "login_tokens") {
		q.n.Add(1)
	}
	return ctx
}
func (q *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// F11: a string that cannot be a token takes the same path (a hash and a lookup) as one
// that merely is not known, so the two cannot be told apart by timing.
func TestMalformedAndUnknownTokensTakeTheSamePath(t *testing.T) {
	newPool(t) // resets the schema
	cfg, err := pgxpool.ParseConfig(testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	counter := &queryCounter{}
	cfg.ConnConfig.Tracer = counter
	traced, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	s, _, _ := newService(t, traced, newRecordingMailer(), open())
	ctx := context.Background()

	unknown, _, _ := newToken() // a well-formed token nobody issued
	inputs := map[string]string{
		"unknown well-formed": unknown,
		"empty":               "",
		"short":               "abc",
		"not base64url":       strings.Repeat("!", 43),
		"very long":           strings.Repeat("A", 100_000),
	}
	for name, in := range inputs {
		before := counter.n.Load()
		if _, err := s.Inspect(ctx, in, "192.0.2.1"); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: Inspect = %v, want ErrInvalidToken", name, err)
		}
		if got := counter.n.Load() - before; got != 1 {
			t.Errorf("%s: Inspect ran %d login_tokens queries, want 1 (the same lookup as any other input)", name, got)
		}
		before = counter.n.Load()
		if _, _, err := s.Verify(ctx, in, "192.0.2.2", ""); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: Verify = %v, want ErrInvalidToken", name, err)
		}
		if got := counter.n.Load() - before; got != 1 {
			t.Errorf("%s: Verify ran %d login_tokens queries, want 1", name, got)
		}
	}
}

// F21: a large backlog is removed in bounded batches, all of it.
func TestPurgeRemovesAllExpiredRowsInBatches(t *testing.T) {
	pool := newPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	u, _, _ := mustLogin(t, s, "purge@example.com")
	const n = 2600
	if _, err := pool.Exec(ctx, `INSERT INTO login_tokens (token_hash, email, throttle_key, created_at, expires_at)
		SELECT sha256(('t' || g)::bytea), 'purge@example.com', 'purge@example.com', now() - interval '3 days', now() - interval '3 days' + interval '10 minutes'
		FROM generate_series(1, $1) g`, n); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at)
		SELECT sha256(('s' || g)::bytea), $2, now() - interval '40 days', now() - interval '3 days', now() - interval '40 days'
		FROM generate_series(1, $1) g`, n, u.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Purge(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got != 2*n {
		t.Errorf("Purge removed %d rows, want %d (batches of %d must loop until done)", got, 2*n, purgeBatch)
	}
	if c := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE email = 'purge@example.com'`); c != 1 {
		// the one left is the link mustLogin spent moments ago: recent, not purged
		t.Errorf("login_tokens left = %d, want 1 (the recent one)", c)
	}
	if c := countRows(t, pool, `SELECT count(*) FROM sessions WHERE user_id = $1`, u.ID); c != 1 {
		t.Errorf("sessions left = %d, want 1 (the live one)", c)
	}
}

func TestRunPurgesAtStartup(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := pool.Exec(ctx, `INSERT INTO login_tokens (token_hash, email, throttle_key, created_at, expires_at)
		VALUES (sha256('old'::bytea), 'x@example.com', 'x@example.com', now() - interval '5 days', now() - interval '5 days' + interval '10 minutes')`); err != nil {
		t.Fatal(err)
	}
	// The service clock is the test clock (a moment after real now), so "24 hours ago" still covers 5 days back.
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for countRows(t, pool, `SELECT count(*) FROM login_tokens`) != 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("Run did not purge the expired link at startup")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}
