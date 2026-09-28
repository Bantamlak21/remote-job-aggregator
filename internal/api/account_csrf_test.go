package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

func TestAccountsOff_NoAccountRoutesAndNoCredentials(t *testing.T) {
	h := testHandler(t, job.NewMockRepository())
	for _, r := range []struct{ method, path string }{
		{"POST", "/api/v1/auth/login"}, {"POST", "/api/v1/auth/verify"}, {"POST", "/api/v1/auth/inspect"},
		{"POST", "/api/v1/auth/logout"}, {"POST", "/api/v1/auth/logout-all"},
		{"GET", "/api/v1/me"}, {"DELETE", "/api/v1/me"}, {"GET", "/api/v1/me/bookmarks"}, {"PUT", "/api/v1/me/bookmarks/1"},
	} {
		req := httptest.NewRequest(r.method, r.path, strings.NewReader(`{}`))
		req.Header.Set("Origin", "http://localhost:5173")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s with sign-in off = %d, want 404", r.method, r.path, rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Errorf("public list = %d, ACAO %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if v := rec.Header().Get("Access-Control-Allow-Credentials"); v != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q with sign-in off, want none", v)
	}
}

func TestAccountsOn_ThePublicListStillWorksAndSetsNoCookie(t *testing.T) {
	e := newEnv(t, envOpts{})
	id := seedJob(t, e.pool, seedJobSpec{})
	rec := e.do("GET", "/api/v1/jobs", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"`+idStr(id)+`"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("the public list set a cookie")
	}
}

// ---- CSRF -------------------------------------------------------------------------

type csrfEndpoint struct {
	name, method, path string
	body               string // JSON body, "" for none
	signedIn           bool
}

func csrfEndpoints(jobID int64, linkToken string) []csrfEndpoint {
	return []csrfEndpoint{
		{"login", "POST", "/api/v1/auth/login", `{"email":"victim@example.com"}`, false},
		{"inspect", "POST", "/api/v1/auth/inspect", fmt.Sprintf(`{"token":%q}`, linkToken), false},
		{"verify", "POST", "/api/v1/auth/verify", fmt.Sprintf(`{"token":%q}`, linkToken), false},
		{"logout", "POST", "/api/v1/auth/logout", "", true},
		{"logout-all", "POST", "/api/v1/auth/logout-all", "", true},
		{"delete account", "DELETE", "/api/v1/me", `{"confirm":true}`, true},
		{"save a job", "PUT", bookmarkPath(jobID), "", true},
		{"unsave a job", "DELETE", bookmarkPath(jobID), "", true},
	}
}

