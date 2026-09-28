// Package api serves the public, read-only job API over HTTP — see
// docs/api.md (repo root) for the frozen contract the frontend builds
// against. Depends only on job.Repository, never a concrete
// implementation, so swapping job.NewMockRepository() for a real
// Postgres-backed repository once Phase 3 ships real ingestion is the
// entire migration: no handler, routing, or frontend change required.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

// Config configures the server. Addr and CORSAllowedOrigin come from
// config.APIConfig — this package doesn't read the environment itself,
// matching the project-wide rule that only internal/config does.
type Config struct {
	Addr              string
	CORSAllowedOrigin string

	// Accounts, when set, turns on sign-in and saved jobs (the /auth and /me
	// endpoints). It needs Saved, and CORSAllowedOrigin must then be the exact origin
	// of the web app, which is also the only origin allowed to make state-changing
	// requests.
	Accounts Accounts
	Saved    SavedJobs
	Cookie   CookieConfig
	// SessionTTL is the session cookie's lifetime.
	SessionTTL time.Duration
	// ClientIPHeader names a header a trusted reverse proxy overwrites with the real
	// client address; empty means use the connection's address.
	ClientIPHeader string
	// Ready, when set, is checked by /readyz (for example the database ping).
	Ready func(context.Context) error
}

// Server wraps http.Server with the ListenAndServe/Shutdown shape
// cmd/aggregator's existing installSignalHandling/closeWithTimeout
// pattern expects (see runApp).
type Server struct {
	httpServer *http.Server
}

// NewServer builds a Server over repo. logger receives one line per
// request (method, path, status, duration) plus server lifecycle
// events.
func NewServer(repo job.Repository, cfg Config, logger *slog.Logger) *Server {
	return &Server{httpServer: &http.Server{
		Addr:    cfg.Addr,
		Handler: NewHandler(repo, cfg, logger),
		// A slow or hostile client cannot hold a connection open: headers, the
		// whole request, the response and an idle keep-alive are all bounded.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}}
}

// NewHandler builds the complete http.Handler: routing, CORS, and
// request logging, over repo. Exported separately from NewServer so
// tests can exercise the handler directly with httptest, without a
// real listening socket.
func NewHandler(repo job.Repository, cfg Config, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/jobs", listJobsHandler(repo, logger))
	// Liveness and readiness for the deploy and the monitor: neither touches sign-in or mail.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if cfg.Ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := cfg.Ready(ctx); err != nil {
				reqLogger(r, logger).Error("readiness check failed", "error", err)
				writeError(w, http.StatusServiceUnavailable, "not ready")
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	var acct *accountRoutes
	var handler http.Handler = mux
	credentials := false
	if cfg.Accounts != nil && cfg.Saved != nil {
		acct = &accountRoutes{accounts: cfg.Accounts, saved: cfg.Saved, cookie: cfg.Cookie, sessionTTL: cfg.SessionTTL,
			clientIPHeader: cfg.ClientIPHeader, logger: logger}
		acct.mount(mux)
		handler = csrfGuard(cfg.CORSAllowedOrigin, logger, mux)
		credentials = true
	}
	mux.HandleFunc("GET /api/v1/jobs/{id}", getJobHandler(repo, acct, logger))

	return withRequestLog(logger, withSecurityHeaders(withCORS(cfg.CORSAllowedOrigin, credentials, handler)))
}

// ListenAndServe blocks until the server stops. http.Server.Shutdown's
// documented ErrServerClosed on a clean shutdown is swallowed here so
// callers get a plain nil on the expected path, matching every other
// blocking call in this codebase (e.g. database migration runners).
func (s *Server) ListenAndServe() error {
	err := s.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server, waiting for in-flight requests
// until ctx is done.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
