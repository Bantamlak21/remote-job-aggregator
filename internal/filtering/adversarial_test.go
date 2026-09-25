package filtering

import (
	"strings"
	"testing"
	"time"
)

func nowMillis() int64 { return time.Now().UnixMilli() }

// Postings phrased to look open that are not, found by an independent
// reviewer's probe of the rules (each row was a false "eligible" at 0.8-0.9
// confidence before it was fixed), plus the shapes that keep the rules
// honest about conflict. The costly error is calling a closed job eligible, so
// every row here says what it must NOT be, and most say exactly what it is.

func worldwide(desc string) Input {
	return Input{Source: "himalayas", Title: "Backend Engineer", Location: "Worldwide", RemoteType: "remote", Description: desc}
}

func remoteATS(desc string) Input {
	return Input{Source: "greenhouse", Title: "Backend Engineer", Location: "Remote", RemoteType: "remote", Description: desc}
}

func TestAdversarial_ClosedJobsAreNeverEligible(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want Status // Ineligible unless a conflict is the honest answer
	}{
		{"the U.S.A. with periods", worldwide("Candidates must be based in the U.S.A."), Ineligible},
		{"the U.S. with periods", worldwide("Candidates must be based in the U.S. and work regular hours."), Ineligible},
		{"you must reside in the U.S.", worldwide("This role requires that you reside in the U.S."), Ineligible},
		{"a list after a colon", Input{Source: "greenhouse", Title: "AE", Location: "Remote - EMEA", RemoteType: "remote",
			Description: "Candidates must be based in one of the following countries: Germany, France, Spain."}, Ineligible},
		{"all countries except Ethiopia", worldwide("Open to all countries except Ethiopia."), Ineligible},
		{"worldwide, except Africa", remoteATS("We hire worldwide, except Africa."), Ineligible},
		{"Ethiopia, Nigeria and Kenya are not eligible", worldwide("Candidates from Ethiopia, Nigeria and Kenya are not eligible."), Ineligible},
		{"Sub-Saharan Africa will not be considered", worldwide("Applicants from Sub-Saharan Africa will not be considered."), Ineligible},
		{"US residents only", worldwide("US residents only."), Ineligible},
		{"only candidates residing in", worldwide("Only candidates residing in the United States will be considered."), Ineligible},
		{"only apply if you live in", worldwide("Please only apply if you live in Canada."), Ineligible},
		{"requirements: located in", remoteATS("Requirements: Located in Brazil"), Ineligible},
		{"must permanently reside", worldwide("You must permanently reside in Ireland or the UK."), Ineligible},
		{"a valid UK work permit", remoteATS("You need a valid UK work permit."), Ineligible},
		{"US citizens only", Input{Source: "workingnomads", Title: "Engineer", Location: "UTC+3", RemoteType: "remote", Description: "US citizens only."}, Ineligible},
		{"a sentence broken across lines", worldwide("Candidates must be located in\nthe United States."), Ineligible},
		{"US Remote in the title", Input{Source: "himalayas", Title: "US Remote Backend Engineer", Location: "Worldwide", RemoteType: "remote", Description: "x"}, Ineligible},
		{"duty station elsewhere in the Ethiopian market", Input{Source: "ethiojobs", Title: "Officer", Location: "", Market: "ethiopia", RemoteType: "unknown",
			Description: "Duty station: Nairobi, Kenya"}, Ineligible},
		// Round 2: nearby wordings of the same requirements.
		{"must live in (the gate once missed 'live')", worldwide("You must live in the United States."), Ineligible},
		{"must currently live in", worldwide("You must currently live in Canada."), Ineligible},
		{"with the exception of", worldwide("Open globally, with the exception of Ethiopia."), Ineligible},
		{"excluded countries", worldwide("Excluded countries: Ethiopia, Sudan."), Ineligible},
		{"excl.", worldwide("Open globally (excl. Africa)."), Ineligible},
		{"domiciled in", worldwide("You must be domiciled in the UK."), Ineligible},
		{"physically present in", worldwide("Employees must be physically present in the US."), Ineligible},
		{"eligible locations", worldwide("Eligible locations: United States, Canada"), Ineligible},
		{"supported countries", worldwide("Supported countries: Germany, France"), Ineligible},
		{"only employ people where we have an entity", worldwide("We can only employ people in countries where we have an entity (UK, Germany, Poland)."), Ineligible},
		{"we only hire in", Input{Source: "greenhouse", Title: "Engineer", Location: "EMEA", RemoteType: "remote", Description: "We only hire in the UK and Germany."}, Ineligible},
		{"req: US-based", remoteATS("Req: US-based"), Ineligible},
		{"U. S. with a space", worldwide("U. S. residents only."), Ineligible},
		{"EU citizens on an EMEA tag", Input{Source: "greenhouse", Title: "Engineer", Location: "EMEA", RemoteType: "remote", Description: "You must be an EU citizen."}, Ineligible},
		{"a region listed but the job is in an office", Input{Source: "greenhouse", Title: "Engineer", Location: "EMEA", RemoteType: "unknown",
			Description: "You will work 2 days per week on-site in Dublin."}, Ineligible},
		{"a time zone near but outside UTC+3", board("workingnomads", "UTC-3 to UTC+1"), Uncertain},
		// Where the text disagrees with itself the answer is a conflict, never eligible.
		{"Worldwide but Location: USA (remote)", worldwide("Location: USA (remote)"), Uncertain},
		{"Worldwide but LATAM-based engineers", worldwide("We are hiring LATAM-based engineers."), Uncertain},
		{"Worldwide and a US-based benefits line", worldwide("The benefits listed apply to US-based candidates."), Uncertain},
		{"explicit permission and an explicit requirement", worldwide("Candidates may be based in any geography. You must be located in Germany."), Uncertain},
		{"a location that restricts, a description that hires in Africa", Input{Source: "workingnomads", Title: "Engineer", Location: "Europe", RemoteType: "remote",
			Description: "We hire developers from Africa and Europe."}, Uncertain},
		{"a title territory against a permissive tag", Input{Source: "greenhouse", Title: "Channel Sales Executive, UKI", Location: "Home based - EMEA", RemoteType: "unknown", Description: "x"}, Uncertain},
	}
	c := NewClassifier()
	for _, tc := range cases {
		v := c.Classify(tc.in)
		if v.Status == Eligible {
			t.Errorf("%s: called ELIGIBLE (%s, %v)", tc.name, v.Basis, v.Reasons)
			continue
		}
		if v.Status != tc.want {
			t.Errorf("%s: status %s (%s; %v), want %s", tc.name, v.Status, v.Basis, v.Reasons, tc.want)
		}
	}
}

