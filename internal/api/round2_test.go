package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/account"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("not an error body: %q", rec.Body)
	}
	return b.Code
}

// F4: saving must not be a way to read a job the public cannot see.
func TestSave_ANonOwnerCannotSaveAJobThePublicCannotSee(t *testing.T) {
	e := newEnv(t, envOpts{maxJobAge: 15 * 24 * time.Hour})
	c := e.signIn("curious@example.com")
	aged := seedJob(t, e.pool, seedJobSpec{})
	expired := seedJob(t, e.pool, seedJobSpec{expiresIn: hours(2)})
	closed := seedJob(t, e.pool, seedJobSpec{})
	exec(t, e.pool, `UPDATE jobs SET published_at = now() - interval '40 days' WHERE id = $1`, aged)
	exec(t, e.pool, `UPDATE jobs SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired)
	exec(t, e.pool, `UPDATE jobs SET status = 'closed', closed_at = now() WHERE id = $1`, closed)

	for name, id := range map[string]int64{"aged out": aged, "past its deadline": expired, "closed": closed} {
		rec := e.do("PUT", bookmarkPath(id), "", withCookie(c))
		if rec.Code != http.StatusNotFound || codeOf(t, rec) != "job_unavailable" {
			t.Errorf("saving a job %s = %d %s, want 404 job_unavailable", name, rec.Code, rec.Body)
		}
		if rec := e.do("GET", "/api/v1/jobs/"+idStr(id), "", withCookie(c)); rec.Code != http.StatusNotFound {
			t.Errorf("after the refused save, detail of the job %s = %d, want 404 (no read through saving)", name, rec.Code)
		}
	}
	if n := count(t, e.pool, `SELECT count(*) FROM bookmarks`); n != 0 {
		t.Errorf("bookmarks = %d, want none", n)
	}
	if ids := decode[map[string][]string](t, e.do("GET", "/api/v1/me/bookmarks/ids", "", withCookie(c))); len(ids["ids"]) != 0 {
		t.Errorf("ids = %v", ids)
	}
}

// F24: the public detail has no `closed` field at all.
func TestPublicDetailHasNoClosedField(t *testing.T) {
	e := newEnv(t, envOpts{})
	id := seedJob(t, e.pool, seedJobSpec{})
	c := e.signIn("plain@example.com")
	for name, opts := range map[string][]reqOpt{"signed out": nil, "signed in": {withCookie(c)}} {
		rec := e.do("GET", "/api/v1/jobs/"+idStr(id), "", opts...)
		if rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		if _, has := m["closed"]; has {
			t.Errorf("%s: the public detail has a closed field: %s", name, rec.Body)
		}
	}
}

// The saved list's total is exactly what the list can show, whatever state each job is in.
func TestSavedList_TotalMatchesTheJobsShown(t *testing.T) {
	e := newEnv(t, envOpts{maxJobAge: 15 * 24 * time.Hour})
	c := e.signIn("total@example.com")
	ids := map[string]int64{}
	for _, name := range []string{"open", "closed", "removed", "aged", "expired"} {
		ids[name] = seedJob(t, e.pool, seedJobSpec{})
		if rec := e.do("PUT", bookmarkPath(ids[name]), "", withCookie(c)); rec.Code != 204 {
			t.Fatalf("save %s = %d", name, rec.Code)
		}
		time.Sleep(2 * time.Millisecond)
	}
	exec(t, e.pool, `UPDATE jobs SET status = 'closed', closed_at = now() WHERE id = $1`, ids["closed"])
	exec(t, e.pool, `UPDATE jobs SET status = 'removed', closed_at = now() WHERE id = $1`, ids["removed"])
	exec(t, e.pool, `UPDATE jobs SET published_at = now() - interval '60 days' WHERE id = $1`, ids["aged"])
	exec(t, e.pool, `UPDATE jobs SET expires_at = now() - interval '1 hour' WHERE id = $1`, ids["expired"])

	for _, size := range []int{2, 20} {
		var shown []string
		total := -1
		for page := 1; page <= 5; page++ {
			list := decode[savedListResponse](t, e.do("GET", fmt.Sprintf("/api/v1/me/bookmarks?page=%d&page_size=%d", page, size), "", withCookie(c)))
			total = list.Total
			shown = append(shown, jobIDs(list)...)
			if len(list.Jobs) == 0 {
				break
			}
		}
		if total != 5 || len(shown) != 5 {
			t.Errorf("page_size=%d: total %d, shown %d, want 5 and 5 (a bookmark never goes missing from the list)", size, total, len(shown))
		}
	}
	list := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks", "", withCookie(c)))
	closed := map[string]bool{}
	for _, j := range list.Jobs {
		closed[j.ID] = j.Closed
	}
	for name, want := range map[string]bool{"open": false, "closed": true, "removed": true, "aged": true, "expired": true} {
		if closed[idStr(ids[name])] != want {
			t.Errorf("%s: closed = %v, want %v", name, closed[idStr(ids[name])], want)
		}
	}
}

// F5: signing in with a session cookie already held ends that session.
func TestVerifyEndsTheSessionTheCallerAlreadyHolds(t *testing.T) {
	e := newEnv(t, envOpts{})
	old := e.signIn("fix@example.com")
	token := e.loginToken("fix@example.com")
	rec := e.do("POST", "/api/v1/auth/verify", fmt.Sprintf(`{"token":%q}`, token), withCookie(old))
	if rec.Code != 200 {
		t.Fatalf("verify = %d", rec.Code)
	}
	fresh := e.sessionCookie(rec)
	if fresh == nil || fresh.Value == old.Value {
		t.Fatalf("verify did not issue a new session value")
	}
	e.addSecret(fresh.Value)
	if rec := e.do("GET", "/api/v1/me", "", withCookie(old)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old cookie still works after a sign-in: %d", rec.Code)
	}
	if rec := e.do("GET", "/api/v1/me", "", withCookie(fresh)); rec.Code != 200 {
		t.Errorf("the new cookie does not work: %d", rec.Code)
	}
}

// F10: the cookie slides with the session.
func TestSessionCookieIsReissuedWhenTheSessionSlides(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("slide@example.com")

	// Just signed in: nothing to extend, so no Set-Cookie on every request.
	rec := e.do("GET", "/api/v1/me", "", withCookie(c))
	if rec.Code != 200 || e.sessionCookie(rec) != nil {
		t.Fatalf("a request right after sign-in = %d, Set-Cookie %v; want none", rec.Code, rec.Header()["Set-Cookie"])
	}

	// Used again on day 6: the idle window restarts, and the browser is told.
	exec(t, e.pool, `UPDATE sessions SET created_at = now() - interval '6 days', expires_at = now() + interval '8 days'`)
	rec = e.do("GET", "/api/v1/me", "", withCookie(c))
	if rec.Code != 200 {
		t.Fatalf("GET /me = %d", rec.Code)
	}
	renewed := e.sessionCookie(rec)
	if renewed == nil {
		t.Fatal("no Set-Cookie when the session was extended: the browser would drop the cookie at the old time")
	}
	if renewed.Value != c.Value || !renewed.HttpOnly || !renewed.Secure || renewed.Path != "/" {
		t.Errorf("renewed cookie = %+v, want the same value and the same flags", renewed)
	}
	if fortnight := 14 * 24 * 3600; renewed.MaxAge < fortnight-60 || renewed.MaxAge > fortnight {
		t.Errorf("renewed Max-Age = %d, want about 14 days (%d)", renewed.MaxAge, fortnight)
	}
	if left := count(t, e.pool, `SELECT extract(epoch FROM expires_at - now())::int FROM sessions`); left < 14*24*3600-60 {
		t.Errorf("session expires in %ds, want about 14 days", left)
	}

	// Near the absolute limit the cookie is capped to what is left, not a fresh 14 days.
	exec(t, e.pool, `UPDATE sessions SET created_at = now() - interval '29 days', expires_at = now() + interval '12 hours'`)
	rec = e.do("GET", "/api/v1/me", "", withCookie(c))
	capped := e.sessionCookie(rec)
	if capped == nil {
		t.Fatal("no Set-Cookie for a session extended up to its absolute limit")
	}
	if day := 24 * 3600; capped.MaxAge < day-120 || capped.MaxAge > day {
		t.Errorf("Max-Age near the absolute limit = %d, want about 1 day (%d)", capped.MaxAge, day)
	}
}

// F26: an oversized body is a 413 on every mutating route, including the ones that read none.
func TestOversizedBodyIs413OnEverySignedInRoute(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("bigbody@example.com")
	job1 := seedJob(t, e.pool, seedJobSpec{})
	big := strings.Repeat("x", 1<<20)
	for _, r := range []struct{ method, path string }{
		{"POST", "/api/v1/auth/logout"}, {"POST", "/api/v1/auth/logout-all"}, {"DELETE", "/api/v1/me"},
		{"PUT", bookmarkPath(job1)}, {"DELETE", bookmarkPath(job1)},
	} {
		start := time.Now()
		rec := e.do(r.method, r.path, `{"confirm":true,"pad":"`+big+`"}`, withCookie(c))
		if rec.Code != http.StatusRequestEntityTooLarge || codeOf(t, rec) != "body_too_large" {
			t.Errorf("%s %s with 1 MiB = %d %.80s, want 413 body_too_large", r.method, r.path, rec.Code, rec.Body)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s %s took %v", r.method, r.path, d)
		}
	}
	// Nothing happened: the session lives, the account exists.
	if rec := e.do("GET", "/api/v1/me", "", withCookie(c)); rec.Code != 200 {
		t.Errorf("a refused request changed the account: %d", rec.Code)
	}
}

// F16: stable codes.
func TestErrorBodiesCarryStableCodes(t *testing.T) {
	e := newEnv(t, envOpts{tune: func(c *account.Config) { c.MaxBookmarks = 1; c.LoginPerEmail15Min = 1 }})
	c := e.signIn("codes@example.com")
	j1, j2 := seedJob(t, e.pool, seedJobSpec{}), seedJob(t, e.pool, seedJobSpec{})
	e.do("PUT", bookmarkPath(j1), "", withCookie(c))

	rateLimited := e.do("POST", "/api/v1/auth/login", `{"email":"codes@example.com"}`) // the address already used its 1
	for name, tc := range map[string]struct {
		rec  *httptest.ResponseRecorder
		want string
	}{
		"invalid email":        {e.do("POST", "/api/v1/auth/login", `{"email":"nope"}`), "invalid_email"},
		"rate limited":         {rateLimited, "rate_limited"},
		"invalid link":         {e.do("POST", "/api/v1/auth/verify", `{"token":"`+strings.Repeat("A", 43)+`"}`), "invalid_link"},
		"invalid link inspect": {e.do("POST", "/api/v1/auth/inspect", `{"token":"x"}`), "invalid_link"},
		"unauthorized":         {e.do("GET", "/api/v1/me", ""), "unauthorized"},
		"bookmark limit":       {e.do("PUT", bookmarkPath(j2), "", withCookie(c)), "bookmark_limit"},
		"job unavailable":      {e.do("PUT", bookmarkPath(999999), "", withCookie(c)), "job_unavailable"},
		"cross origin":         {e.do("POST", "/api/v1/auth/login", `{"email":"a@b.co"}`, withHeader("Origin", "https://evil.example")), "cross_origin"},
		"unsupported media":    {e.do("POST", "/api/v1/auth/login", `x`, withHeader("Content-Type", "text/plain"), withHeader("X-Requested-With", "a")), "unsupported_media_type"},
		"body too large":       {e.do("POST", "/api/v1/auth/login", strings.Repeat(" ", 2000)+`{}`), "body_too_large"},
		"bad request":          {e.do("POST", "/api/v1/auth/login", `{"email":5}`), "bad_request"},
		"confirmation":         {e.do("DELETE", "/api/v1/me", "", withCookie(c)), "confirmation_required"},
	} {
		if got := codeOf(t, tc.rec); got != tc.want {
			t.Errorf("%s: code = %q (status %d, body %s), want %q", name, got, tc.rec.Code, tc.rec.Body, tc.want)
		}
	}
	if rateLimited.Code != 429 || rateLimited.Header().Get("Retry-After") == "" {
		t.Errorf("rate limited = %d, Retry-After %q", rateLimited.Code, rateLimited.Header().Get("Retry-After"))
	}
}

// F19: deleting the account needs {"confirm": true}.
func TestDeleteAccountNeedsConfirmation(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("sure@example.com")
	for name, tc := range map[string]struct {
		body string
		opts []reqOpt
	}{
		"no body":          {"", nil},
		"empty object":     {`{}`, nil},
		"confirm false":    {`{"confirm":false}`, nil},
		"confirm a string": {`{"confirm":"true"}`, nil},
		"confirm a number": {`{"confirm":1}`, nil},
		"another field":    {`{"confirm":true,"x":1}`, nil},
		"not JSON":         {`confirm=true`, []reqOpt{withHeader("Content-Type", "text/plain"), withHeader("X-Requested-With", "a")}},
	} {
		opts := append([]reqOpt{withCookie(c)}, tc.opts...)
		rec := e.do("DELETE", "/api/v1/me", tc.body, opts...)
		// S-24: every non-confirming body is confirmation_required, not some other 400 (a
		// generic "invalid request body" tells the caller nothing about what to fix).
		if rec.Code != http.StatusBadRequest || codeOf(t, rec) != "confirmation_required" {
			t.Errorf("%s: DELETE /me = %d %s, want 400 confirmation_required", name, rec.Code, rec.Body)
		}
		if n := count(t, e.pool, `SELECT count(*) FROM users WHERE email = 'sure@example.com'`); n != 1 {
			t.Fatalf("%s: the account was deleted without confirmation", name)
		}
	}
	// An oversized body is its own concern (413), never confused with a missing confirmation.
	big := `{"confirm":true,"padding":"` + strings.Repeat("x", 2000) + `"}`
	if rec := e.do("DELETE", "/api/v1/me", big, withCookie(c)); rec.Code != http.StatusRequestEntityTooLarge || codeOf(t, rec) != "body_too_large" {
		t.Errorf("oversized body = %d %s, want 413 body_too_large", rec.Code, rec.Body)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM users WHERE email = 'sure@example.com'`); n != 1 {
		t.Fatal("the account was deleted by an oversized body")
	}

	if rec := e.do("DELETE", "/api/v1/me", `{"confirm":true}`, withCookie(c)); rec.Code != http.StatusNoContent {
		t.Errorf("confirmed delete = %d %s", rec.Code, rec.Body)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM users`); n != 0 {
		t.Errorf("users = %d after a confirmed delete", n)
	}
}

// F20: a cookie that no longer works is cleared; no cookie means nothing to clear.
func TestRejectedSessionCookieIsCleared(t *testing.T) {
	e := newEnv(t, envOpts{})
	for name, cookie := range map[string]*http.Cookie{
		"garbage":     {Name: e.cookieName, Value: "garbage"},
		"well-formed": {Name: e.cookieName, Value: strings.Repeat("Q", 43)},
	} {
		rec := e.do("GET", "/api/v1/me", "", withCookie(cookie))
		if rec.Code != 401 {
			t.Fatalf("%s: GET /me = %d", name, rec.Code)
		}
		cleared := e.sessionCookie(rec)
		if cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" {
			t.Errorf("%s: Set-Cookie = %+v, want the cookie cleared", name, cleared)
		}
	}
	rec := e.do("GET", "/api/v1/me", "")
	if rec.Code != 401 || e.sessionCookie(rec) != nil {
		t.Errorf("no cookie: %d, Set-Cookie %v; want 401 and nothing to clear", rec.Code, rec.Header()["Set-Cookie"])
	}
}

// F22: a database outage is a 503, not "you were signed out".
func TestDatabaseOutageIsNotASignOut(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("outage@example.com")
	e.pool.Close()

	rec := e.do("GET", "/api/v1/me", "", withCookie(c))
	if rec.Code != http.StatusServiceUnavailable || codeOf(t, rec) != "internal_error" {
		t.Errorf("GET /me with the database down = %d %s, want 503 internal_error", rec.Code, rec.Body)
	}
	if e.sessionCookie(rec) != nil {
		t.Error("an outage cleared the session cookie: the person would be signed out for good")
	}
	for _, body := range []string{rec.Body.String()} {
		if strings.Contains(body, "pool") || strings.Contains(body, "closed") || strings.Contains(body, "pgx") {
			t.Errorf("the body leaks the cause: %s", body)
		}
	}
	if rec := e.do("PUT", bookmarkPath(1), "", withCookie(c)); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT with the database down = %d, want 503", rec.Code)
	}
}

// F13 / F23: baseline security headers everywhere, and no-store under /auth and /me even
// for responses no handler wrote.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("headers@example.com")
	for _, r := range []struct {
		method, path string
		noStore      bool
	}{
		{"GET", "/api/v1/jobs", false}, {"GET", "/api/v1/jobs/1", false}, {"GET", "/api/v1/jobs/abc", false},
		{"GET", "/healthz", false}, {"GET", "/readyz", false}, {"GET", "/nope", false},
		{"GET", "/api/v1/me", true}, {"GET", "/api/v1/me/bookmarks", true},
		{"PUT", "/api/v1/auth/login", true},        // 405: no handler for the method
		{"GET", "/api/v1/auth/nothing-here", true}, // 404 under /auth
		{"GET", "/api/v1/me/nothing-here", true},   // 404 under /me
		{"POST", "/api/v1/me/bookmarks/ids", true}, // 405
		{"OPTIONS", "/api/v1/auth/login", true},    // preflight
	} {
		rec := e.do(r.method, r.path, "", withCookie(c))
		h := rec.Header()
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" ||
			h.Get("Content-Security-Policy") != "frame-ancestors 'none'" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s (%d): headers = nosniff %q, referrer %q, csp %q, xfo %q", r.method, r.path, rec.Code,
				h.Get("X-Content-Type-Options"), h.Get("Referrer-Policy"), h.Get("Content-Security-Policy"), h.Get("X-Frame-Options"))
		}
		if r.noStore && (!strings.Contains(h.Get("Cache-Control"), "no-store") || !strings.Contains(strings.Join(h.Values("Vary"), ","), "Cookie")) {
			t.Errorf("%s %s (%d): Cache-Control %q, Vary %v, want no-store and Vary: Cookie", r.method, r.path, rec.Code, h.Get("Cache-Control"), h.Values("Vary"))
		}
	}
}

// F8: the site-wide cap raises one alert, the other limits do not.
func TestMailBreakerEventFiresForTheSiteWideCapOnly(t *testing.T) {
	e := newEnv(t, envOpts{tune: func(c *account.Config) { c.LoginGlobalHour = 2; c.LoginPerEmail15Min = 1 }})
	post := func(email string) int {
		return e.do("POST", "/api/v1/auth/login", fmt.Sprintf(`{"email":%q}`, email)).Code
	}
	if post("one@example.com") != 202 {
		t.Fatal("first request refused")
	}
	// A per-address limit: not the breaker.
	if post("one@example.com") != 429 {
		t.Fatal("second request for one address not limited")
	}
	if strings.Contains(e.logs.String(), "mail_breaker_open") {
		t.Fatal("a per-address limit raised the site-wide breaker event")
	}
	if post("two@example.com") != 202 {
		t.Fatal("second address refused early")
	}
	for i := 0; i < 5; i++ { // the site-wide cap of 2 is spent
		if code := post(fmt.Sprintf("more%d@example.com", i)); code != 429 {
			t.Fatalf("request past the site-wide cap = %d, want 429", code)
		}
	}
	if n := strings.Count(e.logs.String(), "event=mail_breaker_open"); n != 1 {
		t.Errorf("mail_breaker_open logged %d times for 5 refused requests, want once a minute", n)
	}
	if !strings.Contains(e.logs.String(), "level=WARN") {
		t.Error("the breaker event is not logged at warn level")
	}
}

// A request the client gave up on is not a server error.
type cancelledRepo struct{ job.Repository }

func (cancelledRepo) List(context.Context, job.Filter) (job.ListResult, error) {
	return job.ListResult{}, fmt.Errorf("querying: %w", context.Canceled)
}
func (cancelledRepo) Get(context.Context, string) (job.Job, error) {
	return job.Job{}, fmt.Errorf("querying: %w", context.Canceled)
}

func TestClientAbortIsNotLoggedAsAServerError(t *testing.T) {
	var logs bytes.Buffer
	h := NewHandler(cancelledRepo{}, Config{CORSAllowedOrigin: "http://localhost:5173"}, slog.New(slog.NewTextHandler(&logs, nil)))
	for _, path := range []string{"/api/v1/jobs", "/api/v1/jobs/5"} {
		logs.Reset()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil).WithContext(ctx))
		if rec.Body.Len() != 0 {
			t.Errorf("%s: wrote a body to a client that went away: %s", path, rec.Body)
		}
		out := logs.String()
		if strings.Contains(out, "level=ERROR") || strings.Contains(out, "status=500") {
			t.Errorf("%s: an aborted request was logged as a server error:\n%s", path, out)
		}
		if !strings.Contains(out, "status=499") {
			t.Errorf("%s: an aborted request is not logged as 499:\n%s", path, out)
		}
	}
	// A genuine failure is still a 500.
	logs.Reset()
	h2 := NewHandler(failingRepo{}, Config{CORSAllowedOrigin: "http://localhost:5173"}, slog.New(slog.NewTextHandler(&logs, nil)))
	rec := httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/jobs", nil))
	if rec.Code != 500 || !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a real failure = %d, log %s; want 500 and an error line", rec.Code, logs.String())
	}
}

type failingRepo struct{ job.Repository }

func (failingRepo) List(context.Context, job.Filter) (job.ListResult, error) {
	return job.ListResult{}, fmt.Errorf("boom")
}

// The session cookie name and flags follow configuration, never the request.
func TestSessionCookieFlagsFollowConfigurationNotTheRequest(t *testing.T) {
	e := newEnv(t, envOpts{})
	rec := e.do("POST", "/api/v1/auth/login", `{"email":"flags@example.com"}`, withHeader("X-Forwarded-Proto", "http"))
	if rec.Code != 202 {
		t.Fatal(rec.Code)
	}
	token := tokenRe.FindStringSubmatch(e.mail.next(t).Text)[1]
	e.addSecret(token)
	rec = e.do("POST", "/api/v1/auth/verify", fmt.Sprintf(`{"token":%q}`, token), withHeader("X-Forwarded-Proto", "http"))
	c := e.sessionCookie(rec)
	if c == nil || !c.Secure || c.Name != "__Host-session" || !c.HttpOnly {
		t.Errorf("cookie = %+v; a client-supplied X-Forwarded-Proto must not weaken it", c)
	}
	e.addSecret(c.Value)
}
