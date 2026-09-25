// Package geo resolves the place names that job postings use ("United
// States", "USA", "EMEA", "Remote - Germany", "São Paulo") into countries and
// regions, deterministically and from tables: no network, no model.
//
// Countries and their English names come from golang.org/x/text/language (ISO
// 3166 / CLDR); the macro regions Africa, Europe and so on are UN M.49
// regions from the same package, so "does Africa contain Ethiopia" is data,
// not a hand-typed list. What job postings add on top (USA, UK, UAE, EMEA,
// APAC, LATAM, "Nordics", US states, major cities) lives in aliases.go and
// cities.go.
package geo

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Kind is what a Place is.
type Kind int

const (
	// Country is one ISO 3166 country (Code is its alpha-2 code).
	Country Kind = iota
	// Region is a set of countries: Africa, EMEA, the EU, "North America".
	Region
	// Worldwide is "anywhere": every country.
	Worldwide
	// Subdivision is a state, province or city inside one country; Code is
	// that country's alpha-2 code.
	Subdivision
	// Ambiguous is a name that is a country and something else ("Georgia"):
	// it is a place, but Includes reports nothing about it.
	Ambiguous
)

func (k Kind) String() string {
	return [...]string{"country", "region", "worldwide", "subdivision", "ambiguous"}[k]
}

// Place is a resolved name.
type Place struct {
	Name string // display name, e.g. "United States", "Africa"
	Kind Kind
	Code string // alpha-2 for Country and Subdivision; the region key for Region
	// City is true for a Subdivision that is a city or town (an office), false
	// for a state, province or region (which behaves like its country).
	City bool

	countries map[string]bool // Region: member alpha-2 codes
}

// Includes reports whether the country (an alpha-2 code) is inside the place:
// itself for a country or subdivision, a member for a region, always for
// Worldwide, never for Ambiguous.
func (p Place) Includes(country string) bool {
	switch p.Kind {
	case Worldwide:
		return true
	case Country, Subdivision:
		return p.Code == country
	case Region:
		return p.countries[country]
	}
	return false
}

// Countries lists a region's members (sorted), or the single country of a
// country or subdivision.
func (p Place) Countries() []string {
	switch p.Kind {
	case Country, Subdivision:
		return []string{p.Code}
	case Region:
		out := make([]string, 0, len(p.countries))
		for c := range p.countries {
			out = append(out, c)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

// Mention is a place found in a text.
type Mention struct {
	Place Place
	Text  string // the words as written
	Start int    // token index of the first word
	End   int    // token index after the last word
}

// Fold lower-cases s and removes accents ("São Paulo" is "sao paulo").
func Fold(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		out = s
	}
	return strings.ToLower(out)
}

// Tokens splits text into folded words. Periods inside an initialism are
// dropped first ("U.S.A." is "usa"), and "&" is the word "and".
func Tokens(s string) []string {
	raw := rawTokens(s)
	for i := range raw {
		raw[i] = strings.ToLower(raw[i])
	}
	return raw
}

// rawTokens is Tokens without lower-casing (accents are still removed): a
// two-letter code is only a country code when it is written in capitals.
func rawTokens(s string) []string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if out, _, err := transform.String(t, s); err == nil {
		s = out
	}
	s = dotted.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ToUpper(strings.ReplaceAll(m, ".", ""))
	})
	s = strings.ReplaceAll(s, "&", " and ")
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

var dotted = regexp.MustCompile(`(?i)\bu\.s\.a\.?|\bu\.s\.?|\bu\.k\.?|\bu\.a\.e\.?`)

const maxPhraseTokens = 5

var (
	once    sync.Once
	lexicon map[string]Place // joined folded tokens -> place

	countryByISO = map[string]Place{}
)

