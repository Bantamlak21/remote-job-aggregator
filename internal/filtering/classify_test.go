package filtering

import (
	"encoding/json"
	"strings"
	"testing"
)

// One row per policy of the rubric the classifier is scored against
// (docs/eligibility.md). Each is a small, synthetic posting that isolates the
// rule; the eval (eligibility_eval_test.go) scores the rules on real ones.

type policyCase struct {
	name        string
	in          Input
	want        Status
	basis       Basis // "" = any
	evidenceHas string
}

func board(source, location string) Input {
	return Input{Source: source, Title: "Backend Engineer", Location: location, RemoteType: "remote", Description: "We build things."}
}

func ats(location, remoteType, description string) Input {
	return Input{Source: "greenhouse", Title: "Backend Engineer", Location: location, RemoteType: remoteType, Description: description}
}

func TestPolicies(t *testing.T) {
	cases := []policyCase{
		// P1 worldwide
		{"P1 worldwide", board("himalayas", "Worldwide"), Eligible, BasisWorldwide, "Worldwide"},
		{"P1 anywhere in the world", board("weworkremotely", "Anywhere in the World"), Eligible, BasisWorldwide, ""},
		{"P1 global", board("workingnomads", "Global"), Eligible, BasisWorldwide, ""},
		{"P1 remote global (ATS)", ats("Remote, Global", "remote", "x"), Eligible, BasisWorldwide, ""},
		// P2 a region that contains Ethiopia
		{"P2 EMEA", board("remotive", "EMEA"), Eligible, BasisRegionIncludes, ""},
		{"P2 Africa", board("himalayas", "Africa"), Eligible, BasisRegionIncludes, ""},
		{"P2 East Africa", board("himalayas", "East Africa"), Eligible, BasisRegionIncludes, ""},
		{"P2 home based EMEA (ATS)", ats("Home based - EMEA", "unknown", "x"), Eligible, BasisRegionIncludes, ""},
		{"P2 a list that includes EMEA", ats("EMEA; Hungary; Austria; Ireland", "unknown", "x"), Eligible, BasisRegionIncludes, ""},
		// P3 regions that do not
		{"P3 Europe alone", board("himalayas", "Europe"), Ineligible, BasisRestrictedPlaces, ""},
		{"P3 UK or the EU", board("himalayas", "UK or the European Union"), Ineligible, BasisRestrictedPlaces, ""},
		{"P3 Americas, Europe, Israel", board("himalayas", "Americas, Europe, Israel"), Ineligible, BasisRestrictedPlaces, ""},
		{"P3 LATAM APAC", board("himalayas", "LATAM, APAC"), Ineligible, BasisRestrictedPlaces, ""},
		// P4 country lists; South Africa is a country
		{"P4 country list", board("himalayas", "United States, Canada, Germany"), Ineligible, BasisRestrictedPlaces, ""},
		{"P4 South Africa is not Africa", board("himalayas", "South Africa"), Ineligible, BasisRestrictedPlaces, ""},
		{"P4 US states", board("himalayas", "Texas"), Ineligible, BasisRestrictedPlaces, ""},
		// P5 exclusion lists
		{"P5 exclusion list ok", board("workingnomads", "Internationally located (not in the US, CA, UK, NZ, or AU)"), Eligible, BasisExclusionListOK, ""},
		{"P5 worldwide except US", board("remotive", "Worldwide except the US"), Eligible, "", ""},
		{"P5b excludes Africa", board("remotive", "Worldwide except Africa"), Ineligible, BasisExcluded, ""},
		// P6 time zones
		{"P6 CET +/- 3 hours", board("workingnomads", "Time zone: CET (+/- 3 hours)"), Eligible, BasisTimezoneWindow, ""},
		{"P6 UTC+1 to UTC+4", board("workingnomads", "UTC+1 to UTC+4"), Eligible, BasisTimezoneWindow, ""},
		{"P6b US time zones (location)", board("workingnomads", "US time zones"), Ineligible, "", ""},
		{"P6b must be located in US time zones (description)", ats("Remote", "remote", "You must be located in the US time zones."), Ineligible, BasisTimezoneOutside, ""},
		// P7 permissive location contradicted / restrictive location contradicted
		{"P7 Global but resident in France", board("workingnomads", "Global") /* description below */, Eligible, "", ""},
		{"P7b Remote US but any geography", ats("Remote, United States", "remote", "Candidates may be based in any geography as long as they overlap with customer time zones."), Eligible, BasisExplicitPermitted, "any geography"},
		// P8 We Work Remotely: the Headquarters line wins over "Anywhere in the World"
		{"P8 headquarters remote-in-country", Input{Source: "weworkremotely", Title: "Software Engineer", Location: "Anywhere in the World", RemoteType: "remote",
			Description: "Headquarters: Remote - US\nWho We Are: a company."}, Ineligible, BasisRestrictedPlaces, "Headquarters: Remote - US"},
		{"P8b headquarters is only a city", Input{Source: "weworkremotely", Title: "Software Engineer", Location: "Anywhere in the World", RemoteType: "remote",
			Description: "Headquarters: New York City URL: http://x.com\nWho We Are: a company."}, Eligible, BasisWorldwide, ""},
		// P10 located in Ethiopia
		{"P10 Addis Ababa", Input{Source: "ethiojobs", Title: "Accountant", Location: "Addis Ababa", RemoteType: "unknown", Market: "ethiopia"}, Eligible, BasisLocalEthiopia, "Addis Ababa"},
		{"P10 Ethiopian region", Input{Source: "ethiojobs", Title: "Officer", Location: "Oromia", Market: "ethiopia"}, Eligible, BasisLocalEthiopia, ""},
		{"P10 on-site in Ethiopia is still eligible", ats("Addis Ababa, Ethiopia", "onsite", "In-office role."), Eligible, BasisLocalEthiopia, ""},
		{"P10 Ethiopian market with no location", Input{Source: "ethiojobs", Title: "Officer", Location: "", Market: "ethiopia"}, Eligible, BasisLocalEthiopia, ""},
		// P11 on-site / hybrid outside Ethiopia
		{"P11 onsite typed", ats("San Francisco, CA", "onsite", "x"), Ineligible, BasisOnsiteElsewhere, ""},
		{"P11 hybrid typed", ats("London, UK", "hybrid", "x"), Ineligible, BasisOnsiteElsewhere, ""},
		{"P11 explicit office statement", ats("Madrid", "unknown", "This role is based in our Madrid office, three days a week."), Ineligible, BasisOnsiteElsewhere, ""},
		{"P11 hybrid with a city in an EMEA list", ats("Hybrid - EMEA; London, England, United Kingdom", "unknown", "x"), Ineligible, BasisOnsiteElsewhere, ""},
		// P12 an office city with no work mode
		{"P12 office city, silent", ats("Seattle, WA", "unknown", "We build payments infrastructure."), Uncertain, BasisNoSignal, ""},
		{"P12 EMEA as an office's region tag", ats("Novi Sad, South Backa, Serbia, EMEA", "unknown", "We work at the intersection."), Uncertain, BasisNoSignal, ""},
		{"P12 a company-wide hybrid line is not this role's", ats("Paris, France", "unknown", "We operate as a hybrid workplace so our Datadogs can find balance."), Uncertain, BasisNoSignal, ""},
		// P13 bare country
		{"P13 bare country (ATS)", ats("Spain", "unknown", "x"), Ineligible, BasisRestrictedPlaces, ""},
		{"P13 bare country, remote", ats("Canada", "remote", "x"), Ineligible, BasisRestrictedPlaces, ""},
		// P14 bare Remote / Hybrid / nothing
		{"P14 bare Remote", ats("Remote", "remote", "We build things."), Uncertain, BasisNoSignal, ""},
		{"P14 bare Hybrid", ats("Hybrid", "hybrid", "We build things."), Uncertain, BasisNoSignal, ""},
		{"P14 nothing at all", ats("", "unknown", ""), Uncertain, BasisNoSignal, ""},
		// P15 territories and US-state locations
		{"P15 remote in a US state", ats("Remote - TX", "remote", "x"), Ineligible, BasisRestrictedPlaces, ""},
		{"P15 territory in the title", Input{Source: "greenhouse", Title: "Channel Partner Sales Executive, UKI", Location: "Home based - EMEA", RemoteType: "unknown", Description: "x"}, Ineligible, BasisRestrictedPlaces, ""},
		// P16 title qualifiers
		{"P16 (US based) in the title", Input{Source: "workingnomads", Title: "Support Engineer (US based)", Location: "Worldwide", RemoteType: "remote", Description: "x"}, Ineligible, BasisRestrictedPlaces, "(US based)"},
		{"P16 Based in London", Input{Source: "jobicy", Title: "Account Executive - Based in London or Home Counties", Location: "EMEA", RemoteType: "remote", Description: "x"}, Ineligible, BasisRestrictedPlaces, ""},
		// P17 company-level statements do not widen a country
		{"P17 we hire globally", ats("Spain", "remote", "We are a global company with teams in 75+ countries and we hire globally. Work from anywhere for up to 6 weeks a year."), Ineligible, BasisRestrictedPlaces, ""},
		// P18 work authorization and citizenship
		{"P18 US citizen", ats("Remote", "remote", "You must be a US citizen."), Ineligible, BasisWorkAuthorization, ""},
		{"P18 authorized to work in the US", ats("Remote", "remote", "Candidates must be authorized to work in the United States."), Ineligible, BasisWorkAuthorization, ""},
		{"P18 security clearance", ats("Remote", "remote", "An active Top Secret clearance is required."), Ineligible, BasisWorkAuthorization, ""},
		{"P18b generic EEO and export text has no effect", ats("Remote", "remote", "We are an equal opportunity employer. Export control laws may apply to our products."), Uncertain, BasisNoSignal, ""},
		// Restrictions written in the description
		{"description: must be located in", ats("Remote", "remote", "You must be located in Germany or Austria."), Ineligible, BasisRestrictedPlaces, "must be located in"},
		{"description: remote within a country", ats("Remote", "remote", "Location: Mostly remote (within Germany)."), Ineligible, BasisRestrictedPlaces, ""},
		{"description: role can be based anywhere within a country is a restriction", ats("Remote", "remote", "This remote role can be based anywhere within the UK."), Ineligible, BasisRestrictedPlaces, ""},
		{"description: candidates located in one of these regions (list follows)", ats("EMEA Remote", "remote", "Location: We are hiring for positions based in the UK and Ireland. Candidates must be located in one of these regions."), Ineligible, BasisRestrictedPlaces, ""},
		{"description: except residents of", board("workingnomads", "Worldwide"), Eligible, BasisWorldwide, ""},
		{"description: work from anywhere as a perk does not widen a country", ats("United States", "remote", "As long as work is timely and you are authorized to work where you live, you can work from anywhere."), Ineligible, BasisRestrictedPlaces, ""},
		{"description: within-country phrasing is not permission", ats("United States", "remote", "NOTE: this position can be remote anywhere within the United States."), Ineligible, BasisRestrictedPlaces, ""},
		// Precedence: an explicit restriction beats a permissive location
		{"precedence: worldwide tag, description says resident in France", Input{Source: "workingnomads", Title: "Rater", Location: "Global", RemoteType: "remote",
			Description: "Resident in France for the last 5 consecutive years."}, Ineligible, BasisRestrictedPlaces, "Resident in France"},
	}
	// The P7 row that needs a description.
	cases[22].in.Description = "Resident in Germany for the last 5 years."
	cases[22].want, cases[22].basis = Ineligible, BasisRestrictedPlaces

	c := NewClassifier()
	for _, tc := range cases {
		if tc.name == "description: except residents of" {
			tc.in.Description = "We welcome applications from candidates worldwide, except residents of the United States."
		}
		v := c.Classify(tc.in)
		if v.Status != tc.want {
			t.Errorf("%s: status %s (%s; %v), want %s", tc.name, v.Status, v.Basis, v.Reasons, tc.want)
			continue
		}
		if tc.basis != "" && v.Basis != tc.basis {
			t.Errorf("%s: basis %s, want %s (%v)", tc.name, v.Basis, tc.basis, v.Reasons)
		}
		if tc.evidenceHas != "" {
			found := false
			for _, e := range v.Evidence {
				if strings.Contains(strings.ToLower(e.Text), strings.ToLower(tc.evidenceHas)) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: no evidence quotes %q: %+v", tc.name, tc.evidenceHas, v.Evidence)
			}
		}
	}
}

