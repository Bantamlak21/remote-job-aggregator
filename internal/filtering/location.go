package filtering

import (
	"regexp"
	"slices"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/geo"
)

// location is what a raw location string says.
type location struct {
	text     string
	places   []geo.Mention // where the job is / who may apply
	excluded []geo.Mention // "not in the US, CA, UK"
	// worldwide: the text says "anywhere" / "worldwide" / "global".
	worldwide bool
	remote    bool // "remote", "work from home", "distributed"
	hybrid    bool
	onsite    bool
	only      bool // "only" appears ("US only")
	windows   []geo.Window
	// unknownSegments counts the comma/semicolon separated parts of the text
	// that name no known place and are not "remote" or the like: an office in
	// a city the tables do not have.
	unknownSegments int
}

var (
	remoteWords = map[string]bool{"remote": true, "remotely": true, "telecommute": true, "telecommuting": true, "distributed": true, "wfh": true, "virtual": true}
	onsiteWords = []string{"onsite", "on site", "in office", "in person", "office based", "on premise"}
)

// analyzeLocation reads a location string.
func analyzeLocation(text string) location {
	l := location{text: strings.TrimSpace(text)}
	if l.text == "" {
		return l
	}
	toks := geo.Tokens(l.text)
	ex := geo.ExclusionStart(toks)
	mentions := geo.Scan(l.text)
	mentions = append(mentions, cityStates(l.text, mentions)...)
	if len(mentions) == 0 {
		mentions = append(mentions, stateMentions(l.text)...)
	}
	for _, m := range mentions {
		switch {
		case ex >= 0 && m.Start >= ex:
			l.excluded = append(l.excluded, m)
		case m.Place.Kind == geo.Worldwide:
			l.worldwide = true
		default:
			l.places = append(l.places, m)
		}
	}
	joined := " " + strings.Join(toks, " ") + " "
	for _, t := range toks {
		switch {
		case remoteWords[t]:
			l.remote = true
		case t == "hybrid":
			l.hybrid = true
		case t == "only":
			l.only = true
		}
	}
	if strings.Contains(joined, " work from home ") || strings.Contains(joined, " home based ") {
		l.remote = true
	}
	for _, w := range onsiteWords {
		if strings.Contains(joined, " "+w+" ") {
			l.onsite = true
		}
	}
	l.windows = geo.ParseTimezones(l.text)
	l.unknownSegments = unknownSegments(l.text)
	return l
}

var segmentRe = regexp.MustCompile(`[;|/\n]`)

var cityStateRe = regexp.MustCompile(`\b([A-Z][A-Za-z.'\-]+(?:\s+[A-Za-z.'\-]+){0,2}),\s*([A-Z]{2})\b`)

var (
	stateUnderscoreRe = regexp.MustCompile(`^([A-Z]{2})_[A-Za-z .]+_(?:HQ|Office)`)
	remoteStateRe     = regexp.MustCompile(`(?i:remote)[\s,\-\x{2013}(]+([A-Z]{2})\b|\b([A-Z]{2})[\s,\-\x{2013}]+(?i:remote)\b`)
)

// stateMentions finds US state and Canadian province codes written the way
// postings write them without a city: "AZ_Mesa_HQ", "Remote, NY", "DC - Remote".
func stateMentions(text string) []geo.Mention {
	var out []geo.Mention
	add := func(code string) {
		if country := geo.StateCountry(code); country != "" {
			out = append(out, geo.Mention{Place: geo.Place{Name: code, Kind: geo.Subdivision, Code: country}, Text: strings.ToLower(code), Start: -1, End: -1})
		}
	}
	if m := stateUnderscoreRe.FindStringSubmatch(text); m != nil {
		add(m[1])
	}
	for _, m := range remoteStateRe.FindAllStringSubmatch(text, -1) {
		if m[1] != "" {
			add(m[1])
		} else {
			add(m[2])
		}
	}
	return out
}

