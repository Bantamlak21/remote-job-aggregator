package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// clearAll resets every env var Load reads to "unset" (via t.Setenv, which
// restores the original value after the test) so tests never depend on
// whatever happens to be in the surrounding shell environment.
func clearAll(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"APP_ENV", "DATABASE_URL", "DB_MAX_OPEN_CONNS", "DB_MIN_CONNS",
		"DB_CONN_MAX_LIFETIME", "DB_CONN_MAX_IDLE_TIME", "LOG_LEVEL",
		"LOG_FORMAT", "SHUTDOWN_TIMEOUT", "HTTP_TIMEOUT", "HTTP_MAX_RESPONSE_SIZE",
		"HTTP_USER_AGENT", "DISCOVERY_WORKERS", "SERPER_API_KEY", "API_ADDR", "CORS_ALLOWED_ORIGIN",
	} {
		t.Setenv(key, "")
	}
}

func TestLoad_MissingRequiredDatabaseURL(t *testing.T) {
	clearAll(t)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing DATABASE_URL, got nil")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Errorf("error = %q, want it to mention DATABASE_URL is required", err.Error())
	}
}

func TestLoad_DefaultsAppliedWhenOnlyRequiredFieldSet(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.AppEnv != EnvDevelopment {
		t.Errorf("AppEnv = %q, want %q", cfg.AppEnv, EnvDevelopment)
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("Database.MaxConns = %d, want 10", cfg.Database.MaxConns)
	}
	if cfg.Database.MinConns != 2 {
		t.Errorf("Database.MinConns = %d, want 2", cfg.Database.MinConns)
	}
	if cfg.Database.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("Database.ConnMaxLifetime = %s, want 30m", cfg.Database.ConnMaxLifetime)
	}
	if cfg.Database.ConnMaxIdleTime != 5*time.Minute {
		t.Errorf("Database.ConnMaxIdleTime = %s, want 5m", cfg.Database.ConnMaxIdleTime)
	}
	if cfg.Log.Level != slog.LevelInfo {
		t.Errorf("Log.Level = %v, want info", cfg.Log.Level)
	}
	if cfg.Log.Format != LogFormatJSON {
		t.Errorf("Log.Format = %q, want %q", cfg.Log.Format, LogFormatJSON)
	}
	if cfg.Shutdown.Timeout != 15*time.Second {
		t.Errorf("Shutdown.Timeout = %s, want 15s", cfg.Shutdown.Timeout)
	}
	if cfg.HTTP.Timeout != 10*time.Second {
		t.Errorf("HTTP.Timeout = %s, want 10s", cfg.HTTP.Timeout)
	}
	if cfg.HTTP.MaxResponseBytes != 5*1024*1024 {
		t.Errorf("HTTP.MaxResponseBytes = %d, want 5MiB", cfg.HTTP.MaxResponseBytes)
	}
	if strings.TrimSpace(cfg.HTTP.UserAgent) == "" {
		t.Error("HTTP.UserAgent default is blank")
	}
	if cfg.Discovery.Workers != 5 {
		t.Errorf("Discovery.Workers = %d, want 5", cfg.Discovery.Workers)
	}
}

