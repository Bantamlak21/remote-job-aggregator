package geo

import (
	"regexp"
	"strconv"
	"strings"
)

// Window is a range of UTC offsets, in hours, a posting accepts.
type Window struct {
	Lo, Hi float64
	Text   string // the words it was read from
}

// Contains reports whether the UTC offset (hours) is inside the window.
func (w Window) Contains(offset float64) bool { return offset >= w.Lo-1e-9 && offset <= w.Hi+1e-9 }

// EthiopiaOffset is East Africa Time (UTC+3), Ethiopia's only time zone, with
// no daylight saving.
const EthiopiaOffset = 3.0

// abbreviations are the zone abbreviations postings use, as UTC offsets in
// hours. An abbreviation of three letters or fewer must be written in capitals
// to count ("ET" is a zone, "et" is not). The ambiguous ones (IST, CST, BST)
// take the reading that is far the most common in remote job postings.
var abbreviations = map[string]float64{
	"pst": -8, "pdt": -7, "pt": -8, "mst": -7, "mdt": -6, "mt": -7, "cst": -6, "cdt": -5, "ct": -6, "est": -5, "edt": -4, "et": -5,
	"ast": -4, "nst": -3.5, "hst": -10, "akst": -9, "akdt": -8, "brt": -3, "art": -3, "clt": -4, "cot": -5, "pet": -5,
	"gmt": 0, "utc": 0, "wet": 0, "west": 1, "bst": 1, "cet": 1, "cest": 2, "eet": 2, "eest": 3, "msk": 3, "trt": 3,
	"eat": 3, "wat": 1, "cat": 2, "sast": 2, "ist": 5.5, "pkt": 5, "bdt": 6, "ict": 7, "wib": 7, "sgt": 8, "hkt": 8, "pht": 8,
	"awst": 8, "jst": 9, "kst": 9, "acst": 9.5, "aest": 10, "aedt": 11, "nzst": 12, "nzdt": 13,
}

// zoneNames are zones written out.
var zoneNames = []struct {
	name   string
	offset float64
}{
	{"central european time", 1}, {"central european summer time", 2}, {"eastern european time", 2}, {"western european time", 0},
	{"east africa time", 3}, {"east african time", 3}, {"west africa time", 1}, {"south africa standard time", 2},
	{"pacific time", -8}, {"pacific standard time", -8}, {"mountain time", -7}, {"mountain standard time", -7},
	{"central time", -6}, {"central standard time", -6}, {"eastern time", -5}, {"eastern standard time", -5}, {"eastern daylight time", -4},
	{"greenwich mean time", 0}, {"british summer time", 1}, {"india standard time", 5.5}, {"japan standard time", 9},
	{"moscow time", 3}, {"gulf standard time", 4}, {"singapore time", 8},
}

// zoneGroups are zone families ("US time zones") as a window.
var zoneGroups = []struct {
	re     *regexp.Regexp
	lo, hi float64
}{
	{regexp.MustCompile(`(?i)\b(?:us|usa|u\.s\.a?\.?|united states|north american?|american?)\s+(?:time ?zones?|hours|business hours)`), -10, -3},
	{regexp.MustCompile(`(?i)\b(?:latam|latin american?|south american?)\s+(?:time ?zones?|hours)`), -6, -3},
	{regexp.MustCompile(`(?i)\b(?:emea)\s+(?:time ?zones?|hours)`), 0, 4},
	{regexp.MustCompile(`(?i)\b(?:african?|africa)\s+(?:time ?zones?|hours)`), -1, 4},
	{regexp.MustCompile(`(?i)\b(?:european?|europe|eu)\s+(?:time ?zones?|hours|business hours)`), 0, 2},
	{regexp.MustCompile(`(?i)\b(?:apac|asia[- ]pacific|asian?)\s+(?:time ?zones?|hours)`), 5, 13},
	{regexp.MustCompile(`(?i)\b(?:uk|british)\s+(?:time ?zones?|hours)`), 0, 1},
}

var (
	offsetRe = regexp.MustCompile(`(?i)\b(?:utc|gmt)\s*([+\-\x{2212}])\s*(\d{1,2})(?::?(\d{2}))?`)
	rangeRe  = regexp.MustCompile(`(?i)(?:utc|gmt)\s*([+\-\x{2212}])\s*(\d{1,2})(?::?(\d{2}))?\s*(?:to|-|\x{2013}|\x{2014}|and|/)\s*(?:utc|gmt)?\s*([+\-\x{2212}])\s*(\d{1,2})(?::?(\d{2}))?`)
	// within/plus-minus N hours of a zone, written either side of it.
	pmRe      = regexp.MustCompile(`(?i)(?:\+\s*/?\s*-|\x{00b1}|plus or minus|plus/minus|within)\s*(\d{1,2}(?:\.\d)?)\s*(?:hours?|hrs?|h)\b`)
	abbrevRe  = regexp.MustCompile(`\b([A-Za-z]{2,4})\b`)
	hoursOfRe = regexp.MustCompile(`(?i)(\d{1,2}(?:\.\d)?)\s*(?:hours?|hrs?)\s+(?:of|from|to)`)
)

