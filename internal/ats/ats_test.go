package ats

import (
	"strings"
	"testing"
)

func TestHTMLToText_StripsTagsKeepsText(t *testing.T) {
	got := HTMLToText(`<p>Hello <strong>world</strong></p>`)
	if got != "Hello world" {
		t.Errorf("got %q, want %q", got, "Hello world")
	}
}

func TestHTMLToText_DecodesEntities(t *testing.T) {
	got := HTMLToText(`<p>Design &amp; build &mdash; you&#39;re invited.</p>`)
	if strings.Contains(got, "&amp;") || strings.Contains(got, "&#39;") || strings.Contains(got, "&mdash;") {
		t.Errorf("got %q, want decoded entities, not raw escape sequences", got)
	}
	if !strings.Contains(got, "&") || !strings.Contains(got, "you're") {
		t.Errorf("got %q, want a literal & and \"you're\"", got)
	}
}

func TestHTMLToText_BlockElementsBecomeNewlines(t *testing.T) {
	got := HTMLToText(`<div><p>First paragraph.</p><p>Second paragraph.</p></div><ul><li>One</li><li>Two</li></ul>`)
	lines := strings.Split(got, "\n")
	if len(lines) < 4 {
		t.Fatalf("got %q (%d lines), want at least 4 separate lines for 2 paragraphs + 2 list items", got, len(lines))
	}
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "First paragraph.") || !strings.Contains(joined, "Second paragraph.") {
		t.Errorf("got %q, missing expected paragraph text", got)
	}
	if !strings.Contains(joined, "One") || !strings.Contains(joined, "Two") {
		t.Errorf("got %q, missing expected list items", got)
	}
}

func TestHTMLToText_CollapsesInsignificantWhitespace(t *testing.T) {
	got := HTMLToText("<p>\n\t\tLots   of\n\t\twhitespace\n\t</p>")
	if got != "Lots of whitespace" {
		t.Errorf("got %q, want %q", got, "Lots of whitespace")
	}
}

func TestHTMLToText_EmptyInputReturnsEmptyString(t *testing.T) {
	if got := HTMLToText(""); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
	if got := HTMLToText("   \n\t  "); got != "" {
		t.Errorf("got %q, want empty string for whitespace-only input", got)
	}
}

// Real content byte-for-byte from a live Greenhouse job description
// (GitLab's board, fetched 2026-09-23) — not synthesized, so this test
// catches anything synthetic examples above would miss about Greenhouse's
// actual entity-encoding and markup style.
func TestHTMLToText_RealGreenhouseContentFixture(t *testing.T) {
	const raw = `&lt;div class=&quot;content-intro&quot;&gt;&lt;p&gt;GitLab is the intelligent orchestration platform for DevSecOps. GitLab enables organizations to increase developer productivity, improve operational efficiency, reduce security and compliance risk, and accelerate digital transformation. More than 50 million registered users and more than 50% of the Fortune 100* trust GitLab to ship better, more secure software faster.&lt;/p&gt;
&lt;p&gt;The same principles built into our products are reflected in how our team works: we embrace AI as a core productivity multiplier.&lt;/p&gt;`

	got := HTMLToText(raw)

	// The raw fixture is itself HTML-entity-escaped (Greenhouse's JSON
	// value contains literal "&lt;div..." text, not a real "<div>" tag) --
	// this is what the live API actually returns, confirmed by curl
	// during Phase 3 research. A single HTMLToText pass must fully
	// resolve it to plain, tag-free text: no lingering "&lt;", no "<div>"
	// literal tag syntax visible, no HTML tags parsed as markup.
	if strings.Contains(got, "&lt;") || strings.Contains(got, "&gt;") || strings.Contains(got, "&quot;") {
		t.Errorf("got %q, still contains raw entity escape sequences", got)
	}
	if strings.Contains(got, "<div") || strings.Contains(got, "<p>") {
		t.Errorf("got %q, still contains literal HTML tag syntax", got)
	}
	if !strings.Contains(got, "GitLab is the intelligent orchestration platform for DevSecOps.") {
		t.Errorf("got %q, missing expected real text", got)
	}
	if !strings.Contains(got, "50% of the Fortune 100") {
		t.Errorf("got %q, missing expected real text with a literal %%", got)
	}
}

// Regression tests for defects found by adversarial review.

func TestHTMLToText_InlineTagsAddNoSpuriousSpaces(t *testing.T) {
	got := HTMLToText(`<p><strong>Note</strong>: Java<sup>TM</sup> is <em>un</em>believable, see <a href="x">link</a>.</p>`)
	want := "Note: JavaTM is unbelievable, see link."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHTMLToText_DropsScriptAndStyleBodies(t *testing.T) {
	got := HTMLToText(`<p>Hello</p><script>alert(1)</script><style>.a{color:red}</style><p>world</p>`)
	if strings.Contains(got, "alert") || strings.Contains(got, "color") {
		t.Errorf("got %q, script/style body leaked into text", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "world") {
		t.Errorf("got %q, lost real text", got)
	}
}

func TestHTMLToText_BrVariantsAllBreakLines(t *testing.T) {
	for _, in := range []string{"a<br>b", "a<br/>b", "a<br />b"} {
		if got := HTMLToText(in); got != "a\nb" {
			t.Errorf("HTMLToText(%q) = %q, want %q", in, got, "a\nb")
		}
	}
}

func TestHTMLToText_NestedListsStayCompact(t *testing.T) {
	got := HTMLToText(`<ul><li>a<ul><li>b</li></ul></li><li>c</li></ul>`)
	if strings.Contains(got, "\n\n") {
		t.Errorf("got %q, want no blank lines inside a list", got)
	}
	if got != "a\nb\nc" {
		t.Errorf("got %q, want %q", got, "a\nb\nc")
	}
}

// A description containing genuine markup plus text that legitimately
// contains the entity for "<b>" must keep that text, not have it
// pre-unescaped into a fake tag and swallowed.
func TestHTMLToText_DoesNotPreUnescapeRealMarkup(t *testing.T) {
	got := HTMLToText(`<p>Salary &lt;b&gt; text</p>`)
	if !strings.Contains(got, "<b>") {
		t.Errorf("got %q, want the literal text \"<b>\" preserved", got)
	}
}

func TestCleanText_StripsNULInvalidUTF8AndTrims(t *testing.T) {
	if got := CleanText("  Eng\x00ineer\xff\xfe \t"); got != "Engineer" {
		t.Errorf("CleanText = %q, want %q", got, "Engineer")
	}
	if got := HTMLToText("<p>x\x00y</p>"); strings.Contains(got, "\x00") {
		t.Errorf("HTMLToText output still contains a NUL: %q", got)
	}
}
