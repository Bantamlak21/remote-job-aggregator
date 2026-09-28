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
	"net"
	"net/mail"
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

// APIConfig configures the public read-only job API server (the
// `serve` command). Addr is a net.Listen address (":8080", not just a
// bare port) so it matches http.Server.Addr's own documented form
// directly. CORSAllowedOrigin is a single explicit origin, not "*" —
// the frontend is a known, specific origin during development
// (Vite's default dev server), and an explicit allow-list origin is
// the deliberate choice over a wildcard even though this API serves
// only public, non-authenticated data.
type APIConfig struct {
	Addr              string
	CORSAllowedOrigin string
	// JobRepository selects which internal/job.Repository implementation
	// serve constructs: "postgres" (the real, Phase 3 default) or "mock"
	// (internal/job.MockRepository's 12 fixture jobs — no database
	// required, useful for frontend development against guaranteed
	// non-empty data before any real ingestion has run).
	JobRepository string
	// JobMaxAge hides jobs whose posting date is older than this from the
	// public API (list and detail). Zero disables the limit. The rows stay
	// in the database; only what the API serves changes. Applies to the
	// postgres repository only (the mock's fixtures are illustrative).
	JobMaxAge time.Duration
}

const (
	JobRepositoryPostgres = "postgres"
	JobRepositoryMock     = "mock"
)

// DiscoveryConfig bounds discovery's own concurrency, independent of the
// database pool or any future ingestion worker pool (CLAUDE.md: "Database
// and HTTP concurrency should also be bounded" — separately from each
// other).
type DiscoveryConfig struct {
	Workers int
}

// IngestionConfig bounds ingestion's own concurrency (one target board
// fetched at a time per worker), independent of the database pool,
// discovery's own worker pool, and the HTTP client's connection pool.
//
// MaxResponseBytes/Timeout size the ingestion HTTP client separately
// from the shared HTTP_* defaults: a real board fetched with
// content=true is far larger than a discovery probe or a search
// response (measured live: databricks, 883 jobs, is 9.7 MB decoded;
// stripe, 695 jobs, 5.4 MB — both over the 5 MiB HTTP_MAX_RESPONSE_SIZE
// default, which would fail those boards on every run).
type IngestionConfig struct {
	Workers          int
	MaxResponseBytes int64
	Timeout          time.Duration
	// EthiojobsMaxPages caps how many listing pages (12 jobs each) one run
	// of the Ethiojobs collector reads. The site has about 100.
	EthiojobsMaxPages int
	// HimalayasMaxPages caps how many pages (20 jobs each) one Himalayas
	// collection reads. The feed holds about 100,000 jobs, newest first; a run
	// stops sooner when it passes the age window.
	HimalayasMaxPages int
}

// SearchConfig configures the Serper (google.serper.dev) search API
// client used by the search-based discovery mechanism. Optional — unlike
// DATABASE_URL, most commands (run, migrate-*, the seed-file discover)
// never need it, so Load does not require it. Unlike the Google Custom
// Search API this replaced, Serper needs only one credential: it wraps
// regular Google search directly rather than a separately provisioned
// "custom search engine," so there is no second id to configure or
// validate as a pair.
type SearchConfig struct {
	SerperAPIKey string
	// MaxQueriesPerRun caps how many Serper queries one "ingest" run may
	// spend on the search-based job source (two per priority company).
	// Serper's free tier is a fixed 2,500 queries, not a monthly
	// allowance, so a misconfigured cron must not be able to burn it.
	MaxQueriesPerRun int
}

// LogValue redacts the API key so a SearchConfig can be logged directly
// without leaking a credential — same reasoning as DatabaseConfig's
// LogValue, applied before this struct has ever actually been logged
// anywhere, since that mistake is cheaper to prevent than to catch.
func (s SearchConfig) LogValue() slog.Value {
	key := s.SerperAPIKey
	if key != "" {
		key = "REDACTED"
	}
	return slog.GroupValue(slog.String("serper_api_key", key), slog.Int("max_queries_per_run", s.MaxQueriesPerRun))
}

