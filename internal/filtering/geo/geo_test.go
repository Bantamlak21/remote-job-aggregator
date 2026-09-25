package geo

import (
	"slices"
	"testing"
)

func TestLookup_CountriesAliasesAndRegions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     Kind
		code     string
		includes []string // alpha-2 codes the place must include
		excludes []string
	}{
		{"United States", Country, "US", []string{"US"}, []string{"ET", "CA"}},
		{"USA", Country, "US", []string{"US"}, nil},
		{"U.S.A.", Country, "US", []string{"US"}, nil},
		{"United States of America", Country, "US", []string{"US"}, nil},
		{"UK", Country, "GB", []string{"GB"}, nil},
		{"England", Country, "GB", []string{"GB"}, nil},
		{"Czech Republic", Country, "CZ", []string{"CZ"}, nil},
		{"Türkiye", Country, "TR", []string{"TR"}, nil},
		{"São Tomé & Príncipe", Country, "ST", []string{"ST"}, nil},
		{"Ethiopia", Country, "ET", []string{"ET"}, []string{"KE"}},
		{"Africa", Region, "africa", []string{"ET", "KE", "ZA", "NG", "EG"}, []string{"US", "DE"}},
		{"Sub-Saharan Africa", Region, "sub saharan africa", []string{"ET", "KE"}, []string{"EG", "MA"}},
		{"East Africa", Region, "east africa", []string{"ET", "KE", "TZ"}, []string{"NG", "ZA"}},
		{"EMEA", Region, "emea", []string{"ET", "DE", "GB", "AE", "ZA", "IL"}, []string{"US", "IN", "BR", "JP"}},
		{"Europe", Region, "europe", []string{"DE", "GB", "PL", "UA"}, []string{"ET", "US", "TR"}},
		{"EU", Region, "european union", []string{"DE", "FR", "PL"}, []string{"GB", "CH", "NO", "ET"}},
		{"EEA", Region, "eea", []string{"DE", "NO", "IS"}, []string{"GB", "CH"}},
		{"APAC", Region, "apac", []string{"IN", "JP", "AU", "NZ", "SG"}, []string{"ET", "DE", "US"}},
		{"LATAM", Region, "latam", []string{"BR", "MX", "AR", "CO"}, []string{"US", "ET", "ES"}},
		{"North America", Region, "north america", []string{"US", "CA"}, []string{"MX", "ET"}},
		{"Middle East", Region, "middle east", []string{"AE", "IL", "SA", "TR"}, []string{"ET", "EG"}},
		{"MENA", Region, "mena", []string{"EG", "AE", "MA"}, []string{"ET", "NG"}},
		{"Nordics", Region, "nordics", []string{"SE", "NO", "DK", "FI", "IS"}, []string{"DE"}},
		{"Worldwide", Worldwide, "", []string{"ET", "US", "DE", "XX"}, nil},
		{"Anywhere in the World", Worldwide, "", []string{"ET"}, nil},
		{"Global", Worldwide, "", []string{"ET"}, nil},
		{"Texas", Subdivision, "US", []string{"US"}, []string{"ET"}},
		{"Ontario", Subdivision, "CA", []string{"CA"}, []string{"US"}},
		{"Addis Ababa", Subdivision, "ET", []string{"ET"}, nil},
		{"São Paulo", Subdivision, "BR", []string{"BR"}, nil},
		{"Georgia", Ambiguous, "", nil, []string{"US", "GE", "ET"}},
	} {
		p, ok := Lookup(tc.name)
		if !ok {
			t.Errorf("Lookup(%q): not found", tc.name)
			continue
		}
		if p.Kind != tc.kind || (tc.code != "" && p.Code != tc.code) {
			t.Errorf("Lookup(%q) = %v %q, want %v %q", tc.name, p.Kind, p.Code, tc.kind, tc.code)
		}
		for _, c := range tc.includes {
			if !p.Includes(c) {
				t.Errorf("%q should include %s", tc.name, c)
			}
		}
		for _, c := range tc.excludes {
			if p.Includes(c) {
				t.Errorf("%q should not include %s", tc.name, c)
			}
		}
	}
	for _, name := range []string{"", "Narnia", "Remote", "Hybrid", "the moon"} {
		if p, ok := Lookup(name); ok {
			t.Errorf("Lookup(%q) = %+v, want no place", name, p)
		}
	}
}

