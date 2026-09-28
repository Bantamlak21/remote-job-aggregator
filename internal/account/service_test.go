package account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bantamlak12/remote-job-aggregator/internal/mailer"
)

// recordingMailer captures what the workers send. failFirst makes the first n
// sends fail.
type recordingMailer struct {
	mu        sync.Mutex
	failFirst int
	calls     int
	ch        chan mailer.Message
}

func newRecordingMailer() *recordingMailer {
	return &recordingMailer{ch: make(chan mailer.Message, 256)}
}

func (m *recordingMailer) Send(_ context.Context, msg mailer.Message) error {
	m.mu.Lock()
	m.calls++
	fail := m.calls <= m.failFirst
	m.mu.Unlock()
	if fail {
		return errors.New("smtp: 421 try later")
	}
	m.ch <- msg
	return nil
}

func (m *recordingMailer) next(t *testing.T) mailer.Message {
	t.Helper()
	select {
	case msg := <-m.ch:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no email was sent within 5s")
		return mailer.Message{}
	}
}

// syncBuffer is a bytes.Buffer safe for a logger used by several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const testBase = "https://jobs.example.com"

// newService builds a Service on the pool with every clock (store, service and all
// limiters) on one test clock. Zero limits in cfg take the production defaults, so
// tests that are not about limits pass generous ones through open().
func newService(t *testing.T, pool *pgxpool.Pool, m mailer.Mailer, cfg Config) (*Service, *testClock, *syncBuffer) {
	t.Helper()
	clock := newTestClock()
	logs := &syncBuffer{}
	if cfg.PublicBaseURL == "" {
		cfg.PublicBaseURL = testBase
	}
	s := NewService(NewStore(pool).WithClock(clock.Now), m, cfg, slog.New(slog.NewTextHandler(logs, nil)))
	s.now = clock.Now
	s.emailShort.WithClock(clock.Now)
	s.emailDay.WithClock(clock.Now)
	s.ipShort.WithClock(clock.Now)
	s.ipDay.WithClock(clock.Now)
	s.global.WithClock(clock.Now)
	s.verifyIP.WithClock(clock.Now)
	return s, clock, logs
}

// open returns a config whose limits are out of the way, for tests about something else.
func open() Config {
	return Config{LoginPerEmail15Min: 1000, LoginPerEmailDay: 1000, LoginPerIP15Min: 1000, LoginPerIPDay: 1000, LoginGlobalHour: 1000, VerifyPerIP15Min: 1000}
}

// take pops the message a RequestLogin queued (Run is not running in these tests).
func take(t *testing.T, s *Service) mailer.Message {
	t.Helper()
	select {
	case m := <-s.queue:
		return m
	default:
		t.Fatal("no message was queued")
		return mailer.Message{}
	}
}

var linkRe = regexp.MustCompile(`https?://[^\s"<>]+`)

// tokenOf returns the token in the sign-in link of a message.
func tokenOf(t *testing.T, m mailer.Message) string {
	t.Helper()
	links := linkRe.FindAllString(m.Text, -1)
	if len(links) != 1 {
		t.Fatalf("the email has %d links, want exactly 1: %q", len(links), m.Text)
	}
	u, err := url.Parse(links[0])
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimPrefix(u.Fragment, "token=")
	if tok == u.Fragment {
		t.Fatalf("the link %q has no #token= fragment", links[0])
	}
	return tok
}