// Shapes that must stay eligible: the fixes above must not close real jobs.
func TestAdversarial_OpenJobsStayEligible(t *testing.T) {
	c := NewClassifier()
	for name, in := range map[string]Input{
		"E-Verify boilerplate does not conflict with Global Remote": Input{Source: "lever", Title: "Engineer", Location: "Global Remote", RemoteType: "remote",
			Description: "For US applicants: we participate in the federal E-Verify program, which confirms employment authorization of newly hired US based employees."},
		"no need to be based in the US":            worldwide("No need to be based in the US, we hire everywhere."),
		"residents of the EU are not eligible":     worldwide("Residents of the EU are not eligible."),
		"except residents of the US":               worldwide("We welcome applications from candidates worldwide, except residents of the United States."),
		"anywhere in the world":                    Input{Source: "greenhouse", Title: "Engineer", Location: "Remote", RemoteType: "remote", Description: "Candidates may be based anywhere in the world."},
		"a U.S. mention that is not a requirement": worldwide("Our company was founded in the U.S. in 2010 and we hire worldwide."),
		"Remote, ET is Eastern Time, not Ethiopia": Input{Source: "greenhouse", Title: "Engineer", Location: "Remote (ET)", RemoteType: "remote", Description: "x"},
	} {
		v := c.Classify(in)
		want := Eligible
		if strings.Contains(name, "Eastern Time") {
			want = Ineligible
		}
		if v.Status != want {
			t.Errorf("%s: status %s (%s; %v), want %s", name, v.Status, v.Basis, v.Reasons, want)
		}
	}
}

