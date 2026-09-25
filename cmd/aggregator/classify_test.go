package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
)

func TestParseClassifyArgs(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		want    bool
		wantErr bool
	}{
		{nil, false, false},
		{[]string{"--reclassify"}, true, false},
		{[]string{"--force"}, false, true},
		{[]string{"--reclassify", "extra"}, false, true},
		{[]string{"reclassify"}, false, true},
	} {
		got, err := parseClassifyArgs(tc.args)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseClassifyArgs(%q) = %t, %v; want %t, error %t", tc.args, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestRelevanceProfile_BuiltInByDefault_AFileWhenConfigured_ErrorWhenBroken(t *testing.T) {
	cfg := &config.Config{}
	p, err := relevanceProfile(cfg)
	if err != nil || !p.Classify("Senior Backend Engineer").Relevant {
		t.Fatalf("built-in profile: %v, %v", p, err)
	}

	dir := t.TempDir()
	designer := filepath.Join(dir, "designer.json")
	if err := os.WriteFile(designer, []byte(`{"default":"other","relevant":["design"],"rules":[{"family":"design","any":["product designer","ux designer"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Filtering.RelevanceProfile = designer
	p, err = relevanceProfile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Classify("Senior Product Designer"); got.Family != "design" || !got.Relevant {
		t.Errorf("configured profile: %+v, want design/relevant", got)
	}
	if got := p.Classify("Senior Backend Engineer"); got.Family != "other" || got.Relevant {
		t.Errorf("configured profile counted a backend engineer as relevant: %+v", got)
	}

	cfg.Filtering.RelevanceProfile = filepath.Join(dir, "missing.json")
	if _, err := relevanceProfile(cfg); err == nil {
		t.Errorf("a missing profile file was accepted")
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"default":"","rules":[]}`), 0o600)
	cfg.Filtering.RelevanceProfile = bad
	if _, err := relevanceProfile(cfg); err == nil {
		t.Errorf("a profile with no default family was accepted")
	}
}