func TestRequestLogin_SameResultForExistingAndNewAddress(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	ctx := context.Background()
	mustLogin(t, s.store, "known@example.com")

	errKnown := s.RequestLogin(ctx, "known@example.com", "192.0.2.1")
	errNew := s.RequestLogin(ctx, "brand-new@example.com", "192.0.2.1")
	if errKnown != nil || errNew != nil {
		t.Fatalf("RequestLogin = %v (existing), %v (new); want nil for both", errKnown, errNew)
	}
	known, fresh := take(t, s), take(t, s)
	// Same shape: same subject, same text once the address and token are masked out.
	mask := func(m mailer.Message) string {
		return strings.ReplaceAll(m.Text, tokenOf(t, m), "TOKEN")
	}
	if known.Subject != fresh.Subject || mask(known) != mask(fresh) {
		t.Errorf("the email differs between an existing and a new address:\n%q\n%q", mask(known), mask(fresh))
	}
	if strings.Contains(known.Text, "known@") || strings.Contains(known.Text, "account") {
		t.Errorf("the email body hints at whether an account exists or repeats the address: %q", known.Text)
	}
	for _, e := range []string{"known@example.com", "brand-new@example.com"} {
		if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE email = $1 AND used_at IS NULL`, e); n != 1 {
			t.Errorf("unused links for %s = %d, want 1", e, n)
		}
	}
	// Asking for a link creates no account: that happens when the link is used.
	if n := countRows(t, pool, `SELECT count(*) FROM users WHERE email = 'brand-new@example.com'`); n != 0 {
		t.Errorf("RequestLogin created an account for a new address (%d), it must only store a link", n)
	}
}

func TestRequestLogin_InvalidEmailStoresAndSendsNothing(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	for _, bad := range []string{"", "nope", "a@b", "a@example.com\r\nBcc: v@example.org", "\"a b\"@example.com", "é@example.com"} {
		if err := s.RequestLogin(context.Background(), bad, "192.0.2.1"); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("RequestLogin(%q) = %v, want ErrInvalidEmail", bad, err)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens`); n != 0 {
		t.Errorf("login_tokens = %d, want 0", n)
	}
	if len(s.queue) != 0 {
		t.Errorf("queued messages = %d, want 0", len(s.queue))
	}
}

