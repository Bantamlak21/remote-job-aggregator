package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
)

func TestNewLogger_FiltersBelowConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormatJSON}, &buf)

	logger.Debug("should be filtered out")
	logger.Info("should appear")

	out := buf.String()
	if strings.Contains(out, "should be filtered out") {
		t.Errorf("Debug message appeared despite Level=Info: %s", out)
	}
	if !strings.Contains(out, "should appear") {
		t.Errorf("Info message missing: %s", out)
	}
}

func TestNewLogger_DebugLevelAllowsDebugMessages(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(config.LogConfig{Level: slog.LevelDebug, Format: config.LogFormatJSON}, &buf)

	logger.Debug("debug message")

	if !strings.Contains(buf.String(), "debug message") {
		t.Errorf("Debug message missing at Level=Debug: %s", buf.String())
	}
}

func TestNewLogger_JSONFormatProducesValidJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormatJSON}, &buf)

	logger.Info("hello", "key", "value")

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON handler produced invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if decoded["msg"] != "hello" {
		t.Errorf("decoded msg = %v, want %q", decoded["msg"], "hello")
	}
	if decoded["key"] != "value" {
		t.Errorf("decoded key = %v, want %q", decoded["key"], "value")
	}
}

func TestNewLogger_TextFormatIsNotJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormatText}, &buf)

	logger.Info("hello", "key", "value")

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err == nil {
		t.Errorf("text handler produced parseable JSON, want key=value text: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("text output = %q, want it to contain msg=hello", buf.String())
	}
}

// Regression test: DatabaseConfig must never leak its credential through
// a real slog.Handler, not just through its own LogValue().String() in
// isolation — this is the path a leaked password would actually travel in
// production, and is what let the query-string-password bug (D2) survive
// a unit test that only checked LogValue() directly.
func TestNewLogger_RedactsDatabaseConfigCredentials(t *testing.T) {
	cases := map[string]config.DatabaseConfig{
		"userinfo password":     {URL: "postgres://user:supersecret@localhost:5432/jobs"},
		"query-string password": {URL: "postgres://user@localhost:5432/jobs?password=supersecret&sslmode=disable"},
	}

	for name, dbCfg := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := NewLogger(config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormatJSON}, &buf)

			logger.Info("starting aggregator", "database", dbCfg)

			out := buf.String()
			if strings.Contains(out, "supersecret") {
				t.Errorf("logger output leaked the password: %s", out)
			}
			if !strings.Contains(out, "REDACTED") {
				t.Errorf("logger output = %q, want it to contain REDACTED", out)
			}
		})
	}
}

// Regression test: SearchConfig must never leak GOOGLE_SEARCH_API_KEY
// through a real slog.Handler, mirroring
// TestNewLogger_RedactsDatabaseConfigCredentials above — a config test
// that only calls LogValue().String() in isolation would miss a
// regression where slog's own attribute-resolution stopped calling
// LogValue at all (e.g. a field passed as a raw struct rather than
// through slog.Any, or a handler that doesn't call Resolve).
func TestNewLogger_RedactsSearchConfigCredentials(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormatJSON}, &buf)

	searchCfg := config.SearchConfig{
		GoogleAPIKey:         "AIzaSyTOPSECRETKEY123456789",
		GoogleSearchEngineID: "test-engine-id",
	}
	logger.Info("starting search-discover", "search", searchCfg)

	out := buf.String()
	if strings.Contains(out, "AIzaSyTOPSECRETKEY123456789") {
		t.Errorf("logger output leaked the search API key: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("logger output = %q, want it to contain REDACTED", out)
	}
	if !strings.Contains(out, "test-engine-id") {
		t.Errorf("logger output = %q, want the non-secret engine ID to still be visible", out)
	}
}

func TestNewLogger_DefaultsToJSONForUnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormat("bogus")}, &buf)

	logger.Info("hello")

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Errorf("expected JSON fallback for an unrecognized format, got: %s", buf.String())
	}
}

func TestNewLogger_IncludesTimestamp(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormatJSON}, &buf)

	before := time.Now()
	logger.Info("hello")

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	ts, ok := decoded["time"].(string)
	if !ok {
		t.Fatalf("decoded output has no \"time\" field: %v", decoded)
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("time field %q is not RFC3339: %v", ts, err)
	}
	if parsed.Before(before.Add(-time.Second)) {
		t.Errorf("logged time %s is implausibly earlier than test start %s", parsed, before)
	}
}
