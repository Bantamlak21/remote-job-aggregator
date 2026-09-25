package relevance

import (
	"strings"
	"testing"
)

func TestClassify_WholeWordPhrasesFirstRuleWins(t *testing.T) {
	p := Default()
	for _, tc := range []struct {
		title  string
		family string
		rel    bool
	}{
		{"Senior Backend Engineer", "software_engineering", true},
		{"Staff Software Engineer, Payments", "software_engineering", true},
		{"C++ Developer", "software_engineering", true},
		{"Engineering Manager - Search", "software_engineering", true},
		{"Sr. Manager, Engineering", "software_engineering", true},
		{"CTO", "software_engineering", true},
		{"Site Reliability Engineer", "devops_security", true},
		{"Application Security Engineer (AppSec)", "devops_security", true},
		{"Data Engineer", "data_ml", true},
		{"Machine Learning Engineer", "data_ml", true},
		{"Sales Engineer", "other_tech", false}, // "engineer" is not enough: a specific family is listed first
		{"Solutions Architect", "other_tech", false},
		{"Developer Advocate", "other_tech", false}, // contains "developer"
		{"Technical Program Manager", "other_tech", false},
		{"Product Manager, Payments", "product_design", false},
		{"Senior UX Designer", "product_design", false},
		{"Technical Support Engineer", "it_support", false},
		{"Recruiter", "non_tech", false},
		{"Account Executive", "non_tech", false},
		{"Business Development Manager", "non_tech", false}, // "development" is not "developer"
		{"Barista", "non_tech", false},
		// A bare "engineer" is not a software role (found on held-out titles).
		{"Site Engineer", "other_tech", false},
		{"Rack Power Engineer", "other_tech", false},
		{"PCB Layout Engineer", "other_tech", false},
		{"Field Deploy Engineer", "other_tech", false},
		{"Technical Services Engineer", "other_tech", false},
		{"Business Value Engineer", "other_tech", false},
		{"Social Media Manager, Developer Community", "non_tech", false},
		{"Widget Engineer", "non_tech", false}, // no bare-engineer catch-all: this guards the round-1 fix
		{"Acoustic Design Engineer", "non_tech", false},
		{"Python Engineer", "software_engineering", true},
		{"Software Engineering Intern", "software_engineering", true},
		{"Distinguished Engineer", "software_engineering", true},
		{"Payments Engineer", "software_engineering", true},
		{"Performance Engineer", "software_engineering", true},
		{"Software Engineer, Infrastructure", "software_engineering", true},
		{"Staff Software Engineer, Machine Learning", "data_ml", true},
		{"Senior IAM Engineer", "devops_security", true},
		{"People Operations Systems Administrator", "non_tech", false},
		{"Mobile Architect", "software_engineering", true},
		{"", "non_tech", false},
		{"Ingénieur Logiciel Backend", "software_engineering", true}, // accents fold
	} {
		got := p.Classify(tc.title)
		if got.Family != tc.family || got.Relevant != tc.rel {
			t.Errorf("Classify(%q) = %+v, want %s relevant=%t", tc.title, got, tc.family, tc.rel)
		}
	}
	// A phrase is whole words: "sre" is not in "asrea", "devops" is in "DevOps".
	if got := p.Classify("Asrea Coordinator"); got.Family != "non_tech" {
		t.Errorf("a phrase matched inside a word: %+v", got)
	}
	if got := p.Classify("Senior DevOps Engineer"); got.Family != "devops_security" || got.Matched != "devops" {
		t.Errorf("Classify(DevOps) = %+v", got)
	}
}

func TestParse_ValidatesAProfile(t *testing.T) {
	for name, tc := range map[string]struct {
		json string
		want string
	}{
		"no default":     {`{"rules":[]}`, "default"},
		"empty family":   {`{"default":"x","rules":[{"family":"","any":["a"]}]}`, "family"},
		"no phrases":     {`{"default":"x","rules":[{"family":"f","any":[]}]}`, "phrase"},
		"empty phrase":   {`{"default":"x","rules":[{"family":"f","any":["  "]}]}`, "empty phrase"},
		"unknown field":  {`{"default":"x","rulez":[]}`, "decoding"},
		"not json":       {`nope`, "decoding"},
		"trailing field": {`{"default":"x","rules":[{"family":"f","any":["a"],"nope":1}]}`, "decoding"},
	} {
		if _, err := Parse([]byte(tc.json)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
	p, err := Parse([]byte(`{"default":"other","relevant":["design"],"rules":[{"family":"design","any":["Product Designer"],"not":["intern"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Classify("PRODUCT designer"); got.Family != "design" || !got.Relevant {
		t.Errorf("case-insensitive match: %+v", got)
	}
	if got := p.Classify("Product Designer Intern"); got.Family != "other" {
		t.Errorf("a 'not' phrase did not veto the rule: %+v", got)
	}
}

func TestDefaultProfile_LoadsAndIsWellFormed(t *testing.T) {
	p := Default()
	seen := map[string]bool{}
	for _, r := range p.Rules {
		seen[r.Family] = true
	}
	for _, f := range []string{"software_engineering", "data_ml", "devops_security", "product_design", "it_support", "other_tech", "non_tech"} {
		if !seen[f] && f != p.Default {
			t.Errorf("the built-in profile has no rule for %s", f)
		}
	}
	if _, err := Load("/nonexistent/profile.json"); err == nil {
		t.Errorf("Load of a missing file returned no error")
	}
}