// cityStates finds "Falmouth, MA" and "Edinburg, TX": a place-sized word, a
// comma and a US state or Canadian province code, when the words are not
// already a known place. The city is unknown, but the country is not.
func cityStates(text string, known []geo.Mention) []geo.Mention {
	var out []geo.Mention
	for _, m := range cityStateRe.FindAllStringSubmatch(text, -1) {
		country := geo.StateCountry(m[2])
		if country == "" {
			continue
		}
		city := strings.ToLower(m[1])
		dup := false
		for _, k := range known {
			if strings.Contains(city, k.Text) || k.Text == city {
				dup = true
			}
		}
		if dup || remoteWords[strings.Fields(city)[0]] {
			continue
		}
		out = append(out, geo.Mention{Place: geo.Place{Name: m[1] + ", " + m[2], Kind: geo.Subdivision, Code: country, City: true}, Text: city, Start: -1, End: -1})
	}
	return out
}

var unknownSplitRe = regexp.MustCompile(`[,;|/\x{2022}()\n]`)

// unknownSegments counts the parts of a location that are not a known place,
// not a remote/hybrid word and not empty.
func unknownSegments(text string) int {
	n := 0
	for _, seg := range unknownSplitRe.Split(text, -1) {
		toks := geo.Tokens(seg)
		if len(toks) == 0 || len(geo.Scan(seg)) > 0 {
			continue
		}
		generic := true
		for _, tk := range toks {
			if !filler[tk] && !remoteWords[tk] && tk != "hybrid" && tk != "onsite" && tk != "office" && tk != "site" && tk != "on" && len(tk) > 1 {
				generic = false
			}
		}
		if !generic && len(strings.Join(toks, "")) >= 3 {
			n++
		}
	}
	return n
}

// hasScope reports whether the text names any geography at all.
func (l location) hasScope() bool {
	return len(l.places) > 0 || l.worldwide || len(l.excluded) > 0
}

// includes reports whether the target country is inside the geography, and
// why. Worldwide includes everyone; a list of exclusions with nothing else
// means everyone else.
func (l location) includes(target string) (bool, string) {
	if len(l.excluded) > 0 {
		for _, m := range l.excluded {
			if m.Place.Includes(target) {
				return false, "excludes " + m.Place.Name
			}
		}
		if len(l.places) == 0 {
			return true, "open everywhere except " + names(l.excluded)
		}
	}
	if l.worldwide {
		return true, "open worldwide"
	}
	for _, m := range l.places {
		if m.Place.Includes(target) {
			return true, "includes " + m.Place.Name
		}
	}
	return false, "limited to " + names(l.places)
}

// inTarget reports whether a place in the text is the target country itself
// (or a city or region of it), as opposed to a region that contains it.
func (l location) inTarget(target string) bool {
	for _, m := range l.places {
		if (m.Place.Kind == geo.Country || m.Place.Kind == geo.Subdivision) && m.Place.Code == target {
			return true
		}
	}
	return false
}

// regionListed reports whether a region that contains the target is an item of
// its own in a list ("EMEA; Hungary; Austria"), as opposed to the last word of
// one office's address ("Novi Sad, Serbia, EMEA").
func (l location) regionListed(target string) bool {
	for _, seg := range segmentRe.Split(l.text, -1) {
		ms := geo.Scan(seg)
		if len(ms) != 1 || ms[0].Place.Kind != geo.Region || !ms[0].Place.Includes(target) {
			continue
		}
		// The rest of the segment may only be the words that say "remote".
		extra := 0
		for _, tk := range geo.Tokens(seg) {
			if !filler[tk] {
				extra++
			}
		}
		if extra <= ms[0].End-ms[0].Start {
			return true
		}
	}
	return false
}

// filler are the words that may surround a region in a remote list item
// ("Home based - EMEA", "Remote, EMEA", "EMEA only").
var filler = map[string]bool{"remote": true, "home": true, "based": true, "only": true, "work": true, "from": true, "remotely": true,
	"working": true, "employee": true, "employees": true, "hybrid": true, "office": true, "in": true, "the": true, "or": true, "and": true}

func names(ms []geo.Mention) string {
	var out []string
	for _, m := range ms {
		if !slices.Contains(out, m.Place.Name) {
			out = append(out, m.Place.Name)
		}
	}
	return strings.Join(out, ", ")
}