func TestLoad_OverridesRespected(t *testing.T) {
	clearAll(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("DATABASE_URL", "postgresql://user:pass@db:5432/jobs")
	t.Setenv("DB_MAX_OPEN_CONNS", "25")
	t.Setenv("DB_MIN_CONNS", "10")
	t.Setenv("DB_CONN_MAX_LIFETIME", "1h")
	t.Setenv("DB_CONN_MAX_IDLE_TIME", "10m")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("SHUTDOWN_TIMEOUT", "30s")
	t.Setenv("HTTP_TIMEOUT", "5s")
	t.Setenv("HTTP_MAX_RESPONSE_SIZE", "1048576")
	t.Setenv("HTTP_USER_AGENT", "custom-agent/1.0")
	t.Setenv("DISCOVERY_WORKERS", "20")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.AppEnv != EnvProduction {
		t.Errorf("AppEnv = %q, want %q", cfg.AppEnv, EnvProduction)
	}
	if cfg.Database.MaxConns != 25 {
		t.Errorf("Database.MaxConns = %d, want 25", cfg.Database.MaxConns)
	}
	if cfg.Database.MinConns != 10 {
		t.Errorf("Database.MinConns = %d, want 10", cfg.Database.MinConns)
	}
	if cfg.Database.ConnMaxLifetime != time.Hour {
		t.Errorf("Database.ConnMaxLifetime = %s, want 1h", cfg.Database.ConnMaxLifetime)
	}
	if cfg.Log.Level != slog.LevelDebug {
		t.Errorf("Log.Level = %v, want debug", cfg.Log.Level)
	}
	if cfg.Log.Format != LogFormatText {
		t.Errorf("Log.Format = %q, want %q", cfg.Log.Format, LogFormatText)
	}
	if cfg.Shutdown.Timeout != 30*time.Second {
		t.Errorf("Shutdown.Timeout = %s, want 30s", cfg.Shutdown.Timeout)
	}
	if cfg.HTTP.Timeout != 5*time.Second {
		t.Errorf("HTTP.Timeout = %s, want 5s", cfg.HTTP.Timeout)
	}
	if cfg.HTTP.MaxResponseBytes != 1048576 {
		t.Errorf("HTTP.MaxResponseBytes = %d, want 1048576", cfg.HTTP.MaxResponseBytes)
	}
	if cfg.HTTP.UserAgent != "custom-agent/1.0" {
		t.Errorf("HTTP.UserAgent = %q, want %q", cfg.HTTP.UserAgent, "custom-agent/1.0")
	}
	if cfg.Discovery.Workers != 20 {
		t.Errorf("Discovery.Workers = %d, want 20", cfg.Discovery.Workers)
	}
}

func TestLoad_ZeroHTTPTimeoutRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("HTTP_TIMEOUT", "0s")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for HTTP_TIMEOUT=0s, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP_TIMEOUT must be positive") {
		t.Errorf("error = %q, want it to mention HTTP_TIMEOUT must be positive", err.Error())
	}
}

func TestLoad_NegativeHTTPMaxResponseSizeRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("HTTP_MAX_RESPONSE_SIZE", "-1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for negative HTTP_MAX_RESPONSE_SIZE, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP_MAX_RESPONSE_SIZE must be positive") {
		t.Errorf("error = %q, want it to mention HTTP_MAX_RESPONSE_SIZE must be positive", err.Error())
	}
}

func TestLoad_HTTPMaxResponseSizeBeyondInt32IsAccepted(t *testing.T) {
	// Regression guard: MaxResponseBytes is int64 specifically so response
	// size limits aren't accidentally capped the way pool sizes are — a
	// limit larger than 2^31 must not be rejected or truncated.
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("HTTP_MAX_RESPONSE_SIZE", "4294967296") // 4GiB, > math.MaxInt32

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if cfg.HTTP.MaxResponseBytes != 4294967296 {
		t.Errorf("HTTP.MaxResponseBytes = %d, want 4294967296", cfg.HTTP.MaxResponseBytes)
	}
}

func TestLoad_BlankHTTPUserAgentRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("HTTP_USER_AGENT", "   ")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for blank HTTP_USER_AGENT, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP_USER_AGENT must not be blank") {
		t.Errorf("error = %q, want it to mention HTTP_USER_AGENT must not be blank", err.Error())
	}
}

func TestLoad_DiscoveryWorkersOutOfRangeRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	for _, v := range []string{"0", "101", "-1"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("DISCOVERY_WORKERS", v)
			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for DISCOVERY_WORKERS=%s, got nil", v)
			}
			if !strings.Contains(err.Error(), "DISCOVERY_WORKERS must be between") {
				t.Errorf("error = %q, want it to mention the valid range", err.Error())
			}
		})
	}
}

func TestLoad_InvalidAppEnv(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("APP_ENV", "prooduction")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid APP_ENV, got nil")
	}
	if !strings.Contains(err.Error(), "APP_ENV must be one of") {
		t.Errorf("error = %q, want it to mention APP_ENV must be one of", err.Error())
	}
}

func TestLoad_InvalidDatabaseURLScheme(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "mysql://user:pass@localhost:3306/jobs")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for non-postgres DATABASE_URL, got nil")
	}
	if !strings.Contains(err.Error(), "must be a valid postgres") {
		t.Errorf("error = %q, want it to mention postgres scheme requirement", err.Error())
	}
}

