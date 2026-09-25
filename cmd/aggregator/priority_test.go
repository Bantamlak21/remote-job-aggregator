package main

import (
	"bytes"
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
		args      []string
		want      []string
		wantForce bool
		wantErr   bool
	}{
		{nil, nil, false, false},
		{[]string{}, nil, false, false},
		{[]string{"--providers=feed"}, []string{"feed"}, false, false},
		{[]string{"--providers=feed, search ,careers-site"}, []string{"feed", "search", "careers-site"}, false, false},
		{[]string{"--providers=feed,,"}, []string{"feed"}, false, false},
		{[]string{"--force"}, nil, true, false},
		{[]string{"--providers=remotive", "--force"}, []string{"remotive"}, true, false},
		{[]string{"--force", "--providers=remotive"}, []string{"remotive"}, true, false},
		{[]string{"--providers="}, nil, false, true},
		{[]string{"--providers=,"}, nil, false, true},
		{[]string{"feed"}, nil, false, true},
		{[]string{"--provider=feed"}, nil, false, true},
		{[]string{"--providers=feed", "extra"}, nil, false, true},
		{[]string{"--providers=feed", "--providers=search"}, nil, false, true},
		{[]string{"--forced"}, nil, false, true},
	}
	for _, tc := range cases {
		got, force, err := parseIngestArgs(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseIngestArgs(%q) error = %v, wantErr %t", tc.args, err, tc.wantErr)
			continue
		}
		if !slices.Equal(got, tc.want) || force != tc.wantForce {
			t.Errorf("parseIngestArgs(%q) = %q, force %t; want %q, force %t", tc.args, got, force, tc.want, tc.wantForce)
		}
	}
}

func shippedQueriesFile() string {
	return filepath.Join("..", "..", "configs", "linkedin_queries.json")
}

func sourcesFor(serperKey string) ingestSources {
	return newIngestSources(testConfig(serperKey), httpclient.New(httpclient.DefaultConfig()),
		shippedPriorityFile(), shippedQueriesFile(), discardLogger())
}

// The free sources run by default; the Serper-backed ones never do.
var freeDefaults = []string{
	"careers-site", "ethiojobs", "feed", "greenhouse",
	"himalayas", "jobicy", "remoteok", "remotive", "weworkremotely", "workingnomads",
}

func TestNewIngestSources_WithoutASerperKeyOnlyTheFreeSourcesExist(t *testing.T) {
	s := sourcesFor("")

	if !slices.Equal(s.defaultProviders, freeDefaults) {
		t.Errorf("defaultProviders = %v, want %v", s.defaultProviders, freeDefaults)
	}
	if _, ok := s.clients["search"]; ok || s.budget != nil {
		t.Errorf("search client registered without a SERPER_API_KEY")
	}
	if _, ok := s.collectors["linkedin"]; ok {
		t.Errorf("linkedin collector registered without a SERPER_API_KEY")
	}
	if _, ok := s.collectors["ethiojobs"]; !ok {
		t.Errorf("ethiojobs collector missing; it needs no key")
	}
	if s.resolver == nil {
		t.Errorf("no priority resolver although the priority list loads")
	}
}

func TestNewIngestSources_WithAKeyTheSerperSourcesAreBudgetedTogetherAndOptIn(t *testing.T) {
	s := sourcesFor("test-key")

	if _, ok := s.clients["search"]; !ok {
		t.Fatalf("search client missing")
	}
	if _, ok := s.collectors["linkedin"]; !ok {
		t.Fatalf("linkedin collector missing")
	}
	if s.budget == nil || s.budget.Max() != 60 || s.budget.Used() != 0 {
		t.Errorf("budget = %+v, want one fresh budget of 60 for both Serper sources", s.budget)
	}
	// Registered, but never a default: they spend the fixed Serper allowance,
	// so a plain "ingest" must not run them.
	if !slices.Equal(s.defaultProviders, freeDefaults) {
		t.Errorf("defaultProviders = %v, want %v (Serper sources must be opt-in)", s.defaultProviders, freeDefaults)
	}
	for _, name := range []string{"search", "linkedin"} {
		got, err := chooseProviders([]string{name}, s)
		if err != nil || !slices.Equal(got, []string{name}) {
			t.Errorf("chooseProviders(%s) = %v, %v; want it accepted when named", name, got, err)
		}
	}
	def, _ := chooseProviders(nil, s)
	if slices.Contains(def, "search") || slices.Contains(def, "linkedin") {
		t.Errorf("a plain ingest would run a paid source: %v", def)
	}
}

func TestNewIngestSources_AKeyButNoListLeavesPerCompanySearchOutButKeepsTheRest(t *testing.T) {
	s := newIngestSources(testConfig("test-key"), httpclient.New(httpclient.DefaultConfig()),
		filepath.Join(t.TempDir(), "missing.json"), shippedQueriesFile(), discardLogger())
	if _, ok := s.clients["search"]; ok {
		t.Errorf("search client registered although the company list could not be loaded")
	}
	if !slices.Equal(s.defaultProviders, freeDefaults) {
		t.Errorf("defaultProviders = %v, want %v", s.defaultProviders, freeDefaults)
	}
	if _, ok := s.collectors["linkedin"]; !ok {
		t.Errorf("linkedin collector missing; it does not need the priority list")
	}
	if s.resolver != nil {
		t.Errorf("resolver is set although the list is missing (a nil *Matcher in an interface is a trap)")
	}
}

