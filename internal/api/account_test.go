package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/account"
)

type userResponse struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

func TestSignIn_EndToEnd(t *testing.T) {
	e := newEnv(t, envOpts{})

	// Signed out.
	if rec := e.do("GET", "/api/v1/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /me without a session = %d, want 401", rec.Code)
	}

	// Ask for a link.
	rec := e.do("POST", "/api/v1/auth/login", `{"email":"  Alice@Example.COM "}`)
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != `{"status":"sent"}` {
		t.Fatalf("login = %d %s, want 202 {\"status\":\"sent\"}", rec.Code, rec.Body)
	}
	m := e.mail.next(t)
	if m.To != "alice@example.com" {
		t.Errorf("mailed to %q, want the normalized address", m.To)
	}
	token := tokenRe.FindStringSubmatch(m.Text)[1]
	e.addSecret(token)

	// Inspect twice: shows the masked address and spends nothing.
	for i := 0; i < 2; i++ {
		rec = e.do("POST", "/api/v1/auth/inspect", fmt.Sprintf(`{"token":%q}`, token))
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"email_masked":"a***@***.com"}` {
			t.Fatalf("inspect #%d = %d %s", i+1, rec.Code, rec.Body)
		}
		if e.sessionCookie(rec) != nil {
			t.Fatal("inspect set a session cookie")
		}
	}
	if rec := e.do("GET", "/api/v1/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("inspect signed the visitor in: /me = %d", rec.Code)
	}

	// Verify signs in.
	rec = e.do("POST", "/api/v1/auth/verify", fmt.Sprintf(`{"token":%q}`, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body)
	}
	got := decode[userResponse](t, rec)
	if got.User.Email != "alice@example.com" || got.User.ID == "" {
		t.Errorf("verify body = %s", rec.Body)
	}
	cookie := e.sessionCookie(rec)
	if cookie == nil || len(cookie.Value) != 43 {
		t.Fatalf("session cookie = %+v, want a 43 character token", cookie)
	}
	e.addSecret(cookie.Value)

	// The link is spent.
	for _, path := range []string{"/api/v1/auth/verify", "/api/v1/auth/inspect"} {
		rec = e.do("POST", path, fmt.Sprintf(`{"token":%q}`, token))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s with a used link = %d, want 400", path, rec.Code)
		}
	}

	// /me with the cookie.
	rec = e.do("GET", "/api/v1/me", "", withCookie(cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me = %d %s", rec.Code, rec.Body)
	}
	me := decode[userResponse](t, rec)
	if me != got {
		t.Errorf("/me = %+v, verify said %+v", me, got)
	}
	var raw map[string]map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if len(raw) != 1 || len(raw["user"]) != 2 {
		t.Errorf("/me body has more than {user:{id,email}}: %s", rec.Body)
	}

	// Logout ends it on the server, not only in the browser.
	rec = e.do("POST", "/api/v1/auth/logout", "", withCookie(cookie))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("logout = %d %q, want 204 with no body", rec.Code, rec.Body)
	}
	if c := e.sessionCookie(rec); c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("logout cookie = %+v, want an expired empty cookie", c)
	}
	if rec := e.do("GET", "/api/v1/me", "", withCookie(cookie)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old cookie still works after logout: %d", rec.Code)
	}
}

func TestLogin_AnswersTheSameForKnownAndNewAddresses(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.signIn("known@example.com")
	a := e.do("POST", "/api/v1/auth/login", `{"email":"known@example.com"}`)
	b := e.do("POST", "/api/v1/auth/login", `{"email":"never-seen@example.com"}`)
	if a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Errorf("known: %d %s\nnew: %d %s\nthe two answers must be identical (account enumeration)", a.Code, a.Body, b.Code, b.Body)
	}
	for _, k := range []string{"Content-Type", "Retry-After", "Set-Cookie"} {
		if a.Header().Get(k) != b.Header().Get(k) {
			t.Errorf("header %s differs: %q vs %q", k, a.Header().Get(k), b.Header().Get(k))
		}
	}
}

func TestLogin_InvalidEmailIs400(t *testing.T) {
	e := newEnv(t, envOpts{})
	for _, email := range []string{"", "nope", "a@b", "a@example.com\\r\\nBcc: x@y.z", "Alice <a@example.com>"} {
		rec := e.do("POST", "/api/v1/auth/login", fmt.Sprintf(`{"email":"%s"}`, email))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("login with %q = %d, want 400", email, rec.Code)
		}
	}
	if n := count(t, e.pool, `SELECT count(*) FROM login_tokens`); n != 0 {
		t.Errorf("login_tokens = %d, want 0", n)
	}
}

func TestLogin_RateLimitIs429WithRetryAfter(t *testing.T) {
	e := newEnv(t, envOpts{tune: func(c *account.Config) { c.LoginPerEmail15Min = 2 }})
	for i := 0; i < 2; i++ {
		if rec := e.do("POST", "/api/v1/auth/login", `{"email":"limited@example.com"}`); rec.Code != http.StatusAccepted {
			t.Fatalf("login %d = %d", i+1, rec.Code)
		}
	}
	rec := e.do("POST", "/api/v1/auth/login", `{"email":"limited@example.com"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd login = %d %s, want 429", rec.Code, rec.Body)
	}
	if n, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || n < 1 || n > 15*60+1 {
		t.Errorf("Retry-After = %q, want seconds up to 900", rec.Header().Get("Retry-After"))
	}
	// A browser's fetch() cannot read Retry-After on this cross-origin response unless
	// the server also lists it in Access-Control-Expose-Headers (on the real response,
	// not only on the OPTIONS preflight tested elsewhere).
	if !strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "Retry-After") {
		t.Errorf("Expose-Headers = %q, the SPA cannot read Retry-After without it", rec.Header().Get("Access-Control-Expose-Headers"))
	}
	if strings.Contains(rec.Body.String(), "limited@") {
		t.Errorf("the 429 body repeats the address: %s", rec.Body)
	}
	if rec := e.do("POST", "/api/v1/auth/login", `{"email":"other@example.com"}`); rec.Code != http.StatusAccepted {
		t.Errorf("another address = %d, want 202", rec.Code)
	}
}

