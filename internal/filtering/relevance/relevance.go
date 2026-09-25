// Package relevance says what kind of role a job title is (its family) and
// whether that kind is one the job seeker wants. The families, the phrases that
// put a title in one and which families count as relevant are data
// (profile.json, replaceable with a file), not code: a profile for a designer
// or a nurse is a different JSON, not a different program.
package relevance

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

//go:embed profile.json
var defaultProfile []byte

// Rule puts a title in a family when any phrase of Any appears in it as whole
// words and no phrase of Not does. Rules are tried in order and the first that
// matches decides, so a specific family ("sales engineer") is listed before a
// general one ("engineer").
type Rule struct {
	Family string   `json:"family"`
	Any    []string `json:"any"`
	Not    []string `json:"not,omitempty"`

	any, not [][]string // folded word lists
}

// Profile is a set of rules and the families that count as relevant.
type Profile struct {
	// Default is the family of a title no rule matches.
	Default string `json:"default"`
	// Relevant lists the families that are relevant to the job seeker.
	Relevant []string `json:"relevant"`
	Rules    []Rule   `json:"rules"`

	relevant map[string]bool
}

// Result is the classification of one title.
type Result struct {
	Family   string
	Relevant bool
	// Matched is the phrase that decided the family ("" for the default).
	Matched string
}

// Default returns the built-in profile: software, data, DevOps and security
// roles are relevant.
func Default() *Profile {
	p, err := Parse(defaultProfile)
	if err != nil {
		panic("relevance: built-in profile is invalid: " + err.Error())
	}
	return p
}

// Load reads a profile from a JSON file.
func Load(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("relevance: reading profile: %w", err)
	}
	return Parse(b)
}

// Parse reads a profile from JSON and validates it.
func Parse(b []byte) (*Profile, error) {
	var p Profile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("relevance: decoding profile: %w", err)
	}
	if strings.TrimSpace(p.Default) == "" {
		return nil, fmt.Errorf("relevance: profile has no default family")
	}
	p.relevant = map[string]bool{}
	for _, f := range p.Relevant {
		p.relevant[f] = true
	}
	for i := range p.Rules {
		r := &p.Rules[i]
		if strings.TrimSpace(r.Family) == "" || len(r.Any) == 0 {
			return nil, fmt.Errorf("relevance: rule %d needs a family and at least one phrase", i)
		}
		for _, ph := range r.Any {
			w := words(ph)
			if len(w) == 0 {
				return nil, fmt.Errorf("relevance: rule %d (%s) has an empty phrase", i, r.Family)
			}
			r.any = append(r.any, w)
		}
		for _, ph := range r.Not {
			if w := words(ph); len(w) > 0 {
				r.not = append(r.not, w)
			}
		}
	}
	return &p, nil
}

// Classify puts a job title in a family.
func (p *Profile) Classify(title string) Result {
	tw := words(title)
	for _, r := range p.Rules {
		if hit := firstPhrase(tw, r.any); hit != "" && firstPhrase(tw, r.not) == "" {
			return Result{Family: r.Family, Relevant: p.relevant[r.Family], Matched: hit}
		}
	}
	return Result{Family: p.Default, Relevant: p.relevant[p.Default]}
}

// firstPhrase returns the first phrase found in the words as a run of whole words.
func firstPhrase(tw []string, phrases [][]string) string {
	for _, ph := range phrases {
		for i := 0; i+len(ph) <= len(tw); i++ {
			ok := true
			for j := range ph {
				if tw[i+j] != ph[j] {
					ok = false
					break
				}
			}
			if ok {
				return strings.Join(ph, " ")
			}
		}
	}
	return ""
}

// words folds a string (lower case, no accents) and splits it into words.
// "+" and "#" stay inside a word ("c++", "c#"); everything else that is not a
// letter or digit separates words.
func words(s string) []string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if out, _, err := transform.String(t, s); err == nil {
		s = out
	}
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '#'
	})
}
