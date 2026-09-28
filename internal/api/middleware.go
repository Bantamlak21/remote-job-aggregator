package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"
)

// withCORS allows exactly one origin — never "*" — to call this API
// from a browser. This is public, non-authenticated data, so a
// wildcard would carry no real confidentiality risk, but the frontend
// is a known, specific origin (config.APIConfig.CORSAllowedOrigin), and
// naming it explicitly is the deliberate choice over a wildcard that
// would silently also allow any other site to embed this API.
//
// With accounts on, credentials (the session cookie) are allowed, only to that one
// origin: a request from any other origin gets no CORS headers at all, so a browser
// will not let its page read the response, and the "*" wildcard (which a browser
// refuses with credentials anyway) is refused at startup.
func withCORS(allowedOrigin string, credentials bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		w.Header().Add("Vary", "Origin")
		if origin == "" || origin == allowedOrigin || allowedOrigin == "*" {
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			if credentials && origin == allowedOrigin && allowedOrigin != "*" {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Requested-With")
			// A browser's fetch() cannot read a response header unless the server lists it
			// here, whatever the same-origin rules would otherwise allow: without this, the
			// client cannot see how long a 429/503 asks it to wait, or the request id to
			// quote back for support, even though the server always sends both.
			w.Header().Set("Access-Control-Expose-Headers", "Retry-After, X-Request-Id")
			w.Header().Set("Access-Control-Max-Age", "600")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code a handler actually wrote,
// since http.ResponseWriter has no getter for it — needed so
// withRequestLog can log the real outcome (200 vs. 404 vs. 500) rather
// than always logging 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
	}
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

type requestLoggerKey struct{}

// validRequestID is what an inbound X-Request-Id must look like to be kept: anything
// else (too long, spaces, control characters) is replaced, so a client cannot put its
// own text in the logs through it.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// reqLogger is the logger of the request being served: every line it writes carries
// the request id. It falls back to fallback outside a request.
func reqLogger(r *http.Request, fallback *slog.Logger) *slog.Logger {
	if l, ok := r.Context().Value(requestLoggerKey{}).(*slog.Logger); ok {
		return l
	}
	return fallback
}

// withRequestLog gives each request an id (a valid inbound X-Request-Id is kept,
// anything else replaced) that is returned in the response and put on every log line
// of the request, recovers a panic into a generic 500 (logged with the id and the
// stack, never sent to the client), and logs one line per request: method, path, the
// status actually written, and duration. Runs outermost (wraps withCORS) so a CORS
// preflight (which withCORS answers directly, never reaching the mux) is logged too.
func withRequestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-Id")
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		rl := logger.With("request_id", id)
		r = r.WithContext(context.WithValue(r.Context(), requestLoggerKey{}, rl))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				rl.Error("panic in handler", "panic", p, "stack", string(debug.Stack()))
				if !rec.wrote {
					writeError(rec, http.StatusInternalServerError, "internal error")
				}
			}
			status := rec.status
			if !rec.wrote && r.Context().Err() != nil {
				status = 499 // the client went away before an answer; not a server error
			}
			rl.Info("api request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

// csrfGuard protects the cookie-authenticated endpoints (/auth and /me) against
// cross-site request forgery. Every state-changing request must
//   - carry exactly one Origin header, equal byte for byte to the web app's origin
//     (a missing Origin, "null", any other origin and a repeated header are all
//     refused: a browser always sends Origin on a cross-origin POST, PUT or DELETE);
//   - be one a cross-site page cannot send without a CORS preflight: a body of media
//     type exactly application/json, or a non-empty X-Requested-With header (which a
//     foreign page cannot add without the preflight this API answers only for the web
//     app).
//
// Reads (GET) are never state-changing here and are not guarded.
func csrfGuard(allowedOrigin string, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v1/auth/") && !strings.HasPrefix(r.URL.Path, "/api/v1/me") {
			next.ServeHTTP(w, r)
			return
		}
		refuse := func() {
			reqLogger(r, logger).Warn("auth event", "event", "csrf_rejected", "method", r.Method, "path", r.URL.Path)
			writeCode(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
		}
		if origins := r.Header.Values("Origin"); len(origins) != 1 || origins[0] != allowedOrigin {
			refuse()
			return
		}
		if !isJSONMediaType(r.Header.Get("Content-Type")) && strings.TrimSpace(r.Header.Get("X-Requested-With")) == "" {
			refuse()
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isJSONMediaType is true for exactly application/json (parameters such as charset are
// allowed), not for a look-alike such as application/jsonp or application/jsonx.
func isJSONMediaType(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}

// withSecurityHeaders sets, on every response, the headers that keep a JSON API from
// being framed, sniffed or leaking its URL in a Referer; and marks everything under
// /auth and /me, whatever its status (404 and 405 included), as never cacheable.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth") || strings.HasPrefix(r.URL.Path, "/api/v1/me") {
			h.Set("Cache-Control", "no-store")
			h.Add("Vary", "Cookie")
		}
		next.ServeHTTP(w, r)
	})
}