func TestRequestLogin_NormalizesTheAddressBeforeStoringAndMailing(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	if err := s.RequestLogin(context.Background(), "  Mixed.Case@Example.COM ", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if m := take(t, s); m.To != "mixed.case@example.com" {
		t.Errorf("mailed to %q, want the normalized address", m.To)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE email = 'mixed.case@example.com'`); n != 1 {
		t.Errorf("stored link for the normalized address = %d, want 1", n)
	}
}

func TestRequestLogin_LinkIsFragmentOnPublicBaseURLOnly(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.PublicBaseURL = "https://jobs.example.com/" // a trailing slash must not double up
	cfg.SiteName = `Jobs & <Co> "Ltd"`
	s, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	if err := s.RequestLogin(context.Background(), "link@example.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	m := take(t, s)

	for name, body := range map[string]string{"text": m.Text, "html": m.HTML} {
		links := linkRe.FindAllString(body, -1)
		if len(links) != 1 {
			t.Fatalf("%s part has %d links, want 1: %q", name, len(links), body)
		}
		u, err := url.Parse(html.UnescapeString(links[0]))
		if err != nil {
			t.Fatal(err)
		}
		if u.Scheme != "https" || u.Host != "jobs.example.com" || u.Path != "/auth/verify" || u.RawQuery != "" {
			t.Errorf("%s link = %q, want https://jobs.example.com/auth/verify#token=...", name, u)
		}
		if !strings.HasPrefix(u.Fragment, "token=") || checkToken(strings.TrimPrefix(u.Fragment, "token=")) != nil {
			t.Errorf("%s link fragment = %q, want token=<43 chars>", name, u.Fragment)
		}
	}
	if tokenOf(t, m) == "" {
		t.Error("empty token")
	}
	if strings.Contains(m.Subject, tokenOf(t, m)) {
		t.Error("the token is in the subject line")
	}
	if strings.Contains(m.HTML, `Jobs & <Co>`) || !strings.Contains(m.HTML, "Jobs &amp; &lt;Co&gt; &quot;Ltd&quot;") {
		t.Errorf("the site name is not HTML-escaped in the HTML part: %q", m.HTML)
	}
	if !strings.Contains(m.Text, "10 minutes") {
		t.Errorf("the email does not state the 10 minute lifetime: %q", m.Text)
	}
}

func TestRequestLogin_TokenInEmailIsTheStoredOneAndOnlyItsDigestIsStored(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	if err := s.RequestLogin(context.Background(), "stored@example.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	tok := tokenOf(t, take(t, s))
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM login_tokens WHERE token_hash = $1`, hashToken(tok)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the digest of the mailed token is not stored: n=%d err=%v", n, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE encode(token_hash, 'escape') LIKE '%' || $1 || '%'`, tok); n != 0 {
		t.Error("the raw token is stored")
	}
}

func TestRequestLogin_RateLimitPerAddress(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginPerEmail15Min = 3
	s, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.RequestLogin(ctx, "spam@example.com", fmt.Sprintf("192.0.2.%d", i+1)); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	err := s.RequestLogin(ctx, "spam@example.com", "192.0.2.99") // a fresh IP: only the address is over
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Fatalf("4th request for one address = %v, want a RateLimitedError with RetryAfter > 0", err)
	}
	if err := s.RequestLogin(ctx, "other@example.com", "192.0.2.99"); err != nil {
		t.Errorf("another address was limited by spam@'s count: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE email = 'spam@example.com'`); n != 3 {
		t.Errorf("links stored for the limited address = %d, want 3 (a refused request stores nothing)", n)
	}
	if len(s.queue) != 4 { // 3 + other@
		t.Errorf("queued = %d, want 4 (a refused request sends nothing)", len(s.queue))
	}
}

func TestRequestLogin_RateLimitPerAddressPerDay(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginPerEmailDay = 2
	s, clock, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.RequestLogin(ctx, "day@example.com", "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
		clock.Advance(2 * time.Hour) // far apart: the short window never trips
	}
	var rl *RateLimitedError
	if err := s.RequestLogin(ctx, "day@example.com", "192.0.2.1"); !errors.As(err, &rl) {
		t.Fatalf("3rd request in a day = %v, want RateLimitedError", err)
	}
	clock.Advance(23 * time.Hour) // the first request is now more than a day old
	if err := s.RequestLogin(ctx, "day@example.com", "192.0.2.1"); err != nil {
		t.Errorf("after the day window moved on: %v", err)
	}
}

func TestRequestLogin_RateLimitPerIP(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginPerIP15Min = 3
	s, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.RequestLogin(ctx, fmt.Sprintf("victim%d@example.com", i), "198.51.100.7"); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	var rl *RateLimitedError
	if err := s.RequestLogin(ctx, "victim9@example.com", "198.51.100.7"); !errors.As(err, &rl) {
		t.Fatalf("4th request from one IP = %v, want RateLimitedError (email bombing of many addresses)", err)
	}
	if err := s.RequestLogin(ctx, "victim9@example.com", "198.51.100.8"); err != nil {
		t.Errorf("another IP was refused: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE email = 'victim9@example.com'`); n != 1 {
		t.Errorf("links stored for victim9 = %d, want 1 (only the allowed request)", n)
	}
}

func TestRequestLogin_RateLimitPerIPPerDay(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginPerIPDay = 2
	s, clock, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.RequestLogin(ctx, fmt.Sprintf("d%d@example.com", i), "198.51.100.7"); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Hour)
	}
	var rl *RateLimitedError
	if err := s.RequestLogin(ctx, "d9@example.com", "198.51.100.7"); !errors.As(err, &rl) {
		t.Fatalf("3rd request from one IP in a day = %v, want RateLimitedError", err)
	}
}

func TestRequestLogin_GlobalRateLimit(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginGlobalHour = 3
	s, clock, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.RequestLogin(ctx, fmt.Sprintf("g%d@example.com", i), fmt.Sprintf("203.0.113.%d", i)); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	var rl *RateLimitedError
	err := s.RequestLogin(ctx, "g9@example.com", "203.0.113.99")
	if !errors.As(err, &rl) {
		t.Fatalf("4th request in the hour from all sources = %v, want RateLimitedError", err)
	}
	clock.Advance(time.Hour + time.Second)
	if err := s.RequestLogin(ctx, "g9@example.com", "203.0.113.99"); err != nil {
		t.Errorf("after the hour: %v", err)
	}
}