func build() {
	lexicon = map[string]Place{}
	// Names that are also something else register first: the first
	// registration of a name wins.
	for _, name := range ambiguousNames {
		add(name, Place{Name: name, Kind: Ambiguous})
	}
	world := language.MustParseRegion("001")
	byCode := map[string]Place{}
	for a := 'A'; a <= 'Z'; a++ {
		for b := 'A'; b <= 'Z'; b++ {
			code := string(a) + string(b)
			r, err := language.ParseRegion(code)
			if err != nil || !r.IsCountry() || !world.Contains(r) || r.String() != code {
				continue
			}
			name := display.English.Regions().Name(r)
			p := Place{Name: name, Kind: Country, Code: code}
			byCode[code] = p
			countryByISO[code] = p
			add(name, p)
		}
	}
	for alias, code := range countryAliases {
		p, ok := byCode[code]
		if !ok {
			continue
		}
		add(alias, p)
	}
	for name, code := range extraCountries {
		p := Place{Name: name, Kind: Country, Code: code}
		byCode[code] = p
		countryByISO[code] = p
		add(name, p)
	}
	addRegions(byCode)
	addSubdivisions()
	add("worldwide", Place{Name: "Worldwide", Kind: Worldwide})
	for _, w := range worldwideNames {
		add(w, Place{Name: "Worldwide", Kind: Worldwide})
	}
}

// add registers a name, keeping the first registration: countries win over
// later aliases and subdivisions, so "Georgia" stays the ambiguous entry and
// "Jersey" is not New Jersey.
func add(name string, p Place) {
	key := strings.Join(Tokens(name), " ")
	if key == "" {
		return
	}
	if _, taken := lexicon[key]; taken {
		return
	}
	lexicon[key] = p
}

// Lookup resolves an exact name ("United States", "usa", "EMEA").
func Lookup(name string) (Place, bool) {
	once.Do(build)
	p, ok := lexicon[strings.Join(Tokens(name), " ")]
	return p, ok
}

// Scan finds every place in the text, longest name first, left to right.
// "South Africa" is one mention (not "Africa"), "New York" is one mention.
// A two-letter country code in capitals ("NZ") is a mention only inside a list
// of places ("US, CA, UK, NZ") or after "not in" / "except": on its own "IN",
// "IT" and "OR" are English words.
func Scan(text string) []Mention {
	once.Do(build)
	raw := rawTokens(text)
	toks := make([]string, len(raw))
	for i, r := range raw {
		toks[i] = strings.ToLower(r)
	}
	var out []Mention
	covered := make([]bool, len(toks))
	for i := 0; i < len(toks); {
		matched := false
		for n := min(maxPhraseTokens, len(toks)-i); n >= 1; n-- {
			key := strings.Join(toks[i:i+n], " ")
			if p, ok := lexicon[key]; ok {
				out = append(out, Mention{Place: p, Text: key, Start: i, End: i + n})
				for k := i; k < i+n; k++ {
					covered[k] = true
				}
				i += n
				matched = true
				break
			}
		}
		if !matched {
			i++
		}
	}

	// Codes: accepted next to an accepted place or after an exclusion word,
	// repeated until nothing more is added (a list of codes chains).
	place := func(i int) bool { return i >= 0 && i < len(toks) && covered[i] }
	neighbour := func(i, step int) int { // skip "and" / "or" between list items
		j := i + step
		for j >= 0 && j < len(toks) && (toks[j] == "and" || toks[j] == "or") {
			j += step
		}
		return j
	}
	after := exclusionEnds(toks)
	for changed := true; changed; {
		changed = false
		for i, r := range raw {
			if covered[i] || len(r) != 2 || r != strings.ToUpper(r) || codeStop[r] {
				continue
			}
			p, ok := countryByCodeLocked(r)
			if !ok {
				continue
			}
			prev, next := neighbour(i, -1), neighbour(i, 1)
			listed := place(prev) || place(next)
			introduced := i > 0 && (toks[i-1] == "remote" || toks[i-1] == "hybrid") // "Remote, DE"
			if usStateCodes[r] && !(place(prev) && kindAt(out, prev) == Country) && !after[i] && !introduced {
				continue // "San Francisco, CA": a state, not Canada
			}
			if !listed && !after[i] && !introduced {
				continue
			}
			out = append(out, Mention{Place: p, Text: strings.ToLower(r), Start: i, End: i + 1})
			covered[i] = true
			changed = true
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Start < out[b].Start })
	return out
}

