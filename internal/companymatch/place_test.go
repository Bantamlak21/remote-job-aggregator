package companymatch

import "testing"

func TestInEthiopia_WholeWordsOnly(t *testing.T) {
	yes := []string{"Ethiopia", "Addis Ababa, Ethiopia", "ADDIS ABEBA", "Addis, Addis Ababa", "in Bole, Addis Ababa."}
	no := []string{"", "Addison, TX", "Ethiopian Airlines Hub, Nairobi", "Nairobi, Kenya", "Paddis"}
	for _, s := range yes {
		if !InEthiopia(s) {
			t.Errorf("InEthiopia(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if InEthiopia(s) {
			t.Errorf("InEthiopia(%q) = true, want false", s)
		}
	}
}

func TestInEthiopiaText_ABareAddisIsACompanyNameNotAPlace(t *testing.T) {
	yes := []string{"Ethiopia", "hiring in Addis Ababa now", "ADDIS ABEBA"}
	no := []string{"Addis Software hiring in Nairobi", "Addison, TX", "", "Ethiopian Airlines"}
	for _, s := range yes {
		if !InEthiopiaText(s) {
			t.Errorf("InEthiopiaText(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if InEthiopiaText(s) {
			t.Errorf("InEthiopiaText(%q) = true, want false", s)
		}
	}
	if !InEthiopia("Addis") {
		t.Errorf(`InEthiopia("Addis") = false: a parsed location may say just Addis`)
	}
}

func TestHasCity(t *testing.T) {
	for loc, want := range map[string]bool{"": false, "Ethiopia": false, "  ethiopia ": false, "Addis Ababa": true, "Hawassa, Ethiopia": true, "Nairobi, Kenya": true} {
		if got := HasCity(loc); got != want {
			t.Errorf("HasCity(%q) = %t, want %t", loc, got, want)
		}
	}
}

func TestSamePlace(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Addis Ababa", "Addis Ababa, Addis Ababa, Ethiopia", true},
		{"Addis Abeba", "addis ababa", true},
		{"Addis", "Addis Ababa, Ethiopia", true},
		{"Ethiopia", "Addis Ababa", true},
		{"", "Hawassa, Ethiopia", true},
		{"Ethiopia", "Hawassa, Ethiopia", true},
		{"Addis Ababa", "Hawassa", false},
		{"Addis Ababa, Ethiopia", "Hawassa, Ethiopia", false},
		{"Ethiopia", "Nairobi, Kenya", false},
		{"", "Nairobi", false},
		{"Nairobi", "Nairobi, Kenya", true},
		{"", "", true},
	}
	for _, tc := range cases {
		if got := SamePlace(tc.a, tc.b); got != tc.want {
			t.Errorf("SamePlace(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
		if got := SamePlace(tc.b, tc.a); got != tc.want {
			t.Errorf("SamePlace(%q, %q) = %t, want %t (must be symmetric)", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestIsPlaceholderEmployer(t *testing.T) {
	for _, s := range []string{"", "  ", "Confidential", "CONFIDENTIAL", "Not Specified", "N/A", "https://acme.example/", "jobs@acme.example", "Anonymous Ltd"} {
		if !IsPlaceholderEmployer(s) {
			t.Errorf("IsPlaceholderEmployer(%q) = false, want true", s)
		}
	}
	// Names Key cannot spell (Amharic) are real names, not placeholders.
	for _, s := range []string{"ኢትዮ ቴሌኮም", "አዲስ ባንክ", "Acme PLC", "Kifiya", "Confidential Records Ltd", "Jobs For Africa"} {
		if IsPlaceholderEmployer(s) {
			t.Errorf("IsPlaceholderEmployer(%q) = true, want false", s)
		}
	}
}
