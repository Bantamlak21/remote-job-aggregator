package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bantamlak12/remote-job-aggregator/internal/account"
	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/database"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/internal/mailer"
	"github.com/Bantamlak12/remote-job-aggregator/migrations"
)

// These DB helpers are duplicated from the other packages' tests on purpose (see
// internal/job/store_test.go): each DB-touching package resets the same schema, and
// `go test -p 1 ./...` serializes them.

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; run `docker compose up -d db` and export TEST_DATABASE_URL to run this test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL does not parse as a URL: %v", err)
	}
	if name := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(name, "_test") {
		t.Fatalf("TEST_DATABASE_URL points at database %q; these tests drop tables and must run against a database whose name ends in \"_test\"", name)
	}
	return raw
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := testDatabaseURL(t)
	if err := database.MigrateDown(context.Background(), dbURL, migrations.FS); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}
	if err := database.MigrateUp(context.Background(), dbURL, migrations.FS); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	t.Cleanup(func() {
		if err := database.MigrateDown(context.Background(), dbURL, migrations.FS); err != nil {
			t.Errorf("cleanup MigrateDown: %v", err)
		}
	})
	db, err := database.New(context.Background(), config.DatabaseConfig{
		URL: dbURL, MaxConns: 20, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(db.Close)
	return db.Pool
}

var seedCounter atomic.Int64

// seedJobSpec describes a job to insert directly.
type seedJobSpec struct {
	status       string        // open (default), closed, removed
	publishedAgo time.Duration // default 1 hour
	expiresIn    *time.Duration
}

func seedJob(t *testing.T, pool *pgxpool.Pool, spec seedJobSpec) int64 {
	t.Helper()
	ctx := context.Background()
	if spec.status == "" {
		spec.status = "open"
	}
	if spec.publishedAgo == 0 {
		spec.publishedAgo = time.Hour
	}
	n := seedCounter.Add(1)
	var companyID, targetID, id int64
	if err := pool.QueryRow(ctx, `INSERT INTO companies (name) VALUES ($1) RETURNING id`, fmt.Sprintf("Acme %d", n)).Scan(&companyID); err != nil {
		t.Fatalf("seeding a company: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO target_companies (company_id, ats_provider, external_board_id) VALUES ($1, 'greenhouse', $2) RETURNING id`,
		companyID, fmt.Sprintf("acme-%d", n)).Scan(&targetID); err != nil {
		t.Fatalf("seeding a target: %v", err)
	}
	var closedAt *time.Time
	if spec.status != "open" {
		now := time.Now()
		closedAt = &now
	}
	var expiresAt *time.Time
	if spec.expiresIn != nil {
		e := time.Now().Add(*spec.expiresIn)
		expiresAt = &e
	}
	err := pool.QueryRow(ctx, `INSERT INTO jobs
		(company_id, target_company_id, source, source_job_id, title, description, application_url, content_hash, status, closed_at, published_at, expires_at)
		VALUES ($1, $2, 'greenhouse', $3, $4, $5, 'https://example.com/apply', 'h', $6, $7, $8, $9) RETURNING id`,
		companyID, targetID, fmt.Sprintf("job-%d", n), fmt.Sprintf("Engineer %d", n), fmt.Sprintf("Description %d", n),
		spec.status, closedAt, time.Now().Add(-spec.publishedAgo), expiresAt).Scan(&id)
	if err != nil {
		t.Fatalf("seeding a job: %v", err)
	}
	return id
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// captureMailer hands every message to a channel the test reads.
type captureMailer struct{ ch chan mailer.Message }

func (c *captureMailer) Send(_ context.Context, m mailer.Message) error {
	c.ch <- m
	return nil
}

func (c *captureMailer) next(t *testing.T) mailer.Message {
	t.Helper()
	select {
	case m := <-c.ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no email arrived within 5s")
		return mailer.Message{}
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

const testOrigin = "https://jobs.example.com"

type envOpts struct {
	insecureCookie bool // plain "session" cookie, not Secure (development over http)
	sameSite       http.SameSite
	tune           func(*account.Config)
	noRun          bool // do not start the mail workers: nothing drains the queue
	clientIPHeader string
	maxJobAge      time.Duration
}

// testEnv is the whole stack on a real database: handler, account service, job store.
type testEnv struct {
	t          *testing.T
	h          http.Handler
	pool       *pgxpool.Pool
	mail       *captureMailer
	svc        *account.Service
	logs       *syncBuf
	origin     string
	cookieName string

	mu         sync.Mutex
	transcript []string // every response body and header (but Set-Cookie) the API produced
	secrets    []string // login and session tokens seen; none may appear in the transcript or logs
}

func newEnv(t *testing.T, o envOpts) *testEnv {
	t.Helper()
	pool := newPool(t)
	logs := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	cfg := account.Config{
		PublicBaseURL:      testOrigin,
		LoginPerEmail15Min: 1000, LoginPerEmailDay: 1000, LoginPerIP15Min: 1000, LoginPerIPDay: 1000, LoginGlobalHour: 1000, VerifyPerIP15Min: 1000,
	}
	if o.tune != nil {
		o.tune(&cfg)
	}
	m := &captureMailer{ch: make(chan mailer.Message, 128)}
	store := job.NewStore(pool)
	if o.maxJobAge > 0 {
		store = store.WithMaxAge(o.maxJobAge)
	}
	svc := account.NewService(account.NewStore(pool), m, cfg, logger).WithJobs(store)
	if !o.noRun {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); svc.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	cookie := CookieConfig{Secure: !o.insecureCookie, SameSite: o.sameSite}
	h := NewHandler(store, Config{
		Addr: ":0", CORSAllowedOrigin: testOrigin, Accounts: svc, Saved: store, Cookie: cookie,
		SessionTTL: svc.Config().SessionTTL, ClientIPHeader: o.clientIPHeader,
	}, logger)
	e := &testEnv{t: t, h: h, pool: pool, mail: m, svc: svc, logs: logs, origin: testOrigin, cookieName: cookie.name()}
	t.Cleanup(e.assertNoSecretsLeaked)
	return e
}

type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func withoutHeader(k string) reqOpt { return func(r *http.Request) { r.Header.Del(k) } }
func withCookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) {
		if c != nil {
			r.AddCookie(c)
		}
	}
}
func withRemote(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }

// withAddedHeader adds a second value for a header the request already has.
func withAddedHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Add(k, v) } }

// do sends a request through the handler. A state-changing request carries what the
// web app's own fetch sends by default: its Origin, and a JSON content type when there
// is a body (X-Requested-With when there is none). Options override any of it.
func (e *testEnv) do(method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		r.Header.Set("Origin", e.origin)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		} else {
			r.Header.Set("X-Requested-With", "app")
		}
	}
	for _, o := range opts {
		o(r)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	e.record(rec)
	return rec
}

