package filtering

import (
	"regexp"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/geo"
)

var (
	titleBracket  = regexp.MustCompile(`[(\[]([^)\]]{2,60})[)\]]`)
	titleBasedIn  = regexp.MustCompile(`(?i)\b(?:based|located)\s+in\s+(.+)`)
	titleBased    = regexp.MustCompile(`(?i)\b((?:the\s+)?[a-z.]{2,20}(?:\s+[a-z]{2,12})?)[- ]based\b`)
	titleTail     = regexp.MustCompile(`(?:\s[-\x{2013}\x{2014}|:]\s*|,\s*)(?:remote\s*[-\x{2013}\x{2014},(]?\s*)?([A-Za-z .,&/]{2,40})\)?\s*$`)
	headquarterRe = regexp.MustCompile(`(?im)\bheadquarters:[ \t]*(.*?)[ \t]*(?:URL:|$)`)
)

var titleRemotePlaceRe = regexp.MustCompile(`(?i)\b([a-z.]{2,15})\s+remote\b|\bremote\s*[-\x{2013}:]?\s*([a-z.]{2,15})\b`)

// scanTitle reads restrictions written into a job title: "(US based)",
// "Remote (North America)", "US-Based", "Account Executive - Germany".
func scanTitle(title string) []signal {
	var out []signal
	add := func(ms []geo.Mention) {
		cityOnly := len(ms) > 0
		for _, m := range ms {
			if !(m.Place.Kind == geo.Subdivision && m.Place.City) {
				cityOnly = false
			}
		}
		if len(ms) > 0 && !cityOnly {
			out = append(out, signal{kind: sigOnlyIn, places: ms, evidence: quote(title), field: "title"})
		}
	}
	if m := titleBasedIn.FindStringSubmatch(title); m != nil {
		if ms := withoutWorldwide(geo.Scan(m[1])); len(ms) > 0 {
			out = append(out, signal{kind: sigOnlyIn, places: ms, evidence: quote(title), field: "title"})
			return out
		}
	}
	for _, m := range titleRemotePlaceRe.FindAllStringSubmatch(title, -1) {
		word := m[1]
		if word == "" {
			word = m[2]
		}
		if ms := geo.Scan(word); len(ms) == 1 && ms[0].Place.Kind != geo.Worldwide && ms[0].Place.Kind != geo.Ambiguous {
			out = append(out, signal{kind: sigOnlyIn, places: ms, evidence: quote(title), field: "title"})
			return out
		}
	}
	for _, m := range titleBracket.FindAllStringSubmatch(title, -1) {
		inner := m[1]
		if ms := geo.Scan(inner); len(ms) > 0 && (mostlyPlaces(inner, ms) || hasAny(strings.ToLower(inner), "based", "remote", "only", "eligible", "resident")) {
			add(withoutWorldwide(ms))
		}
	}
	if len(out) == 0 {
		for _, m := range titleBased.FindAllStringSubmatch(title, -1) {
			add(withoutWorldwide(geo.Scan(m[1])))
		}
	}
	if len(out) == 0 {
		if m := titleTail.FindStringSubmatch(title); m != nil {
			if ms := geo.Scan(m[1]); len(ms) > 0 && mostlyPlaces(m[1], ms) {
				// "Head of CS, DACH": a territory or an office, a hint that only
				// counts against a location that permits Ethiopia.
				n := len(out)
				add(withoutWorldwide(ms))
				for i := n; i < len(out); i++ {
					out[i].weak = true
				}
			}
		}
	}
	return out
}

func withoutWorldwide(ms []geo.Mention) []geo.Mention {
	var out []geo.Mention
	for _, m := range ms {
		if m.Place.Kind != geo.Worldwide && m.Place.Kind != geo.Ambiguous {
			out = append(out, m)
		}
	}
	return out
}

// mostlyPlaces reports whether the mentions make up most of the words of s.
func mostlyPlaces(s string, ms []geo.Mention) bool {
	total := len(geo.Tokens(s))
	covered := 0
	for _, m := range ms {
		covered += m.End - m.Start
	}
	return total > 0 && covered*2 >= total
}

func hasAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// headquarters returns the text of a "Headquarters:" line, which We Work
// Remotely puts at the top of a description and which is where the posting
// really is (as opposed to the "Anywhere in the World" tag).
func headquarters(desc string) string {
	head := desc
	if len(head) > 400 {
		head = head[:400]
	}
	if m := headquarterRe.FindStringSubmatch(head); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// hqSignals turns a headquarters line into a restriction when it names a
// remote-in-place location ("Remote - US", "Ontario, Canada (Remote)"). A line
// that is only a company city or state ("Tampa, Florida") says where the
// company sits, not who may apply, and is ignored.
func hqSignals(hq string) []signal {
	if hq == "" || !strings.Contains(strings.ToLower(hq), "remote") {
		return nil
	}
	// The line runs straight into the company text, so only its first words
	// are the location.
	var ms []geo.Mention
	for _, m := range withoutWorldwide(geo.Scan(hq)) {
		if m.Start < 8 {
			ms = append(ms, m)
		}
	}
	if len(ms) == 0 {
		return nil
	}
	return []signal{{kind: sigOnlyIn, places: ms, evidence: quote("Headquarters: " + hq), field: "description"}}
}
