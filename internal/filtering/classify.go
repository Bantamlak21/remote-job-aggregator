package filtering

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/geo"
)

// Basis says what an eligibility verdict rests on, so a UI can tell a local
// job from a worldwide remote one and a restriction from a missing signal.
type Basis string

const (
	BasisLocalEthiopia     Basis = "local_ethiopia"         // the job is in Ethiopia
	BasisWorldwide         Basis = "worldwide"              // open worldwide
	BasisRegionIncludes    Basis = "region_includes_africa" // a region that contains Ethiopia (EMEA, Africa)
	BasisTimezoneWindow    Basis = "timezone_window"        // a time-zone window that contains UTC+3
	BasisExclusionListOK   Basis = "exclusion_list_ok"      // "anywhere except ..." and Ethiopia is not excepted
	BasisExplicitPermitted Basis = "explicit_permission"    // the description says candidates may be anywhere
	BasisRestrictedPlaces  Basis = "restricted_places"      // limited to places that do not include Ethiopia
	BasisExcluded          Basis = "excluded"               // Ethiopia (or its region) is excluded
	BasisTimezoneOutside   Basis = "timezone_outside"       // a time zone Ethiopia is not in
	BasisOnsiteElsewhere   Basis = "onsite_elsewhere"       // on-site or hybrid outside Ethiopia
	BasisWorkAuthorization Basis = "work_authorization"     // needs work authorization or citizenship elsewhere
	BasisConflict          Basis = "conflicting_signals"    // signals disagree
	BasisNoSignal          Basis = "no_signal"              // the text does not say
)

// Classifier decides eligibility for one target country.
type Classifier struct {
	Target string  // alpha-2 code, "ET"
	Offset float64 // the country's UTC offset in hours, 3
}

// NewClassifier returns the classifier for Ethiopia.
func NewClassifier() *Classifier { return &Classifier{Target: "ET", Offset: geo.EthiopiaOffset} }

// boardSources are the job boards: their location is where applicants may
// come from, not where the job is.
var boardSources = map[string]bool{
	"himalayas": true, "remotive": true, "jobicy": true, "weworkremotely": true, "workingnomads": true, "remoteok": true,
}

// IsBoardSource reports whether a source is a remote job board.
func IsBoardSource(source string) bool { return boardSources[source] }

// cityStateCountries are countries that are one city: an office there is an office in
// a city, not a statement about the whole country.
var cityStateCountries = map[string]bool{"SG": true, "HK": true, "MO": true, "MC": true, "VA": true}

// tzMaxDistance is how many hours a required time zone may be from the
// target's before working hours stop overlapping enough.
const tzMaxDistance = 4.0

type result struct {
	status     Status
	confidence float64
	basis      Basis
	reasons    []string
	evidence   []Evidence
	restrict   []string
	hours      string
}

func (r *result) verdict(locs []string) Verdict {
	return Verdict{Status: r.status, Confidence: r.confidence, Basis: r.basis, Reasons: r.reasons, Evidence: r.evidence,
		Locations: locs, Restrictions: r.restrict, HoursConstraint: r.hours}
}

func decided(status Status, conf float64, basis Basis, reason string, ev ...Evidence) *result {
	return &result{status: status, confidence: conf, basis: basis, reasons: []string{reason}, evidence: ev}
}

// Classify decides whether a candidate in the target country could take the
// job, from the location, the title and the description.
func (c *Classifier) Classify(in Input) Verdict {
	loc := analyzeLocation(in.Location)
	desc := plainText(in.Description)
	board := boardSources[in.Source]

	var locs []string
	for _, m := range append(slices.Clone(loc.places), loc.excluded...) {
		if !slices.Contains(locs, m.Place.Name) {
			locs = append(locs, m.Place.Name)
		}
	}

	res := c.decide(in, loc, desc, board)
	v := res.verdict(locs)
	if v.Reasons == nil {
		v.Reasons = []string{}
	}
	if v.Evidence == nil {
		v.Evidence = []Evidence{}
	}
	if v.Restrictions == nil {
		v.Restrictions = []string{}
	}
	if v.Locations == nil {
		v.Locations = []string{}
	}
	return v
}

