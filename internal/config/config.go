// Package config loads and validates operational configuration from
// environment variables. Nothing in this package talks to a database,
// the network, or the filesystem beyond os.Getenv — it exists purely to
// turn a process environment into a typed, validated Config.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment identifies which deployment environment the process is
// running in.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
)

func (e Environment) valid() bool {
	switch e {
	case EnvDevelopment, EnvStaging, EnvProduction:
		return true
	default:
		return false
	}
}

// LogFormat selects the slog handler used by internal/observability.
type LogFormat string

const (
	LogFormatJSON LogFormat = "json"
	LogFormatText LogFormat = "text"
)

func (f LogFormat) valid() bool {
	switch f {
	case LogFormatJSON, LogFormatText:
		return true
	default:
		return false
	}
}

// DatabaseConfig configures the PostgreSQL connection pool. Pool sizing is
// deliberately independent from HTTP or worker-pool concurrency (CLAUDE.md
// "Database engineering rules").
//
// MinConns is a floor pgxpool tries to keep warm, not a cap — pgxpool has
// no "max idle connections" concept the way database/sql does; idle
// connections above MinConns are reclaimed by ConnMaxIdleTime instead. The
// field is named to match what it actually does rather than borrow
// database/sql's terminology and mean something else.
type DatabaseConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// LogValue redacts every credential embedded in URL — both userinfo
// (postgres://user:pass@...) and a "password" query parameter — so a
// DatabaseConfig can be passed to slog directly without leaking a secret
// into logs.
func (d DatabaseConfig) LogValue() slog.Value {
	redacted := redactURLCredentials(d.URL)
	return slog.GroupValue(
		slog.String("url", redacted),
		slog.Int("max_conns", int(d.MaxConns)),
		slog.Int("min_conns", int(d.MinConns)),
		slog.Duration("conn_max_lifetime", d.ConnMaxLifetime),
		slog.Duration("conn_max_idle_time", d.ConnMaxIdleTime),
	)
}

// redactURLCredentials returns raw with any password redacted, whether it
// arrives as userinfo (postgres://user:PASSWORD@host) or as a "password"
// query parameter (both are accepted by pgx/libpq). If raw doesn't parse
// as a URL, it is returned unchanged rather than risk masking something
// that wasn't actually a credential.
func redactURLCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
	}

	q := u.Query()
	if q.Has("password") {
		q.Set("password", "REDACTED")
		u.RawQuery = q.Encode()
	}

	return u.String()
}

// LogConfig configures the structured logger built in internal/observability.
type LogConfig struct {
	Level  slog.Level
	Format LogFormat
}

// ShutdownConfig bounds how long graceful shutdown waits for active work to
// finish before the process exits anyway.
type ShutdownConfig struct {
	Timeout time.Duration
}

// HTTPConfig configures the shared outbound HTTP client used by discovery
// and (in later phases) ATS ingestion. MaxResponseBytes bounds how much of
// a response body is read before erroring, independent of Timeout, which
// bounds how long that read is allowed to take.
type HTTPConfig struct {
	Timeout          time.Duration
	MaxResponseBytes int64
	UserAgent        string
}

// DiscoveryConfig bounds discovery's own concurrency, independent of the
// database pool or any future ingestion worker pool (CLAUDE.md: "Database
// and HTTP concurrency should also be bounded" — separately from each
// other).
type DiscoveryConfig struct {
	Workers int
}

// SearchConfig configures the Google Custom Search JSON API client used
// by the search-based discovery mechanism. Both fields are optional —
// unlike DATABASE_URL, most commands (run, migrate-*, the seed-file
// discover) never need them, so Load does not require them — but they
// are only ever meaningful together: an API key with no search engine
// ID (or vice versa) can't make a real request, so Load rejects that
// combination rather than deferring the failure to the first search
// call.
type SearchConfig struct {
	GoogleAPIKey         string
	GoogleSearchEngineID string
}

// LogValue redacts the API key so a SearchConfig can be logged directly
// without leaking a credential — same reasoning as DatabaseConfig's
// LogValue, applied before this struct has ever actually been logged
// anywhere, since that mistake is cheaper to prevent than to catch.
func (s SearchConfig) LogValue() slog.Value {
	key := s.GoogleAPIKey
	if key != "" {
		key = "REDACTED"
	}
	return slog.GroupValue(
		slog.String("google_api_key", key),
		slog.String("google_search_engine_id", s.GoogleSearchEngineID),
	)
}

