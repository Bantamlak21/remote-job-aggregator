package filtering

import (
	"math"
	"regexp"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/geo"
)

// sigKind is what a sentence of a description says about who may apply.
type sigKind int

const (
	sigOnlyIn   sigKind = iota // applicants must be in these places
	sigNotIn                   // applicants must not be in these places
	sigOpen                    // anyone, anywhere
	sigAuthIn                  // must be authorized to work in these places
	sigTimezone                // must work in these time zones
)

// signal is one statement found in a description.
type signal struct {
	kind     sigKind
	places   []geo.Mention
	windows  []geo.Window
	evidence string
	field    string // "description" (default), "title"
	// strong: a statement about this role's candidates, which may override the
	// location field. A statement about the company ("we hire globally") is
	// not strong.
	strong bool
	// weak: a hint (pay-range boilerplate), used only when the location field
	// says nothing.
	weak bool
	// located: a time-zone statement about where the candidate lives, as
	// opposed to which hours they work.
	located bool
}

const tailLen = 140

var (
	// Statements that applicants must be in certain places. Each captures the
	// text after the phrase, which is then read for places.
	onlyInPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bhiring\s+(?:for\s+)?(?:\w+\s+){0,3}?(?:based|located)\s+in\s+(.+)`),
		regexp.MustCompile(`\b(?:can|may)\s+be\s+(?:based|located|done|worked|performed)\s+(?:remotely\s+)?(?:anywhere\s+)?(?:in|from|within)\s+(.+)`),
		regexp.MustCompile(`\blocation\s*[:\-]\s*([^.\n(]{3,60}?)\s*\(?\s*required\b`),
		regexp.MustCompile(`\b(?:you|candidates|applicants)\s+(?:are|should be)\s+(?:currently\s+)?(?:based|located|residing|living)\s+in\s+(.+)`),
		regexp.MustCompile(`\bremote\s*\(?\s*(?:within|only in|in|from)\s+(?:the\s+)?(.+)`),
		regexp.MustCompile(`\b(?:hiring|recruiting)\s+for\s+this\s+(?:role|position|job)\s+in\s+(.+)`),
		regexp.MustCompile(`\bfocused\s+on\s+hiring\s+(?:for\s+this\s+(?:role|position|job)\s+)?in\s+(.+)`),
		regexp.MustCompile(`\bexclusively\s+(?:based|located)\s+in\s+(.+)`),
		regexp.MustCompile(`\b(?:must|need to|needs to|have to|has to|required to|should|will need to|are required to)\s+(?:currently\s+|also\s+|legally\s+|physically\s+)*(?:be\s+)?(?:located|based|resident|residing|reside|live|living|work|working|permanently located)\s+(?:remotely\s+)?(?:and\s+\w+\s+)?(?:in|within|from|out of|across)\s+(.+)`),
		regexp.MustCompile(`\b(?:candidates|applicants|employees|people|talent|team members|individuals)\s+(?:must|need to|should|have to)\s+(?:be\s+)?(?:located|based|residing|resident|living)\s+(?:in|within)\s+(.+)`),
		regexp.MustCompile(`\b(?:we\s+(?:are|'re)?\s*(?:currently\s+)?(?:only\s+)?(?:hiring|accepting applications|considering candidates|recruiting|looking for candidates)|this\s+(?:role|position|job|opportunity)\s+is\s+(?:currently\s+)?(?:only\s+)?(?:open|available|for candidates))\s+(?:to\s+(?:candidates|applicants|residents|people)\s+)?(?:located\s+|based\s+|living\s+)?(?:only\s+)?(?:in|within|from)\s+(.+)`),
		regexp.MustCompile(`\b(?:open|available|restricted|limited)\s+(?:only\s+)?to\s+(?:candidates|applicants|residents|people|those|individuals|citizens)\s+(?:who are\s+)?(?:located\s+|based\s+|living\s+|residing\s+)?(?:in|within|from)\s+(.+)`),
		regexp.MustCompile(`\b(?:remote|role|position|job|opportunity|hiring|location)\s*[:(\-]?\s*(?:in\s+)?((?:the\s+)?[a-z][a-z .,/&]{1,40}?)\s*[- ]only\b`),
		regexp.MustCompile(`\b(?:must be|need to be|needs to be|only|prefer|looking for)\s+((?:the\s+)?[a-z][a-z .]{1,30}?)[- ]based\b`),
		regexp.MustCompile(`\b(?:remote|hiring|located|based)\s+(?:within|inside)\s+(?:the\s+)?(?:borders? of\s+)?(.+)`),
		regexp.MustCompile(`\b(?:work|working|employed)\s+(?:remotely\s+)?(?:from|in)\s+(?:one of the following (?:countries|locations|states)|the following (?:countries|locations|states)):?\s*(.+)`),
	}
	// Pay-range boilerplate ("for candidates located in Canada"): a hint about
	// where the job is, weaker than a requirement.
	weakOnlyInPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:for|to)\s+(?:candidates|applicants|employees)\s+(?:who\s+are\s+)?(?:located|based|residing|living)\s+in\s+(.+)`),
	}
	// Residency statements, which name the place straight away ("resident in
	// France", "citizens of the US"; not "a resident in a medical program").
	residencyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:resident|residents|citizens?|nationals?)\s+(?:in|of)\s+(.+)`),
	}
	// Statements that need work authorization in a place.
	authPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:must\s+be|need\s+to\s+be|be)\s+(?:a\s+)?((?:u\.?s\.?|united states)\s+(?:citizens?|nationals?|persons?))`),
		regexp.MustCompile(`\b((?:u\.?s\.?|united states)\s+citizenship)\s+(?:is\s+)?(?:required|needed|mandatory)`),
		regexp.MustCompile(`\b(?:active\s+|current\s+|eligible\s+for\s+(?:an?\s+)?)?((?:top\s+secret|ts/sci|secret)\s+(?:security\s+)?clearance)`),
		regexp.MustCompile(`\b(?:authori[sz]ed|eligible|legally\s+(?:able|entitled|permitted|allowed)|right|permission|permit|allowed|entitled|able)\s+to\s+work\s+(?:legally\s+)?(?:remotely\s+)?(?:in|within)\s+(?:the\s+)?(.+)`),
		regexp.MustCompile(`\bwork\s+(?:authori[sz]ation|permit|visa)\s+(?:for|in)\s+(?:the\s+)?(.+)`),
		regexp.MustCompile(`\b(?:right|rights)\s+to\s+work\s+in\s+(?:the\s+)?(.+)`),
	}
	// Statements that applicants outside some places are not considered.
	outsidePatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:we\s+)?(?:can(?:not|'t)|are\s+unable\s+to|do\s+not|don't|aren't\s+able\s+to|are\s+not\s+able\s+to|cannot\s+currently)\s+(?:currently\s+)?(?:hire|employ|support|consider|accept)\s+(?:candidates\s+|applicants\s+)?(?:in|from|based in|located in)\s+(.+)`),
		regexp.MustCompile(`\b(?:not|no)\s+(?:open|available|eligible)\s+(?:to|for)\s+(?:candidates|applicants|residents)?\s*(?:in|from|located in)\s+(.+)`),
	}
	outsideOnly = regexp.MustCompile(`\b(?:candidates|applicants|residents)?\s*(?:located\s+)?outside\s+(?:of\s+)?(?:the\s+)?(.+?)\s+(?:will\s+not|cannot|can't|are\s+not|aren't|not)\b`)

	// Statements about this role's candidates that permit anywhere. ("We hire
	// globally" is about the company and does not override a country.)
	strongOpenPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:candidates|applicants|you|employees|talent|people)\s+(?:may|can|could|are welcome to|are able to|are free to)\s+be\s+(?:based|located)\s+(?:in\s+|from\s+)?(?:anywhere|any|all|whichever)\b`),
		regexp.MustCompile(`\b(?:this\s+)?(?:role|position|job|opportunity)\s+(?:is|can be)\s+(?:fully\s+|100%\s+)?(?:remote\s+)?(?:done|performed|worked|based|open|available)?\s*(?:from\s+)?(?:anywhere|worldwide|globally)\b`),
		regexp.MustCompile(`\bopen to (?:candidates|applicants) (?:from|in|based in|located in) (?:any|all|anywhere|every)\b`),
		regexp.MustCompile(`\b(?:location|based|where):\s*(?:remote\s*[-\x{2013},]?\s*)?(?:anywhere|worldwide|global)\b`),
	}
	locatedRe  = regexp.MustCompile(`\b(?:located|based|reside|resid(?:e|ing|ent)|live|living|living in|in the .{0,30} time ?zones?|time ?zone of)\b`)
	exceptRe   = regexp.MustCompile(`\b(?:except|excluding|other than|apart from|not|outside)\b`)
	tzSentence = regexp.MustCompile(`(?i)\b(?:overlap|working hours|work hours|business hours|core hours|time ?zones?|available (?:during|between)|online (?:during|between)|hours (?:that )?(?:overlap|align))\b`)
)

// scanDescription finds the eligibility statements in a plain-text
// description.
func scanDescription(text string) []signal {
	var out []signal
	sents := sentences(text)
	for si, s := range sents {
		if len(s) > 600 {
			s = s[:600]
		}
		low := strings.ToLower(s)
		if !mayStateEligibility(low) {
			continue
		}
		exceptBefore := false
		tail := func(re *regexp.Regexp) (string, bool) {
			m := re.FindStringSubmatchIndex(low)
			if m == nil {
				return "", false
			}
			t := low[m[len(m)-2]:]
			if len(t) > tailLen {
				t = t[:tailLen]
			}
			// "worldwide, except residents of the United States": what follows
			// an exception is excluded, not required.
			before := low[:m[0]]
			if len(before) > 30 {
				before = before[len(before)-30:]
			}
			exceptBefore = exceptRe.MatchString(before)
			return t, true
		}
		found := false
		for _, re := range onlyInPatterns {
			if t, ok := tail(re); ok {
				clause := t
				if next := nextClause(sents, si, t); next != "" {
					clause = strings.ReplaceAll(t, ":", " ") + next
				}
				if ms := scanPlaces(clause); len(ms) > 0 {
					kind := sigOnlyIn
					if exceptBefore {
						kind = sigNotIn
					}
					out = append(out, signal{kind: kind, places: ms, evidence: quote(s), strong: true, weak: relocRe.MatchString(low)})
					found = true
					break
				}
			}
		}
		if !found {
			for _, re := range residencyPatterns {
				if t, ok := tail(re); ok {
					t = strings.TrimPrefix(scanPlacesText(t), "the ")
					if ms := firstPlaces(t, 1); len(ms) > 0 {
						kind := sigOnlyIn
						if exceptBefore {
							kind = sigNotIn
						}
						out = append(out, signal{kind: kind, places: ms, evidence: quote(s), strong: true})
						found = true
						break
					}
				}
			}
		}
		if !found {
			for _, re := range weakOnlyInPatterns {
				if t, ok := tail(re); ok {
					if ms := scanPlaces(t); len(ms) > 0 {
						out = append(out, signal{kind: sigOnlyIn, places: ms, evidence: quote(s), weak: true})
						found = true
						break
					}
				}
			}
		}
		if !found {
			for _, re := range authPatterns {
				if t, ok := tail(re); ok {
					ms := firstPlaces(t, 6)
					if len(ms) == 0 && strings.Contains(t, "clearance") {
						// A US security clearance needs US citizenship.
						if us, ok := geo.Lookup("United States"); ok {
							ms = []geo.Mention{{Place: us, Text: "united states"}}
						}
					}
					if len(ms) > 0 {
						out = append(out, signal{kind: sigAuthIn, places: ms, evidence: quote(s)})
						found = true
						break
					}
				}
			}
		}
		if !found {
			for _, re := range outsidePatterns {
				if t, ok := tail(re); ok {
					if ms := scanPlaces(t); len(ms) > 0 {
						out = append(out, signal{kind: sigNotIn, places: ms, evidence: quote(s)})
						found = true
						break
					}
				}
			}
		}
		if !found {
			if t, ok := tail(outsideOnly); ok {
				_ = t
				if m := outsideOnly.FindStringSubmatch(low); m != nil {
					if ms := scanPlaces(m[1]); len(ms) > 0 {
						out = append(out, signal{kind: sigOnlyIn, places: ms, evidence: quote(s)})
						found = true
					}
				}
			}
		}
		if !found {
			for _, re := range strongOpenPatterns {
				if m := re.FindStringIndex(low); m != nil && !strings.Contains(low, " not ") && !strings.Contains(low, "n't") && !narrowed(low[m[1]:]) {
					out = append(out, signal{kind: sigOpen, evidence: around(s, m[0], m[1]), strong: true})
					found = true
					break
				}
			}
		}
		if tzSentence.MatchString(s) {
			if ws := geo.ParseTimezones(s); len(ws) > 0 {
				out = append(out, signal{kind: sigTimezone, windows: ws, evidence: quote(s), located: locatedRe.MatchString(low)})
			}
		}
	}
	return out
}

var narrowedRe = regexp.MustCompile(`^\s*(?:within|in|inside|across|throughout|of|on|near|around|from)\s+(?:the\s+)?[a-z]`)

// narrowed reports whether what follows an "anywhere" takes it back
// ("anywhere within the UK", "anywhere in the US").
func narrowed(after string) bool { return narrowedRe.MatchString(after) }

// around quotes the part of a sentence that carries a match, with some
// context, rather than the whole sentence.
func around(s string, from, to int) string {
	lo, hi := from-70, to+90
	if lo < 0 {
		lo = 0
	}
	if hi > len(s) {
		hi = len(s)
	}
	out := strings.TrimSpace(s[lo:hi])
	if lo > 0 {
		out = "..." + out
	}
	if hi < len(s) {
		out += "..."
	}
	return out
}

// gateWords are stems at least one of which every eligibility statement
// contains. A sentence with none of them is skipped before the (much slower)
// patterns run.
var gateWords = []string{"locat", "based", "resid", "citizen", "national", "authori", "eligib", "permit", "visa", "hiring", "hire",
	"recruit", "only", "within", "remote", "licen", "exclusive", "outside", "cannot", "can't", "unable", "not open", "not available",
	"anywhere", "worldwide", "globally", "time zone", "timezone", "time-zone", "overlap", "working hours", "work hours", "business hours",
	"core hours", "open to", "available to", "available during", "online", "right to work", "countries", "in the us", "in the uk",
	"any geography", "hours", "pst", "est", "cet", "utc", "gmt", "eet", "pacific", "eastern", "central", "clearance", "citizen", "required"}

// mayStateEligibility is the cheap test that gates the pattern scan.
func mayStateEligibility(low string) bool {
	for _, w := range gateWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

var relocRe = regexp.MustCompile(`\b(?:relocat\w*|open to remote|remote arrangements?|other locations?)\b`)

// nextClause returns the start of the next sentence when a requirement is
// introduced and its list follows ("must be located in one of these regions:"
// then the countries on the next lines), otherwise "".
func nextClause(sents []string, i int, tail string) string {
	if i+1 >= len(sents) {
		return ""
	}
	if !strings.Contains(tail, "one of") && !strings.Contains(tail, "following") && !strings.Contains(tail, "these") {
		return ""
	}
	n := sents[i+1]
	if len(n) > 120 {
		n = n[:120]
	}
	return " " + strings.ToLower(n)
}

// scanPlaces reads the places in the start of a clause, stopping at a
// sentence-level break so that "in the US. We offer ..." does not run on.
func scanPlaces(t string) []geo.Mention {
	if i := strings.IndexAny(t, ".;:"); i > 0 && i < len(t)-1 {
		t = t[:i]
	}
	return withoutWorldwide(geo.Scan(t))
}

// scanPlacesText cuts a clause at the first sentence-level break.
func scanPlacesText(t string) string {
	if i := strings.IndexAny(t, ".;:"); i > 0 && i < len(t)-1 {
		t = t[:i]
	}
	return t
}

// firstPlaces returns the mentions among the first n words.
func firstPlaces(t string, n int) []geo.Mention {
	var out []geo.Mention
	for _, m := range withoutWorldwide(geo.Scan(t)) {
		if m.Start < n {
			out = append(out, m)
		}
	}
	return out
}

// tzDistance is how far, in hours, the target's UTC offset is from the nearest
// window: 0 inside one.
func tzDistance(ws []geo.Window, offset float64) float64 {
	best := math.Inf(1)
	for _, w := range ws {
		d := 0.0
		switch {
		case offset < w.Lo:
			d = w.Lo - offset
		case offset > w.Hi:
			d = offset - w.Hi
		}
		best = math.Min(best, d)
	}
	return best
}
