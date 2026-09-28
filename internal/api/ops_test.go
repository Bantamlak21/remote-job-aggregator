package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

func TestRequestID_ValidInboundIsKeptAndOthersReplaced(t *testing.T) {
	var logs bytes.Buffer
	h := withRequestLog(slog.New(slog.NewTextHandler(&logs, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqLogger(r, nil).Info("inside the handler")
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		name, in string
		keep     bool
	}{
		{"valid", "abc-123_X.y", true},
		{"64 characters", strings.Repeat("a", 64), true},
		{"65 characters", strings.Repeat("a", 65), false},
		{"200 characters", strings.Repeat("a", 200), false},
		{"space", "has space", false},
		{"forged log line", `x" level=ERROR msg=forged`, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			r := httptest.NewRequest("GET", "/x", nil)
			if tc.in != "" {
				r.Header.Set("X-Request-Id", tc.in)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			got := rec.Header().Get("X-Request-Id")
			if tc.keep && got != tc.in {
				t.Errorf("X-Request-Id = %q, want the inbound %q", got, tc.in)
			}
			if !tc.keep && (got == tc.in || !validRequestID.MatchString(got)) {
				t.Errorf("X-Request-Id = %q, want a fresh valid id", got)
			}
			// Every line of the request carries the id: the handler's own and the summary.
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("log = %q, want 2 lines", logs.String())
			}
			for _, l := range lines {
				if !strings.Contains(l, "request_id="+got) {
					t.Errorf("log line lacks request_id=%s: %s", got, l)
				}
			}
			if strings.Contains(logs.String(), "forged") {
				t.Errorf("client-supplied text reached the log: %s", logs.String())
			}
		})
	}
}

func TestPanicIsRecoveredIntoAGeneric500WithTheRequestID(t *testing.T) {
	var logs bytes.Buffer
	h := withRequestLog(slog.New(slog.NewTextHandler(&logs, nil)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret internal detail")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "secret") || !strings.Contains(body, `"error"`) {
		t.Errorf("body = %q, want a generic error", body)
	}
	id := rec.Header().Get("X-Request-Id")
	out := logs.String()
	if !strings.Contains(out, "panic in handler") || !strings.Contains(out, "secret internal detail") || !strings.Contains(out, "request_id="+id) {
		t.Errorf("the panic was not logged with the request id: %s", out)
	}
	if !strings.Contains(out, "status=500") {
		t.Errorf("the request line does not say 500: %s", out)
	}
}

func TestPanicAfterTheResponseStartedDoesNotWriteASecondBody(t *testing.T) {
	h := withRequestLog(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("late")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Errorf("got %d %q, want the already-sent 202 and no extra body", rec.Code, rec.Body)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	ready := errors.New("db down")
	var check func(context.Context) error
	h := NewHandler(job.NewMockRepository(), Config{CORSAllowedOrigin: "http://localhost:5173",
		Ready: func(ctx context.Context) error { return check(ctx) }}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	check = func(context.Context) error { return nil }
	if rec := get("/healthz"); rec.Code != 200 {
		t.Errorf("/healthz = %d", rec.Code)
	}
	if rec := get("/readyz"); rec.Code != 200 {
		t.Errorf("/readyz with a healthy dependency = %d", rec.Code)
	}

	check = func(context.Context) error { return ready }
	if rec := get("/healthz"); rec.Code != 200 {
		t.Errorf("/healthz must stay 200 when the database is down (liveness), got %d", rec.Code)
	}
	rec := get("/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with the database down = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "db down") {
		t.Errorf("/readyz leaks the cause: %s", rec.Body)
	}
}

func TestOversizedAuthBodyIs413(t *testing.T) {
	e := newEnv(t, envOpts{})
	big := `{"email":"` + strings.Repeat("a", 1<<20) + `"}`
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/inspect", "/api/v1/auth/verify"} {
		if rec := e.do("POST", path, big); rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("POST %s with 1 MiB = %d, want 413", path, rec.Code)
		}
	}
}

func TestAuthEventsAreLoggedWithoutSecretsOrFullAddresses(t *testing.T) {
	e := newEnv(t, envOpts{})
	const addr = "alice.example@example.org"
	c := e.signIn(addr)
	id := func(rec *httptest.ResponseRecorder) string { return rec.Header().Get("X-Request-Id") }

	if rec := e.do("POST", "/api/v1/auth/verify", `{"token":"`+strings.Repeat("A", 43)+`"}`); rec.Code != 400 {
		t.Fatalf("bad token = %d", rec.Code)
	}
	if rec := e.do("POST", "/api/v1/auth/login", `{"email":"x@y.co"}`, withHeader("Origin", "https://evil.example")); rec.Code != 403 {
		t.Fatalf("cross-origin = %d", rec.Code)
	}
	if rec := e.do("POST", "/api/v1/auth/logout-all", "", withCookie(c)); rec.Code != 204 {
		t.Fatalf("logout-all = %d", rec.Code)
	}
	c2 := e.signIn(addr)
	rec := e.do("DELETE", "/api/v1/me", `{"confirm":true}`, withCookie(c2))
	if rec.Code != 204 || id(rec) == "" {
		t.Fatalf("delete = %d, request id %q", rec.Code, id(rec))
	}
	c3 := e.signIn(addr)
	if rec := e.do("POST", "/api/v1/auth/logout", "", withCookie(c3)); rec.Code != 204 {
		t.Fatalf("logout = %d", rec.Code)
	}

	logs := e.logs.String()
	for _, ev := range []string{"login_requested", "login_verified", "login_failed", "csrf_rejected", "logout_all", "account_deleted", "logout"} {
		if !strings.Contains(logs, "event="+ev) {
			t.Errorf("no %q event in the log", ev)
		}
	}
	if strings.Contains(logs, addr) || strings.Contains(logs, "alice.example") {
		t.Errorf("the log holds the full address:\n%s", logs)
	}
	if !strings.Contains(logs, "a***@***.org") {
		t.Errorf("the log lacks the masked address")
	}
	if strings.Contains(logs, c.Value) || strings.Contains(logs, "session=") {
		t.Errorf("the log holds a session value")
	}
}