func (c *Classifier) decide(in Input, loc location, desc string, board bool) *result {
	locEv := Evidence{Field: "location", Text: quote(in.Location)}

	// 1. The job is in the target country.
	if loc.inTarget(c.Target) {
		return decided(Eligible, 0.95, BasisLocalEthiopia, "the job is located in Ethiopia", locEv)
	}
	if in.Market == "ethiopia" && !board && !loc.hasScope() {
		return decided(Eligible, 0.75, BasisLocalEthiopia, "listed in the Ethiopian market with no place elsewhere", Evidence{Field: "market", Text: "ethiopia"})
	}

	// Signals from the text around the location.
	sigs := scanDescription(desc)
	sigs = append(sigs, scanTitle(in.Title)...)
	hq := headquarters(desc)
	if board && in.Source == "weworkremotely" {
		sigs = append(sigs, hqSignals(hq)...)
	}

	// 2. An explicit restriction, and 3. an explicit permission, in the text.
	locIncl, _ := loc.includes(c.Target)
	neg, pos := c.split(sigs, loc.hasScope(), locIncl)
	hours := ""
	kept := pos[:0]
	for _, p := range pos {
		if p.conf == 0 {
			if hours == "" {
				hours = p.hours
			}
			continue
		}
		kept = append(kept, p)
	}
	pos = kept
	// A time-zone statement does not widen a list of places that leaves the
	// target out.
	if in, _ := loc.includes(c.Target); loc.hasScope() && !in {
		kept = pos[:0]
		for _, p := range pos {
			if p.basis != BasisTimezoneWindow {
				kept = append(kept, p)
			}
		}
		pos = kept
	}
	r := c.decideFromSignals(in, neg, pos, loc, desc, board, locEv)
	if r != nil && r.status == Eligible && r.hours == "" {
		r.hours = hours
	}
	return r
}

func (c *Classifier) decideFromSignals(in Input, neg, pos []finding, loc location, desc string, board bool, locEv Evidence) *result {
	if permits, _ := loc.includes(c.Target); loc.hasScope() && permits && len(pos) == 0 && len(neg) > 0 && allWeak(neg) {
		// Only hints, against a location that permits: a conflict, not a verdict.
		r := decided(Uncertain, 0.3, BasisConflict, "the location permits Ethiopia but the description hints at a narrower place", locEv, neg[0].ev())
		return r
	}
	switch {
	case len(neg) > 0 && len(pos) > 0:
		r := decided(Uncertain, 0.3, BasisConflict, "the text both restricts and permits candidates outside the listed places")
		r.evidence = append(r.evidence, neg[0].ev(), pos[0].ev())
		return r
	case len(neg) > 0:
		s := neg[0]
		r := decided(Ineligible, s.conf, s.basis, s.reason, s.ev())
		r.restrict = append(r.restrict, s.limit)
		return r
	case len(pos) > 0:
		s := pos[0]
		r := decided(Eligible, s.conf, s.basis, s.reason, s.ev())
		r.hours = s.hours
		return r
	}

	// 4. and 5. The location field and the workplace type.
	r := c.fromLocation(in, loc, desc, board, locEv)
	// A restriction from the location field, next to a sentence that says the
	// company hires in a region containing the target, is a conflict, not a
	// verdict.
	if r.status == Ineligible && r.basis == BasisRestrictedPlaces {
		if ev, ok := c.hiresInTarget(desc); ok {
			out := decided(Uncertain, 0.3, BasisConflict, "the location is limited, but the description says the company hires in a region that includes Ethiopia", locEv, Evidence{Field: "description", Text: ev})
			return out
		}
	}
	return r
}

var hiringVerbRe = regexp.MustCompile(`(?i)\b(?:we\s+(?:work with|hire|are hiring|recruit)|hiring|recruiting|hire\s+(?:in|from|developers|engineers))\b`)

// hiresInTarget finds a sentence that says people are hired in a place that
// includes the target ("... and Africa (including Morocco and South Africa)").
func (c *Classifier) hiresInTarget(desc string) (string, bool) {
	for _, s := range sentences(desc) {
		if !hiringVerbRe.MatchString(s) {
			continue
		}
		for _, m := range geo.Scan(s) {
			if m.Place.Kind == geo.Region && m.Place.Includes(c.Target) {
				return quote(s), true
			}
		}
	}
	return "", false
}

// finding is a signal turned into a verdict fragment.
type finding struct {
	weak   bool
	conf   float64
	basis  Basis
	reason string
	limit  string
	hours  string
	field  string
	text   string
}

