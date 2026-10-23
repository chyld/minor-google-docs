package core

import (
	"regexp"
	"strings"
)

// Sections (SPEC.md §22): a line "## {colour} Name" starts a section that runs to
// the next one or the end of the note. The colour is one of HighlightNames and
// the name is not blank; anything else is ordinary text. The editor
// (web/src/lib/heading.ts) has the same grammar; both are checked against
// testdata/sections.json.
var sectionRe = regexp.MustCompile(`^## \{([a-z]+(?:-deep)?)\} (.*)$`)

var highlightNameSet = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range HighlightNames {
		m[n] = true
	}
	return m
}()

// ParseSection returns the colour and name of a section heading line.
func ParseSection(line string) (color, name string, ok bool) {
	if !strings.HasPrefix(line, "## {") {
		return "", "", false
	}
	m := sectionRe.FindStringSubmatch(line)
	if m == nil || !highlightNameSet[m[1]] || strings.TrimSpace(m[2]) == "" {
		return "", "", false
	}
	return m[1], m[2], true
}

func IsSection(line string) bool {
	_, _, ok := ParseSection(line)
	return ok
}

// StripSections removes the "## {colour} " part of section headings, keeping the name.
func StripSections(s string) string {
	if !strings.Contains(s, "## {") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if _, name, ok := ParseSection(l); ok {
			lines[i] = name
		}
	}
	return strings.Join(lines, "\n")
}
