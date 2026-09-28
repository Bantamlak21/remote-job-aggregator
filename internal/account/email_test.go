package account

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeEmail_Accepts(t *testing.T) {
	long63 := strings.Repeat("a", 63)
	cases := []struct{ in, want string }{
		{"alice@example.com", "alice@example.com"},
		{"Alice@Example.COM", "alice@example.com"},
		{"  alice@example.com \n", "alice@example.com"},
		{"a+jobs@x.io", "a+jobs@x.io"}, // plus-addressing is kept: it is the provider's rule, not ours
		{"first.last@sub.example.org", "first.last@sub.example.org"},
		{"user_name-1@ex-ample.com", "user_name-1@ex-ample.com"},
		{"o'brien@example.com", "o'brien@example.com"},
		{"a@b.123", "a@b.123"}, // a numeric last label is fine: the name is not all-numeric
		{"x@" + long63 + ".com", "x@" + long63 + ".com"},
		{strings.Repeat("a", 64) + "@example.com", strings.Repeat("a", 64) + "@example.com"},
	}
	for _, tc := range cases {
		got, err := NormalizeEmail(tc.in)
		if err != nil {
			t.Errorf("NormalizeEmail(%q) error = %v, want %q", tc.in, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeEmail_TwoSpellingsOfOneMailboxAreOneAddress(t *testing.T) {
	a, _ := NormalizeEmail("Bob@Example.com")
	b, _ := NormalizeEmail(" bob@EXAMPLE.COM")
	if a == "" || a != b {
		t.Errorf("spellings normalized to %q and %q, want the same non-empty address", a, b)
	}
}

func TestNormalizeEmail_Rejects(t *testing.T) {
	cases := map[string]string{
		"empty":                 "",
		"spaces only":           "   ",
		"no at":                 "alice.example.com",
		"no domain":             "alice@",
		"no local":              "@example.com",
		"no dot in domain":      "a@localhost",
		"double at":             "a@@example.com",
		"two ats":               "a@b@example.com",
		"space in local":        "a b@example.com",
		"space in domain":       "a@exa mple.com",
		"CRLF in local":         "a\r\n@example.com",
		"CRLF then header":      "a@example.com\r\nBcc: victim@example.org",
		"LF header injection":   "a@example.com\nBcc: victim@example.org",
		"NUL":                   "a\x00@example.com",
		"tab":                   "a\t@example.com",
		"DEL":                   "a\x7f@example.com",
		"quoted local":          `"a"@example.com`,
		"quoted local with @":   `"a@b"@example.com`,
		"display name":          "Alice <alice@example.com>",
		"angle brackets":        "<alice@example.com>",
		"comma list":            "a@example.com,b@example.com",
		"semicolon":             "a;b@example.com",
		"comment":               "a(comment)@example.com",
		"non-ASCII local":       "é@example.com",
		"non-ASCII domain":      "a@münchen.de",
		"line separator":        "a @example.com",
		"fullwidth at":          "a＠example.com",
		"IPv4 literal":          "a@[127.0.0.1]",
		"IPv6 literal":          "a@[::1]",
		"bare IP":               "a@127.0.0.1",
		"leading dot local":     ".a@example.com",
		"trailing dot local":    "a.@example.com",
		"double dot local":      "a..b@example.com",
		"leading hyphen label":  "a@-example.com",
		"trailing hyphen label": "a@example-.com",
		"empty label":           "a@example..com",
		"trailing dot domain":   "a@example.com.",
		"underscore in domain":  "a@exa_mple.com",
		"label over 63":         "a@" + strings.Repeat("b", 64) + ".com",
		"local over 64":         strings.Repeat("a", 65) + "@example.com",
		"over 254":              strings.Repeat("a", 60) + "@" + strings.Repeat(strings.Repeat("b", 60)+".", 4) + "com",
		"percent route":         "a%b@example.com@evil.com",
		"backslash":             `a\b@example.com`,
		// S-18b: a punycode label is ASCII bytes standing in for a non-ASCII identity (an
		// IDN homograph of a real domain), which the ASCII-only rule means to refuse, not
		// merely to check byte-by-byte. "xn--80ak6aa92e.com" decodes to a Cyrillic
		// look-alike of a domain a person might trust.
		"punycode domain (IDN homograph)": "a@xn--80ak6aa92e.com",
		"punycode subdomain":              "a@xn--mnchen-3ya.example.com",
		"XN-- upper case":                 "a@XN--80AK6AA92E.com",
	}
	for name, in := range cases {
		got, err := NormalizeEmail(in)
		if !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("%s: NormalizeEmail(%q) = %q, %v; want ErrInvalidEmail", name, in, got, err)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"alice@example.com":      "a***@***.com",
		"a@b.co":                 "a***@***.co",
		"alice@mail.example.org": "a***@***.org", // the domain is not shown, only its top level
		"nope":                   "***",
		"@x.com":                 "***",
		"":                       "***",
	}
	for in, want := range cases {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
	}
	if got := MaskEmail("alice@example.com"); strings.Contains(got, "lice") || strings.Contains(got, "example") {
		t.Errorf("MaskEmail leaked the local part or the domain: %q", got)
	}
}
