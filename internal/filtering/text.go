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

// plainText turns an HTML or plain description into plain text: tags removed
// (block tags become line breaks), entities decoded, spaces collapsed.
func plainText(s string) string {
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

// sentences splits plain text into sentences (and lines), keeping each short
// enough to quote as evidence.
func sentences(s string) []string {
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
