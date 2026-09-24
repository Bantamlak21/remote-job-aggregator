package companymatch

import "testing"

func TestKey(t *testing.T) {
	cases := map[string]string{
		"Ethswitch S.C.":                  "ethswitch",
		"EthSwitch":                       "ethswitch",
		"Kifiya Financial Technology PLC": "kifiya-financial-technology",
		"Gebeya Inc.":                     "gebeya",
		"Gebeya":                          "gebeya",
		"Chaka Gebeya":                    "chaka-gebeya",
		"Chapa De Indian Health":          "chapa-de-indian-health",
		"Chapa":                           "chapa",
		"4Africa Systems":                 "4africa-systems",
		"251 Technologies":                "251-technologies",
		"Ethiopia":                        "ethiopia", // never strip down to nothing
		"PLC":                             "plc",
		"":                                "",
		"  Zare  Innovations  ":           "zare-innovations",
	}
	for in, want := range cases {
		if got := Key(in); got != want {
			t.Errorf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"UI/UX Designer":             "ui-ux-designer",
		"  Hello,   World!  ":        "hello-world",
		"Frontend Developer (React)": "frontend-developer-react",
		"Écrivain":                   "crivain",
		"":                           "",
		"---":                        "",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatcher_Resolve(t *testing.T) {
	m := NewMatcher([]Entry{
		{Name: "Kifiya Financial Technology", Aliases: []string{"Kifiya Financial Technologies", "Kifiya"}},
		{Name: "EthSwitch"},
		{Name: "Gebeya Inc.", Aliases: []string{"Gebeya Developer As A Service P.L.C"}},
	})
	matched := map[string]string{
		"Kifiya Financial Technologies":     "Kifiya Financial Technology",
		"kifiya financial technology plc":   "Kifiya Financial Technology",
		"KIFIYA":                            "Kifiya Financial Technology",
		"Ethswitch S.C.":                    "EthSwitch",
		"  ethswitch ":                      "EthSwitch",
		"Gebeya":                            "Gebeya Inc.",
		"Gebeya Developer As A Service PLC": "Gebeya Inc.",
	}
	for in, want := range matched {
		if got, ok := m.Resolve(in); !ok || got != want {
			t.Errorf("Resolve(%q) = %q, %t; want %q, true", in, got, ok, want)
		}
	}
	for _, in := range []string{"Chaka Gebeya", "Kifiya Bank", "Ethswitch Holdings", "", "   ", "!!!", "Qena Software Design & Development PLC"} {
		if got, ok := m.Resolve(in); ok {
			t.Errorf("Resolve(%q) = %q, true; want no match", in, got)
		}
	}
}

func TestMatcher_NilAndEmptyMatchNothing(t *testing.T) {
	var nilMatcher *Matcher
	if _, ok := nilMatcher.Resolve("EthSwitch"); ok {
		t.Error("a nil Matcher matched something")
	}
	if _, ok := NewMatcher(nil).Resolve("EthSwitch"); ok {
		t.Error("an empty Matcher matched something")
	}
}

func TestMatcher_FirstEntryWinsAKeyCollisionAndOwnNameResolvesToItself(t *testing.T) {
	m := NewMatcher([]Entry{
		{Name: "Alpha", Aliases: []string{"Shared Name"}},
		{Name: "Beta", Aliases: []string{"Shared Name"}},
	})
	if got, _ := m.Resolve("Shared Name"); got != "Alpha" {
		t.Errorf("Resolve(Shared Name) = %q, want the first entry Alpha", got)
	}
	if got, _ := m.Resolve("Beta"); got != "Beta" {
		t.Errorf("Resolve(Beta) = %q, want Beta", got)
	}
}

func TestTitleKey(t *testing.T) {
	distinct := []string{"C# Developer", "C++ Developer", "C Developer", "Senior Accountant", "የሂሳብ ባለሙያ", "የሽያጭ ባለሙያ"}
	seen := map[string]string{}
	for _, title := range distinct {
		k := TitleKey(title)
		if k == "" {
			t.Errorf("TitleKey(%q) is empty", title)
		}
		if other, dup := seen[k]; dup {
			t.Errorf("TitleKey(%q) == TitleKey(%q) == %q; distinct titles collapsed", title, other, k)
		}
		seen[k] = title
	}
	same := [][2]string{
		{"Senior  Engineer", "senior engineer"},
		{"Senior Engineer!", "SENIOR ENGINEER"},
		{"Re- Advertised", "Re-Advertised"},
	}
	for _, p := range same {
		if TitleKey(p[0]) != TitleKey(p[1]) {
			t.Errorf("TitleKey(%q) = %q != TitleKey(%q) = %q", p[0], TitleKey(p[0]), p[1], TitleKey(p[1]))
		}
	}
}