func (e *testEnv) record(rec *httptest.ResponseRecorder) {
	var sb strings.Builder
	sb.WriteString(rec.Body.String())
	for k, vs := range rec.Header() {
		if k == "Set-Cookie" {
			continue
		}
		sb.WriteString("\n" + k + ": " + strings.Join(vs, ","))
	}
	e.mu.Lock()
	e.transcript = append(e.transcript, sb.String())
	e.mu.Unlock()
}

func (e *testEnv) addSecret(s string) {
	e.mu.Lock()
	e.secrets = append(e.secrets, s)
	e.mu.Unlock()
}

// leaks lists where a login token, a session token or a digest of either appears in
// any response body, header (other than the cookie that carries the session by
// design) or log line.
func (e *testEnv) leaks() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	logs := e.logs.String()
	var out []string
	for _, s := range e.secrets {
		sum := sha256.Sum256([]byte(s))
		forms := map[string]string{
			"the token": s, "its hex digest": hex.EncodeToString(sum[:]),
			"its base64 digest": base64.StdEncoding.EncodeToString(sum[:]), "its base64url digest": base64.RawURLEncoding.EncodeToString(sum[:]),
		}
		for what, needle := range forms {
			for _, tr := range e.transcript {
				if strings.Contains(tr, needle) {
					out = append(out, fmt.Sprintf("a response leaks %s of a secret: %.200s", what, tr))
				}
			}
			if strings.Contains(logs, needle) {
				out = append(out, fmt.Sprintf("the log leaks %s of a secret", what))
			}
		}
	}
	return out
}

// assertNoSecretsLeaked runs at the end of every test.
func (e *testEnv) assertNoSecretsLeaked() {
	for _, l := range e.leaks() {
		e.t.Error(l)
	}
}

func (e *testEnv) sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == e.cookieName {
			return c
		}
	}
	return nil
}

var tokenRe = regexp.MustCompile(`#token=([A-Za-z0-9_-]{43})`)

// signIn runs the whole flow (login, read the emailed link, inspect, verify) and
// returns the session cookie.
func (e *testEnv) signIn(email string) *http.Cookie {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/auth/login", fmt.Sprintf(`{"email":%q}`, email))
	if rec.Code != http.StatusAccepted {
		e.t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	m := e.mail.next(e.t)
	sub := tokenRe.FindStringSubmatch(m.Text)
	if sub == nil {
		e.t.Fatalf("no token in the email: %q", m.Text)
	}
	e.addSecret(sub[1])
	rec = e.do("POST", "/api/v1/auth/verify", fmt.Sprintf(`{"token":%q}`, sub[1]))
	if rec.Code != http.StatusOK {
		e.t.Fatalf("verify = %d %s", rec.Code, rec.Body)
	}
	c := e.sessionCookie(rec)
	if c == nil || c.Value == "" {
		e.t.Fatalf("verify set no session cookie; Set-Cookie = %v", rec.Header()["Set-Cookie"])
	}
	e.addSecret(c.Value)
	return c
}

// loginToken requests a link for the address and returns the token from the email.
func (e *testEnv) loginToken(email string) string {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/auth/login", fmt.Sprintf(`{"email":%q}`, email))
	if rec.Code != http.StatusAccepted {
		e.t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	sub := tokenRe.FindStringSubmatch(e.mail.next(e.t).Text)
	if sub == nil {
		e.t.Fatal("no token in the email")
	}
	e.addSecret(sub[1])
	return sub[1]
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

func idStr(id int64) string { return strconv.FormatInt(id, 10) }

func bookmarkPath(id int64) string { return "/api/v1/me/bookmarks/" + idStr(id) }

func jobIDs(list savedListResponse) []string {
	out := make([]string, len(list.Jobs))
	for i, j := range list.Jobs {
		out[i] = j.ID
	}
	return out
}