// Regression test: an invalid DATABASE_URL's error message must never
// contain the raw credential, in any of the shapes a plausible operator
// typo produces — not just a wrong-but-well-formed scheme. A URL with an
// unparseable port fails url.Parse entirely; a URL missing its scheme
// entirely (a forgotten "postgres://") parses successfully but with the
// password landing in an opaque, unrecognized position. Both used to
// leak the raw value verbatim because the old code fell back to
// echoing it whenever it couldn't confidently redact.
func TestLoad_InvalidDatabaseURLErrorNeverContainsThePassword(t *testing.T) {
	cases := map[string]string{
		"wrong scheme, well-formed": "mysql://user:supersecret@localhost:3306/jobs",
		"unparseable port":          "postgres://user:supersecret@localhost:port/jobs",
		"missing scheme entirely":   "user:supersecret@localhost:5432/jobs",
	}
	for name, dbURL := range cases {
		t.Run(name, func(t *testing.T) {
			clearAll(t)
			t.Setenv("DATABASE_URL", dbURL)

			_, err := Load()
			if err == nil {
				t.Fatal("expected error for invalid DATABASE_URL, got nil")
			}
			if strings.Contains(err.Error(), "supersecret") {
				t.Errorf("error leaked the password: %q", err.Error())
			}
		})
	}
}

func TestLoad_MinConnsExceedingMaxConnsRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_MAX_OPEN_CONNS", "5")
	t.Setenv("DB_MIN_CONNS", "20")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when min conns exceed max conns, got nil")
	}
	if !strings.Contains(err.Error(), "cannot exceed DB_MAX_OPEN_CONNS") {
		t.Errorf("error = %q, want it to mention the min/max conflict", err.Error())
	}
}

func TestLoad_MinConnsZeroIsValid(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_MIN_CONNS", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error for DB_MIN_CONNS=0: %v", err)
	}
	if cfg.Database.MinConns != 0 {
		t.Errorf("Database.MinConns = %d, want 0", cfg.Database.MinConns)
	}
}

func TestLoad_MaxConnsOutOfInt32RangeRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_MAX_OPEN_CONNS", "4294967306")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for DB_MAX_OPEN_CONNS beyond int32 range, got nil")
	}
	if !strings.Contains(err.Error(), "DB_MAX_OPEN_CONNS must be between") {
		t.Errorf("error = %q, want it to mention the valid range", err.Error())
	}
}

func TestLoad_ZeroMaxConnsRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_MAX_OPEN_CONNS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for DB_MAX_OPEN_CONNS=0, got nil")
	}
}

func TestLoad_NegativeConnMaxLifetimeRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_CONN_MAX_LIFETIME", "-5m")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for negative DB_CONN_MAX_LIFETIME, got nil")
	}
	if !strings.Contains(err.Error(), "DB_CONN_MAX_LIFETIME must be positive") {
		t.Errorf("error = %q, want it to mention DB_CONN_MAX_LIFETIME must be positive", err.Error())
	}
}

func TestLoad_NegativeConnMaxIdleTimeRejected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_CONN_MAX_IDLE_TIME", "-1s")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for negative DB_CONN_MAX_IDLE_TIME, got nil")
	}
	if !strings.Contains(err.Error(), "DB_CONN_MAX_IDLE_TIME must be positive") {
		t.Errorf("error = %q, want it to mention DB_CONN_MAX_IDLE_TIME must be positive", err.Error())
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("LOG_LEVEL", "verbose")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid LOG_LEVEL, got nil")
	}
	if !strings.Contains(err.Error(), "LOG_LEVEL must be one of") {
		t.Errorf("error = %q, want it to mention LOG_LEVEL must be one of", err.Error())
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("DB_CONN_MAX_LIFETIME", "not-a-duration")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
	if !strings.Contains(err.Error(), "DB_CONN_MAX_LIFETIME must be a valid duration") {
		t.Errorf("error = %q, want it to mention DB_CONN_MAX_LIFETIME", err.Error())
	}
}

func TestLoad_MultipleFailuresAreAllReported(t *testing.T) {
	clearAll(t)
	t.Setenv("APP_ENV", "bogus")
	t.Setenv("LOG_LEVEL", "bogus")
	// DATABASE_URL intentionally left unset.

	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, want := range []string{"APP_ENV must be one of", "LOG_LEVEL must be one of", "DATABASE_URL is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to also contain %q", err.Error(), want)
		}
	}
}

