package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/config"
	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(serperKey string) *config.Config {
	return &config.Config{
		HTTP:   config.HTTPConfig{UserAgent: "remote-job-aggregator/1.0 (+https://github.com/Bantamlak12/remote-job-aggregator)"},
		Search: config.SearchConfig{SerperAPIKey: serperKey, MaxQueriesPerRun: 60},
	}
}

func shippedPriorityFile() string {
	return filepath.Join("..", "..", "configs", "ethiopian_companies.json")
}

func TestProductToken(t *testing.T) {
	cases := map[string]string{
		"remote-job-aggregator/1.0 (+https://github.com/x/y)": "remote-job-aggregator",
		"remote-job-aggregator/1.0":                           "remote-job-aggregator",
		"remote-job-aggregator":                               "remote-job-aggregator",
		"  spaced/2 (x)  ":                                    "spaced",
		"":                                                    "",
	}
	for in, want := range cases {
		if got := productToken(in); got != want {
			t.Errorf("productToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseIngestArgs(t *testing.T) {
	cases := []struct {
		args    []string
		want    []string
		wantErr bool
	}{
		{nil, nil, false},
		{[]string{}, nil, false},
		{[]string{"--providers=feed"}, []string{"feed"}, false},
		{[]string{"--providers=feed, search ,careers-site"}, []string{"feed", "search", "careers-site"}, false},
		{[]string{"--providers=feed,,"}, []string{"feed"}, false},
		{[]string{"--providers="}, nil, true},
		{[]string{"--providers=,"}, nil, true},
		{[]string{"feed"}, nil, true},
		{[]string{"--provider=feed"}, nil, true},
		{[]string{"--providers=feed", "extra"}, nil, true},
	}
	for _, tc := range cases {
		got, err := parseIngestArgs(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseIngestArgs(%q) error = %v, wantErr %t", tc.args, err, tc.wantErr)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("parseIngestArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestNewIngestSources_WithoutASerperKeyTheSearchSourceIsLeftOut(t *testing.T) {
	s := newIngestSources(testConfig(""), httpclient.New(httpclient.DefaultConfig()), shippedPriorityFile(), discardLogger())

	want := []string{"careers-site", "feed", "greenhouse"}
	if !slices.Equal(s.defaultProviders, want) {
		t.Errorf("defaultProviders = %v, want %v", s.defaultProviders, want)
	}
	if _, ok := s.clients["search"]; ok || s.budget != nil {
		t.Errorf("search client registered without a SERPER_API_KEY")
	}
}

func TestNewIngestSources_WithAKeyAndTheListTheSearchSourceIsBudgeted(t *testing.T) {
	s := newIngestSources(testConfig("test-key"), httpclient.New(httpclient.DefaultConfig()), shippedPriorityFile(), discardLogger())

	if _, ok := s.clients["search"]; !ok {
		t.Fatalf("search client missing; clients = %v", s.defaultProviders)
	}
	if s.budget == nil || s.budget.Max() != 60 || s.budget.Used() != 0 {
		t.Errorf("budget = %+v, want a fresh budget of 60", s.budget)
	}
	// Registered, but never a default: it spends the fixed Serper allowance,
	// so a plain "ingest" must not run it.
	want := []string{"careers-site", "feed", "greenhouse"}
	if !slices.Equal(s.defaultProviders, want) {
		t.Errorf("defaultProviders = %v, want %v (search must be opt-in)", s.defaultProviders, want)
	}
	got, err := chooseProviders([]string{"search"}, s)
	if err != nil || !slices.Equal(got, []string{"search"}) {
		t.Errorf("chooseProviders(search) = %v, %v; want it accepted when named", got, err)
	}
	if def, _ := chooseProviders(nil, s); slices.Contains(def, "search") {
		t.Errorf("a plain ingest would run the paid search source: %v", def)
	}
}

func TestNewIngestSources_AKeyButNoListLeavesSearchOutInsteadOfCrashing(t *testing.T) {
	s := newIngestSources(testConfig("test-key"), httpclient.New(httpclient.DefaultConfig()),
		filepath.Join(t.TempDir(), "missing.json"), discardLogger())
	if _, ok := s.clients["search"]; ok {
		t.Errorf("search client registered although the company list could not be loaded")
	}
	if len(s.defaultProviders) != 3 {
		t.Errorf("defaultProviders = %v, want the 3 non-search providers", s.defaultProviders)
	}
}

func TestChooseProviders(t *testing.T) {
	without := newIngestSources(testConfig(""), httpclient.New(httpclient.DefaultConfig()), shippedPriorityFile(), discardLogger())

	got, err := chooseProviders(nil, without)
	if err != nil || !slices.Equal(got, without.defaultProviders) {
		t.Errorf("chooseProviders(nil) = %v, %v; want the defaults", got, err)
	}
	got, err = chooseProviders([]string{"feed"}, without)
	if err != nil || !slices.Equal(got, []string{"feed"}) {
		t.Errorf("chooseProviders(feed) = %v, %v", got, err)
	}
	// Asking for a source that cannot run must fail loudly.
	for _, bad := range [][]string{{"search"}, {"feed", "nonsense"}} {
		_, err := chooseProviders(bad, without)
		if err == nil || !strings.Contains(err.Error(), "no client for provider") {
			t.Errorf("chooseProviders(%v) error = %v, want 'no client for provider'", bad, err)
		}
	}
}

func TestRun_UnknownIngestOptionFailsBeforeTouchingTheDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody:nothing@127.0.0.1:1/none")
	err := run(context.Background(), []string{"ingest", "--bogus"})
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("run(ingest --bogus) error = %v, want a usage error", err)
	}
}

func TestRun_NewCommandsAreRegistered(t *testing.T) {
	// A registered command with bad arguments reports its own usage error,
	// while an unregistered one reports "unknown command".
	t.Setenv("DATABASE_URL", "postgres://nobody:nothing@127.0.0.1:1/none")
	for _, args := range [][]string{
		{"priority-report", "extra"},
		{"seed-priority", "a", "b"},
	} {
		err := run(context.Background(), args)
		if err == nil || strings.Contains(err.Error(), "unknown command") {
			t.Errorf("run(%v) error = %v, want a per-command usage error", args, err)
		}
	}
}

func TestRunSeedPriority_MissingListFailsBeforeConnecting(t *testing.T) {
	cfg := testConfig("")
	err := runSeedPriority(context.Background(), cfg, discardLogger(), []string{filepath.Join(t.TempDir(), "nope.json")})
	if err == nil || !os.IsNotExist(unwrapAll(err)) {
		t.Errorf("runSeedPriority() error = %v, want a file-not-found error", err)
	}
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}
