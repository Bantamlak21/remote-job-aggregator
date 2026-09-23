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
		// Guards against a slow/hostile client trickling headers in
		// forever; every other timeout (request body size, handler
		// duration) is out of scope for a read-only, no-body-accepting
		// API with no current abuse signal to size them against.
		ReadHeaderTimeout: 5 * time.Second,
	}}
}

// NewHandler builds the complete http.Handler: routing, CORS, and
// request logging, over repo. Exported separately from NewServer so
// tests can exercise the handler directly with httptest, without a
// real listening socket.
func NewHandler(repo job.Repository, cfg Config, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/jobs", listJobsHandler(repo, logger))
	mux.HandleFunc("GET /api/v1/jobs/{id}", getJobHandler(repo, logger))

	return withRequestLog(logger, withCORS(cfg.CORSAllowedOrigin, mux))
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
