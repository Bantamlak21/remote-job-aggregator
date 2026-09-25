// Package filtering decides, deterministically, two things about a job
// posting: whether a candidate living in a target country (Ethiopia by
// default) could take it without relocating (geographic eligibility), and
// whether it is a role the job seeker cares about (relevance).
//
// Both are rules and lookup tables, not a model: the same posting always gets
// the same verdict, a verdict says why (reasons and the text that decided it),
// and every rule is covered by a test and scored by an eval on real postings
// (see eligibility_eval_test.go). Places come from package geo.
package filtering

// Status is the eligibility verdict.
type Status string

const (
	// Eligible: the posting says or clearly implies a candidate in the target
	// country may take the job.
	Eligible Status = "eligible"
	// Ineligible: the job is on-site or hybrid outside the target country, or
	// remote but restricted to places that do not include it.
	Ineligible Status = "ineligible"
	// Uncertain: the text does not decide it.
	Uncertain Status = "uncertain"
)

// Valid reports whether s is one of the three statuses.
func (s Status) Valid() bool { return s == Eligible || s == Ineligible || s == Uncertain }

// Input is what the classifier reads about one job.
type Input struct {
	// Source is the ats_provider the job came from ("greenhouse", "himalayas",
	// ...). Job boards' location is the places applicants may come from; an
	// ATS's location is where the job is.
	Source string
	Title  string
	// Location is the raw location text.
	Location string
	// RemoteType is what ingestion knows: "remote", "hybrid", "onsite" or
	// "unknown" ("" is unknown).
	RemoteType string
	// Description is the job description, plain text or HTML.
	Description string
	// Market is the list the job is in ("ethiopia" or "worldwide", "" if
	// unknown). A job in the Ethiopian list with no location of its own is
	// taken to be in Ethiopia.
	Market string
}

// Evidence is a piece of text a verdict rests on.
type Evidence struct {
	Field string `json:"field"` // "location", "description", "title", "remote_type"
	Text  string `json:"text"`
}

// Verdict is the result of classifying one job.
type Verdict struct {
	Status Status
	// Confidence is 0..1: how sure the rules are (1.0 for an explicit
	// statement, less for an inference, low for a guess between conflicting
	// signals).
	Confidence float64
	// Reasons say in a sentence each why the status was chosen, most
	// important first.
	Reasons []string
	// Evidence is the text the reasons rest on.
	Evidence []Evidence
	// Locations are the places found in the posting, as written in English.
	Locations []string
	// Restrictions are the limits found ("United States only", "UTC-8 to
	// UTC-5 time zones", "not in United States").
	Restrictions []string
	// Basis is the kind of thing the verdict rests on (see the Basis constants).
	Basis Basis
	// HoursConstraint is set for an eligible job that asks for working hours
	// far from Ethiopia's ("works US hours"): the phrase that says so.
	HoursConstraint string
}

// Version identifies the rules: bump it when a change can alter a verdict, so
// stored verdicts are recomputed.
const Version = 1