func newFinding(conf float64, basis Basis, reason, limit, hours, field, text string) finding {
	return finding{conf: conf, basis: basis, reason: reason, limit: limit, hours: hours, field: field, text: text}
}

func (f finding) ev() Evidence { return Evidence{Field: f.field, Text: f.text} }

// split turns signals into restrictions (that rule the target out) and
// permissions (that rule it in).
func (c *Classifier) split(sigs []signal, hasScope, locIncludes bool) (neg, pos []finding) {
	for _, s := range sigs {
		// A hint (pay-range boilerplate, a restriction with a relocation
		// escape) is ignored when the location already restricts, and is a
		// conflict when the location permits.
		if s.weak && hasScope && !locIncludes {
			continue
		}
		field := s.field
		if field == "" {
			field = "description"
		}
		includes := false
		for _, m := range s.places {
			if m.Place.Includes(c.Target) {
				includes = true
			}
		}
		switch s.kind {
		case sigOnlyIn:
			if includes {
				if s.strong {
					pos = append(pos, newFinding(0.8, BasisRegionIncludes, "the posting is limited to "+names(s.places)+", which includes Ethiopia", "", "", field, s.evidence))
				}
				continue
			}
			f := newFinding(0.85, BasisRestrictedPlaces, "the posting says candidates must be in "+names(s.places), names(s.places)+" only", "", field, s.evidence)
			f.weak = s.weak
			neg = append(neg, f)
		case sigAuthIn:
			if includes {
				continue
			}
			neg = append(neg, newFinding(0.8, BasisWorkAuthorization, "the job needs authorization to work in "+names(s.places), "work authorization: "+names(s.places), "", field, s.evidence))
		case sigNotIn:
			if includes {
				neg = append(neg, newFinding(0.85, BasisExcluded, "the posting excludes "+names(s.places), "not in "+names(s.places), "", field, s.evidence))
			}
		case sigOpen:
			if s.strong {
				pos = append(pos, newFinding(0.8, BasisExplicitPermitted, "the description says candidates may be based anywhere", "", "", field, s.evidence))
			}
		case sigTimezone:
			d := tzDistance(s.windows, c.Offset)
			switch {
			case d > tzMaxDistance && s.located:
				neg = append(neg, newFinding(0.7, BasisTimezoneOutside, "the candidate must be in a time zone far from Ethiopia", "time zone: "+s.windows[0].Text, "", field, s.evidence))
			case d > tzMaxDistance:
				pos = append(pos, finding{conf: 0, basis: "", hours: quote(s.evidence), field: field, text: s.evidence})
			case d == 0 && s.located:
				pos = append(pos, newFinding(0.7, BasisTimezoneWindow, "the required time zone includes UTC+3", "", "", field, s.evidence))
			}
		}
	}
	return neg, pos
}

var (
	descRemoteRe = regexp.MustCompile(`(?i)\b(?:fully|100%|entirely|completely|totally)\s+remote\b|\bremote[- ]first\b|\bthis\s+(?:is\s+)?(?:a\s+)?(?:fully\s+)?remote\s+(?:role|position|job|opportunity)\b|\b(?:role|position|job)\s+is\s+(?:fully\s+)?remote\b|\bwork\s+(?:fully\s+)?remotely\b|\bwork\s+from\s+home\b`)
	// Statements about THIS role's place of work. "We operate as a hybrid
	// workplace" is about the company and does not say where this job is done.
	descOnsiteRe = regexp.MustCompile(`(?i)\bbased\s+(?:in|at|out of)\s+our\s+[\w .,'-]{0,40}?\s*(?:office|hq|headquarters)\b|\b(?:this\s+)?(?:role|position|job)\s+(?:is|will be)\s+(?:a\s+)?(?:fully\s+|exclusively\s+)?(?:hybrid|on-?site|in[- ]office|office[- ]based|in[- ]person)\b|\b\d\s+days?\s+(?:a|per|each|every)\s+week\s+(?:in|at)\s+(?:the|our|one of our)\s+|\bwork(?:ing)?\s+(?:from the office|from our\s+[\w .-]{0,30}office)\b|#li-onsite\b|\b(?:this\s+)?(?:role|position|job)\s+is\s+(?:located|based)\s+(?:on-?site\s+)?(?:at|in)\b|\byou\s+will\s+be\s+(?:based|located)\s+(?:in|at|from)\b|\blocated\s+on-?site\b|\(on[- ]?site\)|\bwithin\s+\d+\s+miles\s+of\b|\bmust\s+(?:live|reside)\s+within\s+\d+|\blocated\s+from\s+our\s+[\w .-]{0,20}(?:office|hq|headquarters)\b|\bteam\s+is\s+located\s+in\s+(?:the\s+)?offices?\b|\bon-?site\s+(?:role|position|job)\b|\brequired\s+to\s+be\s+(?:on-?site|in the office)\b|\bmust\s+be\s+(?:on-?site|in the office|able to commute)\b|\bhybrid\s+(?:working\s+model|work(?:ing)?\s+(?:model|schedule|arrangement)|role|position)\b`)
)

