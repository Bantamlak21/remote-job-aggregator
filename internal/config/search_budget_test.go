package config

import (
	"strings"
	"testing"
)

func TestLoad_SearchMaxQueriesPerRunDefaultsTo60(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Search.MaxQueriesPerRun != 60 {
		t.Errorf("MaxQueriesPerRun = %d, want 60 (two queries for each of 25 companies plus headroom)", cfg.Search.MaxQueriesPerRun)
	}
}

func TestLoad_SearchMaxQueriesPerRunValidation(t *testing.T) {
	cases := []struct {
		value   string
		want    int
		wantErr string
	}{
		{"1", 1, ""},
		{"2500", 2500, ""},
		{"0", 0, "SEARCH_MAX_QUERIES_PER_RUN must be between 1 and 2500"},
		{"-5", 0, "SEARCH_MAX_QUERIES_PER_RUN must be between 1 and 2500"},
		{"2501", 0, "SEARCH_MAX_QUERIES_PER_RUN must be between 1 and 2500"},
		{"lots", 0, "SEARCH_MAX_QUERIES_PER_RUN"},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			clearAll(t)
			t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
			t.Setenv("SEARCH_MAX_QUERIES_PER_RUN", tc.value)

			cfg, err := Load()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Search.MaxQueriesPerRun != tc.want {
				t.Errorf("MaxQueriesPerRun = %d, want %d", cfg.Search.MaxQueriesPerRun, tc.want)
			}
		})
	}
}

func TestSearchConfig_LogValueStillRedactsTheKeyAndShowsTheBudget(t *testing.T) {
	got := SearchConfig{SerperAPIKey: "sk-super-secret", MaxQueriesPerRun: 60}.LogValue().String()
	if strings.Contains(got, "sk-super-secret") {
		t.Errorf("LogValue() leaked the API key: %s", got)
	}
	if !strings.Contains(got, "REDACTED") || !strings.Contains(got, "60") {
		t.Errorf("LogValue() = %q, want REDACTED and the budget", got)
	}
}
