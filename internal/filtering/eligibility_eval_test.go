package filtering

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// The eval: real postings (testdata/dev.jsonl: 325 open jobs sampled across
// every source, full descriptions) with gold labels made by an independent
// labeler who never saw this code. It measures classification quality, which
// a handful of unit tests cannot: precision of "eligible" (a wrong "eligible"
// wastes a job seeker's application) and of "ineligible", recall of
// "eligible", and how often the rules give up ("uncertain").
//
// The bars below are the frozen rubric's (see docs/eligibility.md). A second,
// held-out set the builder never saw is scored separately by the reviewer.

type devCase struct {
	ID          int64  `json:"id"`
	Source      string `json:"source"`
	Company     string `json:"company"`
	Title       string `json:"title"`
	Location    string `json:"location"`
	RemoteType  string `json:"remote_type"`
	Market      string `json:"market"`
	Description string `json:"description"`
	Gold        struct {
		Eligibility string `json:"eligibility"`
		Confidence  string `json:"confidence"`
		RoleFamily  string `json:"role_family"`
		Relevant    bool   `json:"relevant"`
		Note        string `json:"note"`
	} `json:"gold"`
}

func loadDev(t testing.TB) []devCase {
	t.Helper()
	f, err := os.Open("testdata/dev.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []devCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var c devCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (c devCase) input() Input {
	return Input{Source: c.Source, Title: c.Title, Location: c.Location, RemoteType: c.RemoteType, Description: c.Description, Market: c.Market}
}

type confusion map[[2]string]int // {gold, predicted}

func (m confusion) count(gold, pred string) int { return m[[2]string{gold, pred}] }

func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}

func TestDevGold_Eligibility(t *testing.T) {
	cases := loadDev(t)
	c := NewClassifier()
	conf := confusion{}
	var misses []string
	var eligibleWrong, ineligibleAsEligible, ineligibleHigh, eligibleAsIneligible int
	uncertainOnHigh, highDecided := 0, 0
	uncertainAsEligible, goldUncertain := 0, 0
	n := 0
	for _, dc := range cases {
		v := c.Classify(dc.input())
		n++
		g, p := dc.Gold.Eligibility, string(v.Status)
		conf[[2]string{g, p}]++
		if g != p {
			misses = append(misses, fmt.Sprintf("%d\t%s\tgold=%s(%s)\tpred=%s(%.2f,%s)\t%s | %s | %s\t%s", dc.ID, dc.Source, g, dc.Gold.Confidence, p, v.Confidence, v.Basis,
				dc.Title, dc.Location, dc.RemoteType, strings.Join(reasons(v), " / ")))
		}
		hm := dc.Gold.Confidence == "high" || dc.Gold.Confidence == "medium"
		if g == "ineligible" && p == "eligible" && hm {
			ineligibleAsEligible++
			if dc.Gold.Confidence == "high" {
				ineligibleHigh++
			}
		}
		if g == "eligible" && p == "ineligible" && hm {
			eligibleAsIneligible++
		}
		if dc.Gold.Confidence == "high" && g != "uncertain" {
			highDecided++
			if p == "uncertain" {
				uncertainOnHigh++
			}
		}
		if g == "uncertain" {
			goldUncertain++
			if p == "eligible" {
				uncertainAsEligible++
			}
		}
		_ = eligibleWrong
	}

	precision := func(class string) float64 {
		tp := conf.count(class, class)
		pred := 0
		for _, g := range []string{"eligible", "ineligible", "uncertain"} {
			pred += conf.count(g, class)
		}
		return ratio(tp, pred)
	}
	recall := func(class string) float64 {
		tot := 0
		for _, p := range []string{"eligible", "ineligible", "uncertain"} {
			tot += conf.count(class, p)
		}
		return ratio(conf.count(class, class), tot)
	}
	correct := conf.count("eligible", "eligible") + conf.count("ineligible", "ineligible") + conf.count("uncertain", "uncertain")
	uncertainShare := ratio(conf.count("eligible", "uncertain")+conf.count("ineligible", "uncertain")+conf.count("uncertain", "uncertain"), n)

	t.Logf("dev gold: %d cases | eligible P %.3f R %.3f | ineligible P %.3f R %.3f | uncertain share %.3f | accuracy %.3f",
		n, precision("eligible"), recall("eligible"), precision("ineligible"), recall("ineligible"), uncertainShare, ratio(correct, n))
	t.Logf("ineligible->eligible (high+medium gold): %d (high: %d) | eligible->ineligible: %d | uncertain on high-confidence decided rows: %d of %d | gold-uncertain called eligible: %d of %d",
		ineligibleAsEligible, ineligibleHigh, eligibleAsIneligible, uncertainOnHigh, highDecided, uncertainAsEligible, goldUncertain)

	if path := os.Getenv("PHASE4_DUMP"); path != "" {
		sort.Strings(misses)
		_ = os.WriteFile(path, []byte(strings.Join(misses, "\n")+"\n"), 0o644)
	}

	// The rubric's bars.
	check := func(name string, got, min float64) {
		if got < min {
			t.Errorf("%s = %.3f, want >= %.2f", name, got, min)
		}
	}
	check("eligible precision", precision("eligible"), 0.90)
	check("eligible recall", recall("eligible"), 0.85)
	check("ineligible precision", precision("ineligible"), 0.95)
	check("ineligible recall", recall("ineligible"), 0.85)
	check("accuracy", ratio(correct, n), 0.85)
	if uncertainShare > 0.25 {
		t.Errorf("uncertain share %.3f, want <= 0.25", uncertainShare)
	}
	if ineligibleAsEligible > 1 || ineligibleHigh > 0 {
		t.Errorf("ineligible predicted eligible: %d (high-confidence: %d), want at most 1 and 0", ineligibleAsEligible, ineligibleHigh)
	}
	if eligibleAsIneligible > 1 {
		t.Errorf("eligible predicted ineligible: %d, want at most 1", eligibleAsIneligible)
	}
	if highDecided > 0 && float64(uncertainOnHigh)/float64(highDecided) > 0.10 {
		t.Errorf("uncertain on %d of %d high-confidence decided rows, want <= 10%%", uncertainOnHigh, highDecided)
	}
	if goldUncertain > 0 && float64(uncertainAsEligible)/float64(goldUncertain) > 0.10 {
		t.Errorf("gold-uncertain called eligible: %d of %d, want <= 10%%", uncertainAsEligible, goldUncertain)
	}
}