var (
	onsiteGates = []string{"office", "hq", "headquarters", "on-site", "onsite", "on site", "hybrid", "in-person", "in person", "#li-onsite", "commute", "days", "based", "located", "miles", "reside"}
	remoteGates = []string{"remote", "home", "distributed"}
)

// firstSentenceMatch returns the first match of re in a sentence that
// contains one of the gate words (the big patterns are slow on a whole
// description), or "".
func firstSentenceMatch(desc string, gates []string, re *regexp.Regexp) string {
	for _, s := range sentences(desc) {
		low := strings.ToLower(s)
		ok := false
		for _, g := range gates {
			if strings.Contains(low, g) {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		if m := re.FindString(s); m != "" {
			return m
		}
	}
	return ""
}

// fromLocation decides from the location field and the workplace type.
func (c *Classifier) fromLocation(in Input, loc location, desc string, board bool, locEv Evidence) *result {
	rt := in.RemoteType
	remote := rt == "remote" || loc.remote
	hybrid := rt == "hybrid" || loc.hybrid
	onsite := rt == "onsite" || loc.onsite

	if in.Location != "" && (loc.hasScope() || len(loc.windows) > 0) {
		// Time zones alone.
		if !loc.hasScope() {
			d := tzDistance(loc.windows, c.Offset)
			switch {
			case d == 0:
				return &result{status: Eligible, confidence: 0.8, basis: BasisTimezoneWindow, evidence: []Evidence{locEv},
					reasons: []string{"the time-zone window includes UTC+3"}}
			case d <= 2:
				return &result{status: Eligible, confidence: 0.6, basis: BasisTimezoneWindow, evidence: []Evidence{locEv},
					reasons: []string{"the time zone is within two hours of UTC+3"}}
			case d > tzMaxDistance:
				r := decided(Ineligible, 0.7, BasisTimezoneOutside, "the required time zone is far from UTC+3", locEv)
				r.restrict = []string{"time zone " + loc.windows[0].Text}
				return r
			}
			return decided(Uncertain, 0.4, BasisNoSignal, "the time zone is a few hours from UTC+3", locEv)
		}

		includes, why := loc.includes(c.Target)
		if board {
			return c.boardScope(loc, includes, why, locEv)
		}
		return c.atsScope(in, loc, includes, why, remote, hybrid, onsite, desc, locEv)
	}

	// No geography in the location.
	switch {
	case in.Location == "" && desc == "":
		return decided(Uncertain, 0.2, BasisNoSignal, "no location or description")
	case hybrid && !remote:
		return decided(Uncertain, 0.35, BasisNoSignal, "hybrid with no place given", locEv)
	}
	return decided(Uncertain, 0.35, BasisNoSignal, "no geography in the location or the description", locEv)
}

// boardScope: a job board's location is where applicants may come from.
func (c *Classifier) boardScope(loc location, includes bool, why string, locEv Evidence) *result {
	if includes {
		basis, conf := BasisRegionIncludes, 0.9
		switch {
		case len(loc.excluded) > 0 && len(loc.places) == 0 && !loc.worldwide:
			basis = BasisExclusionListOK
		case loc.worldwide:
			basis = BasisWorldwide
		}
		return &result{status: Eligible, confidence: conf, basis: basis, evidence: []Evidence{locEv}, reasons: []string{"the location " + why}}
	}
	r := decided(Ineligible, 0.9, BasisRestrictedPlaces, "the location is "+why, locEv)
	if strings.HasPrefix(why, "excludes") {
		r.basis = BasisExcluded
	}
	r.restrict = []string{why}
	return r
}

// atsScope: an ATS's location is where the job is (or, for remote roles, the
// places it may be done from).
func (c *Classifier) atsScope(in Input, loc location, includes bool, why string, remote, hybrid, onsite bool, desc string, locEv Evidence) *result {
	specific := 0 // countries and cities outside the target
	cities := 0
	for _, m := range loc.places {
		if m.Place.Kind == geo.Country || m.Place.Kind == geo.Subdivision {
			specific++
		}
		if m.Place.Kind == geo.Subdivision || cityStateCountries[m.Place.Code] && m.Place.Kind == geo.Country {
			cities++
		}
	}
	// Hybrid or on-site with a region in the list: the office is the specific
	// place; with no place named the text does not say where the office is.
	if includes && !loc.worldwide && !remote && (hybrid || onsite) {
		if specific > 0 {
			r := decided(Ineligible, 0.75, BasisOnsiteElsewhere, "the job is hybrid or on-site in "+names(loc.places), locEv)
			r.restrict = []string{"on-site in " + names(loc.places)}
			return r
		}
		return decided(Uncertain, 0.4, BasisNoSignal, "hybrid in a region, and no office is named", locEv)
	}
	cities += loc.unknownSegments
	// "Novi Sad, Serbia, EMEA": a region next to a specific place is the
	// office's region tag, not an invitation to remote workers.
	if includes && !loc.worldwide && specific > 0 && !loc.regionListed(c.Target) {
		includes = false
		why = "is an office in " + names(loc.places)
	}
	if includes && !remote && !loc.worldwide && loc.regionListed(c.Target) {
		return &result{status: Eligible, confidence: 0.75, basis: BasisRegionIncludes, evidence: []Evidence{locEv}, reasons: []string{"the location lists a region that includes Ethiopia"}}
	}
	if includes {
		basis := BasisRegionIncludes
		if loc.worldwide {
			basis = BasisWorldwide
		} else if len(loc.excluded) > 0 && len(loc.places) == 0 {
			basis = BasisExclusionListOK
		}
		if remote || loc.worldwide {
			return &result{status: Eligible, confidence: 0.85, basis: basis, evidence: []Evidence{locEv}, reasons: []string{"the remote location " + why}}
		}
		return decided(Uncertain, 0.4, BasisNoSignal, "the location names a region but not whether the job is remote", locEv)
	}
	switch {
	case remote:
		r := decided(Ineligible, 0.85, BasisRestrictedPlaces, "the remote role is "+why, locEv)
		r.restrict = []string{why}
		return r
	case hybrid, onsite:
		conf := 0.85
		if in.RemoteType != "" && !loc.hybrid && !loc.onsite {
			conf = 0.75
		}
		r := decided(Ineligible, conf, BasisOnsiteElsewhere, "the job is on-site or hybrid in "+names(loc.places), locEv)
		r.restrict = []string{"on-site in " + names(loc.places)}
		return r
	}
	// Workplace type unknown: the description may say.
	onsiteHit := firstSentenceMatch(desc, onsiteGates, descOnsiteRe)
	remoteHit := firstSentenceMatch(desc, remoteGates, descRemoteRe)
	switch {
	case onsiteHit != "":
		m := onsiteHit
		r := decided(Ineligible, 0.75, BasisOnsiteElsewhere, "the description describes office work in "+names(loc.places), locEv, Evidence{Field: "description", Text: m})
		r.restrict = []string{"on-site in " + names(loc.places)}
		return r
	case remoteHit != "":
		m := remoteHit
		r := decided(Ineligible, 0.7, BasisRestrictedPlaces, "the remote role is "+why, locEv, Evidence{Field: "description", Text: m})
		r.restrict = []string{why}
		return r
	}
	if cities == 0 {
		r := decided(Ineligible, 0.6, BasisRestrictedPlaces, "the job is tied to "+names(loc.places), locEv)
		r.restrict = []string{names(loc.places)}
		return r
	}
	return decided(Uncertain, 0.4, BasisNoSignal, fmt.Sprintf("an office in %s and the text does not say whether the job is remote", names(loc.places)), locEv)
}

func allWeak(fs []finding) bool {
	for _, f := range fs {
		if !f.weak {
			return false
		}
	}
	return true
}
