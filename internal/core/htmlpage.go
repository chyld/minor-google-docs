package core

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"

	"golang.org/x/sys/unix"

	"homelab-notes/internal/pageassets"
)

// A note saved as one self-contained web page (SPEC §23). PreparePage takes a
// snapshot (text, tags, files and their sizes) before the first byte is written;
// Size and WriteTo walk the same renderer, so the size shown to the user, the
// Content-Length and the bytes written are the same number by construction.

const (
	pageChunk      = 3 * 1398101 // raw bytes per base64 block (a multiple of 3: no padding mid-file)
	pageSmallImage = 2 << 20     // images up to this size are also inline data: URLs (shown without script)
)

// TagHues maps the palette names to their OKLCH hue; web/src/lib/colors.ts has the same table.
var TagHues = map[string]int{
	"coral": 25, "orange": 50, "amber": 78, "lime": 125, "green": 150, "mint": 170,
	"teal": 195, "sky": 228, "blue": 262, "violet": 295, "magenta": 328, "pink": 355,
}

func hueOf(name string) int {
	if h, ok := TagHues[strings.TrimSuffix(name, "-deep")]; ok {
		return h
	}
	return 262
}

// PageFile is one file the page carries.
type PageFile struct {
	Att        Attachment
	path       string
	size       int64
	own        bool        // in this note (otherwise a copy from another note, shown in the text)
	shown      bool        // shown in the text by a marker
	info       os.FileInfo // the file as snapshotted: the write refuses a different one
	missing    bool        // no readable file on disk
	superseded bool        // this note's unreadable copy, replaced by another note's (not listed)
	n          int
}

func (f *PageFile) inline() bool { return InlineImageTypes[f.Att.Mime] }
func (f *PageFile) small() bool  { return f.inline() && f.size <= pageSmallImage }

// Page is a snapshot of a note ready to be written as HTML.
type Page struct {
	uid, id  string
	Title    string // DeriveTitle: for <title> and the file name
	body     string
	trashed  bool
	created  int64
	updated  int64
	exported time.Time
	tags     []metaTag
	theme    string
	files    []*PageFile
	byRef    map[string]*PageFile
	size     int64
}