func reasons(v Verdict) []string {
	out := append([]string{}, v.Reasons...)
	for _, e := range v.Evidence {
		out = append(out, e.Field+": "+quote(e.Text))
	}
	return out
}

// Every Ethiopia-located job in the dev set must be eligible, and every
// decided verdict carries a reason and evidence that appears in the job text.
func TestDevGold_EthiopiaLocalAndExplainable(t *testing.T) {
	c := NewClassifier()
	checked, ok := 0, 0
	for _, dc := range loadDev(t) {
		v := c.Classify(dc.input())
		if strings.Contains(strings.ToLower(dc.Location), "ethiopia") || strings.Contains(strings.ToLower(dc.Location), "addis") {
			if v.Status != Eligible {
				t.Errorf("job %d in %q: %s, want eligible", dc.ID, dc.Location, v.Status)
			}
		}
		if v.Status == Uncertain {
			continue
		}
		if len(v.Reasons) == 0 || v.Basis == "" {
			t.Errorf("job %d: decided %s without a reason or basis", dc.ID, v.Status)
		}
		for _, e := range v.Evidence {
			checked++
			if evidenceInJob(e, dc) {
				ok++
			} else {
				t.Errorf("job %d: evidence %q not found in the %s", dc.ID, e.Text, e.Field)
			}
		}
	}
	t.Logf("evidence snippets found in the job text: %d of %d", ok, checked)
}

func evidenceInJob(e Evidence, dc devCase) bool {
	text := strings.TrimSuffix(e.Text, "...")
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	switch e.Field {
	case "location":
		return strings.Contains(norm(dc.Location), norm(text))
	case "title":
		return strings.Contains(norm(dc.Title), norm(text))
	case "market":
		return true
	}
	return strings.Contains(norm(plainText(dc.Description)), norm(text)) || strings.Contains(norm(plainText(dc.Description)), strings.TrimPrefix(norm(text), "headquarters: "))
}

// The classifier is a pure function.
func TestClassify_IsDeterministic(t *testing.T) {
	c := NewClassifier()
	for _, dc := range loadDev(t)[:60] {
		a, b := c.Classify(dc.input()), c.Classify(dc.input())
		aj, _ := json.Marshal(a)
		bj, _ := json.Marshal(b)
		if string(aj) != string(bj) {
			t.Fatalf("job %d: two runs differ:\n%s\n%s", dc.ID, aj, bj)
		}
	}
}