func TestLogin_FullMailQueueIs503WithRetryAfter(t *testing.T) {
	e := newEnv(t, envOpts{noRun: true, tune: func(c *account.Config) { c.MailQueue = 1 }})
	if rec := e.do("POST", "/api/v1/auth/login", `{"email":"a@example.com"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("first login = %d", rec.Code)
	}
	rec := e.do("POST", "/api/v1/auth/login", `{"email":"b@example.com"}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
		t.Errorf("login with a full queue = %d, Retry-After %q; want 503 and 30", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestVerifyAndInspect_RateLimitIs429(t *testing.T) {
	e := newEnv(t, envOpts{tune: func(c *account.Config) { c.VerifyPerIP15Min = 3 }})
	guess := strings.Repeat("A", 43)
	for i, path := range []string{"/api/v1/auth/inspect", "/api/v1/auth/verify", "/api/v1/auth/inspect"} {
		if rec := e.do("POST", path, fmt.Sprintf(`{"token":%q}`, guess)); rec.Code != http.StatusBadRequest {
			t.Fatalf("guess %d at %s = %d, want 400", i+1, path, rec.Code)
		}
	}
	for _, path := range []string{"/api/v1/auth/inspect", "/api/v1/auth/verify"} {
		rec := e.do("POST", path, fmt.Sprintf(`{"token":%q}`, guess))
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s over the limit = %d, Retry-After %q; want 429 with Retry-After", path, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
}

func TestVerifyAndInspect_AllFailuresLookAlike(t *testing.T) {
	e := newEnv(t, envOpts{})
	used := e.loginToken("used@example.com")
	if rec := e.do("POST", "/api/v1/auth/verify", fmt.Sprintf(`{"token":%q}`, used)); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	expired := e.loginToken("expired@example.com")
	exec(t, e.pool, `UPDATE login_tokens SET created_at = now() - interval '20 minutes', expires_at = now() - interval '10 minutes' WHERE email = 'expired@example.com'`)

	cases := map[string]string{
		"used": used, "expired": expired, "never issued": strings.Repeat("B", 43), "too short": "abc", "empty": "", "sql": "' OR '1'='1",
	}
	for _, path := range []string{"/api/v1/auth/verify", "/api/v1/auth/inspect"} {
		var first string
		for name, tok := range cases {
			rec := e.do("POST", path, fmt.Sprintf(`{"token":%q}`, tok))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s with a %s token = %d, want 400", path, name, rec.Code)
			}
			if first == "" {
				first = rec.Body.String()
			} else if rec.Body.String() != first {
				t.Errorf("%s: the %s token gets a different answer %q than another (%q): the cause must not leak", path, name, rec.Body, first)
			}
			if e.sessionCookie(rec) != nil {
				t.Errorf("%s with a %s token set a cookie", path, name)
			}
		}
	}
	if n := count(t, e.pool, `SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("sessions = %d, want only the one from the successful sign-in", n)
	}
}

func TestSession_ExpiredOnTheServerIs401(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("old@example.com")
	exec(t, e.pool, `UPDATE sessions SET created_at = now() - interval '2 days', expires_at = now() - interval '1 hour'`)
	if rec := e.do("GET", "/api/v1/me", "", withCookie(c)); rec.Code != http.StatusUnauthorized {
		t.Errorf("/me with an expired session = %d, want 401", rec.Code)
	}
}

func TestLogout_IsIdempotentAndNeverFails(t *testing.T) {
	e := newEnv(t, envOpts{})
	for name, opt := range map[string]reqOpt{
		"no cookie":     func(*http.Request) {},
		"garbage":       withCookie(&http.Cookie{Name: e.cookieName, Value: "garbage"}),
		"never issued":  withCookie(&http.Cookie{Name: e.cookieName, Value: strings.Repeat("C", 43)}),
		"empty":         withCookie(&http.Cookie{Name: e.cookieName, Value: ""}),
		"foreign named": withCookie(&http.Cookie{Name: "session", Value: strings.Repeat("C", 43)}),
	} {
		rec := e.do("POST", "/api/v1/auth/logout", "", opt)
		if rec.Code != http.StatusNoContent {
			t.Errorf("logout (%s) = %d, want 204", name, rec.Code)
		}
	}
}

func TestLogoutAll_EndsEverySessionOfTheUserOnly(t *testing.T) {
	e := newEnv(t, envOpts{})
	phone, laptop := e.signIn("multi@example.com"), e.signIn("multi@example.com")
	other := e.signIn("someone@example.com")
	pending := e.loginToken("multi@example.com") // an unused link for the address

	if rec := e.do("POST", "/api/v1/auth/logout-all", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("logout-all without a session = %d, want 401", rec.Code)
	}
	rec := e.do("POST", "/api/v1/auth/logout-all", "", withCookie(phone))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout-all = %d %s", rec.Code, rec.Body)
	}
	if c := e.sessionCookie(rec); c == nil || c.MaxAge >= 0 {
		t.Errorf("logout-all cookie = %+v, want it cleared", c)
	}
	for name, c := range map[string]*http.Cookie{"phone": phone, "laptop": laptop} {
		if rec := e.do("GET", "/api/v1/me", "", withCookie(c)); rec.Code != http.StatusUnauthorized {
			t.Errorf("the %s session survived logout-all: %d", name, rec.Code)
		}
	}
	if rec := e.do("GET", "/api/v1/me", "", withCookie(other)); rec.Code != http.StatusOK {
		t.Errorf("another user's session was ended: %d", rec.Code)
	}
	if rec := e.do("POST", "/api/v1/auth/verify", fmt.Sprintf(`{"token":%q}`, pending)); rec.Code != http.StatusBadRequest {
		t.Errorf("a link requested before logout-all still works: %d (a stolen mailbox link must die too)", rec.Code)
	}
}

func TestDeleteAccount_ErasesEverythingAndTheAddressCanStartOver(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("bye@example.com")
	other := e.signIn("stays@example.com")
	jobID := seedJob(t, e.pool, seedJobSpec{})
	for _, cookie := range []*http.Cookie{c, other} {
		if rec := e.do("PUT", bookmarkPath(jobID), "", withCookie(cookie)); rec.Code != http.StatusNoContent {
			t.Fatalf("save = %d", rec.Code)
		}
	}
	first := decode[userResponse](t, e.do("GET", "/api/v1/me", "", withCookie(c)))

	rec := e.do("DELETE", "/api/v1/me", `{"confirm":true}`, withCookie(c))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /me = %d %s", rec.Code, rec.Body)
	}
	if cc := e.sessionCookie(rec); cc == nil || cc.MaxAge >= 0 {
		t.Errorf("delete cookie = %+v, want it cleared", cc)
	}
	if rec := e.do("GET", "/api/v1/me", "", withCookie(c)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the deleted account's session works: %d", rec.Code)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM users WHERE email = 'bye@example.com'`); n != 0 {
		t.Errorf("user rows = %d", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM login_tokens WHERE email = 'bye@example.com'`); n != 0 {
		t.Errorf("login_tokens rows = %d, want 0 (used links carry the address too)", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM bookmarks`); n != 1 {
		t.Errorf("bookmarks = %d, want only the other user's", n)
	}
	if rec := e.do("GET", "/api/v1/me", "", withCookie(other)); rec.Code != http.StatusOK {
		t.Errorf("another user was affected: %d", rec.Code)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM jobs`); n != 1 {
		t.Error("deleting an account touched a job")
	}

	// A new sign-in with the same address is a fresh account with nothing saved.
	c2 := e.signIn("bye@example.com")
	second := decode[userResponse](t, e.do("GET", "/api/v1/me", "", withCookie(c2)))
	if second.User.ID == first.User.ID {
		t.Errorf("the recreated account has the old id %s", second.User.ID)
	}
	ids := decode[map[string][]string](t, e.do("GET", "/api/v1/me/bookmarks/ids", "", withCookie(c2)))
	if len(ids["ids"]) != 0 {
		t.Errorf("the recreated account has saved jobs: %v", ids)
	}
}

func TestSignedInRoutesRequireASession(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("gone@example.com")
	e.do("POST", "/api/v1/auth/logout", "", withCookie(c))
	jobID := seedJob(t, e.pool, seedJobSpec{})
	routes := []struct{ method, path string }{
		{"GET", "/api/v1/me"}, {"DELETE", "/api/v1/me"}, {"POST", "/api/v1/auth/logout-all"},
		{"GET", "/api/v1/me/bookmarks"}, {"GET", "/api/v1/me/bookmarks/ids"},
		{"PUT", bookmarkPath(jobID)}, {"DELETE", bookmarkPath(jobID)},
	}
	cookies := map[string]reqOpt{
		"no cookie":        func(*http.Request) {},
		"logged out":       withCookie(c),
		"garbage":          withCookie(&http.Cookie{Name: e.cookieName, Value: "not-a-token"}),
		"well-formed fake": withCookie(&http.Cookie{Name: e.cookieName, Value: strings.Repeat("Z", 43)}),
		"wrong name":       withCookie(&http.Cookie{Name: "session", Value: c.Value}),
	}
	for _, r := range routes {
		for name, opt := range cookies {
			rec := e.do(r.method, r.path, "", opt)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s = %d, want 401", r.method, r.path, name, rec.Code)
			}
		}
	}
	if n := count(t, e.pool, `SELECT count(*) FROM bookmarks`); n != 0 {
		t.Errorf("an unauthenticated request created %d bookmarks", n)
	}
}

// ---- cookie ---------------------------------------------------------------------

func TestSessionCookie_Flags(t *testing.T) {
	cases := []struct {
		name     string
		opts     envOpts
		wantName string
		secure   bool
		sameSite http.SameSite
	}{
		{"secure lax (default)", envOpts{}, "__Host-session", true, http.SameSiteLaxMode},
		{"secure strict", envOpts{sameSite: http.SameSiteStrictMode}, "__Host-session", true, http.SameSiteStrictMode},
		{"secure none", envOpts{sameSite: http.SameSiteNoneMode}, "__Host-session", true, http.SameSiteNoneMode},
		{"development over http", envOpts{insecureCookie: true}, "session", false, http.SameSiteLaxMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts)
			token := e.loginToken("flags@example.com")
			rec := e.do("POST", "/api/v1/auth/verify", fmt.Sprintf(`{"token":%q}`, token))
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("verify set %d cookies, want 1", len(cookies))
			}
			c := cookies[0]
			e.addSecret(c.Value)
			if c.Name != tc.wantName {
				t.Errorf("name = %q, want %q", c.Name, tc.wantName)
			}
			if !c.HttpOnly {
				t.Error("cookie is not HttpOnly: page scripts could read the session")
			}
			if c.Secure != tc.secure {
				t.Errorf("Secure = %v, want %v", c.Secure, tc.secure)
			}
			if c.SameSite != tc.sameSite {
				t.Errorf("SameSite = %v, want %v", c.SameSite, tc.sameSite)
			}
			if c.Path != "/" {
				t.Errorf("Path = %q, want /", c.Path)
			}
			if c.Domain != "" {
				t.Errorf("Domain = %q, want none (host-only)", c.Domain)
			}
			if want := int(e.svc.Config().SessionTTL / time.Second); c.MaxAge != want {
				t.Errorf("MaxAge = %d, want %d (the session lifetime)", c.MaxAge, want)
			}
			raw := rec.Header().Get("Set-Cookie")
			if strings.Contains(raw, "Domain=") {
				t.Errorf("Set-Cookie carries a Domain: %s", raw)
			}
			if tc.secure && (!strings.Contains(raw, "; Secure") || !strings.Contains(raw, "; Path=/")) {
				t.Errorf("a __Host- cookie needs Secure and Path=/ in the header: %s", raw)
			}
			if !strings.Contains(raw, "HttpOnly") {
				t.Errorf("Set-Cookie lacks HttpOnly: %s", raw)
			}
			if len(c.Value) != 43 {
				t.Errorf("cookie value/lifetime malformed: %s", raw)
			}

			// The cookie works when sent back under its name, and only under that name.
			if rec := e.do("GET", "/api/v1/me", "", withCookie(c)); rec.Code != http.StatusOK {
				t.Errorf("/me with the cookie = %d", rec.Code)
			}
			other := "session"
			if !tc.secure {
				other = "__Host-session"
			}
			if rec := e.do("GET", "/api/v1/me", "", withCookie(&http.Cookie{Name: other, Value: c.Value})); rec.Code != http.StatusUnauthorized {
				t.Errorf("/me with the session under the name %q = %d, want 401 (a planted cookie without the __Host- prefix must not count)", other, rec.Code)
			}

			// Clearing keeps the same attributes so the browser matches and removes the cookie.
			out := e.do("POST", "/api/v1/auth/logout", "", withCookie(c))
			cleared := e.sessionCookie(out)
			if cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" || !cleared.HttpOnly || cleared.Secure != tc.secure || cleared.Path != "/" || cleared.Domain != "" {
				t.Errorf("clearing cookie = %+v (%s)", cleared, out.Header().Get("Set-Cookie"))
			}
		})
	}
}

// ---- response headers ---------------------------------------------------------------

func TestAccountResponsesAreNeverCachedAndNotSniffed(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("cache@example.com")
	jobID := seedJob(t, e.pool, seedJobSpec{})
	token := e.loginToken("cache2@example.com")
	reqs := []struct {
		name, method, path, body string
		opts                     []reqOpt
		wantCode                 int
	}{
		{"login ok", "POST", "/api/v1/auth/login", `{"email":"x@example.com"}`, nil, 202},
		{"login bad email", "POST", "/api/v1/auth/login", `{"email":"x"}`, nil, 400},
		{"login bad body", "POST", "/api/v1/auth/login", `{`, nil, 400},
		{"login wrong media type", "POST", "/api/v1/auth/login", `x`, []reqOpt{withHeader("Content-Type", "text/plain"), withHeader("X-Requested-With", "app")}, 415},
		{"inspect ok", "POST", "/api/v1/auth/inspect", fmt.Sprintf(`{"token":%q}`, token), nil, 200},
		{"inspect bad", "POST", "/api/v1/auth/inspect", `{"token":"x"}`, nil, 400},
		{"verify bad", "POST", "/api/v1/auth/verify", `{"token":"x"}`, nil, 400},
		{"verify ok (sets the cookie)", "POST", "/api/v1/auth/verify", fmt.Sprintf(`{"token":%q}`, token), nil, 200},
		{"me ok", "GET", "/api/v1/me", "", []reqOpt{withCookie(c)}, 200},
		{"me signed out", "GET", "/api/v1/me", "", nil, 401},
		{"ids", "GET", "/api/v1/me/bookmarks/ids", "", []reqOpt{withCookie(c)}, 200},
		{"list", "GET", "/api/v1/me/bookmarks", "", []reqOpt{withCookie(c)}, 200},
		{"list bad page", "GET", "/api/v1/me/bookmarks?page=0", "", []reqOpt{withCookie(c)}, 400},
		{"save", "PUT", bookmarkPath(jobID), "", []reqOpt{withCookie(c)}, 204},
		{"save missing", "PUT", bookmarkPath(999999), "", []reqOpt{withCookie(c)}, 404},
		{"unsave", "DELETE", bookmarkPath(jobID), "", []reqOpt{withCookie(c)}, 204},
		{"logout", "POST", "/api/v1/auth/logout", "", []reqOpt{withCookie(c)}, 204},
		{"logout-all signed out", "POST", "/api/v1/auth/logout-all", "", nil, 401},
	}
	for _, r := range reqs {
		rec := e.do(r.method, r.path, r.body, r.opts...)
		if rec.Code != r.wantCode {
			t.Errorf("%s = %d, want %d (%s)", r.name, rec.Code, r.wantCode, rec.Body)
			continue
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", r.name, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", r.name, got)
		}
		if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Cookie") {
			t.Errorf("%s: Vary = %v, want Cookie", r.name, rec.Header().Values("Vary"))
		}
		if rec.Body.Len() > 0 && !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: Content-Type = %q", r.name, rec.Header().Get("Content-Type"))
		}
	}
}

// ---- request bodies ---------------------------------------------------------------

func TestLogin_BodyRules(t *testing.T) {
	e := newEnv(t, envOpts{})
	ok := `{"email":"body@example.com"}`
	pad := func(total int) string { // whitespace before the value, so the decoder must read it all
		return `{"email":` + strings.Repeat(" ", total-len(ok)) + `"body@example.com"}`
	}
	cases := []struct {
		name string
		body string
		opts []reqOpt
		want int
	}{
		{"valid", ok, nil, 202},
		{"content type with charset", ok, []reqOpt{withHeader("Content-Type", "application/json; charset=utf-8")}, 202},
		{"content type in upper case", ok, []reqOpt{withHeader("Content-Type", "Application/JSON")}, 202},
		{"exactly 1024 bytes", pad(1024), nil, 202},
		{"1025 bytes", pad(1025), nil, 413},
		{"4097 bytes", pad(4097), nil, 413},
		{"64 KiB", pad(64 << 10), nil, 413},
		{"unknown field", `{"email":"body@example.com","admin":true}`, nil, 400},
		{"unknown field first", `{"role":"admin","email":"body@example.com"}`, nil, 400},
		{"two documents", ok + ok, nil, 400},
		{"trailing garbage", ok + " x", nil, 400},
		{"a stray closing brace after the document", ok + "}", nil, 400}, // S-91: must not be mailed
		{"a stray closing bracket after the document", ok + "]", nil, 400},
		{"trailing whitespace only", ok + " \n\t ", nil, 202},
		{"array", `["body@example.com"]`, nil, 400},
		{"string", `"body@example.com"`, nil, 400},
		{"email is a number", `{"email":5}`, nil, 400},
		{"email is an object", `{"email":{"a":1}}`, nil, 400},
		{"null document", `null`, nil, 400}, // decodes to an empty address: invalid email
		{"truncated", `{"email":"body@exam`, nil, 400},
		{"not JSON", `email=body@example.com`, nil, 400},
		{"form content type (with the header that passes CSRF)", `email=body@example.com`, []reqOpt{withHeader("Content-Type", "application/x-www-form-urlencoded"), withHeader("X-Requested-With", "app")}, 415},
		{"text/plain", ok, []reqOpt{withHeader("Content-Type", "text/plain"), withHeader("X-Requested-With", "app")}, 415},
		{"xml", `<a/>`, []reqOpt{withHeader("Content-Type", "application/xml"), withHeader("X-Requested-With", "app")}, 415},
		{"no content type", ok, []reqOpt{withoutHeader("Content-Type"), withHeader("X-Requested-With", "app")}, 415},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do("POST", "/api/v1/auth/login", tc.body, tc.opts...)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body %.120s", rec.Code, tc.want, rec.Body)
			}
		})
	}
	// Only the two valid-address requests that were accepted stored anything.
	want := 5 // valid, charset, upper case, 1024, trailing whitespace
	if n := count(t, e.pool, `SELECT count(*) FROM login_tokens`); n != want {
		t.Errorf("login_tokens = %d, want %d: a refused body must not create a link", n, want)
	}
}

func TestInspectAndVerify_BodyRules(t *testing.T) {
	e := newEnv(t, envOpts{})
	tok := strings.Repeat("A", 43)
	for _, path := range []string{"/api/v1/auth/inspect", "/api/v1/auth/verify"} {
		for name, tc := range map[string]struct {
			body string
			opts []reqOpt
			want int
		}{
			"unknown field":  {`{"token":"` + tok + `","x":1}`, nil, 400},
			"token a number": {`{"token":1}`, nil, 400},
			"text/plain":     {`{"token":"` + tok + `"}`, []reqOpt{withHeader("Content-Type", "text/plain"), withHeader("X-Requested-With", "app")}, 415},
			"oversized":      {`{"token":"` + strings.Repeat("A", 5000) + `"}`, nil, 413},
			"well-formed":    {`{"token":"` + tok + `"}`, nil, 400}, // valid request, unknown link
		} {
			if rec := e.do("POST", path, tc.body, tc.opts...); rec.Code != tc.want {
				t.Errorf("%s (%s) = %d, want %d", path, name, rec.Code, tc.want)
			}
		}
	}
}

// ---- client address for rate limits -----------------------------------------------

func TestClientIP_TheHeaderIsTrustedOnlyWhenConfigured(t *testing.T) {
	login := func(e *testEnv, n int, hdr string) int {
		opts := []reqOpt{withRemote("192.0.2.1:4000")}
		if hdr != "" {
			opts = append(opts, withHeader("X-Real-IP", hdr))
		}
		return e.do("POST", "/api/v1/auth/login", fmt.Sprintf(`{"email":"ip%d@example.com"}`, n), opts...).Code
	}
	ipLimit := func(c *account.Config) { c.LoginPerIP15Min = 2 }

	t.Run("header not configured: a client cannot pick its own key", func(t *testing.T) {
		e := newEnv(t, envOpts{tune: ipLimit})
		got := []int{login(e, 1, "198.51.100.1"), login(e, 2, "198.51.100.2"), login(e, 3, "198.51.100.3")}
		if got[0] != 202 || got[1] != 202 || got[2] != 429 {
			t.Errorf("statuses = %v, want [202 202 429]: a spoofed X-Real-IP must not create new buckets", got)
		}
	})
	t.Run("header configured: each value is its own bucket", func(t *testing.T) {
		e := newEnv(t, envOpts{tune: ipLimit, clientIPHeader: "X-Real-IP"})
		got := []int{
			login(e, 1, "198.51.100.1"), login(e, 2, "198.51.100.2"), login(e, 3, "198.51.100.3"),
			login(e, 4, "198.51.100.1"), login(e, 5, "198.51.100.1"),
		}
		want := []int{202, 202, 202, 202, 429}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("statuses = %v, want %v", got, want)
				break
			}
		}
	})
	t.Run("X-Real-IP is one value: a list or junk falls back to the connection's address", func(t *testing.T) {
		e := newEnv(t, envOpts{tune: ipLimit, clientIPHeader: "X-Real-IP"})
		got := []int{
			login(e, 1, "198.51.100.9, 10.0.0.1"), login(e, 2, "not-an-ip"), login(e, 3, ""), // one bucket: the connection's address
			login(e, 4, "198.51.100.9"), login(e, 5, "198.51.100.9"), login(e, 6, "198.51.100.9"), // another: .9
		}
		want := []int{202, 202, 429, 202, 202, 429}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("statuses = %v, want %v", got, want)
				break
			}
		}
	})
	// S-24: behind one proxy that APPENDS to X-Forwarded-For, only the last entry is the
	// proxy's; everything before it is whatever the client wrote and must not choose the bucket.
	t.Run("X-Forwarded-For: the last entry is the client, the ones before it are spoofable", func(t *testing.T) {
		e := newEnv(t, envOpts{tune: ipLimit, clientIPHeader: "X-Forwarded-For"})
		post := func(n int, xff string) int {
			return e.do("POST", "/api/v1/auth/login", fmt.Sprintf(`{"email":"xf%d@example.com"}`, n),
				withRemote("192.0.2.1:4000"), withHeader("X-Forwarded-For", xff)).Code
		}
		got := []int{
			post(1, "6.6.6.1, 198.51.100.7"), post(2, "6.6.6.2, 198.51.100.7"), post(3, "6.6.6.3, 198.51.100.7"), // spoofed leftmost: one bucket
			post(4, "6.6.6.1, 203.0.113.5"), // a different real client
		}
		want := []int{202, 202, 429, 202}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("statuses = %v, want %v: the leftmost entry chose the rate-limit bucket", got, want)
				break
			}
		}
	})
	// S-24 / AC-08: one subscriber holds a whole /64, so every address in it is one client.
	t.Run("IPv6 addresses in one /64 share a bucket, other /64s do not", func(t *testing.T) {
		e := newEnv(t, envOpts{tune: ipLimit})
		post := func(n int, remote string) int {
			return e.do("POST", "/api/v1/auth/login", fmt.Sprintf(`{"email":"v6-%d@example.com"}`, n), withRemote(remote)).Code
		}
		got := []int{
			post(1, "[2001:db8:1:2::1]:4000"), post(2, "[2001:db8:1:2:ffff:ffff:ffff:ffff]:4000"), post(3, "[2001:db8:1:2:1234::9]:4000"),
			post(4, "[2001:db8:1:3::1]:4000"),
		}
		want := []int{202, 202, 429, 202}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("statuses = %v, want %v", got, want)
				break
			}
		}
	})
}