func TestCSRF_ForgedRequestsAreRefusedAndChangeNothing(t *testing.T) {
	e := newEnv(t, envOpts{})
	cookie := e.signIn("victim-account@example.com")
	jobID := seedJob(t, e.pool, seedJobSpec{})
	token := e.loginToken("victim-link@example.com")
	// Something for the forged unsave to remove.
	if rec := e.do("PUT", bookmarkPath(jobID), "", withCookie(cookie)); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}

	attacks := []struct {
		name string
		mod  func(ep csrfEndpoint) []reqOpt
	}{
		{"another site's Origin", func(csrfEndpoint) []reqOpt { return []reqOpt{withHeader("Origin", "https://evil.example")} }},
		{"Origin null (sandboxed iframe, redirect)", func(csrfEndpoint) []reqOpt { return []reqOpt{withHeader("Origin", "null")} }},
		{"a subdomain of the app", func(csrfEndpoint) []reqOpt { return []reqOpt{withHeader("Origin", "https://evil.jobs.example.com")} }},
		{"a host that starts with the app's", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withHeader("Origin", "https://jobs.example.com.evil.example")}
		}},
		{"the app's host on http", func(csrfEndpoint) []reqOpt { return []reqOpt{withHeader("Origin", "http://jobs.example.com")} }},
		{"the app's host on another port", func(csrfEndpoint) []reqOpt { return []reqOpt{withHeader("Origin", "https://jobs.example.com:8443")} }},
		{"the app's Origin with a trailing slash", func(csrfEndpoint) []reqOpt { return []reqOpt{withHeader("Origin", "https://jobs.example.com/")} }},
		{"Origin in another case", func(csrfEndpoint) []reqOpt { return []reqOpt{withHeader("Origin", "https://JOBS.example.com")} }},
		{"no Origin, browser says cross-site", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin"), withHeader("Sec-Fetch-Site", "cross-site")}
		}},
		{"no Origin, browser says same-site (a sibling subdomain)", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin"), withHeader("Sec-Fetch-Site", "same-site")}
		}},
		{"an HTML form post (urlencoded), no Origin", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin"), withHeader("Content-Type", "application/x-www-form-urlencoded"), withoutHeader("X-Requested-With")}
		}},
		{"an HTML form post (multipart), no Origin", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin"), withHeader("Content-Type", "multipart/form-data; boundary=x"), withoutHeader("X-Requested-With")}
		}},
		{"a text/plain post (the no-preflight JSON trick), no Origin", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin"), withHeader("Content-Type", "text/plain"), withoutHeader("X-Requested-With")}
		}},
		{"the right Origin but a form content type and no X-Requested-With", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withHeader("Content-Type", "application/x-www-form-urlencoded"), withoutHeader("X-Requested-With")}
		}},
		{"no Origin, no content type, no X-Requested-With", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin"), withoutHeader("Content-Type"), withoutHeader("X-Requested-With")}
		}},
		// S-76: a missing Origin is refused whatever else the request carries.
		{"no Origin at all (JSON or X-Requested-With as the web app sends)", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin")}
		}},
		{"no Origin, only X-Requested-With", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin"), withoutHeader("Content-Type"), withHeader("X-Requested-With", "fetch")}
		}},
		{"no Origin, Sec-Fetch-Site same-origin", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin"), withHeader("Sec-Fetch-Site", "same-origin")}
		}},
		{"no Origin, Sec-Fetch-Site none", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withoutHeader("Origin"), withHeader("Sec-Fetch-Site", "none")}
		}},
		// Two Origin headers: whichever comes first, the request is refused.
		{"the right Origin, then another site's", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withAddedHeader("Origin", "https://evil.example")}
		}},
		{"another site's Origin, then the right one", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withHeader("Origin", "https://evil.example"), withAddedHeader("Origin", testOrigin)}
		}},
		{"the right Origin twice", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withAddedHeader("Origin", testOrigin)}
		}},
		// A content type that only starts with application/json is not JSON.
		{"application/jsonp, no X-Requested-With", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withHeader("Content-Type", "application/jsonp"), withoutHeader("X-Requested-With")}
		}},
		{"application/jsonx, no X-Requested-With", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withHeader("Content-Type", "application/jsonx"), withoutHeader("X-Requested-With")}
		}},
		{"application/json-seq, no X-Requested-With", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withHeader("Content-Type", "application/json-seq"), withoutHeader("X-Requested-With")}
		}},
		{"an empty X-Requested-With and no JSON", func(csrfEndpoint) []reqOpt {
			return []reqOpt{withHeader("Content-Type", "text/plain"), withHeader("X-Requested-With", "  ")}
		}},
	}

	for _, atk := range attacks {
		for _, ep := range csrfEndpoints(jobID, token) {
			opts := atk.mod(ep)
			if ep.signedIn {
				opts = append([]reqOpt{withCookie(cookie)}, opts...)
			}
			rec := e.do(ep.method, ep.path, ep.body, opts...)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s -> %s %s = %d %s, want 403", atk.name, ep.method, ep.path, rec.Code, rec.Body)
			}
			if e.sessionCookie(rec) != nil {
				t.Errorf("%s -> %s set or cleared the session cookie on a refused request", atk.name, ep.name)
			}
		}
	}

	// Nothing happened: the session lives, the account and its saved job are there, no link
	// was mailed or spent, and no session was opened.
	if rec := e.do("GET", "/api/v1/me", "", withCookie(cookie)); rec.Code != 200 {
		t.Errorf("the victim's session was ended by a forged request: %d", rec.Code)
	}
	ids := decode[map[string][]string](t, e.do("GET", "/api/v1/me/bookmarks/ids", "", withCookie(cookie)))
	if len(ids["ids"]) != 1 {
		t.Errorf("saved jobs after forged requests = %v, want the one saved before", ids)
	}
	select {
	case m := <-e.mail.ch:
		t.Errorf("a forged login request mailed %q", m.To)
	default:
	}
	if rec := e.do("POST", "/api/v1/auth/inspect", fmt.Sprintf(`{"token":%q}`, token)); rec.Code != 200 {
		t.Errorf("the link was spent by a forged verify: %d", rec.Code)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("sessions = %d, want 1", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM users`); n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM login_tokens WHERE email = 'victim@example.com'`); n != 0 {
		t.Errorf("a forged login request stored %d links", n)
	}
}

