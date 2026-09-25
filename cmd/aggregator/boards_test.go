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

func namedTarget(id int64, name, provider, board string) company.NamedTarget {
	return company.NamedTarget{TargetCompany: company.TargetCompany{ID: id, ATSProvider: provider, ExternalBoardID: board, IsActive: true}, CompanyName: name}
}

// Only a board the fresh look-up did not verify, for a company that was fully
// checked, is stale.
func TestStaleBoards(t *testing.T) {
	existing := []company.NamedTarget{
		namedTarget(1, "Wise", "greenhouse", "wise"),         // no longer verifies: stale
		namedTarget(2, "Zapier", "ashby", "zapier"),          // verifies: kept
		namedTarget(3, "Neon", "lever", "neon"),              // not verified, but the run did not finish checking Neon
		namedTarget(4, "GitLab", "greenhouse", "gitlab-old"), // a second board that did not verify: stale
		namedTarget(5, "GitLab", "greenhouse", "gitlab"),     // verifies: kept
	}
	verified := []discovery.Candidate{
		{CompanyName: "Zapier", ATSProvider: "ashby", ExternalBoardID: "zapier"},
		{CompanyName: "GitLab", ATSProvider: "greenhouse", ExternalBoardID: "GitLab"},
	}
	got := staleBoards(existing, verified, map[string]bool{"neon": true})
	var ids []int64
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []int64{1, 4}) {
		t.Errorf("stale board ids = %v, want [1 4]", ids)
	}
}

func TestUncheckedNames(t *testing.T) {
	got := uncheckedNames(discovery.GuessReport{
		Unreached: []string{"Alpha Co"},
		Failed:    []string{"Grafana Labs greenhouse/grafanalabs: probe failed: status 429", "GitLab lever/gitlab: probe failed: status 500"},
	})
	for _, want := range []string{"alpha co", "grafana labs", "gitlab"} {
		if !got[want] {
			t.Errorf("uncheckedNames lacks %q: %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("uncheckedNames = %v, want exactly 3", got)
	}
}
