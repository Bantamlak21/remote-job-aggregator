package api

import (
	"log/slog"
	"net/http"
	"time"
)

// withCORS allows exactly one origin — never "*" — to call this API
// from a browser. This is public, non-authenticated data, so a
// wildcard would carry no real confidentiality risk, but the frontend
// is a known, specific origin (config.APIConfig.CORSAllowedOrigin), and
// naming it explicitly is the deliberate choice over a wildcard that
// would silently also allow any other site to embed this API.
func withCORS(allowedOrigin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

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
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// withRequestLog logs one line per request: method, path, the status
// actually written, and duration. Runs outermost (wraps withCORS) so a
// CORS preflight (which withCORS answers directly, never reaching the
// mux) is logged too.
func withRequestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("api request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