func TestCSRF_LegitimateRequestsPass(t *testing.T) {
	e := newEnv(t, envOpts{})
	cookie := e.signIn("real@example.com")
	jobID := seedJob(t, e.pool, seedJobSpec{})

	cases := []struct {
		name string
		opts []reqOpt
	}{
		{"the web app: its Origin and JSON", nil},
		{"the web app on a different site (Origin matches, Sec-Fetch-Site cross-site)", []reqOpt{withHeader("Sec-Fetch-Site", "cross-site")}},
		{"the web app on a sibling subdomain (Origin matches, same-site)", []reqOpt{withHeader("Sec-Fetch-Site", "same-site")}},
		{"same-origin fetch", []reqOpt{withHeader("Sec-Fetch-Site", "same-origin")}},
		{"JSON with a charset parameter", []reqOpt{withHeader("Content-Type", "application/json; charset=utf-8")}},
		{"X-Requested-With instead of JSON", []reqOpt{withHeader("X-Requested-With", "fetch"), withHeader("Content-Type", "text/plain")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// login (JSON body): passes the guard when the content type is JSON or X-Requested-With is set.
			opts := tc.opts
			rec := e.do("POST", "/api/v1/auth/login", `{"email":"ok@example.com"}`, opts...)
			wantLogin := 202
			if strings.Contains(tc.name, "X-Requested-With instead") {
				wantLogin = 415 // through the guard; the handler wants JSON
			}
			if rec.Code != wantLogin {
				t.Errorf("login = %d %s, want %d", rec.Code, rec.Body, wantLogin)
			}
			select {
			case <-e.mail.ch:
			default:
			}
			// save and unsave (no body): X-Requested-With is what the web app sends.
			put := append([]reqOpt{withCookie(cookie)}, opts...)
			if rec := e.do("PUT", bookmarkPath(jobID), "", put...); rec.Code != http.StatusNoContent {
				t.Errorf("PUT bookmark = %d %s, want 204", rec.Code, rec.Body)
			}
			if rec := e.do("DELETE", bookmarkPath(jobID), "", put...); rec.Code != http.StatusNoContent {
				t.Errorf("DELETE bookmark = %d %s, want 204", rec.Code, rec.Body)
			}
		})
	}
}

func TestCSRF_ReadsAreNotGuardedButAreNotReadableCrossOrigin(t *testing.T) {
	e := newEnv(t, envOpts{})
	cookie := e.signIn("reader@example.com")
	rec := e.do("GET", "/api/v1/me", "", withCookie(cookie), withHeader("Origin", "https://evil.example"))
	if rec.Code != 200 {
		t.Errorf("GET /me = %d (reads are never state-changing, so not guarded)", rec.Code)
	}
	// The browser would run the request but must not hand the response to the evil page.
	if v := rec.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("Access-Control-Allow-Origin = %q for another origin, want none", v)
	}
	if v := rec.Header().Get("Access-Control-Allow-Credentials"); v != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q for another origin, want none", v)
	}
}

