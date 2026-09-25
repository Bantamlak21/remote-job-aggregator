package filtering

import (
	"html"
	"regexp"
	"strings"
)

var (
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
	blockRe = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|h[1-6]|tr|table|section)\b[^>]*>`)
	spaceRe = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
)

var acronymRe = regexp.MustCompile(`(?i)\bu\.s\.a\.?|\bu\.s\.?(?:\b|$)|\bu\.k\.?|\bu\.a\.e\.?`)

// normalizeAcronyms writes "U.S.A." and "U.S." as "USA" and "US" (and U.K., U.A.E.):
// the periods would otherwise end the sentence and cut "the U.S." to "the u".
func normalizeAcronyms(s string) string {
	return acronymRe.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ToUpper(strings.ReplaceAll(m, ".", ""))
	})
}

// plainText turns an HTML or plain description into plain text: tags removed
// (block tags become line breaks), entities decoded, spaces collapsed.
func plainText(s string) string {
	s = normalizeAcronyms(s)
	if strings.ContainsAny(s, "<&") {
		s = blockRe.ReplaceAllString(s, "\n")
		s = tagRe.ReplaceAllString(s, " ")
		s = html.UnescapeString(s)
	}
	s = spaceRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

var sentenceEnd = regexp.MustCompile(`[.!?;]\s+|\n`)

var danglingRe = regexp.MustCompile(`(?i)\b(?:in|within|from|of|to|the|at|and|or|following|located|based|residing|resident|reside|live|living|one of|except|excluding)$`)

// sentences splits plain text into sentences (and lines), keeping each short
// enough to quote as evidence. A line that ends mid-phrase ("located in") is
// joined to the next.
func sentences(s string) []string {
	parts := splitSentences(s)
	var out []string
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		for i+1 < len(parts) && danglingRe.MatchString(p) {
			i++
			p += " " + parts[i]
		}
		out = append(out, p)
	}
	return out
}

func splitSentences(s string) []string {
	var out []string
	last := 0
	for _, m := range sentenceEnd.FindAllStringIndex(s, -1) {
		if seg := strings.TrimSpace(s[last:m[0]]); seg != "" {
			out = append(out, seg)
		}
		last = m[1]
	}
	if seg := strings.TrimSpace(s[last:]); seg != "" {
		out = append(out, seg)
	}
	return out
}

// quote shortens evidence text.
func quote(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "..."
	}
	return s
}
