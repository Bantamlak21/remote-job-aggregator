// Package companymatch decides whether two spellings of an employer's name
// refer to the same company. Job sites write names differently from the
// companies themselves ("Ethswitch S.C." vs "EthSwitch", "Kifiya Financial
// Technologies" vs "Kifiya Financial Technology PLC"), and a wrong match
// either attributes a stranger's job to a priority company or drops a real
// one, so every source that has to answer "is this employer that company?"
// answers it here, the same way.
package companymatch

import (
	"strings"
	"unicode"
)

// Slug lower-cases s and collapses every run of characters that are not
// ASCII letters or digits into a single dash, trimming leading and trailing
// dashes.
func Slug(s string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(s) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// corporateSuffixes are trailing name tokens that differ between how a
// company writes its own name and how a job site does ("Ethswitch S.C.",
// "Kifiya Financial Technology PLC", "Gebeya Inc.").
var corporateSuffixes = map[string]bool{"inc": true, "plc": true, "sc": true, "s": true, "c": true, "ltd": true,
	"llc": true, "pvt": true, "co": true, "company": true, "limited": true, "corp": true, "corporation": true,
	"sa": true, "gmbh": true, "et": true, "ethiopia": true}

// Key reduces a company name to a comparison key: lower-case ASCII words
// joined by dashes, with trailing corporate-form words dropped (but never
// all of them). "Ethswitch S.C." and "EthSwitch" share a key; "Chaka
// Gebeya" and "Gebeya Inc." do not. The empty string is the key of a name
// with no usable characters and never matches anything.
func Key(name string) string {
	// Dots inside abbreviations are dropped first so "P.L.C", "S.C." and
	// "Inc." read the same as "PLC", "SC" and "Inc" (otherwise "P.L.C" would
	// slug to three one-letter tokens and never match "PLC").
	tokens := strings.Split(Slug(strings.ReplaceAll(name, ".", "")), "-")
	for len(tokens) > 1 && corporateSuffixes[tokens[len(tokens)-1]] {
		tokens = tokens[:len(tokens)-1]
	}
	return strings.Join(tokens, "-")
}

// Entry is one known company: its canonical name and the other names it
// posts under.
type Entry struct {
	Name    string
	Aliases []string
}

// Matcher resolves an employer name from a job site to a known company.
// Immutable after NewMatcher, safe for concurrent use.
type Matcher struct {
	byKey map[string]string // Key(name or alias) -> canonical name
}

// NewMatcher builds a Matcher over entries. If two entries claim the same
// key the first wins, and an entry's own canonical name always resolves to
// itself.
func NewMatcher(entries []Entry) *Matcher {
	m := &Matcher{byKey: make(map[string]string)}
	for _, e := range entries {
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			if k := Key(n); k != "" {
				if _, taken := m.byKey[k]; !taken {
					m.byKey[k] = e.Name
				}
			}
		}
	}
	return m
}

// Resolve returns the canonical name of the known company that employer
// names, and whether there is one. A nil Matcher matches nothing.
func (m *Matcher) Resolve(employer string) (canonical string, ok bool) {
	if m == nil {
		return "", false
	}
	k := Key(employer)
	if k == "" {
		return "", false
	}
	canonical, ok = m.byKey[k]
	return canonical, ok
}

// TitleKey is a comparison key for a job title: lower-cased, with every run
// of characters that are not letters, digits, '#' or '+' collapsed to one
// space. Unlike Slug it keeps non-ASCII letters (Amharic titles must not
// all collapse to "") and the characters that make "C# Developer",
// "C++ Developer" and "C Developer" three different jobs.
func TitleKey(title string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '#' || r == '+' {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSuffix(b.String(), " ")
}