func TestClassify_CapsTheInputsSoAHugePostingCannotStallIt(t *testing.T) {
	c := NewClassifier()
	huge := strings.Repeat("Berlin, ", 200_000) // 1.6 MB
	for name, in := range map[string]Input{
		"title":       {Title: huge, Location: "Remote", RemoteType: "remote"},
		"title words": {Title: strings.Repeat("ab cd ", 100_000), Location: "Remote", RemoteType: "remote"},
		"title tail":  {Title: strings.Repeat("a, ", 200_000), Location: "Remote", RemoteType: "remote"},
		"location":    {Title: "x", Location: huge, RemoteType: "remote"},
	} {
		start := nowMillis()
		c.Classify(in)
		if took := nowMillis() - start; took > 1500 {
			t.Errorf("%s: %d ms for a huge input, want the cap to keep it fast", name, took)
		}
	}
	// A description far past the cap costs about what one at the cap costs (a
	// ratio, so it holds under -race too).
	unit := "Remote in Germany. "
	at := Input{Title: "x", Location: "Remote", RemoteType: "remote", Description: strings.Repeat(unit, maxDescriptionBytes/len(unit))}
	over := Input{Title: "x", Location: "Remote", RemoteType: "remote", Description: strings.Repeat(unit, 20*maxDescriptionBytes/len(unit))}
	t0 := nowMillis()
	c.Classify(at)
	atMs := nowMillis() - t0
	t0 = nowMillis()
	c.Classify(over)
	overMs := nowMillis() - t0
	if overMs > 3*atMs+300 {
		t.Errorf("a %d MB description took %d ms, a capped one %d ms: the cap does not bound the work", len(over.Description)>>20, overMs, atMs)
	}
	if got := capText("héllo", 2); got != "h" { // "é" is two bytes: cut at a character boundary
		t.Errorf("capText cut inside a character: %q", got)
	}
	if got := capText("abc", 10); got != "abc" {
		t.Errorf("capText(abc) = %q", got)
	}
}

// The sentence splitter is linear (a 200 KB posting whose lines all end in "located in" once took
// minutes), never joins across a full stop, and the acronym normalizer keeps what follows.
func TestSentences_LinearAndOnlyJoinAcrossLineBreaks(t *testing.T) {
	line := "Candidates must be located in\n"
	small := strings.Repeat(line, 2000)
	big := strings.Repeat(line, 8000)
	t0 := nowMillis()
	sentences(small)
	smallMs := nowMillis() - t0
	t0 = nowMillis()
	sentences(big)
	bigMs := nowMillis() - t0
	if bigMs > 12*smallMs+300 { // 4x the input: linear is about 4x, quadratic is 16x
		t.Errorf("4x the lines took %d ms against %d ms: not linear", bigMs, smallMs)
	}
	if got := sentences("Candidates must be located in\nthe United States."); len(got) != 1 {
		t.Errorf("a phrase broken across lines was not joined: %q", got)
	}
	if got := sentences("you'll fit right in. Available Location: Dubai, UAE"); len(got) != 2 {
		t.Errorf("sentences joined across a full stop: %q", got)
	}
	for in, want := range map[string]string{"U.S.-based": "US-based", "the U.S. and more": "the US and more", "U.S.A.": "USA", "U.K. and": "UK and", "U. S. only": "US only"} {
		if got := normalizeAcronyms(in); got != want {
			t.Errorf("normalizeAcronyms(%q) = %q, want %q", in, got, want)
		}
	}
}