func TestClassify_EveryVerdictIsWellFormed(t *testing.T) {
	c := NewClassifier()
	for _, in := range []Input{
		{}, {Location: "Remote"}, {Location: "Worldwide"}, {Location: "  "}, {Title: "x", Description: "<p>You must be located in <b>Canada</b></p>"},
		{Location: "\x00\xff", Description: "\xff\xfe"}, {Location: strings.Repeat("Berlin; ", 5000), Description: strings.Repeat("Remote in Germany. ", 5000)},
	} {
		v := c.Classify(in)
		if !v.Status.Valid() || v.Confidence < 0 || v.Confidence > 1 || v.Basis == "" && v.Status != Uncertain {
			t.Errorf("malformed verdict %+v for %q", v, in.Location)
		}
		if v.Reasons == nil || v.Evidence == nil || v.Locations == nil || v.Restrictions == nil {
			t.Errorf("nil slice in the verdict for %q (JSON would render null)", in.Location)
		}
		if _, err := json.Marshal(v); err != nil {
			t.Errorf("verdict does not marshal: %v", err)
		}
	}
}

func TestClassify_HoursConstraintAndTimezoneRules(t *testing.T) {
	c := NewClassifier()
	// Worldwide but working US hours: eligible, with the caveat.
	v := c.Classify(Input{Source: "workingnomads", Title: "Engineer", Location: "Worldwide", RemoteType: "remote",
		Description: "Work from anywhere, but you will be working during US business hours (EST)."})
	if v.Status != Eligible || v.HoursConstraint == "" {
		t.Errorf("worldwide + US hours: %s hours=%q, want eligible with the hours caveat", v.Status, v.HoursConstraint)
	}
	// A time-zone permission does not widen a list of countries that leaves Ethiopia out.
	v = c.Classify(Input{Source: "greenhouse", Title: "SDR", Location: "Kenya; Morocco; South Africa", RemoteType: "remote",
		Description: "Based in a timezone within ±3 hours of CET."})
	if v.Status == Eligible {
		t.Errorf("a country list without Ethiopia was widened by a time-zone sentence: %+v", v)
	}
}

func TestPlainTextAndSentences(t *testing.T) {
	got := plainText("<p>Hello&nbsp;<b>world</b></p><ul><li>One</li><li>Two</li></ul>")
	if got != "Hello world\nOne\nTwo" {
		t.Errorf("plainText = %q", got)
	}
	if s := sentences("A b. C d!\nE f"); len(s) != 3 {
		t.Errorf("sentences = %q", s)
	}
	if q := quote(strings.Repeat("x", 300)); len([]rune(q)) != 203 {
		t.Errorf("quote length %d", len([]rune(q)))
	}
}