// Configured reports whether the search credential is present, i.e.
// whether search-based discovery can actually run.
func (s SearchConfig) Configured() bool {
	return s.SerperAPIKey != ""
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
	Ingestion IngestionConfig
	Search    SearchConfig
	API       APIConfig
	Filtering FilteringConfig
	Auth      AuthConfig
}

// AuthConfig configures sign-in and saved jobs. They are on exactly when
// PUBLIC_BASE_URL is set: that is the origin of the web app, where sign-in links
// point, and the only origin allowed to make state-changing requests.
type AuthConfig struct {
	Enabled bool
	// PublicBaseURL is the web app's origin ("https://jobs.example.com"), without a path.
	PublicBaseURL string
	// Mailer is "smtp" or "log". "log" prints sign-in links to the log and is for
	// development only.
	Mailer string
	SMTP   SMTPConfig
	// CookieSecure marks the session cookie HTTPS-only (and gives it the __Host- prefix).
	CookieSecure bool
	// CookieSameSite is "lax" (the default) or "strict". The web app and the API are
	// served from one site, so the cookie is first-party.
	CookieSameSite string
	// ClientIPHeader names a header a trusted reverse proxy overwrites with the client's
	// address ("X-Real-IP"); empty uses the connection's address. Never set it unless a
	// proxy always overwrites it: a client could otherwise choose its own rate-limit key.
	// For X-Forwarded-For the last entry is used (the one the single trusted proxy appended).
	ClientIPHeader string
	// MaxLinksPerHour caps the sign-in links the whole site emails in an hour (the
	// circuit breaker on mail cost and sender reputation).
	MaxLinksPerHour int
}

// SMTPConfig is the outgoing mail server.
type SMTPConfig struct {
	Host, Port, Username, Password, From string
	// TLS is "starttls" (port 587), "tls" (port 465, TLS from the first byte) or "none"
	// (a local relay; refused in production).
	TLS string
}