func signed(sign string, h, m string) float64 {
	hh, _ := strconv.Atoi(h)
	mm := 0
	if m != "" {
		mm, _ = strconv.Atoi(m)
	}
	v := float64(hh) + float64(mm)/60
	if sign == "-" || sign == "−" {
		v = -v
	}
	return v
}

// ParseTimezones returns the UTC-offset windows a text asks for: explicit
// offsets and ranges ("UTC+1 to UTC+3", "GMT-5"), zone abbreviations ("CET",
// "EST"), written-out zones ("Pacific Time"), and zone families ("US time
// zones", "EMEA time zones"). A "+/- 3 hours" or "within 3 hours of" next to a
// zone widens it. The text is not folded: "ET" and "et" differ.
func ParseTimezones(text string) []Window {
	var out []Window

	used := map[[2]int]bool{}
	for _, m := range rangeRe.FindAllStringSubmatchIndex(text, -1) {
		a := signed(text[m[2]:m[3]], text[m[4]:m[5]], sub(text, m, 3))
		b := signed(text[m[8]:m[9]], text[m[10]:m[11]], sub(text, m, 6))
		if a > b {
			a, b = b, a
		}
		out = append(out, Window{Lo: a, Hi: b, Text: text[m[0]:m[1]]})
		used[[2]int{m[0], m[1]}] = true
	}
	for _, m := range offsetRe.FindAllStringSubmatchIndex(text, -1) {
		if inside(used, m[0], m[1]) {
			continue
		}
		v := signed(text[m[2]:m[3]], text[m[4]:m[5]], sub(text, m, 3))
		w := widen(Window{Lo: v, Hi: v, Text: text[m[0]:m[1]]}, text, m[1])
		out = append(out, w)
	}

	lower := strings.ToLower(text)
	for _, z := range zoneNames {
		for from := 0; ; {
			i := strings.Index(lower[from:], z.name)
			if i < 0 {
				break
			}
			end := from + i + len(z.name)
			out = append(out, widen(Window{Lo: z.offset, Hi: z.offset, Text: text[from+i : end]}, text, end))
			from = end
		}
	}
	for _, g := range zoneGroups {
		for _, m := range g.re.FindAllStringIndex(text, -1) {
			out = append(out, Window{Lo: g.lo, Hi: g.hi, Text: text[m[0]:m[1]]})
		}
	}
	for _, m := range abbrevRe.FindAllStringSubmatchIndex(text, -1) {
		word := text[m[2]:m[3]]
		off, ok := abbreviations[strings.ToLower(word)]
		if !ok {
			continue
		}
		if word != strings.ToUpper(word) {
			continue // "West Palm Beach" and "eat" are words, not zones
		}
		if covered(out, text[m[0]:m[1]]) {
			continue
		}
		out = append(out, widen(Window{Lo: off, Hi: off, Text: word}, text, m[1]))
	}
	return out
}

func sub(text string, m []int, group int) string {
	if m[2*group] < 0 {
		return ""
	}
	return text[m[2*group]:m[2*group+1]]
}

func inside(used map[[2]int]bool, a, b int) bool {
	for k := range used {
		if a >= k[0] && b <= k[1] {
			return true
		}
	}
	return false
}

func covered(ws []Window, word string) bool {
	for _, w := range ws {
		if strings.Contains(strings.ToLower(w.Text), strings.ToLower(word)) && len(w.Text) > len(word) {
			return true
		}
	}
	return false
}

// widen applies a "+/- N hours" (or "within N hours of") written right after
// the zone, or "N hours of" right before it.
func widen(w Window, text string, end int) Window {
	tail := text[end:]
	if len(tail) > 40 {
		tail = tail[:40]
	}
	if m := pmRe.FindStringSubmatch(tail); m != nil && pmRe.FindStringIndex(tail)[0] <= 6 {
		n, _ := strconv.ParseFloat(m[1], 64)
		w.Lo -= n
		w.Hi += n
		return w
	}
	head := text[:end-len(w.Text)]
	if len(head) > 30 {
		head = head[len(head)-30:]
	}
	if m := hoursOfRe.FindStringSubmatch(head); m != nil {
		n, _ := strconv.ParseFloat(m[1], 64)
		w.Lo -= n
		w.Hi += n
	}
	return w
}
