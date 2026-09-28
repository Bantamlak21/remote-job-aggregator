package config

import (
	"strings"
	"testing"
)

// loadWith clears the environment, sets the given variables and loads.
func loadWith(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
	for k, v := range env {
		t.Setenv(k, v)
	}
	return Load()
}

// prod is a complete, valid production sign-in setup; cases override single variables.
func prod(over map[string]string) map[string]string {
	env := map[string]string{
		"APP_ENV":         "production",
		"PUBLIC_BASE_URL": "https://jobs.example.com",
		"AUTH_MAILER":     "smtp",
		"SMTP_HOST":       "smtp.example.com",
		"SMTP_FROM":       "no-reply@jobs.example.com",
		"SMTP_USERNAME":   "smtpuser",
		"SMTP_PASSWORD":   "smtppass",
	}
	for k, v := range over {
		env[k] = v
	}
	return env
}

func TestLoadAuth_OffByDefault(t *testing.T) {
	cfg, err := loadWith(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth != (AuthConfig{}) {
		t.Errorf("Auth = %+v, want the zero value (sign-in is off unless PUBLIC_BASE_URL is set)", cfg.Auth)
	}
	if cfg.API.CORSAllowedOrigin != "http://localhost:5173" {
		t.Errorf("CORS default = %q, want the dev origin when sign-in is off", cfg.API.CORSAllowedOrigin)
	}
}

func TestLoadAuth_OtherAuthVariablesAreIgnoredWhileOff(t *testing.T) {
	// Outside development a blank PUBLIC_BASE_URL is an error (a likely typo, not a
	// decision) unless AUTH_ENABLED=false says so explicitly; with that said, nothing
	// about the mailer or cookie is validated, and nothing turns sign-in on.
	cfg, err := loadWith(t, map[string]string{
		"APP_ENV": "production", "AUTH_ENABLED": "false", "AUTH_MAILER": "log", "COOKIE_SECURE": "false", "SMTP_TLS": "none",
		"CORS_ALLOWED_ORIGIN": "*",
	})
	if err != nil {
		t.Fatalf("Load() = %v, want no error while sign-in is off", err)
	}
	if cfg.Auth.Enabled {
		t.Error("Auth.Enabled is true without PUBLIC_BASE_URL")
	}
}

func TestLoadAuth_MissingPublicBaseURLOutsideDevelopmentIsAnError(t *testing.T) {
	if _, err := loadWith(t, map[string]string{"APP_ENV": "production"}); err == nil ||
		!strings.Contains(err.Error(), "PUBLIC_BASE_URL is required in production or staging") {
		t.Errorf("Load() = %v, want the missing-PUBLIC_BASE_URL error", err)
	}
	if _, err := loadWith(t, map[string]string{"APP_ENV": "production", "AUTH_ENABLED": "true"}); err == nil ||
		!strings.Contains(err.Error(), "AUTH_ENABLED=true needs PUBLIC_BASE_URL") {
		t.Errorf("Load() = %v, want the AUTH_ENABLED=true error", err)
	}
}

func TestLoadAuth_ProductionSetupLoadsWithSafeDefaults(t *testing.T) {
	cfg, err := loadWith(t, prod(nil))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if !a.Enabled || a.PublicBaseURL != "https://jobs.example.com" || a.Mailer != "smtp" {
		t.Errorf("Auth = %+v", a)
	}
	if !a.CookieSecure {
		t.Error("CookieSecure defaults to false in production")
	}
	if a.CookieSameSite != "lax" {
		t.Errorf("CookieSameSite = %q, want lax", a.CookieSameSite)
	}
	if a.SMTP.Port != "587" || a.SMTP.TLS != "starttls" {
		t.Errorf("SMTP defaults = port %q tls %q, want 587 and starttls", a.SMTP.Port, a.SMTP.TLS)
	}
	if a.ClientIPHeader != "" {
		t.Errorf("ClientIPHeader = %q, want empty: never trust a client-set header by default", a.ClientIPHeader)
	}
	if cfg.API.CORSAllowedOrigin != "https://jobs.example.com" {
		t.Errorf("CORS origin = %q, want it to follow PUBLIC_BASE_URL", cfg.API.CORSAllowedOrigin)
	}
}

func TestLoadAuth_DevelopmentDefaults(t *testing.T) {
	cfg, err := loadWith(t, map[string]string{"PUBLIC_BASE_URL": "http://localhost:5173"})
	if err != nil {
		t.Fatalf("a development setup over http: %v", err)
	}
	a := cfg.Auth
	if a.Mailer != "log" {
		t.Errorf("development mailer = %q, want log", a.Mailer)
	}
	if a.CookieSecure {
		t.Error("development over http must default to a non-Secure cookie or sign-in cannot work locally")
	}
	if cfg.API.CORSAllowedOrigin != "http://localhost:5173" {
		t.Errorf("CORS origin = %q", cfg.API.CORSAllowedOrigin)
	}

	// The log mailer is refused with an https PUBLIC_BASE_URL even in development (a
	// forgotten APP_ENV must not be enough to print a live link), so this case needs an
	// explicit smtp mailer; development does not require SMTP credentials.
	cfg, err = loadWith(t, map[string]string{
		"PUBLIC_BASE_URL": "https://dev.example.com", "AUTH_MAILER": "smtp", "SMTP_HOST": "h.example.com", "SMTP_FROM": "a@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.CookieSecure {
		t.Error("development over https must default to a Secure cookie")
	}
}

func TestLoadAuth_LogMailerRefusedWithAnHTTPSBaseURLEvenInDevelopment(t *testing.T) {
	if _, err := loadWith(t, map[string]string{"PUBLIC_BASE_URL": "https://dev.example.com"}); err == nil ||
		!strings.Contains(err.Error(), "AUTH_MAILER=log is not allowed with an https PUBLIC_BASE_URL") {
		t.Errorf("Load() = %v, want the https+log error", err)
	}
	if _, err := loadWith(t, map[string]string{"PUBLIC_BASE_URL": "http://localhost:5173"}); err != nil {
		t.Errorf("the log mailer over plain http in development must still work: %v", err)
	}
}

func TestLoadAuth_StagingDefaultsToSMTPAndSecure(t *testing.T) {
	_, err := loadWith(t, map[string]string{"APP_ENV": "staging", "PUBLIC_BASE_URL": "https://stage.example.com"})
	if err == nil || !strings.Contains(err.Error(), "AUTH_MAILER=smtp needs SMTP_HOST and SMTP_FROM") {
		t.Errorf("staging without a mailer = %v, want the default to be smtp (which needs a host)", err)
	}
	cfg, err := loadWith(t, map[string]string{
		"APP_ENV": "staging", "PUBLIC_BASE_URL": "https://stage.example.com", "SMTP_HOST": "h.example.com", "SMTP_FROM": "a@example.com",
		"SMTP_USERNAME": "u", "SMTP_PASSWORD": "p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.CookieSecure {
		t.Error("staging defaults to an insecure cookie")
	}
}

func TestLoadAuth_StagingIsHeldToTheProductionRules(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"http base URL":   {"PUBLIC_BASE_URL": "http://stage.example.com"},
		"log mailer":      {"AUTH_MAILER": "log"},
		"insecure cookie": {"COOKIE_SECURE": "false"},
		"smtp tls none":   {"SMTP_TLS": "none"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"APP_ENV": "staging", "PUBLIC_BASE_URL": "https://stage.example.com",
				"SMTP_HOST": "h.example.com", "SMTP_FROM": "a@example.com"}
			for k, v := range extra {
				env[k] = v
			}
			if _, err := loadWith(t, env); err == nil {
				t.Errorf("staging accepted %s", name)
			}
		})
	}
}