// FilteringConfig configures job classification (internal/filtering).
type FilteringConfig struct {
	// RelevanceProfile is the path of a JSON role profile (which families a
	// title falls in and which count as relevant); empty means the built-in
	// one, which counts software, data, DevOps and security roles as relevant.
	// See internal/filtering/relevance/profile.json for the format.
	RelevanceProfile string
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

	ingestionWorkersRaw, err := getEnvInt("INGESTION_WORKERS", 5)
	if err != nil {
		errs = append(errs, err)
	} else if ingestionWorkersRaw < 1 || ingestionWorkersRaw > 100 {
		errs = append(errs, fmt.Errorf("INGESTION_WORKERS must be between 1 and 100, got %d", ingestionWorkersRaw))
	}

	ingestionMaxBytes, err := getEnvInt64("INGESTION_MAX_RESPONSE_SIZE", 32*1024*1024)
	if err != nil {
		errs = append(errs, err)
	} else if ingestionMaxBytes <= 0 {
		errs = append(errs, fmt.Errorf("INGESTION_MAX_RESPONSE_SIZE must be positive, got %d", ingestionMaxBytes))
	}

	ingestionTimeout, err := getEnvDuration("INGESTION_HTTP_TIMEOUT", 30*time.Second)
	if err != nil {
		errs = append(errs, err)
	} else if ingestionTimeout <= 0 {
		errs = append(errs, fmt.Errorf("INGESTION_HTTP_TIMEOUT must be positive, got %s", ingestionTimeout))
	}

	ethiojobsMaxPages, err := getEnvInt("ETHIOJOBS_MAX_PAGES", 100)
	if err != nil {
		errs = append(errs, err)
	} else if ethiojobsMaxPages < 1 || ethiojobsMaxPages > 200 {
		errs = append(errs, fmt.Errorf("ETHIOJOBS_MAX_PAGES must be between 1 and 200, got %d", ethiojobsMaxPages))
	}

	himalayasMaxPages, err := getEnvInt("HIMALAYAS_MAX_PAGES", 50)
	if err != nil {
		errs = append(errs, err)
	} else if himalayasMaxPages < 1 || himalayasMaxPages > 100 {
		errs = append(errs, fmt.Errorf("HIMALAYAS_MAX_PAGES must be between 1 and 100, got %d", himalayasMaxPages))
	}

	auth := loadAuth(appEnv, &errs)

	jobRepository := getEnv("JOB_REPOSITORY", JobRepositoryPostgres)
	if jobRepository != JobRepositoryPostgres && jobRepository != JobRepositoryMock {
		errs = append(errs, fmt.Errorf("JOB_REPOSITORY must be one of [%s %s], got %q",
			JobRepositoryPostgres, JobRepositoryMock, jobRepository))
	}

	serperAPIKey := getEnv("SERPER_API_KEY", "")

	searchMaxQueries, err := getEnvInt("SEARCH_MAX_QUERIES_PER_RUN", 60)
	if err != nil {
		errs = append(errs, err)
	} else if searchMaxQueries < 1 || searchMaxQueries > 2500 {
		errs = append(errs, fmt.Errorf("SEARCH_MAX_QUERIES_PER_RUN must be between 1 and 2500, got %d", searchMaxQueries))
	}

	apiAddr := getEnv("API_ADDR", ":8080")
	if strings.TrimSpace(apiAddr) == "" {
		errs = append(errs, errors.New("API_ADDR must not be blank"))
	} else if _, _, err := net.SplitHostPort(apiAddr); err != nil {
		errs = append(errs, fmt.Errorf("API_ADDR must be a valid host:port (e.g. \":8080\"): %w", err))
	}

	corsDefault := "http://localhost:5173"
	if auth.Enabled && auth.PublicBaseURL != "" {
		corsDefault = auth.PublicBaseURL
	}
	corsAllowedOrigin := getEnv("CORS_ALLOWED_ORIGIN", corsDefault)
	if strings.TrimSpace(corsAllowedOrigin) == "" {
		errs = append(errs, errors.New("CORS_ALLOWED_ORIGIN must not be blank"))
	}
	if auth.Enabled && auth.PublicBaseURL != "" && corsAllowedOrigin != auth.PublicBaseURL {
		// Cookies are sent only to the one origin that may read the responses, and that
		// origin is the web app: "*" (or any other origin) with sign-in on is a mistake.
		errs = append(errs, fmt.Errorf("CORS_ALLOWED_ORIGIN (%q) must equal PUBLIC_BASE_URL (%q) when sign-in is on", corsAllowedOrigin, auth.PublicBaseURL))
	}

	jobMaxAgeDays, err := getEnvInt("JOB_MAX_AGE_DAYS", 15)
	if err != nil {
		errs = append(errs, err)
	} else if jobMaxAgeDays < 0 || jobMaxAgeDays > 3650 {
		errs = append(errs, fmt.Errorf("JOB_MAX_AGE_DAYS must be between 0 (no limit) and 3650, got %d", jobMaxAgeDays))
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
		Ingestion: IngestionConfig{
			Workers:           ingestionWorkersRaw,
			MaxResponseBytes:  ingestionMaxBytes,
			Timeout:           ingestionTimeout,
			EthiojobsMaxPages: ethiojobsMaxPages,
			HimalayasMaxPages: himalayasMaxPages,
		},
		Filtering: FilteringConfig{
			RelevanceProfile: strings.TrimSpace(getEnv("RELEVANCE_PROFILE", "")),
		},
		Search: SearchConfig{
			SerperAPIKey:     serperAPIKey,
			MaxQueriesPerRun: searchMaxQueries,
		},
		API: APIConfig{
			Addr:              apiAddr,
			CORSAllowedOrigin: corsAllowedOrigin,
			JobRepository:     jobRepository,
			JobMaxAge:         time.Duration(jobMaxAgeDays) * 24 * time.Hour,
		},
		Auth: auth,
	}, nil
}

