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

// highlightRe matches =={name}text== on one line: a known name, at least one
// character, ending at the first "==" after it. Longest names first so
// "coral-deep" is never read as "coral".
var highlightRe = func() *regexp.Regexp {
	names := append([]string(nil), HighlightNames...)
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
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
