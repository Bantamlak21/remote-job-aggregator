package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoad_JobMaxAgeDefaultsTo15Days(t *testing.T) {
	clearAll(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.API.JobMaxAge != 15*24*time.Hour {
		t.Errorf("JobMaxAge = %v, want 15 days", cfg.API.JobMaxAge)
	}
}

func TestLoad_JobMaxAgeDaysValidation(t *testing.T) {
	cases := []struct {
		value   string
		want    time.Duration
		wantErr string
	}{
		{"30", 30 * 24 * time.Hour, ""},
		{"1", 24 * time.Hour, ""},
		{"0", 0, ""}, // explicitly no limit
		{"3650", 3650 * 24 * time.Hour, ""},
		{"-1", 0, "JOB_MAX_AGE_DAYS must be between 0"},
		{"3651", 0, "JOB_MAX_AGE_DAYS must be between 0"},
		{"two weeks", 0, "JOB_MAX_AGE_DAYS must be an integer"},
		{"15.5", 0, "JOB_MAX_AGE_DAYS must be an integer"},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			clearAll(t)
			t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/jobs")
			t.Setenv("JOB_MAX_AGE_DAYS", tc.value)

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
			if cfg.API.JobMaxAge != tc.want {
				t.Errorf("JobMaxAge = %v, want %v", cfg.API.JobMaxAge, tc.want)
			}
		})
	}
}