// loadAuth reads and validates the sign-in settings. Everything here fails closed:
// an unsafe combination (a wildcard CORS origin with cookies, the log mailer or an
// insecure cookie, a sign-in link that is not HTTPS) is an error at startup, never a
// warning. Staging is treated like production: only development may relax these.
func loadAuth(env Environment, errs *[]error) AuthConfig {
	raw := strings.TrimSpace(getEnv("PUBLIC_BASE_URL", ""))
	// AUTH_ENABLED is the explicit switch: "false" runs without sign-in whatever else
	// is set; "true" demands PUBLIC_BASE_URL. Unset, sign-in is on exactly when
	// PUBLIC_BASE_URL is set in development, and outside development the operator must
	// choose: a missing PUBLIC_BASE_URL there is more likely a typo in the environment
	// than a decision, and would ship a site with no sign-in without a word.
	explicit := strings.TrimSpace(getEnv("AUTH_ENABLED", ""))
	if explicit != "" {
		on, err := strconv.ParseBool(explicit)
		if err != nil {
			*errs = append(*errs, errors.New("AUTH_ENABLED must be true or false"))
			return AuthConfig{}
		}
		if !on {
			return AuthConfig{}
		}
	}
	if raw == "" {
		switch {
		case explicit != "":
			*errs = append(*errs, errors.New("AUTH_ENABLED=true needs PUBLIC_BASE_URL"))
		case env != EnvDevelopment:
			*errs = append(*errs, errors.New("PUBLIC_BASE_URL is required in production or staging (set AUTH_ENABLED=false to run without sign-in)"))
		}
		return AuthConfig{}
	}
	a := AuthConfig{Enabled: true}
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		*errs = append(*errs, errors.New("PUBLIC_BASE_URL must be an origin such as https://jobs.example.com (no path, query or credentials)"))
	default:
		// The origin exactly as a browser writes it in the Origin header (lower case,
		// no default port): the CSRF guard compares it byte for byte, so any other
		// spelling would make every sign-in request fail with 403.
		host := strings.ToLower(u.Host)
		if p := u.Port(); (u.Scheme == "https" && p == "443") || (u.Scheme == "http" && p == "80") {
			host = strings.TrimSuffix(host, ":"+p)
		}
		a.PublicBaseURL = u.Scheme + "://" + host
		if env != EnvDevelopment && u.Scheme != "https" {
			*errs = append(*errs, errors.New("PUBLIC_BASE_URL must be https in production or staging"))
		}
	}

	a.Mailer = strings.ToLower(getEnv("AUTH_MAILER", defaultMailer(env)))
	switch a.Mailer {
	case "smtp":
		a.SMTP = SMTPConfig{
			Host: strings.TrimSpace(getEnv("SMTP_HOST", "")), Port: getEnv("SMTP_PORT", "587"),
			Username: getEnv("SMTP_USERNAME", ""), Password: getEnv("SMTP_PASSWORD", ""),
			From: strings.TrimSpace(getEnv("SMTP_FROM", "")), TLS: strings.ToLower(getEnv("SMTP_TLS", "starttls")),
		}
		if a.SMTP.Host == "" || a.SMTP.From == "" {
			*errs = append(*errs, errors.New("AUTH_MAILER=smtp needs SMTP_HOST and SMTP_FROM"))
		}
		if env != EnvDevelopment && (a.SMTP.Username == "" || a.SMTP.Password == "") {
			// Without credentials every send fails at runtime while /auth/login still answers
			// 202: sign-in would look fine and never deliver.
			*errs = append(*errs, errors.New("AUTH_MAILER=smtp needs SMTP_USERNAME and SMTP_PASSWORD in production or staging"))
		}
		if _, err := mail.ParseAddress(a.SMTP.From); a.SMTP.From != "" && err != nil {
			*errs = append(*errs, errors.New("SMTP_FROM is not a valid address"))
		}
		switch a.SMTP.TLS {
		case "starttls", "tls":
		case "none":
			if env != EnvDevelopment {
				*errs = append(*errs, errors.New("SMTP_TLS=none is not allowed in production or staging"))
			}
		default:
			*errs = append(*errs, fmt.Errorf("SMTP_TLS must be starttls, tls or none, got %q", a.SMTP.TLS))
		}
		if (a.SMTP.Username == "") != (a.SMTP.Password == "") {
			*errs = append(*errs, errors.New("SMTP_USERNAME and SMTP_PASSWORD must be set together"))
		}
		if _, err := strconv.Atoi(a.SMTP.Port); err != nil {
			*errs = append(*errs, fmt.Errorf("SMTP_PORT must be a number, got %q", a.SMTP.Port))
		}
	case "log":
		if env != EnvDevelopment {
			*errs = append(*errs, errors.New("AUTH_MAILER=log is not allowed in production or staging: the log would hold live sign-in links"))
		} else if strings.HasPrefix(a.PublicBaseURL, "https://") {
			// APP_ENV defaults to development, so a forgotten APP_ENV must not be enough to
			// print live links: an https site is a real one.
			*errs = append(*errs, errors.New("AUTH_MAILER=log is not allowed with an https PUBLIC_BASE_URL (is APP_ENV set?): the log would hold live sign-in links"))
		}
	default:
		*errs = append(*errs, fmt.Errorf("AUTH_MAILER must be smtp or log, got %q", a.Mailer))
	}

	secureDefault := "true"
	if env == EnvDevelopment && strings.HasPrefix(a.PublicBaseURL, "http://") {
		secureDefault = "false"
	}
	secure, err := strconv.ParseBool(getEnv("COOKIE_SECURE", secureDefault))
	if err != nil {
		*errs = append(*errs, errors.New("COOKIE_SECURE must be true or false"))
	}
	a.CookieSecure = secure
	if env != EnvDevelopment && !secure {
		*errs = append(*errs, errors.New("COOKIE_SECURE=false is not allowed in production or staging"))
	}
	a.CookieSameSite = strings.ToLower(getEnv("COOKIE_SAMESITE", "lax"))
	switch a.CookieSameSite {
	case "lax", "strict":
	default:
		*errs = append(*errs, fmt.Errorf("COOKIE_SAMESITE must be lax or strict, got %q (serve the web app and the API from one site)", a.CookieSameSite))
	}
	a.ClientIPHeader = strings.TrimSpace(getEnv("CLIENT_IP_HEADER", ""))
	if a.ClientIPHeader != "" {
		ok := false
		for _, h := range clientIPHeaders {
			ok = ok || strings.EqualFold(a.ClientIPHeader, h)
		}
		if !ok {
			*errs = append(*errs, fmt.Errorf("CLIENT_IP_HEADER must be one of %s, got %q", strings.Join(clientIPHeaders, ", "), a.ClientIPHeader))
		}
	}
	links, err := getEnvInt("AUTH_MAX_LINKS_PER_HOUR", 300)
	if err != nil {
		*errs = append(*errs, err)
	} else if links < 1 || links > 100000 {
		*errs = append(*errs, fmt.Errorf("AUTH_MAX_LINKS_PER_HOUR must be between 1 and 100000, got %d", links))
	}
	a.MaxLinksPerHour = links
	return a
}

// clientIPHeaders are the headers a reverse proxy commonly sets to the client's address.
// Anything else (Host, Cookie, Origin...) is either attacker-controlled or meaningless, so
// naming it is a mistake.
var clientIPHeaders = []string{"X-Real-IP", "X-Forwarded-For", "CF-Connecting-IP", "True-Client-IP", "Fly-Client-IP"}

func defaultMailer(env Environment) string {
	if env == EnvDevelopment {
		return "log"
	}
	return "smtp"
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
