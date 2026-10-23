package core

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBodyBytes  = 10 << 20
	maxTitleRunes = 120
	maxPreview    = 160
)

// MarkerRe matches an inline image marker occupying an entire line.
var MarkerRe = regexp.MustCompile(`^\[image:([a-z2-7]{10})\](?: (.*))?$`)

func IsMarker(line string) bool { return MarkerRe.MatchString(line) }

// NormalizeBody validates and normalizes a note body.
func NormalizeBody(body string) (string, error) {
	if len(body) > MaxBodyBytes {
		return "", Invalid("note is larger than %d MiB", MaxBodyBytes>>20)
	}
	if !utf8.ValidString(body) {
		return "", Invalid("note must be valid UTF-8 text")
	}
	if strings.ContainsRune(body, 0) {
		return "", Invalid("note must not contain NUL characters")
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	return strings.ReplaceAll(body, "\r", "\n"), nil
}

// DeriveTitle returns the first non-empty, non-marker line, truncated (highlights and section markup stripped first).
func DeriveTitle(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if IsMarker(line) {
			continue
		}
		t := strings.TrimSpace(StripSections(StripHighlights(line)))
		if t == "" {
			continue
		}
		return truncateRunes(t, maxTitleRunes)
	}
	return "Untitled"
}

// DerivePreview returns text after the title line, markers skipped, collapsed to one line.
func DerivePreview(body string) string {
	var b strings.Builder
	seenTitle := false
	for line := range strings.SplitSeq(body, "\n") {
		if IsMarker(line) {
			continue
		}
		t := strings.TrimSpace(StripSections(StripHighlights(line)))
		if t == "" {
			continue
		}
		if !seenTitle {
			seenTitle = true
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(t)
		if b.Len() > maxPreview*4 {
			break
		}
	}
	return truncateRunes(b.String(), maxPreview)
}

// StripMarkers removes marker lines (used for the search index).
func StripMarkers(body string) string {
	if !strings.Contains(body, "[image:") {
		return body
	}
	lines := strings.Split(body, "\n")
	out := lines[:0]
	for _, l := range lines {
		if !IsMarker(l) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	cut := n - 1
	// never split a grapheme: back off over combining marks, ZWJ sequences,
	// variation selectors, Hangul jamo and the second half of a flag pair
	for cut > 0 && (joinsPrevious(r[cut]) || r[cut-1] == '\u200d' || splitsFlag(r, cut)) {
		cut--
	}
	return strings.TrimSpace(string(r[:cut])) + "…"
}

func joinsPrevious(r rune) bool {
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Mc, r) || r == '\u200d' ||
		(r >= 0xfe00 && r <= 0xfe0f) || (r >= 0x1f3fb && r <= 0x1f3ff) || (r >= 0xe0020 && r <= 0xe007f) ||
		(r >= 0x1160 && r <= 0x11ff) || (r >= 0xd7b0 && r <= 0xd7ff) // Hangul medial vowels / final consonants
}

func isRegionalIndicator(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

// splitsFlag reports whether cutting before r[i] would leave half of a flag
// (flags are pairs of regional indicators, counted from the start of the run).
func splitsFlag(r []rune, i int) bool {
	if !isRegionalIndicator(r[i]) {
		return false
	}
	run := 0
	for j := i - 1; j >= 0 && isRegionalIndicator(r[j]); j-- {
		run++
	}
	return run%2 == 1
}
