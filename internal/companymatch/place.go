package companymatch

import (
	"regexp"
	"strings"
	"unicode"
)

// ethiopiaWord matches Ethiopia or Addis as whole words, so "Addison, TX"
// and "Ethiopian Airlines Hub, Nairobi" are not Ethiopian.
var ethiopiaWord = regexp.MustCompile(`\b(?:ethiopia|addis)\b`)

// ethiopiaText is the stricter form for free text (a snippet, a title),
// where "Addis" alone proves nothing: "Addis Software" is a company name.
var ethiopiaText = regexp.MustCompile(`\b(?:ethiopia|addis ababa|addis abeba)\b`)

// InEthiopia reports whether a location string ("Addis", "Hawassa, Ethiopia")
// names Ethiopia or Addis Ababa.
func InEthiopia(location string) bool {
	return ethiopiaWord.MatchString(strings.ToLower(location))
}

// InEthiopiaText reports whether free text names Ethiopia or Addis
// Ababa/Abeba. Unlike InEthiopia it does not accept a bare "Addis", which is
// also part of company names.
func InEthiopiaText(text string) bool {
	return ethiopiaText.MatchString(strings.ToLower(text))
}

// HasCity reports whether a location names a city, as opposed to nothing or
// only the country.
func HasCity(location string) bool { return cityKey(location) != "" }

// cityKey is the first, most specific part of a location, lower-cased, with
// the spellings of Addis Ababa unified. "" means the location names no city
// ("", "Ethiopia").
func cityKey(location string) string {
	first, _, _ := strings.Cut(location, ",")
	c := strings.Join(strings.Fields(strings.ToLower(first)), " ")
	switch c {
	case "ethiopia":
		return ""
	case "addis", "addis abeba", "addis ababa":
		return "addis ababa"
	}
	return c
}

// SamePlace reports whether two job locations could be the same opening's
// place. It is deliberately coarse, because job sites word locations
// differently: two named cities match only when equal ("Addis Ababa" and
// "Hawassa" are two places, so the same title in both is two openings), and a
// location naming no city ("Ethiopia", or nothing) matches any Ethiopian one
// but not a foreign one ("Nairobi, Kenya").
func SamePlace(a, b string) bool {
	ka, kb := cityKey(a), cityKey(b)
	switch {
	case ka == kb:
		return true
	case ka == "":
		return InEthiopia(b)
	case kb == "":
		return InEthiopia(a)
	}
	return false
}

// placeholderEmployers are names a job site shows when the real employer is
// hidden or is not a company; a company row for one would be wrong.
var placeholderEmployers = map[string]bool{
	"confidential": true, "anonymous": true, "linkedin": true, "hiring": true,
	"jobs": true, "job": true, "recruiter": true, "recruitment": true, "company": true,
	"not-specified": true, "n-a": true, "na": true, "unknown": true,
}

// IsPlaceholderEmployer reports whether name is a stand-in for a hidden
// employer, or a web address or e-mail rather than a name.
func IsPlaceholderEmployer(name string) bool {
	if strings.Contains(name, "://") || strings.Contains(name, "@") {
		return true
	}
	k := Key(name)
	if k == "" {
		// Key keeps ASCII only, so an Amharic-only name has an empty key. It
		// is a real name; only a name with no letter or digit is not.
		return !strings.ContainsFunc(name, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
	}
	return placeholderEmployers[k]
}