func TestNewIngestSources_AMissingKeywordFileLeavesLinkedInOut(t *testing.T) {
	s := newIngestSources(testConfig("test-key"), httpclient.New(httpclient.DefaultConfig()),
		shippedPriorityFile(), filepath.Join(t.TempDir(), "missing.json"), discardLogger())
	if _, ok := s.collectors["linkedin"]; ok {
		t.Errorf("linkedin collector registered although its keyword list could not be loaded")
	}
	if _, ok := s.clients["search"]; !ok {
		t.Errorf("per-company search lost because of the keyword file")
	}
	if _, err := chooseProviders([]string{"linkedin"}, s); err == nil {
		t.Errorf("chooseProviders(linkedin) accepted without a collector")
	}
}

func TestChooseProviders(t *testing.T) {
	without := sourcesFor("")

	got, err := chooseProviders(nil, without)
	if err != nil || !slices.Equal(got, without.defaultProviders) {
		t.Errorf("chooseProviders(nil) = %v, %v; want the defaults", got, err)
	}
	got, err = chooseProviders([]string{"feed", "ethiojobs"}, without)
	if err != nil || !slices.Equal(got, []string{"feed", "ethiojobs"}) {
		t.Errorf("chooseProviders(feed, ethiojobs) = %v, %v", got, err)
	}
	// Asking for a source that cannot run must fail loudly.
	for _, bad := range [][]string{{"search"}, {"linkedin"}, {"feed", "nonsense"}} {
		_, err := chooseProviders(bad, without)
		if err == nil || !strings.Contains(err.Error(), "no client for provider") {
			t.Errorf("chooseProviders(%v) error = %v, want 'no client for provider'", bad, err)
		}
	}
}

func TestSplitProviders_SeparatesCollectorsAndOrdersThem(t *testing.T) {
	s := sourcesFor("test-key")
	ats, cols := splitProviders([]string{"linkedin", "feed", "search", "remoteok", "ethiojobs", "himalayas"}, s)
	if !slices.Equal(ats, []string{"feed", "search"}) {
		t.Errorf("ATS providers = %v, want [feed search]", ats)
	}
	// Ethiojobs runs before LinkedIn, and both before the remote boards
	// (Himalayas, with the fullest text, before Remote OK), whatever the order
	// asked for: an opening several sources list is stored from the first.
	if !slices.Equal(cols, []string{"ethiojobs", "linkedin", "himalayas", "remoteok"}) {
		t.Errorf("collectors = %v, want [ethiojobs linkedin himalayas remoteok]", cols)
	}
	ats, cols = splitProviders([]string{"feed"}, s)
	if len(cols) != 0 || !slices.Equal(ats, []string{"feed"}) {
		t.Errorf("feed only: ats %v, collectors %v", ats, cols)
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

func TestNewIngestSources_WarnsWhenTheKeywordListCannotFitTheBudget(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := testConfig("test-key")
	cfg.Search.MaxQueriesPerRun = 5 // the shipped list has 24 keywords
	newIngestSources(cfg, httpclient.New(httpclient.DefaultConfig()), shippedPriorityFile(), shippedQueriesFile(), logger)
	if !strings.Contains(buf.String(), "needs more queries than SEARCH_MAX_QUERIES_PER_RUN") {
		t.Errorf("no budget warning in the log:\n%s", buf.String())
	}

	buf.Reset()
	newIngestSources(testConfig("test-key"), httpclient.New(httpclient.DefaultConfig()), shippedPriorityFile(), shippedQueriesFile(), logger)
	if strings.Contains(buf.String(), "needs more queries") {
		t.Errorf("budget warning with the default budget:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "test-key") {
		t.Errorf("the Serper key was logged")
	}
}

func TestExpandGroups_RemoteBoardsNamesEveryBoardOnce(t *testing.T) {
	got := expandGroups([]string{"feed", "remote-boards", "remotive", "ethiojobs"})
	want := []string{"feed", "himalayas", "remotive", "jobicy", "weworkremotely", "workingnomads", "remoteok", "ethiojobs"}
	if !slices.Equal(got, want) {
		t.Errorf("expandGroups = %v, want %v (in place, no duplicate remotive)", got, want)
	}
	if got := expandGroups([]string{"feed"}); !slices.Equal(got, []string{"feed"}) {
		t.Errorf("no group: %v", got)
	}
}

func TestChooseProviders_TheRemoteBoardsGroupIsAcceptedAndExpanded(t *testing.T) {
	s := sourcesFor("")
	got, err := chooseProviders([]string{"remote-boards"}, s)
	if err != nil || len(got) != 6 || slices.Contains(got, "remote-boards") {
		t.Errorf("chooseProviders(remote-boards) = %v, %v; want the six boards", got, err)
	}
	for _, b := range got {
		if _, ok := s.collectors[b]; !ok {
			t.Errorf("board %s has no collector", b)
		}
	}
}

// A worldwide board's employer only resolves to a priority company that hires
// outside Ethiopia (Gebeya): "EthSwitch" on Remotive is some other company.
func TestNewIngestSources_TheWorldwideResolverKnowsOnlyCompaniesThatHireOutsideEthiopia(t *testing.T) {
	s := sourcesFor("")
	if c, ok := s.resolver.Resolve("EthSwitch S.C."); c != "EthSwitch" || !ok {
		t.Errorf("Ethiopian resolver on EthSwitch S.C. = %q, %v; want EthSwitch", c, ok)
	}
	if c, ok := s.worldwideResolver.Resolve("EthSwitch S.C."); c != "" || ok {
		t.Errorf("worldwide resolver on EthSwitch S.C. = %q, %v; want no match", c, ok)
	}
	if c, ok := s.worldwideResolver.Resolve("Gebeya Inc."); c == "" || !ok {
		t.Errorf("worldwide resolver on Gebeya Inc. = %q, %v; want a match (it hires outside Ethiopia)", c, ok)
	}
}