func kindAt(ms []Mention, tok int) Kind {
	for _, m := range ms {
		if tok >= m.Start && tok < m.End {
			return m.Place.Kind
		}
	}
	return Ambiguous
}

// exclusionEnds marks the tokens that follow an exclusion phrase.
func exclusionEnds(toks []string) map[int]bool {
	out := map[int]bool{}
	for i := range toks {
		for _, trig := range exclusionTriggers {
			if i+len(trig) <= len(toks) && strings.Join(toks[i:i+len(trig)], " ") == strings.Join(trig, " ") {
				for j := i + len(trig); j < len(toks); j++ {
					out[j] = true
				}
			}
		}
	}
	return out
}

var exclusionTriggers = [][]string{{"not", "in"}, {"except"}, {"except", "for"}, {"excluding"}, {"outside", "of"}, {"outside"},
	{"other", "than"}, {"apart", "from"}, {"but", "not"}, {"not", "located", "in"}, {"not", "available", "in"}}

// ExclusionStart returns the token index at which an exclusion phrase ("not
// in", "except", "excluding") begins, or -1. Tokens are those of Tokens(text).
func ExclusionStart(toks []string) int {
	first := -1
	for i := range toks {
		for _, trig := range exclusionTriggers {
			if i+len(trig) <= len(toks) && strings.Join(toks[i:i+len(trig)], " ") == strings.Join(trig, " ") {
				if first < 0 || i < first {
					first = i
				}
			}
		}
	}
	return first
}

// codeStop are two-letter capitals that are ISO country codes and also
// ordinary English words in an all-capitals posting.
var codeStop = map[string]bool{"IN": true, "IT": true, "IS": true, "ME": true, "OR": true, "AS": true, "AT": true, "BE": true, "BY": true,
	"DO": true, "GO": true, "IF": true, "MY": true, "NO": true, "OF": true, "ON": true, "SO": true, "TO": true, "UP": true, "WE": true,
	"HE": true, "AM": true, "AN": true, "ID": true, "AD": true, "AL": true, "AR": true, "ET": true, "PM": true, "TV": true, "UN": true}

// usStateCodes are the USPS codes: in "City, ST" they name a state.
var usStateCodes = map[string]bool{"AL": true, "AK": true, "AZ": true, "AR": true, "CA": true, "CO": true, "CT": true, "DE": true,
	"FL": true, "GA": true, "HI": true, "ID": true, "IL": true, "IN": true, "IA": true, "KS": true, "KY": true, "LA": true, "ME": true,
	"MD": true, "MA": true, "MI": true, "MN": true, "MS": true, "MO": true, "MT": true, "NE": true, "NV": true, "NH": true, "NJ": true,
	"NM": true, "NY": true, "NC": true, "ND": true, "OH": true, "OK": true, "OR": true, "PA": true, "RI": true, "SC": true, "SD": true,
	"TN": true, "TX": true, "UT": true, "VT": true, "VA": true, "WA": true, "WV": true, "WI": true, "WY": true, "DC": true}

var caProvinceCodes = map[string]bool{"ON": true, "BC": true, "AB": true, "QC": true, "MB": true, "NS": true, "NB": true, "SK": true,
	"NL": true, "PE": true, "YT": true, "NT": true, "NU": true}

// StateCountry returns "US" for a US state postal code and "CA" for a Canadian
// province code, or "" (codes as written: capitals).
func StateCountry(code string) string {
	switch {
	case usStateCodes[code]:
		return "US"
	case caProvinceCodes[code]:
		return "CA"
	}
	return ""
}

func countryByCodeLocked(code string) (Place, bool) {
	p, ok := countryByISO[code]
	return p, ok
}

// CountryByCode returns the country with an alpha-2 code.
func CountryByCode(code string) (Place, bool) {
	once.Do(build)
	p, ok := countryByISO[strings.ToUpper(code)]
	return p, ok
}

// member reports whether alpha-2 code is inside M.49 region m.
func member(m, code string) bool {
	r, err := language.ParseRegion(code)
	if err != nil {
		return false
	}
	return language.MustParseRegion(m).Contains(r)
}
