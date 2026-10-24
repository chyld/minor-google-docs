package core

import (
	"regexp"
	"sort"
	"strings"
)

// HighlightHues are the 12 highlight colours; each also has a "-deep" variant.
// The editor (web/src/lib/editor/highlights.ts) keeps the same list; both are
// checked against testdata/highlights.json.
var HighlightHues = []string{"coral", "orange", "amber", "lime", "green", "mint", "teal", "sky", "blue", "violet", "magenta", "pink"}

// HighlightNames lists every valid colour name (24).
var HighlightNames = func() []string {
	var out []string
	for _, h := range HighlightHues {
		out = append(out, h, h+"-deep")
	}
	return out
}()

// HighlightStyleNames lists every valid highlight name (99): a style set of an optional
// colour, then "b" (bold), then "u" (underline), joined by "+". Same order as
// HL_STYLE_NAMES in web/src/lib/highlight.ts (checked against testdata/highlights.json).
var HighlightStyleNames = func() []string {
	var out []string
	for _, c := range HighlightNames {
		out = append(out, c, c+"+b", c+"+u", c+"+b+u")
	}
	return append(out, "b", "u", "b+u")
}()

var colorSet = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range HighlightNames {
		m[n] = true
	}
	return m
}()

// Style is a parsed highlight name.
type Style struct {
	Color string // "" for none
	B, U  bool
}

// ParseStyle splits a valid highlight name into its parts.
func ParseStyle(name string) Style {
	var st Style
	parts := strings.Split(name, "+")
	i := 0
	if colorSet[parts[0]] {
		st.Color = parts[0]
		i++
	}
	if i < len(parts) && parts[i] == "b" {
		st.B = true
		i++
	}
	if i < len(parts) && parts[i] == "u" {
		st.U = true
	}
	return st
}

// StyleName is the canonical name of a style ("" for none).
func StyleName(st Style) string {
	var parts []string
	if st.Color != "" {
		parts = append(parts, st.Color)
	}
	if st.B {
		parts = append(parts, "b")
	}
	if st.U {
		parts = append(parts, "u")
	}
	return strings.Join(parts, "+")
}

// highlightRe matches =={name}text== on one line: a known style name, at least one
// character, ending at the first "==" after it. Names are quoted ("+" is literal) and
// tried longest first so "coral-deep+b" is never read as "coral".
var highlightRe = func() *regexp.Regexp {
	names := append([]string(nil), HighlightStyleNames...)
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for i, n := range names {
		names[i] = regexp.QuoteMeta(n)
	}
	return regexp.MustCompile(`==\{(` + strings.Join(names, "|") + `)\}([^\n]+?)==`)
}()

// StripHighlights removes highlight delimiters, keeping the highlighted text.
// Image marker lines and section headings are never highlights and are left as they are.
func StripHighlights(s string) string {
	if !strings.Contains(s, "=={") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.Contains(l, "=={") && !IsMarker(l) && !IsSection(l) {
			lines[i] = highlightRe.ReplaceAllString(l, "$2")
		}
	}
	return strings.Join(lines, "\n")
}

// SearchText is what the search index and snippets see: no image markers, no highlight
// delimiters, no section markup (the section names stay).
func SearchText(body string) string { return StripSections(StripHighlights(StripMarkers(body))) }

// HighlightSpan is one highlight on a line: [From, To) is the whole =={name}text==,
// [TextFrom, TextTo) the coloured text (byte offsets).
type HighlightSpan struct {
	From, To, TextFrom, TextTo int
	Name                       string
}

// ParseHighlights returns every highlight on one line, in order, with the same rules as
// the editor's parseLine (web/src/lib/highlight.ts): none on image or section lines.
func ParseHighlights(line string) []HighlightSpan {
	if !strings.Contains(line, "=={") || IsMarker(line) || IsSection(line) {
		return nil
	}
	var out []HighlightSpan
	for _, m := range highlightRe.FindAllStringSubmatchIndex(line, -1) {
		out = append(out, HighlightSpan{From: m[0], To: m[1], TextFrom: m[4], TextTo: m[5], Name: line[m[2]:m[3]]})
	}
	return out
}