func TestDatabaseConfig_LogValueRedactsUserinfoPassword(t *testing.T) {
	d := DatabaseConfig{URL: "postgres://user:supersecret@localhost:5432/jobs"}

	got := d.LogValue().String()
	if strings.Contains(got, "supersecret") {
		t.Errorf("LogValue() leaked the password: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("LogValue() = %q, want it to contain REDACTED", got)
	}
}

// Regression test: pgx/libpq also accept a "password" query parameter as
// an alternative to userinfo. redactURLCredentials must catch both forms,
// not just postgres://user:pass@host.
func TestDatabaseConfig_LogValueRedactsQueryStringPassword(t *testing.T) {
	d := DatabaseConfig{URL: "postgres://user@localhost:5432/jobs?password=supersecret&sslmode=disable"}

	got := d.LogValue().String()
	if strings.Contains(got, "supersecret") {
		t.Errorf("LogValue() leaked the query-string password: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("LogValue() = %q, want it to contain REDACTED", got)
	}
	if !strings.Contains(got, "sslmode=disable") {
		t.Errorf("LogValue() = %q, want it to preserve non-secret query params", got)
	}
}

func TestDatabaseConfig_LogValuePassesThroughURLWithoutCredentials(t *testing.T) {
	d := DatabaseConfig{URL: "postgres://localhost:5432/jobs"}

	got := d.LogValue().String()
	if !strings.Contains(got, "localhost:5432/jobs") {
		t.Errorf("LogValue() = %q, want it to preserve a credential-free URL", got)
	}
}

func TestLoad_SearchConfigDefaultsToUnconfigured(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if cfg.Search.Configured() {
		t.Error("Search.Configured() = true, want false when SERPER_API_KEY is not set")
	}
	if cfg.Search.SerperAPIKey != "" {
		t.Errorf("Search.SerperAPIKey = %q, want empty by default", cfg.Search.SerperAPIKey)
	}
}

func TestLoad_SearchConfigOverrideRespected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("SERPER_API_KEY", "test-api-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if !cfg.Search.Configured() {
		t.Error("Search.Configured() = false, want true when SERPER_API_KEY is set")
	}
	if cfg.Search.SerperAPIKey != "test-api-key" {
		t.Errorf("Search.SerperAPIKey = %q, want %q", cfg.Search.SerperAPIKey, "test-api-key")
	}
}

func TestLoad_APIConfigDefaults(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if cfg.API.Addr != ":8080" {
		t.Errorf("API.Addr = %q, want %q", cfg.API.Addr, ":8080")
	}
	if cfg.API.CORSAllowedOrigin != "http://localhost:5173" {
		t.Errorf("API.CORSAllowedOrigin = %q, want %q", cfg.API.CORSAllowedOrigin, "http://localhost:5173")
	}
}

func TestLoad_APIConfigOverridesRespected(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("API_ADDR", ":9090")
	t.Setenv("CORS_ALLOWED_ORIGIN", "https://jobs.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if cfg.API.Addr != ":9090" {
		t.Errorf("API.Addr = %q, want %q", cfg.API.Addr, ":9090")
	}
	if cfg.API.CORSAllowedOrigin != "https://jobs.example.com" {
		t.Errorf("API.CORSAllowedOrigin = %q, want %q", cfg.API.CORSAllowedOrigin, "https://jobs.example.com")
	}
}

func TestLoad_APIConfigRejectsMalformedAddr(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("API_ADDR", "not-a-host-port")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for a malformed API_ADDR, got nil")
	}
	if !strings.Contains(err.Error(), "API_ADDR") {
		t.Errorf("error = %q, want it to mention API_ADDR", err.Error())
	}
}

func TestLoad_APIConfigRejectsBlankCORSOrigin(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	t.Setenv("CORS_ALLOWED_ORIGIN", "   ")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for a blank CORS_ALLOWED_ORIGIN, got nil")
	}
	if !strings.Contains(err.Error(), "CORS_ALLOWED_ORIGIN") {
		t.Errorf("error = %q, want it to mention CORS_ALLOWED_ORIGIN", err.Error())
	}
}

func TestSearchConfig_LogValueRedactsAPIKey(t *testing.T) {
	s := SearchConfig{SerperAPIKey: "super-secret-key"}

	got := s.LogValue().String()
	if strings.Contains(got, "super-secret-key") {
		t.Errorf("LogValue() leaked the API key: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("LogValue() = %q, want it to contain REDACTED", got)
	}
}

func TestSearchConfig_LogValueOnUnconfiguredDoesNotClaimRedaction(t *testing.T) {
	s := SearchConfig{}

	got := s.LogValue().String()
	if strings.Contains(got, "REDACTED") {
		t.Errorf("LogValue() = %q, want no REDACTED marker when there is no key to redact", got)
	}
}
