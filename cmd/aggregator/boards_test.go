package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
	"github.com/Bantamlak12/remote-job-aggregator/internal/discovery"
)

func TestParseDiscoverBoardsArgs(t *testing.T) {
	cases := []struct {
		args    []string
		want    discoverBoardsArgs
		wantErr bool
	}{
		{nil, discoverBoardsArgs{namesFile: defaultRemoteCompaniesFile}, false},
		{[]string{"names.txt"}, discoverBoardsArgs{namesFile: "names.txt"}, false},
		{[]string{"-", "--from-boards"}, discoverBoardsArgs{namesFile: "-", fromBoards: true}, false},
		{[]string{"--from-boards", "--limit=25"}, discoverBoardsArgs{namesFile: defaultRemoteCompaniesFile, fromBoards: true, limit: 25}, false},
		{[]string{"--recheck"}, discoverBoardsArgs{namesFile: defaultRemoteCompaniesFile, recheck: true}, false},
		{[]string{"--recheck", "--apply", "x.txt"}, discoverBoardsArgs{namesFile: "x.txt", recheck: true, apply: true}, false},
		{[]string{"--apply"}, discoverBoardsArgs{}, true},
		{[]string{"--recheck", "--from-boards", "-"}, discoverBoardsArgs{namesFile: "-", recheck: true, fromBoards: true}, false},
		{[]string{"a.txt", "b.txt"}, discoverBoardsArgs{}, true},
		{[]string{"--limit=0"}, discoverBoardsArgs{}, true},
		{[]string{"--limit=-3"}, discoverBoardsArgs{}, true},
		{[]string{"--limit=many"}, discoverBoardsArgs{}, true},
		{[]string{"--unknown"}, discoverBoardsArgs{}, true},
	}
	for _, tc := range cases {
		got, err := parseDiscoverBoardsArgs(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseDiscoverBoardsArgs(%q) error = %v, wantErr %t", tc.args, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseDiscoverBoardsArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

func TestCleanBoardEntries(t *testing.T) {
	in := []discovery.Entry{
		{Name: "GitLab"}, {Name: "  gitlab "}, {Name: "Grafana   Labs"}, {Name: ""}, {Name: "   "},
		{Name: "https://africanyouthuniondo.or/"}, {Name: strings.Repeat("x", maxBoardNameRunes+1)}, {Name: "Ramp", Domain: "ramp.com"},
	}
	var got []string
	for _, e := range cleanBoardEntries(in) {
		got = append(got, e.Name+"|"+e.Domain)
	}
	if want := []string{"GitLab|", "Grafana Labs|", "Ramp|ramp.com"}; !slices.Equal(got, want) {
		t.Errorf("cleanBoardEntries = %v, want %v", got, want)
	}
}

func TestWithoutBoards_LeavesCompaniesThatAlreadyHaveABoardAlone(t *testing.T) {
	in := []discovery.Entry{{Name: "GitLab"}, {Name: "Zapier"}, {Name: "Ramp", Domain: "ramp.com"}}
	got := withoutBoards(in, []string{"gitlab", " RAMP "})
	if len(got) != 1 || got[0].Name != "Zapier" {
		t.Errorf("withoutBoards = %+v, want only Zapier", got)
	}
}

// The shipped list must load through the same loader the command uses, with no
// repeats (a repeat costs wasted probes) and a useful size.
func TestShippedRemoteCompaniesLoad(t *testing.T) {
	names, err := discovery.LoadCompanyNames("../../" + defaultRemoteCompaniesFile)
	if err != nil {
		t.Fatalf("%s: %v", defaultRemoteCompaniesFile, err)
	}
	if len(names) < 300 {
		t.Errorf("%d names, want at least 300", len(names))
	}
	var entries []discovery.Entry
	byName := map[string]string{}
	for _, l := range names {
		e := discovery.ParseEntry(l)
		entries = append(entries, e)
		byName[e.Name] = e.Domain
	}
	if cleaned := cleanBoardEntries(entries); len(cleaned) != len(entries) {
		t.Errorf("%d of %d entries would be dropped by cleanBoardEntries (blank, overlong, URL-like or repeated)", len(entries)-len(cleaned), len(entries))
	}
	for _, name := range []string{"GitLab", "Zapier", "Automattic", "Supabase"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("shipped list lacks %q", name)
		}
	}
	// Names that are everyday words must carry a domain, or they are never looked up.
	for _, name := range []string{"Close", "Ramp", "Linear"} {
		if d := byName[name]; d == "" {
			t.Logf("%s has no domain in the list (an everyday word without one is skipped by discover-boards)", name)
		}
	}
	for name, domain := range map[string]string{"Ramp": "ramp.com", "Linear": "linear.app", "Close": "close.com", "Notion": "notion.so"} {
		if byName[name] != domain {
			t.Errorf("%s: domain %q in the list, want %q", name, byName[name], domain)
		}
	}
}

func namedTarget(id int64, name, provider, board, source string) company.NamedTarget {
	return company.NamedTarget{TargetCompany: company.TargetCompany{ID: id, ATSProvider: provider, ExternalBoardID: board, IsActive: true}, CompanyName: name, Source: source}
}

func staleIDs(in []company.NamedTarget) []int64 {
	var ids []int64
	for _, s := range in {
		ids = append(ids, s.ID)
	}
	return ids
}

// A board is stale only when discover-boards registered it, the look-up found
// a board at that provider and slug that names itself as another company's,
// and the company was fully checked. Everything else is left alone.
func TestStaleBoards(t *testing.T) {
	const db = discovery.SourceDiscoverBoards
	existing := []company.NamedTarget{
		namedTarget(1, "Wise", "greenhouse", "wise", db),            // refused as another company's: stale
		namedTarget(2, "Zapier", "ashby", "zapier", db),             // verifies: kept
		namedTarget(3, "Neon", "lever", "neon", db),                 // refused, but the run did not finish checking Neon
		namedTarget(4, "GitLab", "greenhouse", "gitlab-old", db),    // refused next to a verified board: stale
		namedTarget(5, "GitLab", "greenhouse", "gitlab", db),        // verifies: kept
		namedTarget(6, "Sentry", "lever", "sentry", ""),             // refused, but seeded by a person: never touched
		namedTarget(7, "Moved", "ashby", "moved", db),               // simply not found again (no refusal): kept
		namedTarget(8, "Ramp", "ashby", "ramp", db),                 // refused under another name's case: matched case-insensitively
		namedTarget(9, "Notion", "greenhouse", "notion", "other"),   // another tool's board: never touched
		namedTarget(10, "Backblaze", "greenhouse", "backblaze", db), // refused only as unproven: kept
		namedTarget(11, "Toptal", "lever", "toptal", db),            // refused only for want of a shared title: kept
	}
	verified := []discovery.Candidate{
		{CompanyName: "Zapier", ATSProvider: "ashby", ExternalBoardID: "zapier"},
		{CompanyName: "GitLab", ATSProvider: "greenhouse", ExternalBoardID: "GitLab"},
	}
	refused := []discovery.Refusal{
		{Name: "Wise", Provider: "greenhouse", Slug: "wise", Kind: discovery.NamedForAnother},
		{Name: "Neon", Provider: "lever", Slug: "neon", Kind: discovery.NamedForAnother},
		{Name: "GitLab", Provider: "greenhouse", Slug: "gitlab-old", Kind: discovery.NamedForAnother},
		{Name: "Sentry", Provider: "lever", Slug: "sentry", Kind: discovery.NamedForAnother},
		{Name: "ramp", Provider: "ashby", Slug: "RAMP", Kind: discovery.NamedForAnother},
		{Name: "Notion", Provider: "greenhouse", Slug: "notion", Kind: discovery.NamedForAnother},
		{Name: "Backblaze", Provider: "greenhouse", Slug: "backblaze", Kind: discovery.Unproven}, // its jobs never say its name
		{Name: "Toptal", Provider: "lever", Slug: "toptal", Kind: discovery.NoSharedTitle},       // its titles differ from a job board's
	}
	got := staleBoards(existing, verified, refused, map[string]bool{"neon": true})
	if want := []int64{1, 4, 8}; !slices.Equal(staleIDs(got), want) {
		t.Errorf("stale board ids = %v, want %v", staleIDs(got), want)
	}
	// With no refusals nothing is ever stale, whatever was not found.
	if got := staleBoards(existing, nil, nil, nil); len(got) != 0 {
		t.Errorf("stale without any refusal = %v, want none", staleIDs(got))
	}
}

func TestUncheckedNames(t *testing.T) {
	got := uncheckedNames(discovery.GuessReport{
		Unreached: []string{"Alpha Co"},
		Failed: []discovery.Failure{
			{Name: "Grafana Labs", Provider: "greenhouse", Slug: "grafanalabs", Err: "status 429"},
			{Name: "Acme: Labs", Provider: "lever", Slug: "acmelabs", Err: "probe failed: status 500"}, // a ": " in the name
		},
	})
	for _, want := range []string{"alpha co", "grafana labs", "acme: labs"} {
		if !got[want] {
			t.Errorf("uncheckedNames lacks %q: %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("uncheckedNames = %v, want exactly 3", got)
	}
}