func TestFoldIP(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7":            "192.0.2.7",
		"::ffff:192.0.2.7":     "192.0.2.7",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::",
		"2001:db8:1:2:ffff::1": "2001:db8:1:2::",
		"2001:db8:1:3::1":      "2001:db8:1:3::",
		"fe80::1":              "fe80::",
	} {
		if got := foldIP(net.ParseIP(in)); got != want {
			t.Errorf("foldIP(%s) = %s, want %s", in, got, want)
		}
	}
}

// ---- nothing leaks ------------------------------------------------------------------

func TestResponsesNeverContainTokensOrDigests(t *testing.T) {
	// assertNoSecretsLeaked also runs after every other test on its whole transcript;
	// this one drives every endpoint and proves the checker can see a leak.
	e := newEnv(t, envOpts{})
	c := e.signIn("leak@example.com")
	jobID := seedJob(t, e.pool, seedJobSpec{})
	e.do("PUT", bookmarkPath(jobID), "", withCookie(c))
	e.do("GET", "/api/v1/me", "", withCookie(c))
	e.do("GET", "/api/v1/me/bookmarks", "", withCookie(c))
	e.do("GET", "/api/v1/me/bookmarks/ids", "", withCookie(c))
	e.do("GET", "/api/v1/jobs/"+idStr(jobID), "", withCookie(c))
	e.do("POST", "/api/v1/auth/logout-all", "", withCookie(c))

	e.mu.Lock()
	tracked := len(e.secrets)
	e.mu.Unlock()
	if tracked != 2 {
		t.Fatalf("secrets tracked = %d, want the login token and the session token", tracked)
	}
	if leaks := e.leaks(); len(leaks) != 0 {
		t.Fatalf("leaks: %v", leaks)
	}

	// A planted token, hex digest and log line must each be caught.
	e.mu.Lock()
	secret := e.secrets[0]
	e.transcript = append(e.transcript, "planted "+secret)
	e.mu.Unlock()
	if len(e.leaks()) == 0 {
		t.Error("the checker missed a token in a response")
	}
	e.mu.Lock()
	e.transcript = e.transcript[:len(e.transcript)-1]
	e.mu.Unlock()
	_, _ = e.logs.Write([]byte("planted " + secret))
	if len(e.leaks()) == 0 {
		t.Error("the checker missed a token in the log")
	}
	e.mu.Lock()
	e.secrets = nil // the planted log line stays in the log buffer; nothing is left to check
	e.mu.Unlock()
}