// Configured reports whether both search credentials are present, i.e.
// whether search-based discovery can actually run.
func (s SearchConfig) Configured() bool {
	return s.GoogleAPIKey != "" && s.GoogleSearchEngineID != ""
}

// Config is the fully validated, typed configuration for the aggregator
// process. Construct it once via Load and pass it down explicitly; nothing
// in this codebase should read os.Getenv outside this package.
type Config struct {
	AppEnv    Environment
	Database  DatabaseConfig
	Log       LogConfig
	Shutdown  ShutdownConfig
	HTTP      HTTPConfig
	Discovery DiscoveryConfig
	Search    SearchConfig
}

// Load reads and validates configuration from the process environment.
// It returns every validation failure it finds joined into a single error,
// so a misconfigured environment can be fixed in one pass instead of one
// failure at a time. An invalid DATABASE_URL's error message never
// includes any part of the raw value, redacted or not — only its parsed
// scheme (see schemeOf) — since an invalid value is exactly the kind of
// thing that ends up pasted into a bug report or a CI log, and there's no
// position in an unparseable or malformed URL that's reliably safe to
// echo back.
func Load() (*Config, error) {
	var errs []error

	appEnv := Environment(getEnv("APP_ENV", string(EnvDevelopment)))
	if !appEnv.valid() {
		errs = append(errs, fmt.Errorf("APP_ENV must be one of [%s %s %s], got %q",
			EnvDevelopment, EnvStaging, EnvProduction, appEnv))
	}

	dbURL, err := getEnvRequired("DATABASE_URL")
	if err != nil {
		errs = append(errs, err)
	} else if !isValidPostgresURL(dbURL) {
		// Deliberately never echoes dbURL, redacted or not: a value that
		// fails to parse as a URL at all, or that parses without a
		// recognizable scheme (e.g. a forgotten "postgres://" leaves the
		// whole string looking like opaque scheme:data to net/url), has no
		// position redactURLCredentials can reliably find a credential in.
		// The scheme alone is never a secret, so it's the only part of the
		// input this message reflects back.
		errs = append(errs, fmt.Errorf("DATABASE_URL must be a valid postgres:// or postgresql:// URL, got scheme %q", schemeOf(dbURL)))
	}

	maxConnsRaw, err := getEnvInt("DB_MAX_OPEN_CONNS", 10)
	if err != nil {
		errs = append(errs, err)
	} else if maxConnsRaw < 1 || maxConnsRaw > math.MaxInt32 {
		errs = append(errs, fmt.Errorf("DB_MAX_OPEN_CONNS must be between 1 and %d, got %d", math.MaxInt32, maxConnsRaw))
	}

	minConnsRaw, err := getEnvInt("DB_MIN_CONNS", 2)
	if err != nil {
		errs = append(errs, err)
	} else if minConnsRaw < 0 || minConnsRaw > math.MaxInt32 {
		errs = append(errs, fmt.Errorf("DB_MIN_CONNS must be between 0 and %d, got %d", math.MaxInt32, minConnsRaw))
	} else if maxConnsRaw >= 1 && minConnsRaw > maxConnsRaw {
		errs = append(errs, fmt.Errorf("DB_MIN_CONNS (%d) cannot exceed DB_MAX_OPEN_CONNS (%d)", minConnsRaw, maxConnsRaw))
	}

	connMaxLifetime, err := getEnvDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute)
	if err != nil {
		errs = append(errs, err)
	} else if connMaxLifetime <= 0 {
		errs = append(errs, fmt.Errorf("DB_CONN_MAX_LIFETIME must be positive, got %s", connMaxLifetime))
	}

	connMaxIdleTime, err := getEnvDuration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute)
	if err != nil {
		errs = append(errs, err)
	} else if connMaxIdleTime <= 0 {
		errs = append(errs, fmt.Errorf("DB_CONN_MAX_IDLE_TIME must be positive, got %s", connMaxIdleTime))
	}

	logLevel, err := parseLogLevel(getEnv("LOG_LEVEL", "info"))
	if err != nil {
		errs = append(errs, err)
	}

	logFormat := LogFormat(getEnv("LOG_FORMAT", string(LogFormatJSON)))
	if !logFormat.valid() {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be one of [%s %s], got %q", LogFormatJSON, LogFormatText, logFormat))
	}

	shutdownTimeout, err := getEnvDuration("SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		errs = append(errs, err)
	} else if shutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT must be positive, got %s", shutdownTimeout))
	}

	httpTimeout, err := getEnvDuration("HTTP_TIMEOUT", 10*time.Second)
	if err != nil {
		errs = append(errs, err)
	} else if httpTimeout <= 0 {
		errs = append(errs, fmt.Errorf("HTTP_TIMEOUT must be positive, got %s", httpTimeout))
	}

	httpMaxResponseBytes, err := getEnvInt64("HTTP_MAX_RESPONSE_SIZE", 5*1024*1024)
	if err != nil {
		errs = append(errs, err)
	} else if httpMaxResponseBytes <= 0 {
		errs = append(errs, fmt.Errorf("HTTP_MAX_RESPONSE_SIZE must be positive, got %d", httpMaxResponseBytes))
	}

	httpUserAgent := getEnv("HTTP_USER_AGENT", "remote-job-aggregator/1.0 (+https://github.com/Bantamlak12/remote-job-aggregator)")
	if strings.TrimSpace(httpUserAgent) == "" {
		errs = append(errs, errors.New("HTTP_USER_AGENT must not be blank"))
	}

	discoveryWorkersRaw, err := getEnvInt("DISCOVERY_WORKERS", 5)
	if err != nil {
		errs = append(errs, err)
	} else if discoveryWorkersRaw < 1 || discoveryWorkersRaw > 100 {
		errs = append(errs, fmt.Errorf("DISCOVERY_WORKERS must be between 1 and 100, got %d", discoveryWorkersRaw))
	}

	googleAPIKey := getEnv("GOOGLE_SEARCH_API_KEY", "")
	googleSearchEngineID := getEnv("GOOGLE_SEARCH_ENGINE_ID", "")
	if (googleAPIKey == "") != (googleSearchEngineID == "") {
		errs = append(errs, errors.New("GOOGLE_SEARCH_API_KEY and GOOGLE_SEARCH_ENGINE_ID must both be set, or neither"))
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %w", errors.Join(errs...))
	}

	return &Config{
		AppEnv: appEnv,
		Database: DatabaseConfig{
			URL: dbURL,
			// Safe: both were range-checked above against [1,math.MaxInt32]
			// and [0,math.MaxInt32] respectively before this point.
			MaxConns:        int32(maxConnsRaw),
			MinConns:        int32(minConnsRaw),
			ConnMaxLifetime: connMaxLifetime,
			ConnMaxIdleTime: connMaxIdleTime,
		},
		Log: LogConfig{
			Level:  logLevel,
			Format: logFormat,
		},
		Shutdown: ShutdownConfig{
			Timeout: shutdownTimeout,
		},
		HTTP: HTTPConfig{
			Timeout:          httpTimeout,
			MaxResponseBytes: httpMaxResponseBytes,
			UserAgent:        httpUserAgent,
		},
		Discovery: DiscoveryConfig{
			Workers: discoveryWorkersRaw,
		},
		Search: SearchConfig{
			GoogleAPIKey:         googleAPIKey,
			GoogleSearchEngineID: googleSearchEngineID,
		},
	}, nil
}

func isValidPostgresURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "postgres" || u.Scheme == "postgresql"
}

// schemeOf returns raw's URL scheme for use in an error message, or
// "(unrecognized)" if raw doesn't parse as a URL or has no scheme. Never
// returns anything else from raw — see the comment at its call site.
func schemeOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "(unrecognized)"
	}
	return u.Scheme
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL must be one of [debug info warn error], got %q", raw)
	}
}

func getEnv(key, defaultValue string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return defaultValue
}

func getEnvRequired(key string) (string, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}

func getEnvInt(key string, defaultValue int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return defaultValue, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	return v, nil
}

func getEnvInt64(key string, defaultValue int64) (int64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return defaultValue, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	return v, nil
}

func getEnvDuration(key string, defaultValue time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return defaultValue, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration (e.g. \"30s\", \"5m\"), got %q", key, raw)
	}
	return v, nil
}
