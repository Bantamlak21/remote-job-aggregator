package main

import (
	"slices"
	"strings"
	"testing"

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

func TestCleanBoardNames(t *testing.T) {
	got := cleanBoardNames([]string{
		"GitLab", "  gitlab ", "Grafana   Labs", "", "   ",
		"https://africanyouthuniondo.or/", strings.Repeat("x", maxBoardNameRunes+1), "Acme Inc",
	})
	if want := []string{"GitLab", "Grafana Labs", "Acme Inc"}; !slices.Equal(got, want) {
		t.Errorf("cleanBoardNames = %v, want %v", got, want)
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
	if cleaned := cleanBoardNames(names); len(cleaned) != len(names) {
		t.Errorf("%d of %d names would be dropped by cleanBoardNames (blank, overlong, URL-like or repeated)", len(names)-len(cleaned), len(names))
	}
	for _, want := range []string{"GitLab", "Zapier", "Automattic", "Supabase"} {
		if !slices.Contains(names, want) {
			t.Errorf("shipped list lacks %q", want)
		}
	}
}
