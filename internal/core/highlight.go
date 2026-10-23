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
// Image marker lines are never highlights and are left as they are.
func StripHighlights(s string) string {
	if !strings.Contains(s, "=={") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.Contains(l, "=={") && !IsMarker(l) {
			lines[i] = highlightRe.ReplaceAllString(l, "$2")
		}
	}
	return strings.Join(lines, "\n")
}

// SearchText is what the search index and snippets see: no image markers, no highlight delimiters.
func SearchText(body string) string { return StripHighlights(StripMarkers(body)) }