// ---- CORS -------------------------------------------------------------------------

func TestCORS_PreflightIsAnsweredOnlyForTheConfiguredOrigin(t *testing.T) {
	e := newEnv(t, envOpts{})
	preflight := func(origin string) *httptest.ResponseRecorder {
		var opts []reqOpt
		if origin != "" {
			opts = append(opts, withHeader("Origin", origin))
		}
		opts = append(opts, withHeader("Access-Control-Request-Method", "PUT"), withHeader("Access-Control-Request-Headers", "content-type, x-requested-with"))
		return e.do("OPTIONS", "/api/v1/me/bookmarks/1", "", opts...)
	}

	rec := preflight(testOrigin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d", rec.Code)
	}
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != testOrigin {
		t.Errorf("Allow-Origin = %q, want %q", h.Get("Access-Control-Allow-Origin"), testOrigin)
	}
	if h.Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("Allow-Credentials = %q, want true for the web app's origin", h.Get("Access-Control-Allow-Credentials"))
	}
	for _, m := range []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"} {
		if !strings.Contains(h.Get("Access-Control-Allow-Methods"), m) {
			t.Errorf("Allow-Methods %q lacks %s", h.Get("Access-Control-Allow-Methods"), m)
		}
	}
	for _, hd := range []string{"Content-Type", "X-Requested-With"} {
		if !strings.Contains(h.Get("Access-Control-Allow-Headers"), hd) {
			t.Errorf("Allow-Headers %q lacks %s", h.Get("Access-Control-Allow-Headers"), hd)
		}
	}
	// Without this, a browser's fetch() cannot read Retry-After on a 429/503 or
	// X-Request-Id on any response, even though the server always sends both.
	for _, hd := range []string{"Retry-After", "X-Request-Id"} {
		if !strings.Contains(h.Get("Access-Control-Expose-Headers"), hd) {
			t.Errorf("Expose-Headers %q lacks %s", h.Get("Access-Control-Expose-Headers"), hd)
		}
	}
	if !strings.Contains(strings.Join(h.Values("Vary"), ","), "Origin") {
		t.Errorf("Vary = %v, want Origin (the answer depends on it)", h.Values("Vary"))
	}
	if rec.Body.Len() != 0 {
		t.Errorf("preflight body = %q", rec.Body)
	}

	for _, origin := range []string{
		"https://evil.example", "null", "http://jobs.example.com", "https://jobs.example.com:8443",
		"https://jobs.example.com.evil.example", "https://evil.jobs.example.com", "*",
	} {
		rec := preflight(origin)
		for _, k := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Expose-Headers"} {
			if v := rec.Header().Get(k); v != "" {
				t.Errorf("preflight from %q: %s = %q, want the header absent so the browser refuses the request", origin, k, v)
			}
		}
	}
}

func TestCORS_NeverAWildcardAndCredentialsOnlyForTheOrigin(t *testing.T) {
	e := newEnv(t, envOpts{})
	for _, tc := range []struct {
		origin          string
		wantACAO        string
		wantCredentials bool
	}{
		{testOrigin, testOrigin, true},
		{"https://evil.example", "", false},
		{"null", "", false},
		{"", testOrigin, false}, // not a browser cross-origin call: no credentials either
	} {
		var opts []reqOpt
		if tc.origin != "" {
			opts = append(opts, withHeader("Origin", tc.origin))
		}
		rec := e.do("GET", "/api/v1/jobs", "", opts...)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantACAO || got == "*" {
			t.Errorf("Origin %q: Allow-Origin = %q, want %q", tc.origin, got, tc.wantACAO)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials") == "true"; got != tc.wantCredentials {
			t.Errorf("Origin %q: Allow-Credentials = %v, want %v", tc.origin, got, tc.wantCredentials)
		}
	}
}