func TestRequestLogin_PerAddressLimitSurvivesANewService(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.LoginPerEmail15Min = 3
	first, clock, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := first.RequestLogin(ctx, "restart@example.com", "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}

	// A restart, or a second server: fresh in-memory limiters on the same database.
	second := NewService(NewStore(pool).WithClock(clock.Now), newRecordingMailer(), func() Config { c := cfg; c.PublicBaseURL = testBase; return c }(), slog.New(slog.NewTextHandler(&syncBuffer{}, nil)))
	second.now = clock.Now
	var rl *RateLimitedError
	err := second.RequestLogin(ctx, "restart@example.com", "192.0.2.2")
	if !errors.As(err, &rl) {
		t.Fatalf("after a restart, 4th request = %v, want RateLimitedError from the database count", err)
	}
	if rl.RetryAfter != 15*time.Minute {
		t.Errorf("RetryAfter = %v, want the 15 minute window", rl.RetryAfter)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE email = 'restart@example.com'`); n != 3 {
		t.Errorf("links stored = %d, want 3", n)
	}

	clock.Advance(16 * time.Minute)
	if err := second.RequestLogin(ctx, "restart@example.com", "192.0.2.2"); err != nil {
		t.Errorf("after the window: %v", err)
	}
}

func TestRequestLogin_QueueFullIsErrBusyAndTheLinkStaysStored(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.MailQueue = 1
	s, _, _ := newService(t, pool, newRecordingMailer(), cfg) // Run is not started: nothing drains the queue
	ctx := context.Background()
	if err := s.RequestLogin(ctx, "first@example.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	err := s.RequestLogin(ctx, "second@example.com", "192.0.2.1")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("with a full queue RequestLogin = %v, want ErrBusy", err)
	}
	var rl *RateLimitedError
	if errors.As(err, &rl) {
		t.Error("ErrBusy is also a RateLimitedError: callers cannot tell the two apart")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE email = 'second@example.com' AND used_at IS NULL`); n != 1 {
		t.Errorf("stored links for the busy request = %d, want 1 (stored first, so a retry storm is still counted by the database limit)", n)
	}
	if len(s.queue) != 1 {
		t.Errorf("queue length = %d, want 1 (the second message is dropped, not queued)", len(s.queue))
	}
}

func TestBusyRequestsStillCountAgainstThePerAddressLimit(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.MailQueue = 1
	cfg.LoginPerEmail15Min = 2
	s, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	_ = s.RequestLogin(ctx, "fill@example.com", "192.0.2.1")
	if err := s.RequestLogin(ctx, "hammer@example.com", "192.0.2.1"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err := s.RequestLogin(ctx, "hammer@example.com", "192.0.2.1"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	var rl *RateLimitedError
	if err := s.RequestLogin(ctx, "hammer@example.com", "192.0.2.1"); !errors.As(err, &rl) {
		t.Errorf("3rd request = %v, want RateLimitedError: 503 answers must not let a client retry without limit", err)
	}
}

func TestInspect_DoesNotSpendTheLinkButVerifyDoes(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	ctx := context.Background()
	if err := s.RequestLogin(ctx, "Alice@Example.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	tok := tokenOf(t, take(t, s))

	for i := 0; i < 3; i++ { // a mail scanner, a preview, the page itself: any number of times
		masked, err := s.Inspect(ctx, tok, "192.0.2.5")
		if err != nil || masked != "a***@***.com" {
			t.Fatalf("Inspect #%d = %q, %v, want a***@***.com", i+1, masked, err)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE used_at IS NOT NULL`); n != 0 {
		t.Fatalf("Inspect spent the link (%d used)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("Inspect created an account (%d)", n)
	}

	sess, u, err := s.Verify(ctx, tok, "192.0.2.5", "")
	if err != nil || u.Email != "alice@example.com" || checkToken(sess) != nil {
		t.Fatalf("Verify = %q, %+v, %v", sess, u, err)
	}
	if _, err := s.Inspect(ctx, tok, "192.0.2.5"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Inspect after Verify = %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.Verify(ctx, tok, "192.0.2.5", ""); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("second Verify = %v, want ErrInvalidToken", err)
	}
	got, err := s.Authenticate(ctx, sess)
	if err != nil || got.ID != u.ID {
		t.Errorf("the session from Verify does not authenticate: %+v, %v", got, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("sessions = %d, want 1", n)
	}
}

func TestVerify_SessionLifetimeFollowsConfig(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.SessionTTL = 48 * time.Hour
	s, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	if err := s.RequestLogin(ctx, "ttl@example.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Verify(ctx, tokenOf(t, take(t, s)), "192.0.2.1", ""); err != nil {
		t.Fatal(err)
	}
	var d time.Duration
	if err := pool.QueryRow(ctx, `SELECT expires_at - created_at FROM sessions`).Scan(&d); err != nil || d != 48*time.Hour {
		t.Errorf("session lifetime = %v, %v, want 48h", d, err)
	}
}

func TestVerifyAndInspect_RejectMalformedTokensBeforeTheDatabase(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	ctx := context.Background()
	for _, bad := range []string{"", "short", strings.Repeat("A", 44), strings.Repeat("A", 42) + "\n", "' OR 1=1 --" + strings.Repeat("A", 32)} {
		if _, err := s.Inspect(ctx, bad, "192.0.2.1"); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Inspect(%q) = %v, want ErrInvalidToken", bad, err)
		}
		if _, _, err := s.Verify(ctx, bad, "192.0.2.1", ""); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Verify(%q) = %v, want ErrInvalidToken", bad, err)
		}
	}
	// A well-formed token nobody issued is the same error as a used one.
	tok, _, _ := newToken()
	if _, _, err := s.Verify(ctx, tok, "192.0.2.1", ""); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify(unissued) = %v, want ErrInvalidToken", err)
	}
}

func TestVerify_ExpiredLinkIsInvalidTokenLikeAnyOther(t *testing.T) {
	pool := newPool(t)
	s, clock, _ := newService(t, pool, newRecordingMailer(), open())
	ctx := context.Background()
	if err := s.RequestLogin(ctx, "late@example.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	tok := tokenOf(t, take(t, s))
	clock.Advance(10*time.Minute - time.Second)
	if _, err := s.Inspect(ctx, tok, "192.0.2.1"); err != nil {
		t.Fatalf("Inspect just before expiry = %v", err)
	}
	clock.Advance(time.Second)
	if _, err := s.Inspect(ctx, tok, "192.0.2.1"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Inspect at expiry = %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.Verify(ctx, tok, "192.0.2.1", ""); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify at expiry = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyAndInspect_RateLimitedPerIPTogether(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.VerifyPerIP15Min = 4
	s, clock, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	if err := s.RequestLogin(ctx, "guess@example.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	good := tokenOf(t, take(t, s))
	guess := strings.Repeat("A", 43)

	// Two Inspects and two Verifys use up the shared budget of one IP.
	for i := 0; i < 2; i++ {
		if _, err := s.Inspect(ctx, guess, "203.0.113.5"); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Inspect guess %d = %v", i+1, err)
		}
		if _, _, err := s.Verify(ctx, guess, "203.0.113.5", ""); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Verify guess %d = %v", i+1, err)
		}
	}
	var rl *RateLimitedError
	if _, err := s.Inspect(ctx, guess, "203.0.113.5"); !errors.As(err, &rl) {
		t.Errorf("Inspect over the limit = %v, want RateLimitedError", err)
	}
	if _, _, err := s.Verify(ctx, good, "203.0.113.5", ""); !errors.As(err, &rl) {
		t.Errorf("Verify over the limit (even with the right link) = %v, want RateLimitedError", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM login_tokens WHERE used_at IS NOT NULL`); n != 0 {
		t.Error("a rate-limited Verify spent the link")
	}
	// The limit is per IP: another address is unaffected, and the right link still works there.
	if _, _, err := s.Verify(ctx, good, "203.0.113.6", ""); err != nil {
		t.Errorf("Verify from another IP = %v, want success", err)
	}
	clock.Advance(15*time.Minute + time.Second)
	if _, err := s.Inspect(ctx, guess, "203.0.113.5"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Inspect after the window = %v, want the limit lifted", err)
	}
}

func TestAuthenticate_MalformedSessionTokenIsNoSession(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	for _, bad := range []string{"", "x", strings.Repeat("A", 500)} {
		if _, err := s.Authenticate(context.Background(), bad); !errors.Is(err, ErrNoSession) {
			t.Errorf("Authenticate(%q) = %v, want ErrNoSession", bad, err)
		}
	}
	if err := s.Logout(context.Background(), "garbage"); err != nil {
		t.Errorf("Logout(garbage) = %v, want nil", err)
	}
}

func TestService_LogoutLogoutEverywhereAndDelete(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	ctx := context.Background()
	signIn := func(email string) (string, User) {
		t.Helper()
		if err := s.RequestLogin(ctx, email, "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
		sess, u, err := s.Verify(ctx, tokenOf(t, take(t, s)), "192.0.2.1", "")
		if err != nil {
			t.Fatal(err)
		}
		return sess, u
	}
	s1, u := signIn("multi@example.com")
	s2, _ := signIn("multi@example.com")
	other, otherUser := signIn("other@example.com")

	if err := s.Logout(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, s1); !errors.Is(err, ErrNoSession) {
		t.Errorf("logged-out session = %v", err)
	}
	if _, err := s.Authenticate(ctx, s2); err != nil {
		t.Errorf("the other session of the user ended too: %v", err)
	}
	if err := s.LogoutEverywhere(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, s2); !errors.Is(err, ErrNoSession) {
		t.Errorf("session after LogoutEverywhere = %v", err)
	}
	if got, err := s.Authenticate(ctx, other); err != nil || got.ID != otherUser.ID {
		t.Errorf("another user's session = %v, %v", got, err)
	}
	if err := s.DeleteAccount(ctx, otherUser.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, other); !errors.Is(err, ErrNoSession) {
		t.Errorf("session of a deleted account = %v", err)
	}
	if err := s.DeleteAccount(ctx, otherUser.ID); !errors.Is(err, ErrNoUser) {
		t.Errorf("deleting twice = %v, want ErrNoUser", err)
	}
}

func TestService_BookmarksUseTheConfiguredLimit(t *testing.T) {
	pool := newPool(t)
	cfg := open()
	cfg.MaxBookmarks = 2
	s, _, _ := newService(t, pool, newRecordingMailer(), cfg)
	ctx := context.Background()
	u, _, _ := mustLogin(t, s.store, "marks@example.com")
	a, b, c := seedJob(t, pool, "open"), seedJob(t, pool, "open"), seedJob(t, pool, "open")

	for _, id := range []int64{a, b} {
		if err := s.Save(ctx, u.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(ctx, u.ID, c); !errors.Is(err, ErrBookmarkLimit) {
		t.Errorf("3rd save with a limit of 2 = %v, want ErrBookmarkLimit", err)
	}
	if ok, err := s.HasSaved(ctx, u.ID, a); err != nil || !ok {
		t.Errorf("HasSaved = %v, %v", ok, err)
	}
	ids, err := s.SavedIDs(ctx, u.ID)
	if err != nil || len(ids) != 2 {
		t.Errorf("SavedIDs = %v, %v", ids, err)
	}
	marks, total, err := s.Saved(ctx, u.ID, 10, 0)
	if err != nil || total != 2 || len(marks) != 2 {
		t.Errorf("Saved = %v, %d, %v", marks, total, err)
	}
	if err := s.Unsave(ctx, u.ID, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, u.ID, c); err != nil {
		t.Errorf("save after freeing a slot = %v", err)
	}
}

func TestConfig_ZeroValuesTakeTheDefaultsAndTheBaseURLLosesItsSlash(t *testing.T) {
	got := Config{PublicBaseURL: "https://x.example.com///"}.withDefaults()
	want := Defaults()
	want.PublicBaseURL = "https://x.example.com"
	if got != want {
		t.Errorf("withDefaults() = %+v\nwant %+v", got, want)
	}
	if want.LoginTokenTTL != 10*time.Minute || want.SessionTTL != 14*day || want.SessionMaxAge != 30*day || want.MaxBookmarks != 500 {
		t.Errorf("production defaults drifted from docs/accounts.md: %+v", want)
	}
	if want.LoginPerEmail15Min != 3 || want.LoginPerEmailDay != 10 || want.LoginPerIP15Min != 10 || want.LoginPerIPDay != 50 || want.LoginGlobalHour != 300 || want.VerifyPerIP15Min != 30 {
		t.Errorf("rate limit defaults drifted from docs/accounts.md: %+v", want)
	}
	custom := Config{MaxBookmarks: 7, LoginTokenTTL: time.Minute}.withDefaults()
	if custom.MaxBookmarks != 7 || custom.LoginTokenTTL != time.Minute {
		t.Errorf("withDefaults overwrote a set value: %+v", custom)
	}
}

func TestRateLimitedError_Message(t *testing.T) {
	e := &RateLimitedError{RetryAfter: 90*time.Second + 400*time.Millisecond}
	if got := e.Error(); !strings.Contains(got, "1m30s") {
		t.Errorf("Error() = %q, want the rounded wait", got)
	}
}

func TestRun_WorkersSendQueuedMail(t *testing.T) {
	pool := newPool(t)
	m := newRecordingMailer()
	s, _, _ := newService(t, pool, m, open())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()

	for _, e := range []string{"one@example.com", "two@example.com", "three@example.com"} {
		if err := s.RequestLogin(context.Background(), e, "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	for i := 0; i < 3; i++ {
		msg := m.next(t)
		got[msg.To] = true
		if checkToken(tokenOf(t, msg)) != nil {
			t.Errorf("the mailed token is malformed: %q", msg.Text)
		}
	}
	for _, e := range []string{"one@example.com", "two@example.com", "three@example.com"} {
		if !got[e] {
			t.Errorf("no email was sent to %s", e)
		}
	}
}

func TestRun_AFailedSendIsLoggedWithoutTheAddressAndTheWorkerCarriesOn(t *testing.T) {
	pool := newPool(t)
	m := newRecordingMailer()
	m.failFirst = 1
	cfg := open()
	cfg.MailWorkers = 1
	s, _, logs := newService(t, pool, m, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()

	if err := s.RequestLogin(context.Background(), "secretlocal@provider.example", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestLogin(context.Background(), "second@example.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if msg := m.next(t); msg.To != "second@example.com" {
		t.Fatalf("the message after a failure was sent to %q: the worker did not carry on", msg.To)
	}
	out := logs.String()
	if !strings.Contains(out, "sending a sign-in email failed") || !strings.Contains(out, "provider.example") {
		t.Errorf("the failure was not logged with the domain: %q", out)
	}
	if strings.Contains(out, "secretlocal") {
		t.Errorf("the log holds the person's address: %q", out)
	}
}

// blockingMailer holds every send until released, and records how many run at once.
type blockingMailer struct {
	inFlight, maxInFlight atomic.Int64
	started               chan struct{}
	release               chan struct{}
}

func (b *blockingMailer) Send(ctx context.Context, _ mailer.Message) error {
	n := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	for {
		old := b.maxInFlight.Load()
		if n <= old || b.maxInFlight.CompareAndSwap(old, n) {
			break
		}
	}
	b.started <- struct{}{}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRun_SendsAreBoundedByMailWorkers(t *testing.T) {
	pool := newPool(t)
	b := &blockingMailer{started: make(chan struct{}, 16), release: make(chan struct{})}
	cfg := open()
	cfg.MailWorkers = 2
	cfg.MailQueue = 10
	s, _, _ := newService(t, pool, b, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()

	for i := 0; i < 6; i++ {
		if err := s.RequestLogin(context.Background(), fmt.Sprintf("w%d@example.com", i), "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-b.started:
		case <-time.After(5 * time.Second):
			t.Fatal("the workers did not start sending")
		}
	}
	select {
	case <-b.started:
		t.Fatal("a third send started while two workers were busy")
	case <-time.After(200 * time.Millisecond):
	}
	if got := b.maxInFlight.Load(); got != 2 {
		t.Errorf("concurrent sends = %d, want 2", got)
	}
	close(b.release)
}

func TestRun_StopsOnContextCancelEvenMidSend(t *testing.T) {
	pool := newPool(t)
	b := &blockingMailer{started: make(chan struct{}, 16), release: make(chan struct{})}
	s, _, _ := newService(t, pool, b, open())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	if err := s.RequestLogin(context.Background(), "stuck@example.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("send did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled: a worker or the purge loop leaks")
	}
}

func TestRun_StopsWhenIdle(t *testing.T) {
	pool := newPool(t)
	s, _, _ := newService(t, pool, newRecordingMailer(), open())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an idle Run did not return after cancel")
	}
}