// PreparePage snapshots a note for the web page. theme is light, dark or
// anything else (follow the reader's system).
func (s *Store) PreparePage(ctx context.Context, uid, id, theme string) (*Page, error) {
	m, err := s.noteMeta(ctx, s.DB.R, uid, id)
	if err != nil {
		return nil, err
	}
	// a busy upload would be missing from the page: wait for it (rows left behind by
	// an abandoned upload stop counting after a few minutes; the janitor removes them)
	if s.exists(ctx, `SELECT 1 FROM attachments WHERE note_id=? AND user_id=? AND state='uploading' AND updated_at>?`,
		id, uid, now()-int64(uploadQuiet/time.Millisecond)) {
		return nil, ErrUploadPending
	}
	raw, err := readCapped(s.notePath(uid, id), MaxBodyBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound // purged meanwhile, or its text is lost (doctor reports it)
	} else if err != nil {
		return nil, err
	}
	body := cleanPageText(string(raw))
	p := &Page{uid: uid, id: id, Title: DeriveTitle(body), body: body, trashed: m.TrashedAt != nil,
		created: m.CreatedAt, updated: m.UpdatedAt, exported: time.Now(), byRef: map[string]*PageFile{}}
	switch theme {
	case "light", "dark":
		p.theme = theme
	default:
		p.theme = "auto"
	}
	// tags in the app's order (by name)
	rows, err := s.DB.R.QueryContext(ctx, `SELECT t.name,t.color FROM tags t JOIN note_tags nt ON nt.tag_id=t.id
 WHERE nt.note_id=? AND t.user_id=? ORDER BY t.name`, id, uid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t metaTag
		if err := rows.Scan(&t.Name, &t.Color); err != nil {
			rows.Close()
			return nil, err
		}
		p.tags = append(p.tags, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	atts, err := s.ListAttachments(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	own := map[string]*PageFile{}
	for _, a := range atts {
		own[a.Ref] = p.addFile(s, a, true)
	}
	// images the text shows from another note (a copy not made yet, or this note's
	// copy can't be read): the same lookup as the editor's
	for _, ref := range markerRefs(body) {
		if f := own[ref]; f != nil && !f.missing {
			f.shown = true
			p.byRef[ref] = f
			continue
		}
		c, err := s.pageCopy(ctx, uid, ref, id)
		if err != nil {
			return nil, err
		}
		if c != nil {
			if f := own[ref]; f != nil {
				f.superseded = true // the copy stands in for this note's unreadable file
			}
			f := p.addFile(s, *c, false)
			f.shown = true
			p.byRef[ref] = f
		} else if f := own[ref]; f != nil {
			f.shown = true
			p.byRef[ref] = f
		}
	}
	n, err := p.render(nil, nil)
	if err != nil {
		return nil, err
	}
	p.size = n
	return p, nil
}

// ErrUploadPending: an upload to the note is still running.
var ErrUploadPending = errors.New("an upload to this note is still running: wait for it to finish")

// uploads quiet for this long count as abandoned for the page
const uploadQuiet = 5 * time.Minute

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, Invalid("this note is larger than %d MiB", limit>>20)
	}
	return b, nil
}

// addFile adds a file to the page's inventory. A file that can't be opened counts as
// missing, so the snapshot never promises bytes the write can't read.
func (p *Page) addFile(s *Store, a Attachment, own bool) *PageFile {
	f := &PageFile{Att: a, own: own, n: len(p.files), missing: true}
	if a.Ext != "" {
		f.path = s.attPath(p.uid, &a)
		if st, ok := openable(f.path); ok {
			f.info, f.size, f.missing = st, st.Size(), false
		}
	}
	p.files = append(p.files, f)
	return f
}

// openRegular opens a file for reading without following a symlink or waiting on a
// FIFO, and only if it is a regular file.
func openRegularFile(path string) (*os.File, os.FileInfo, error) {
	fh, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err := fh.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if err != nil {
		fh.Close()
		return nil, nil, err
	}
	return fh, st, nil
}

// openable proves a file can be read and returns what it is.
func openable(path string) (os.FileInfo, bool) {
	fh, st, err := openRegularFile(path)
	if err != nil {
		return nil, false
	}
	fh.Close()
	return st, true
}

// pageCopy finds a ready copy of ref in another of the user's notes that can be read.
func (s *Store) pageCopy(ctx context.Context, uid, ref, noteID string) (*Attachment, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT `+attCols+` FROM attachments
 WHERE user_id=? AND ref=? AND state='ready' AND note_id!=? AND ext!='' ORDER BY created_at DESC, id`, uid, ref, noteID)
	if err != nil {
		return nil, err
	}
	var cands []Attachment
	for rows.Next() {
		a, err := scanAtt(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range cands {
		if _, ok := openable(s.attPath(uid, &cands[i])); ok {
			return &cands[i], nil
		}
	}
	return nil, nil
}

// Size is the exact number of bytes WriteTo writes.
func (p *Page) Size() int64 { return p.size }

// Filename is the download name: the title, cleaned like the export's.
func (p *Page) Filename() string { return safeName(p.Title) + ".html" }

// WriteTo writes the page. Any error after the first byte means the output is
// incomplete: the caller must abort the response, never end it normally.
func (p *Page) WriteTo(w io.Writer) (int64, error) {
	if pageBeforeWrite != nil {
		pageBeforeWrite(p)
	}
	files := map[int]*os.File{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	n, err := p.render(w, files)
	if err == nil && n != p.size {
		err = fmt.Errorf("page size changed while writing (%d, expected %d)", n, p.size)
	}
	return n, err
}

// pageBeforeWrite lets tests change files between the snapshot and the write.
var pageBeforeWrite func(*Page)

// PageCSP is the content security policy of every page (also sent as a header).
func PageCSP() string {
	assets()
	return pageCSPValue
}

// ---- assets ----

var (
	assetsOnce   sync.Once
	pageCSS      string
	pageJS       string
	pageCSPValue string
)

func assets() {
	assetsOnce.Do(func() {
		css, err := pageassets.FS.ReadFile("page.css")
		if err != nil {
			panic(err)
		}
		js, err := pageassets.FS.ReadFile("page.js")
		if err != nil {
			panic(err)
		}
		fonts, err := pageassets.FS.ReadFile("fonts/inter.css")
		if err != nil {
			panic(err)
		}
		// the font files become data: URLs, so the page needs nothing from the network
		fontCSS := regexp.MustCompile(`url\(([a-z0-9-]+\.woff2)\)`).ReplaceAllStringFunc(string(fonts), func(m string) string {
			name := m[4 : len(m)-1]
			b, err := pageassets.FS.ReadFile("fonts/" + name)
			if err != nil {
				panic(err)
			}
			return "url(data:font/woff2;base64," + base64.StdEncoding.EncodeToString(b) + ")"
		})
		pageCSS = "/* Inter: SIL Open Font License 1.1, (c) The Inter Project Authors */\n" + fontCSS +
			strings.Replace(string(css), "/*TOKENS*/", tokenCSS(), 1)
		pageJS = string(js)
		sum := sha256.Sum256(js)
		pageCSPValue = "default-src 'none'; img-src data: blob:; style-src 'unsafe-inline'; font-src data:; " +
			"script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; script-src-attr 'none'; " +
			"base-uri 'none'; form-action 'none'"
	})
}

// PageTokens are the colour tokens the page uses, per theme, with the values in
// web/src/app.css (checked by TestPageTokensMatchApp).
var PageTokens = map[string]map[string]string{
	"light": {
		"bg": "#f6f3eb", "bg-2": "#efebe1", "surface": "#fffdf8", "surface-2": "#f9f6ef", "surface-hover": "#f3efe6",
		"border": "#e6e0d3", "border-strong": "#d6cfbf", "ink": "#1c2430", "ink-2": "#4a5260", "muted": "#646a78",
		"danger": "oklch(0.55 0.2 25)",
		"shadow": "0 1px 2px rgb(40 30 10 / 0.05), 0 8px 24px -12px rgb(40 30 10 / 0.18)",
		"chip-l": "0.93", "chip-c": "0.06", "chip-ink-l": "0.42", "chip-ink-c": "0.14", "dot-l": "0.68", "dot-c": "0.18",
		"hl-l": "0.92", "hl-c": "0.075", "hl-ink": "#1c2430", "hl-deep-l": "0.8", "hl-deep-c": "0.13", "hl-deep-ink": "#1c2430",
		"sec-l": "0.48", "sec-c": "0.14", "sec-deep-l": "0.38", "sec-deep-c": "0.17",
	},
	"dark": {
		"bg": "#16141f", "bg-2": "#1b1926", "surface": "#1f1d2b", "surface-2": "#242233", "surface-hover": "#2a283a",
		"border": "#302d42", "border-strong": "#403c56", "ink": "#ece9f5", "ink-2": "#bdb8cf", "muted": "#9a95b0",
		"danger": "oklch(0.7 0.18 25)",
		"shadow": "0 1px 2px rgb(0 0 0 / 0.3), 0 8px 24px -12px rgb(0 0 0 / 0.6)",
		"chip-l": "0.33", "chip-c": "0.07", "chip-ink-l": "0.88", "chip-ink-c": "0.09", "dot-l": "0.74", "dot-c": "0.16",
		"hl-l": "0.4", "hl-c": "0.085", "hl-ink": "#ece9f5", "hl-deep-l": "0.76", "hl-deep-c": "0.13", "hl-deep-ink": "#16141f",
		"sec-l": "0.8", "sec-c": "0.12", "sec-deep-l": "0.88", "sec-deep-c": "0.15",
	},
}

func tokenBlock(theme string) string {
	keys := make([]string, 0, len(PageTokens[theme]))
	for k := range PageTokens[theme] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("color-scheme: " + theme + ";")
	for _, k := range keys {
		b.WriteString(" --" + k + ": " + PageTokens[theme][k] + ";")
	}
	return b.String()
}

func tokenCSS() string {
	light, dark := tokenBlock("light"), tokenBlock("dark")
	return "html, html[data-theme='light'] { " + light + " }\n" +
		"html[data-theme='dark'] { " + dark + " }\n" +
		"@media screen and (prefers-color-scheme: dark) { html[data-theme='auto'] { " + dark + " } }\n" +
		"@media print { html[data-theme] { " + light + " } }\n"
}

// ---- text rules shared with the editor ----

// cleanPageText makes the file's text safe to render: LF line endings, valid UTF-8, no NUL.
func cleanPageText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// isJSSpace is JavaScript's whitespace (String.prototype.trim, \s), which the editor uses.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0xFEFF, 0x2028, 0x2029:
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

func jsBlank(s string) bool { return strings.TrimFunc(s, isJSSpace) == "" }

// pageTitleLine is the editor's title line: the first line that isn't an image
// and has text once highlights are stripped (web/src/lib/editor/markers.ts).
func pageTitleLine(lines []string) int {
	for i, l := range lines {
		if !IsMarker(l) && !jsBlank(StripHighlights(l)) {
			return i
		}
	}
	return -1
}

// ---- rendering ----

type pageWriter struct {
	w     io.Writer // nil: only count
	n     int64
	err   error
	files map[int]*os.File
	p     *Page
	srcAt map[int]bool // small images already written as a data: URL
}

func (w *pageWriter) str(s string) {
	if w.err != nil {
		return
	}
	w.n += int64(len(s))
	if w.w != nil {
		_, w.err = io.WriteString(w.w, s)
	}
}

func (w *pageWriter) esc(s string) { w.str(html.EscapeString(s)) }

// b64 writes n bytes of file f from off as base64. last closes the file afterwards:
// a file's bytes are written in one go (small image) or one run of chunks, so only
// one file is ever open.
func (w *pageWriter) b64(f *PageFile, off, n int64, last bool) {
	if w.err != nil {
		return
	}
	enc := int64(base64.StdEncoding.EncodedLen(int(n)))
	if w.w == nil {
		w.n += enc
		return
	}
	fh := w.files[f.n]
	if fh == nil {
		var st os.FileInfo
		var err error
		if fh, st, err = openRegularFile(f.path); err != nil {
			w.err = err
			return
		}
		// another file moved onto the path since the snapshot: its bytes aren't the ones promised
		if !os.SameFile(st, f.info) || st.Size() < f.size {
			fh.Close()
			w.err = fmt.Errorf("%s changed after the page was prepared", f.Att.Filename)
			return
		}
		w.files[f.n] = fh
	}
	cw := &countWriter{w: w.w}
	e := base64.NewEncoder(base64.StdEncoding, cw)
	got, err := io.Copy(e, io.NewSectionReader(fh, off, n))
	if cerr := e.Close(); err == nil {
		err = cerr
	}
	w.n += cw.n
	if err == nil && got != n {
		err = fmt.Errorf("%s: read %d bytes, expected %d", f.Att.Filename, got, n)
	}
	if last {
		fh.Close()
		delete(w.files, f.n)
	}
	w.err = err
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	k, err := c.w.Write(b)
	c.n += int64(k)
	return k, err
}

func (p *Page) render(out io.Writer, files map[int]*os.File) (int64, error) {
	assets()
	w := &pageWriter{w: out, files: files, p: p, srcAt: map[int]bool{}}
	w.str("<!doctype html>\n<html data-theme=\"" + p.theme + "\"><head><meta charset=\"utf-8\">\n")
	w.str("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	w.str("<meta http-equiv=\"Content-Security-Policy\" content=\"" + html.EscapeString(pageCSPValue) + "\">\n")
	w.str("<meta name=\"color-scheme\" content=\"light dark\">\n<title>")
	if p.Title == "" {
		w.str("Untitled")
	} else {
		w.esc(p.Title)
	}
	w.str("</title>\n<style>\n" + pageCSS + "</style></head>\n<body><main class=\"page\">\n")

	if len(p.tags) > 0 {
		w.str("<div class=\"tags\">")
		for _, t := range p.tags {
			c := t.Color
			if !validColor(c) {
				c = "blue"
			}
			w.str("<span class=\"chip\" style=\"--h:" + strconv.Itoa(hueOf(c)) + "\"><span class=\"dot\"></span>")
			w.esc(t.Name)
			w.str("</span>")
		}
		w.str("</div>\n")
	}
	w.str("<p class=\"meta\">Created " + timeTag(p.created) + " · Updated " + timeTag(p.updated) +
		" · Saved as a page " + timeTag(p.exported.UnixMilli()))
	if p.trashed {
		w.str(" · <span class=\"trash\">In the trash</span>")
	}
	w.str("</p>\n<article class=\"note\">\n")
	p.renderBody(w)
	w.str("</article>\n")
	if !jsBlank(p.body) {
		w.str("<div class=\"note-end\" aria-hidden=\"true\"><span class=\"end-rule\"></span>" +
			"<svg width=\"24\" height=\"20\" viewBox=\"0 0 24 20\"><circle cx=\"12\" cy=\"4.5\" r=\"2.6\"/><circle cx=\"5.5\" cy=\"15\" r=\"2.6\"/><circle cx=\"18.5\" cy=\"15\" r=\"2.6\"/></svg>" +
			"<span class=\"end-rule\"></span></div>\n")
	}
	plain := StripSections(StripHighlights(p.body))
	words, chars := len(strings.FieldsFunc(plain, isJSSpace)), len(utf16.Encode([]rune(plain)))
	w.str("<footer class=\"counts\"><span>" + plural(words, "word", "words") + "</span><span>" + plural(chars, "character", "characters") + "</span></footer>\n")
	p.renderFiles(w)
	w.str("<script>" + pageJS + "</script>\n")
	for _, f := range p.files {
		if f.missing || f.small() {
			continue // small images are already in the page as data: URLs
		}
		for off := int64(0); off < f.size || (off == 0 && f.size == 0); off += pageChunk {
			w.str("<script type=\"application/octet-stream\" data-f=\"" + strconv.Itoa(f.n) + "\">")
			w.b64(f, off, min(pageChunk, f.size-off), off+pageChunk >= f.size)
			w.str("</script>\n")
			if f.size == 0 {
				break
			}
		}
	}
	w.str("</main></body></html>\n")
	return w.n, w.err
}

func timeTag(ms int64) string {
	t := time.UnixMilli(ms).UTC()
	return "<time datetime=\"" + t.Format(time.RFC3339) + "\">" + t.Format("2 Jan 2006, 15:04") + " UTC</time>"
}

func plural(n int, one, many string) string {
	s := commaInt(n)
	if n == 1 {
		return s + " " + one
	}
	return s + " " + many
}

func commaInt(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// renderBody writes the note in document order with the editor's rules: the title
// line, plain text runs, sections (bar from the line after the heading to the last
// line with text), highlights and images.
func (p *Page) renderBody(w *pageWriter) {
	lines := strings.Split(p.body, "\n")
	if p.body == "" {
		return
	}
	title := pageTitleLine(lines)
	// sections: headings after the title line, each with its last non-blank body line
	type sec struct{ head, bodyEnd int }
	secAt := map[int]sec{}
	var heads []int
	for i, l := range lines {
		if i != title && IsSection(l) {
			heads = append(heads, i)
		}
	}
	for k, h := range heads {
		end := len(lines) - 1
		if k+1 < len(heads) {
			end = heads[k+1] - 1
		}
		last := h
		for j := end; j > h; j-- {
			if !jsBlank(lines[j]) {
				last = j
				break
			}
		}
		secAt[h] = sec{h, last}
	}
	var run []string
	flush := func() {
		if len(run) == 0 {
			return
		}
		w.str("<div class=\"text\">")
		for i, l := range run {
			if i > 0 {
				w.str("\n")
			}
			p.writeLine(w, l)
		}
		if run[len(run)-1] == "" {
			w.str("\n") // a trailing blank line keeps its height
		}
		w.str("</div>\n")
		run = run[:0]
	}
	barEnd := -1
	for i, l := range lines {
		switch {
		case i == title:
			flush()
			w.str("<p class=\"title\">")
			if IsSection(l) {
				w.esc(l) // a heading on the title line is shown as typed (not a section)
			} else {
				p.writeLine(w, l)
			}
			w.str("</p>\n")
		case IsMarker(l):
			flush()
			p.writeFigure(w, l)
		default:
			if s, ok := secAt[i]; ok {
				flush()
				if barEnd >= 0 {
					w.str("</div>\n")
					barEnd = -1
				}
				color, name, _ := ParseSection(l)
				h := strconv.Itoa(hueOf(color))
				cls := "sec-head"
				if strings.HasSuffix(color, "-deep") {
					cls += " deep"
				}
				w.str("<h2 class=\"" + cls + "\" style=\"--h:" + h + "\">")
				w.esc(name)
				w.str("</h2>\n")
				if s.bodyEnd > i {
					w.str("<div class=\"sec-body\" style=\"--h:" + h + "\">\n")
					barEnd = s.bodyEnd
				}
			} else {
				run = append(run, l)
			}
		}
		if i == barEnd {
			flush()
			w.str("</div>\n")
			barEnd = -1
		}
	}
	flush()
	if barEnd >= 0 {
		w.str("</div>\n")
	}
}

// writeLine writes one line of text with its highlights (delimiters hidden).
func (p *Page) writeLine(w *pageWriter, l string) {
	at := 0
	for _, h := range ParseHighlights(l) {
		w.esc(l[at:h.From])
		cls := "hl"
		if strings.HasSuffix(h.Name, "-deep") {
			cls += " hl-deep"
		}
		w.str("<span class=\"" + cls + "\" style=\"--h:" + strconv.Itoa(hueOf(h.Name)) + "\">")
		w.esc(l[h.TextFrom:h.TextTo])
		w.str("</span>")
		at = h.To
	}
	w.esc(l[at:])
}

func (p *Page) writeFigure(w *pageWriter, l string) {
	m := MarkerRe.FindStringSubmatch(l)
	ref, caption := m[1], m[2]
	f := p.byRef[ref]
	w.str("<figure class=\"img\">")
	switch {
	case f == nil || f.missing:
		w.str("<div class=\"img-missing\">Image not found · ")
		if caption != "" {
			w.esc(caption)
		} else {
			w.esc(ref)
		}
		w.str("</div>")
	case !f.inline():
		w.str("<div class=\"img-missing\">Not an image · ")
		w.esc(f.Att.Filename)
		w.str("</div>")
	default:
		alt := caption
		if alt == "" {
			alt = "image"
		}
		w.str("<span class=\"img-frame\">")
		p.writeImg(w, f, "", alt)
		w.str("</span>")
		if caption != "" {
			w.str("<figcaption>")
			w.esc(caption)
			w.str("</figcaption>")
		}
	}
	w.str("</figure>\n")
}

// writeImg writes an <img> for an inline image: a small one carries its bytes as a
// data: URL the first time it appears; the script fills in the others.
func (p *Page) writeImg(w *pageWriter, f *PageFile, class, alt string) {
	w.str("<img")
	if class == "" {
		w.str(" tabindex=\"0\"") // Enter or Space zooms, like a click
	}
	if class != "" {
		w.str(" class=\"" + class + "\"")
	}
	w.str(" alt=\"")
	w.esc(alt)
	mt := html.EscapeString(strings.TrimSpace(strings.SplitN(f.Att.Mime, ";", 2)[0]))
	w.str("\" decoding=\"async\" data-f=\"" + strconv.Itoa(f.n) + "\" data-type=\"" + mt + "\"")
	if f.small() && !w.srcAt[f.n] {
		w.srcAt[f.n] = true
		w.str(" src=\"data:" + mt + ";base64,")
		w.b64(f, 0, f.size, true)
		w.str("\"")
	}
	w.str(">")
}

func (p *Page) renderFiles(w *pageWriter) {
	listed := 0
	for _, f := range p.files {
		if !f.superseded {
			listed++
		}
	}
	if listed == 0 {
		return
	}
	w.str("<section class=\"files\"><h2>Attachments</h2><ul>\n")
	saveable := false
	for _, f := range p.files {
		if f.superseded {
			continue
		}
		w.str("<li class=\"file\">")
		if f.inline() && !f.shown && !f.missing {
			p.writeImg(w, f, "thumb", "")
		}
		kind, hue := fileKind(f.Att.Mime, f.Att.Filename)
		w.str("<span class=\"kind\" style=\"--h:" + strconv.Itoa(hue) + "\">" + kind + "</span><span class=\"name\">")
		w.esc(f.Att.Filename)
		w.str("</span>")
		if !f.own {
			w.str("<span class=\"note-flag\">from another note</span>")
		} else if !f.shown {
			w.str("<span class=\"note-flag\">not shown in the note</span>")
		}
		if f.missing {
			w.str("<span class=\"missing\">missing on the server</span></li>\n")
			continue
		}
		saveable = true
		w.str("<span class=\"size\">" + humanSize(f.size) + "</span><button type=\"button\" data-save=\"" + strconv.Itoa(f.n) + "\" data-name=\"")
		w.esc(exportFileName(f.Att.Filename))
		w.str("\" aria-label=\"Save ")
		w.esc(f.Att.Filename)
		w.str("\">Save</button></li>\n")
	}
	w.str("</ul>")
	if saveable {
		w.str("<p class=\"noscript\">Saving the attached files needs JavaScript, which is turned off for this page.</p>")
	}
	w.str("</section>\n")
}

// exportFileName is a file name safe to save on any system (as in the export).
func exportFileName(name string) string {
	return notReserved(SanitizeFilename(strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)))
}

// fileKind mirrors web/src/lib/format.ts fileKind (label and hue of a file chip).
func fileKind(mimeType, name string) (string, int) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	in := func(list ...string) bool {
		for _, x := range list {
			if x == ext {
				return true
			}
		}
		return false
	}
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image", 330
	case strings.HasPrefix(mimeType, "video/"):
		return "video", 295
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio", 260
	case mimeType == "application/pdf" || ext == "pdf":
		return "pdf", 25
	case in("zip", "gz", "tgz", "xz", "zst", "7z", "rar", "tar", "bz2", "iso"):
		return "archive", 80
	case in("js", "ts", "go", "py", "rs", "sh", "json", "yaml", "yml", "toml", "conf", "ini", "xml", "html", "css", "sql", "c", "h", "cpp", "java"):
		return "code", 195
	case strings.HasPrefix(mimeType, "text/") || in("txt", "md", "log", "csv"):
		return "text", 230
	case in("doc", "docx", "odt", "xls", "xlsx", "ods", "ppt", "pptx", "odp"):
		return "doc", 150
	}
	return "file", 55
}

// humanSize mirrors web/src/lib/format.ts bytes().
func humanSize(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n) / 1024
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64) + " " + units[i]
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " " + units[i]
}