func TestScan_LongestMatchAndAllMentions(t *testing.T) {
	names := func(text string) []string {
		var out []string
		for _, m := range Scan(text) {
			out = append(out, m.Place.Name)
		}
		return out
	}
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"South Africa", []string{"South Africa"}}, // one country, not "Africa"
		{"Remote - United States", []string{"United States"}},
		{"USA, Canada, Argentina, Mexico, Peru", []string{"United States", "Canada", "Argentina", "Mexico", "Peru"}},
		{"Europe, LATAM, APAC, the U.S., Canada", []string{"Europe", "Latam", "Apac", "United States", "Canada"}},
		{"San Francisco, CA", []string{"San Francisco"}}, // "CA" alone is not a mention here
		{"Vilnius; Kaunas", []string{"Vilnius", "Kaunas"}},
		{"Anywhere in the World", []string{"Worldwide"}},
		{"New York", []string{"New York"}},
		{"Barbados and United States of America", []string{"Barbados", "United States"}},
		{"Cuba, United States of America, and Russian Federation", []string{"Cuba", "United States", "Russia"}},
		{"Nothing to see", nil},
		// Two-letter codes count only inside a list of places or after "not in".
		{"Internationally located (not in the US, CA, UK, NZ, or AU)", []string{"United States", "Canada", "United Kingdom", "New Zealand", "Australia"}},
		{"US, CA", []string{"United States", "Canada"}},
		{"San Francisco, CA", []string{"San Francisco"}},
		{"Austin, TX", []string{"Austin"}},
		{"REMOTE IN THE US OR IT", []string{"United States"}}, // IN, THE, OR, IT are words
		{"Remote, DE", []string{"Germany"}},                   // a lone code after a place-free remote is a country
		{"Paris, FR", []string{"Paris", "France"}},
	} {
		got := names(tc.text)
		// Region display names are title-cased from their key; compare loosely.
		if !slices.EqualFunc(got, tc.want, func(a, b string) bool { return Fold(a) == Fold(b) }) {
			t.Errorf("Scan(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestParseTimezones(t *testing.T) {
	type w struct{ lo, hi float64 }
	for _, tc := range []struct {
		text string
		want []w
	}{
		{"CET (+/- 3 hours)", []w{{-2, 4}}},
		{"Time zone: CET (+/- 3 hours)", []w{{-2, 4}}},
		{"UTC+1 to UTC+3", []w{{1, 3}}},
		{"GMT-8 - GMT-5", []w{{-8, -5}}},
		{"UTC+5:30", []w{{5.5, 5.5}}},
		{"overlap with EST", []w{{-5, -5}}},
		{"within 3 hours of PST", []w{{-11, -5}}},
		{"US time zones", []w{{-10, -3}}},
		{"EMEA time zones", []w{{0, 4}}},
		{"East Africa Time", []w{{3, 3}}},
		{"Pacific Time or Eastern Time", []w{{-8, -8}, {-5, -5}}},
		{"we like et al and pt. of sale", nil}, // lower-case short words are not zones
	} {
		got := ParseTimezones(tc.text)
		var have []w
		for _, x := range got {
			have = append(have, w{x.Lo, x.Hi})
		}
		if !sameWindows(have, tc.want) {
			t.Errorf("ParseTimezones(%q) = %v, want %v", tc.text, have, tc.want)
		}
	}
	if !ParseTimezones("CET (+/- 3 hours)")[0].Contains(EthiopiaOffset) {
		t.Errorf("CET +/- 3 hours should contain UTC+3")
	}
	if ParseTimezones("US time zones")[0].Contains(EthiopiaOffset) {
		t.Errorf("US time zones should not contain UTC+3")
	}
}

func sameWindows[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

func TestCountryByCode(t *testing.T) {
	if p, ok := CountryByCode("et"); !ok || p.Name != "Ethiopia" {
		t.Errorf("CountryByCode(et) = %+v, %t", p, ok)
	}
	if _, ok := CountryByCode("ZZ"); ok {
		t.Errorf("ZZ is not a country")
	}
}