func TestLoadAuth_RefusedConfigurations(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string // a substring of the error
	}{
		{"http base URL in production", prod(map[string]string{"PUBLIC_BASE_URL": "http://jobs.example.com"}), "PUBLIC_BASE_URL must be https in production"},
		{"log mailer in production", prod(map[string]string{"AUTH_MAILER": "log"}), "AUTH_MAILER=log is not allowed in production"},
		{"log mailer in production, upper case", prod(map[string]string{"AUTH_MAILER": "LOG"}), "AUTH_MAILER=log is not allowed in production"},
		{"insecure cookie in production", prod(map[string]string{"COOKIE_SECURE": "false"}), "COOKIE_SECURE=false is not allowed in production"},
		{"insecure cookie in production, 0", prod(map[string]string{"COOKIE_SECURE": "0"}), "COOKIE_SECURE=false is not allowed in production"},
		{"smtp without host", prod(map[string]string{"SMTP_HOST": ""}), "needs SMTP_HOST and SMTP_FROM"},
		{"smtp without from", prod(map[string]string{"SMTP_FROM": ""}), "needs SMTP_HOST and SMTP_FROM"},
		{"smtp from is not an address", prod(map[string]string{"SMTP_FROM": "not an address"}), "SMTP_FROM is not a valid address"},
		{"smtp tls none in production", prod(map[string]string{"SMTP_TLS": "none"}), "SMTP_TLS=none is not allowed in production"},
		{"smtp tls unknown", prod(map[string]string{"SMTP_TLS": "ssl"}), "SMTP_TLS must be starttls, tls or none"},
		{"smtp username without password", prod(map[string]string{"SMTP_USERNAME": "apikey", "SMTP_PASSWORD": ""}), "SMTP_USERNAME and SMTP_PASSWORD must be set together"},
		{"smtp password without username", prod(map[string]string{"SMTP_PASSWORD": "pw", "SMTP_USERNAME": ""}), "SMTP_USERNAME and SMTP_PASSWORD must be set together"},
		{"smtp without credentials in production", prod(map[string]string{"SMTP_USERNAME": "", "SMTP_PASSWORD": ""}), "needs SMTP_USERNAME and SMTP_PASSWORD in production"},
		{"smtp port not a number", prod(map[string]string{"SMTP_PORT": "smtp"}), "SMTP_PORT must be a number"},
		{"unknown mailer", prod(map[string]string{"AUTH_MAILER": "sendgrid"}), "AUTH_MAILER must be smtp or log"},
		{"samesite none is no longer accepted", prod(map[string]string{"COOKIE_SAMESITE": "none"}), "COOKIE_SAMESITE must be lax or strict"},
		{"samesite unknown", prod(map[string]string{"COOKIE_SAMESITE": "relaxed"}), "COOKIE_SAMESITE must be lax or strict"},
		{"cookie secure not a boolean", prod(map[string]string{"COOKIE_SECURE": "maybe"}), "COOKIE_SECURE must be true or false"},
		{"CORS differs from the base URL", prod(map[string]string{"CORS_ALLOWED_ORIGIN": "https://other.example.com"}), "CORS_ALLOWED_ORIGIN"},
		{"CORS wildcard", prod(map[string]string{"CORS_ALLOWED_ORIGIN": "*"}), "CORS_ALLOWED_ORIGIN"},
		{"CORS wildcard in development", map[string]string{"PUBLIC_BASE_URL": "http://localhost:5173", "CORS_ALLOWED_ORIGIN": "*"}, "must equal PUBLIC_BASE_URL"},
		{"CORS with a trailing slash", prod(map[string]string{"CORS_ALLOWED_ORIGIN": "https://jobs.example.com/"}), "must equal PUBLIC_BASE_URL"},
		{"CORS with another scheme", prod(map[string]string{"CORS_ALLOWED_ORIGIN": "http://jobs.example.com"}), "must equal PUBLIC_BASE_URL"},
		{"CORS a subdomain", prod(map[string]string{"CORS_ALLOWED_ORIGIN": "https://evil.jobs.example.com"}), "must equal PUBLIC_BASE_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, tc.env)
			if err == nil {
				t.Fatalf("Load() succeeded with %+v, want an error containing %q", cfg.Auth, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadAuth_BadPublicBaseURLForms(t *testing.T) {
	for _, raw := range []string{
		"jobs.example.com",                 // no scheme
		"//jobs.example.com",               // scheme-relative
		"ftp://jobs.example.com",           // wrong scheme
		"javascript:alert(1)",              // not a web origin
		"https://",                         // no host
		"https:///path",                    // no host
		"https://jobs.example.com/app",     // a path
		"https://jobs.example.com/app/",    // a path
		"https://jobs.example.com?x=1",     // a query
		"https://jobs.example.com/?x=1",    // a query
		"https://jobs.example.com#frag",    // a fragment
		"https://user@jobs.example.com",    // credentials
		"https://user:pw@jobs.example.com", // credentials
		"https://jobs .example.com",        // a space
		"http://%zz",                       // unparsable
	} {
		t.Run(raw, func(t *testing.T) {
			cfg, err := loadWith(t, prod(map[string]string{"PUBLIC_BASE_URL": raw}))
			if err == nil {
				t.Fatalf("PUBLIC_BASE_URL=%q accepted as %q", raw, cfg.Auth.PublicBaseURL)
			}
			if !strings.Contains(err.Error(), "PUBLIC_BASE_URL must be an origin") {
				t.Errorf("error = %q, want the origin message", err.Error())
			}
		})
	}
}

func TestLoadAuth_PublicBaseURLIsNormalizedToTheOriginABrowserSends(t *testing.T) {
	// A browser's Origin header is lower case and omits the scheme's default port; the
	// CSRF guard compares it to this value exactly, so a config that differs only in
	// those respects would silently refuse every sign-in request.
	for raw, want := range map[string]string{
		"https://jobs.example.com":      "https://jobs.example.com",
		"https://jobs.example.com/":     "https://jobs.example.com",
		"  https://jobs.example.com  ":  "https://jobs.example.com",
		"https://Jobs.Example.COM":      "https://jobs.example.com",
		"HTTPS://jobs.example.com":      "https://jobs.example.com",
		"https://jobs.example.com:443":  "https://jobs.example.com",
		"https://jobs.example.com:8443": "https://jobs.example.com:8443",
		"http://localhost:5173":         "http://localhost:5173",
		"http://localhost:80":           "http://localhost",
		"https://jobs.example.com:80":   "https://jobs.example.com:80",
	} {
		env := map[string]string{"PUBLIC_BASE_URL": raw}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "https") {
			env["SMTP_HOST"], env["SMTP_FROM"], env["AUTH_MAILER"] = "h.example.com", "a@example.com", "smtp"
		}
		cfg, err := loadWith(t, env)
		if err != nil {
			t.Errorf("PUBLIC_BASE_URL=%q: %v", raw, err)
			continue
		}
		if cfg.Auth.PublicBaseURL != want {
			t.Errorf("PUBLIC_BASE_URL=%q -> %q, want %q", raw, cfg.Auth.PublicBaseURL, want)
		}
		if cfg.API.CORSAllowedOrigin != want {
			t.Errorf("PUBLIC_BASE_URL=%q -> CORS origin %q, want %q", raw, cfg.API.CORSAllowedOrigin, want)
		}
	}
}

func TestLoadAuth_AcceptedVariations(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, a AuthConfig)
	}{
		{"explicit CORS equal to the base URL", prod(map[string]string{"CORS_ALLOWED_ORIGIN": "https://jobs.example.com"}), nil},
		{"samesite strict", prod(map[string]string{"COOKIE_SAMESITE": "strict"}), func(t *testing.T, a AuthConfig) {
			if a.CookieSameSite != "strict" {
				t.Errorf("%+v", a)
			}
		}},
		{"implicit TLS on 465 with a login", prod(map[string]string{"SMTP_TLS": "tls", "SMTP_PORT": "465", "SMTP_USERNAME": "u", "SMTP_PASSWORD": "p"}), func(t *testing.T, a AuthConfig) {
			if a.SMTP.TLS != "tls" || a.SMTP.Port != "465" || a.SMTP.Username != "u" || a.SMTP.Password != "p" {
				t.Errorf("%+v", a.SMTP)
			}
		}},
		{"From with a display name", prod(map[string]string{"SMTP_FROM": "Jobs <no-reply@jobs.example.com>"}), nil},
		{"client IP header", prod(map[string]string{"CLIENT_IP_HEADER": "  X-Real-IP "}), func(t *testing.T, a AuthConfig) {
			if a.ClientIPHeader != "X-Real-IP" {
				t.Errorf("ClientIPHeader = %q", a.ClientIPHeader)
			}
		}},
		{"tls none and log mailer outside production", map[string]string{
			"PUBLIC_BASE_URL": "http://localhost:5173", "AUTH_MAILER": "smtp", "SMTP_HOST": "localhost", "SMTP_PORT": "1025", "SMTP_FROM": "a@example.com", "SMTP_TLS": "none"}, func(t *testing.T, a AuthConfig) {
			if a.SMTP.TLS != "none" {
				t.Errorf("%+v", a.SMTP)
			}
		}},
		{"explicit secure cookie in development", map[string]string{"PUBLIC_BASE_URL": "http://localhost:5173", "COOKIE_SECURE": "true"}, func(t *testing.T, a AuthConfig) {
			if !a.CookieSecure {
				t.Errorf("%+v", a)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, tc.env)
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			if !cfg.Auth.Enabled {
				t.Fatal("Auth is not enabled")
			}
			if tc.check != nil {
				tc.check(t, cfg.Auth)
			}
		})
	}
}

func TestLoadAuth_ReportsEveryProblemAtOnce(t *testing.T) {
	_, err := loadWith(t, map[string]string{
		"APP_ENV": "production", "PUBLIC_BASE_URL": "http://jobs.example.com", "AUTH_MAILER": "log", "COOKIE_SECURE": "false",
		"CORS_ALLOWED_ORIGIN": "*",
	})
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"must be https in production", "AUTH_MAILER=log", "COOKIE_SECURE=false", "must equal PUBLIC_BASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestLoadAuth_ErrorsNeverEchoSecrets(t *testing.T) {
	_, err := loadWith(t, prod(map[string]string{"SMTP_USERNAME": "apikey-user", "SMTP_PASSWORD": "", "SMTP_TLS": "none"}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "apikey-user") {
		t.Errorf("the error repeats the SMTP username: %v", err)
	}
	_, err = loadWith(t, prod(map[string]string{"SMTP_USERNAME": "u", "SMTP_PASSWORD": "hunter2-secret", "SMTP_TLS": "bogus"}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "hunter2-secret") {
		t.Errorf("the error repeats the SMTP password: %v", err)
	}
}
