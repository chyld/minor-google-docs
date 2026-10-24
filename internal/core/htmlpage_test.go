package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func renderPage(t *testing.T, s *Store, uid, id string) string {
	t.Helper()
	p, err := s.PreparePage(context.Background(), uid, id, "")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	n, err := p.WriteTo(&b)
	if err != nil {
		t.Fatal(err)
	}
	if n != p.Size() || int64(b.Len()) != p.Size() {
		t.Fatalf("size %d, wrote %d, buffer %d", p.Size(), n, b.Len())
	}
	return b.String()
}

func article(page string) string {
	i, j := strings.Index(page, `<article class="note">`), strings.Index(page, "</article>")
	return page[i:j]
}

// visible text of an HTML fragment (tags removed, entities decoded)
func visible(frag string) string {
	return html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(frag, ""))
}

func TestHighlightSpansFixture(t *testing.T) {
	raw, _ := os.ReadFile("testdata/highlights.json")
	var fx struct {
		Spans []struct {
			In    string
			Spans [][2]string
		}
	}
	if err := json.Unmarshal(raw, &fx); err != nil || len(fx.Spans) == 0 {
		t.Fatal("no span cases", err)
	}
	for _, c := range fx.Spans {
		var got [][2]string
		for _, h := range ParseHighlights(c.In) {
			got = append(got, [2]string{c.In[h.TextFrom:h.TextTo], h.Name})
		}
		if len(got) != len(c.Spans) {
			t.Errorf("%q: got %v, want %v", c.In, got, c.Spans)
			continue
		}
		for i := range got {
			if got[i] != c.Spans[i] {
				t.Errorf("%q: got %v, want %v", c.In, got, c.Spans)
			}
		}
	}
}

func TestPageRendersLikeTheEditor(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	body := strings.Join([]string{
		"",
		"Trip =={amber}notes== and =={sky-deep}more==",
		"first line",
		"",
		"## {teal} Network",
		"ip 10.0.0.1",
		"",
		"",
		"## {coral-deep} Empty",
		"",
		"## {mint} =={coral}not a highlight==",
		"tail",
		"=={red}x== stays",
		"",
	}, "\n")
	n, _ := s.CreateNote(ctx, u.ID, body)
	tag, _ := s.CreateTag(ctx, u.ID, "work", "teal")
	ids := []string{tag.ID}
	s.PatchNote(ctx, u.ID, n.ID, NotePatch{TagIDs: &ids})
	page := renderPage(t, s, u.ID, n.ID)
	art := article(page)
	for _, want := range []string{
		`<p class="title">Trip <span class="hl" style="--h:78">notes</span> and <span class="hl hl-deep" style="--h:228">more</span></p>`,
		`<h2 class="sec-head" style="--h:195">Network</h2>`,
		// the bar covers the body up to the last line with text; the blank lines after it stay outside
		"<div class=\"sec-body\" style=\"--h:195\">\n<div class=\"text\">ip 10.0.0.1</div>\n</div>\n<div class=\"text\">\n\n</div>",
		`<h2 class="sec-head deep" style="--h:25">Empty</h2>` + "\n<div class=\"text\">\n</div>",
		`<h2 class="sec-head" style="--h:170">=={coral}not a highlight==</h2>`,
		"=={red}x== stays",
	} {
		if !strings.Contains(art, want) {
			t.Fatalf("missing %q in\n%s", want, art)
		}
	}
	if strings.Contains(art, `sec-body" style="--h:25"`) {
		t.Fatal("an empty section got a bar")
	}
	if !strings.Contains(page, `<span class="chip" style="--h:195"><span class="dot"></span>work</span>`) {
		t.Fatal("tag chip missing")
	}
	// the visible text is the note's text without markup (headings keep their name)
	want := StripSections(StripHighlights(body))
	got := strings.ReplaceAll(visible(art), "\n\n", "\n")
	if strings.ReplaceAll(want, "\n", "") != strings.ReplaceAll(got, "\n", "") {
		t.Fatalf("visible text differs:\n%q\n%q", got, want)
	}
	if !strings.Contains(page, `class="note-end"`) || !strings.Contains(page, "<span>16 words</span>") {
		t.Fatalf("end mark or counts missing")
	}
}

func TestPageTitleCases(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	long := strings.Repeat("long ", 60)
	cases := []struct{ body, want string }{
		{"## {coral} Heading as title\nbody", `<p class="title">## {coral} Heading as title</p>`}, // shown as typed, not a section
		{"## {coral} \u00a0\nbody", "<p class=\"title\">## {coral} \u00a0</p>"},                   // nbsp name: still the title line
		{long, `<p class="title">` + long + `</p>`},                                               // never truncated
		{"=={coral} ==\nreal title", `<p class="title">real title</p>`},
	}
	for _, c := range cases {
		n, _ := s.CreateNote(ctx, u.ID, c.body)
		if art := article(renderPage(t, s, u.ID, n.ID)); !strings.Contains(art, c.want) {
			t.Errorf("%q:\n%s", c.body, art)
		}
	}
}

func TestPageEscapesEverything(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	evil := `</script><img src=x onerror=alert(1)> "q" 'a' & <!-- -->`
	n, _ := s.CreateNote(ctx, u.ID, evil+"\n## {teal} "+evil+"\n=={coral}"+evil+"==")
	img := uploadPNG(t, s, u.ID, n.ID, pngBytes)
	s.RenameAttachment(ctx, u.ID, img.ID, `x" onload="alert(1).png`)
	s.SaveBody(ctx, u.ID, n.ID, evil+"\n## {teal} "+evil+"\n=={coral}"+evil+"==\n[image:"+img.Ref+"] "+evil, s.mustSHA(t, u.ID, n.ID))
	tag, _ := s.CreateTag(ctx, u.ID, `<b>tag</b>`, "teal")
	ids := []string{tag.ID}
	s.PatchNote(ctx, u.ID, n.ID, NotePatch{TagIDs: &ids})
	page := renderPage(t, s, u.ID, n.ID)
	// exactly one script that runs, and no event handler attributes anywhere
	if c := len(regexp.MustCompile(`<script>`).FindAllString(page, -1)); c != 1 {
		t.Fatalf("%d executable scripts", c)
	}
	// every attribute value is double-quoted with quotes escaped, so dropping the
	// values leaves only the attribute names to check
	for _, tag := range regexp.MustCompile(`<[a-zA-Z][^>]*>`).FindAllString(page, -1) {
		names := regexp.MustCompile(`"[^"]*"`).ReplaceAllString(tag, `""`)
		if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(names) {
			t.Fatalf("an event handler attribute made it into the page: %.120s", tag)
		}
	}
	if strings.Contains(page, "<img src=x") || strings.Contains(page, "<b>tag") {
		t.Fatal("note content became markup")
	}
}

func TestPageImagesAndFiles(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "Images")
	small := uploadPNG(t, s, u.ID, n.ID, pngBytes)
	big := append([]byte("\x89PNG\r\n\x1a\nBIG"), bytes.Repeat([]byte{1}, pageSmallImage+10)...)
	large := uploadPNG(t, s, u.ID, n.ID, big)
	// a non-image file that only claims to be an SVG: never inlined
	id := NewID()
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: n.ID, Filename: "evil.svg", Length: int64(len(svg))})
	ev, _, _ := s.AppendUpload(ctx, u.ID, id, 0, -1, bytes.NewReader(svg))
	panelOnly := uploadPNG(t, s, u.ID, n.ID, append([]byte("\x89PNG\r\n\x1a\n"), 3, 3, 3))
	other, _ := s.CreateNote(ctx, u.ID, "Other")
	borrowed := uploadPNG(t, s, u.ID, other.ID, append([]byte("\x89PNG\r\n\x1a\n"), 4, 4, 4))
	body := "Images\n[image:" + small.Ref + "] one\n[image:" + small.Ref + "]\n[image:" + large.Ref + "] big\n[image:" +
		ev.Ref + "] svg\n[image:" + NewRef() + "] gone\n[image:" + borrowed.Ref + "] borrowed"
	s.Cfg.MinFree = 1 << 62 // keep the borrowed image in the other note only
	if _, err := s.SaveBody(ctx, u.ID, n.ID, body, s.mustSHA(t, u.ID, n.ID)); err != nil {
		t.Fatal(err)
	}
	s.Cfg.MinFree = 0
	page := renderPage(t, s, u.ID, n.ID)
	art := article(page)
	smallB64 := base64.StdEncoding.EncodeToString(pngBytes)
	if c := strings.Count(page, smallB64); c != 1 {
		t.Fatalf("small image embedded %d times", c)
	}
	if !strings.Contains(art, `<figcaption>one</figcaption>`) || strings.Count(art, "<figcaption>") != 3 {
		t.Fatal("captions")
	}
	if !strings.Contains(art, "Not an image · evil.svg") || strings.Contains(page, "data:image/svg") {
		t.Fatal("svg handled as an image")
	}
	if !strings.Contains(art, "Image not found · gone") {
		t.Fatal("missing image box")
	}
	// the large image and the svg travel as chunks; their bytes decode back exactly
	for _, f := range []struct {
		a    *Attachment
		want []byte
	}{{large, big}, {ev, svg}} {
		p, _ := s.PreparePage(ctx, u.ID, n.ID, "")
		var nidx int
		for _, pf := range p.files {
			if pf.Att.ID == f.a.ID {
				nidx = pf.n
			}
		}
		re := regexp.MustCompile(`<script type="application/octet-stream" data-f="` + strconv.Itoa(nidx) + `">([^<]*)</script>`)
		var got []byte
		for _, m := range re.FindAllStringSubmatch(page, -1) {
			b, err := base64.StdEncoding.DecodeString(m[1])
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, b...)
		}
		if !bytes.Equal(got, f.want) {
			t.Fatalf("%s: %d bytes back, want %d", f.a.Filename, len(got), len(f.want))
		}
	}
	if !strings.Contains(page, "not shown in the note") || !strings.Contains(page, "from another note") {
		t.Fatal("file flags")
	}
	_ = panelOnly
}

func TestPageAbortsWhenAFileShrinks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "N")
	id := NewID()
	data := bytes.Repeat([]byte("pdf "), 5000)
	s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: n.ID, Filename: "a.pdf", Length: int64(len(data))})
	a, _, _ := s.AppendUpload(ctx, u.ID, id, 0, -1, bytes.NewReader(data))
	p, err := s.PreparePage(ctx, u.ID, n.ID, "dark")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.attPath(u.ID, a), []byte("short"), 0o600)
	if _, err := p.WriteTo(&bytes.Buffer{}); err == nil {
		t.Fatal("a file that changed under the page didn't fail the write")
	}
}

func TestPageHugeLineAndOddText(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "T")
	// note.txt edited by hand: CRLF, invalid UTF-8, NUL, and a single 9 MiB line
	raw := "Title\r\nline\x00two\xff\r\n" + strings.Repeat("x", 9<<20)
	os.WriteFile(s.notePath(u.ID, n.ID), []byte(raw), 0o600)
	page := renderPage(t, s, u.ID, n.ID)
	if strings.Contains(page, "\r") || strings.Contains(page, "\x00") || !strings.Contains(page, "linetwo\uFFFD") {
		t.Fatal("text not cleaned")
	}
	if !strings.Contains(page, strings.Repeat("x", 9<<20)) {
		t.Fatal("long line cut")
	}
}

func TestPageCrossUserAndTrash(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := newUser(t, s, "alice")
	b := newUser(t, s, "bob")
	n, _ := s.CreateNote(ctx, a.ID, "secret")
	if _, err := s.PreparePage(ctx, b.ID, n.ID, ""); err == nil {
		t.Fatal("bob rendered alice's note")
	}
	tr := true
	s.PatchNote(ctx, a.ID, n.ID, NotePatch{Trashed: &tr})
	if page := renderPage(t, s, a.ID, n.ID); !strings.Contains(page, "In the trash") {
		t.Fatal("trash not shown")
	}
	empty, _ := s.CreateNote(ctx, a.ID, "")
	if page := renderPage(t, s, a.ID, empty.ID); strings.Contains(page, `class="note-end"`) || !strings.Contains(page, "<title>Untitled</title>") {
		t.Fatal("empty note")
	}
}

func TestPageCSPHashesTheScript(t *testing.T) {
	assets()
	js, _ := os.ReadFile("../pageassets/page.js")
	if pageJS != string(js) || !strings.Contains(PageCSP(), "script-src 'sha256-") {
		t.Fatal("script or policy")
	}
	s := newStore(t)
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(context.Background(), u.ID, "x")
	page := renderPage(t, s, u.ID, n.ID)
	if !strings.Contains(page, "<script>"+string(js)+"</script>") {
		t.Fatal("the emitted script isn't byte-identical to the hashed one")
	}
}

// The page's colour tokens and hues are the app's (web/src/app.css, web/src/lib/colors.ts).
func TestPageTokensMatchApp(t *testing.T) {
	css, err := os.ReadFile("../../web/src/app.css")
	if err != nil {
		t.Skip("web sources not available")
	}
	block := func(start string) map[string]string {
		i := bytes.Index(css, []byte(start))
		if i < 0 {
			t.Fatalf("no %q block in app.css", start)
		}
		j := bytes.IndexByte(css[i:], '}')
		out := map[string]string{}
		for _, m := range regexp.MustCompile(`--([a-z0-9-]+):\s*([^;]+);`).FindAllSubmatch(css[i:i+j], -1) {
			out[string(m[1])] = strings.TrimSpace(string(m[2]))
		}
		return out
	}
	for theme, start := range map[string]string{"light": ":root,\n[data-theme='light'] {", "dark": "[data-theme='dark'] {"} {
		app := block(start)
		for k, v := range PageTokens[theme] {
			if app[k] != v {
				t.Errorf("%s --%s: page %q, app %q", theme, k, v, app[k])
			}
		}
	}
	ts, err := os.ReadFile("../../web/src/lib/colors.ts")
	if err != nil {
		t.Fatal(err)
	}
	hues := regexp.MustCompile(`(?m)^\s+([a-z]+): (\d+),`).FindAllSubmatch(ts, -1)
	if len(hues) != len(TagHues) {
		t.Fatalf("%d hues in colors.ts, %d in Go", len(hues), len(TagHues))
	}
	for _, m := range hues {
		if strconv.Itoa(TagHues[string(m[1])]) != string(m[2]) {
			t.Errorf("hue %s: Go %d, app %s", m[1], TagHues[string(m[1])], m[2])
		}
	}
}

// The embedded fonts are the ones the app ships (when web/node_modules is there).
func TestPageFontsMatchApp(t *testing.T) {
	dir := "../../web/node_modules/@fontsource-variable/inter/files"
	if _, err := os.Stat(dir); err != nil {
		t.Skip("web/node_modules not installed")
	}
	entries, _ := os.ReadDir("../pageassets/fonts")
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".woff2") {
			continue
		}
		n++
		a, _ := os.ReadFile(filepath.Join("../pageassets/fonts", e.Name()))
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s differs from the app's font", e.Name())
		}
	}
	if n != 7 {
		t.Fatalf("%d font files", n)
	}
}

// decodeBlocks returns the bytes of file n as stored in the page's chunk blocks.
func decodeBlocks(t *testing.T, page string, n int) []byte {
	t.Helper()
	re := regexp.MustCompile(`<script type="application/octet-stream" data-f="` + strconv.Itoa(n) + `">([^<]*)</script>`)
	var got []byte
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		b, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, b...)
	}
	return got
}

func uploadBytes(t *testing.T, s *Store, uid, noteID, name string, data []byte) *Attachment {
	t.Helper()
	ctx := context.Background()
	id := NewID()
	if _, err := s.CreateUpload(ctx, uid, UploadMeta{ID: id, Ref: NewRef(), NoteID: noteID, Filename: name, Length: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.AppendUpload(ctx, uid, id, 0, -1, bytes.NewReader(data))
	if err != nil || a.State != "ready" {
		t.Fatalf("upload %s: %v", name, err)
	}
	return a
}

func fileIndex(t *testing.T, s *Store, uid, noteID, attID string) int {
	t.Helper()
	p, err := s.PreparePage(context.Background(), uid, noteID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range p.files {
		if f.Att.ID == attID {
			return f.n
		}
	}
	t.Fatal("file not in the page")
	return -1
}

// Files across chunk boundaries (several chunks, exactly one chunk, a ragged end) and
// an empty file come back byte for byte.
func TestPageChunkBoundaries(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "Files")
	cases := map[string][]byte{
		"multi.bin": bytes.Repeat([]byte("0123456789abcdefg"), (2*pageChunk+1234)/17),
		"exact.bin": bytes.Repeat([]byte{7}, pageChunk),
		"empty.bin": {},
	}
	ids := map[string]string{}
	for name, data := range cases {
		ids[name] = uploadBytes(t, s, u.ID, n.ID, name, data).ID
	}
	page := renderPage(t, s, u.ID, n.ID)
	for name, data := range cases {
		if got := decodeBlocks(t, page, fileIndex(t, s, u.ID, n.ID, ids[name])); !bytes.Equal(got, data) {
			t.Errorf("%s: %d bytes back, want %d", name, len(got), len(data))
		}
	}
}

// A large image shown twice is embedded once.
func TestPageLargeImageOnce(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "P")
	big := append([]byte("\x89PNG\r\n\x1a\nBIG"), bytes.Repeat([]byte{1}, pageSmallImage+10)...)
	a := uploadPNG(t, s, u.ID, n.ID, big)
	s.SaveBody(ctx, u.ID, n.ID, "P\n[image:"+a.Ref+"]\n[image:"+a.Ref+"]", s.mustSHA(t, u.ID, n.ID))
	page := renderPage(t, s, u.ID, n.ID)
	if c := strings.Count(page, `data-f="0" data-type="image/png"`); c != 2 {
		t.Fatalf("%d images", c)
	}
	if got := decodeBlocks(t, page, 0); !bytes.Equal(got, big) {
		t.Fatal("bytes differ (or embedded twice)")
	}
}

// This note's copy can't be read: another note's copy stands in, listed once.
func TestPageUnreadableFileUsesACopy(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads anything")
	}
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	b, _ := s.CreateNote(ctx, u.ID, "B\n[image:"+img.Ref+"] pic")
	bc, _ := scanAtt(s.DB.R.QueryRow(`SELECT `+attCols+` FROM attachments WHERE note_id=?`, b.ID))
	os.Chmod(s.attPath(u.ID, &bc), 0)
	defer os.Chmod(s.attPath(u.ID, &bc), 0o600)
	page := renderPage(t, s, u.ID, b.ID)
	if !strings.Contains(page, base64.StdEncoding.EncodeToString(pngBytes)) || strings.Count(page, `<li class="file">`) != 1 ||
		!strings.Contains(page, "from another note") || strings.Contains(page, "missing on the server") {
		t.Fatalf("the copy didn't stand in cleanly")
	}
}

func TestPageFileVanishesOrGrows(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "N")
	data := bytes.Repeat([]byte("zip "), 4000)
	a := uploadBytes(t, s, u.ID, n.ID, "a.zip", data)
	path := s.attPath(u.ID, a)
	defer func() { pageBeforeWrite = nil }()

	pageBeforeWrite = func(*Page) { os.Remove(path) }
	p, _ := s.PreparePage(ctx, u.ID, n.ID, "")
	if _, err := p.WriteTo(&bytes.Buffer{}); err == nil {
		t.Fatal("a file deleted after the snapshot didn't fail the write")
	}
	os.WriteFile(path, data, 0o600)
	pageBeforeWrite = func(*Page) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		f.Write([]byte("more"))
		f.Close()
	}
	p, _ = s.PreparePage(ctx, u.ID, n.ID, "")
	var buf bytes.Buffer
	if _, err := p.WriteTo(&buf); err != nil {
		t.Fatalf("a file that grew: %v", err)
	}
	if got := decodeBlocks(t, buf.String(), 0); !bytes.Equal(got, data) {
		t.Fatal("the page should hold the snapshot's bytes")
	}
}

func TestPageImageAboveTitle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "x")
	img := uploadPNG(t, s, u.ID, n.ID, pngBytes)
	s.SaveBody(ctx, u.ID, n.ID, "[image:"+img.Ref+"] cover\nThe title\nbody", s.mustSHA(t, u.ID, n.ID))
	art := article(renderPage(t, s, u.ID, n.ID))
	if f, ti := strings.Index(art, "<figure"), strings.Index(art, `<p class="title">The title</p>`); f < 0 || ti < 0 || f > ti {
		t.Fatalf("image not above the title:\n%s", art)
	}
}

func TestPageUploadsAndMissingText(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "N")
	id, ref := NewID(), NewRef()
	s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: ref, NoteID: n.ID, Filename: "big.png", Length: 100})
	s.SaveBody(ctx, u.ID, n.ID, "N\n[image:"+ref+"] coming", s.mustSHA(t, u.ID, n.ID))
	if _, err := s.PreparePage(ctx, u.ID, n.ID, ""); !errors.Is(err, ErrUploadPending) {
		t.Fatalf("a running upload: %v", err)
	}
	// an upload abandoned long ago doesn't block the page; its marker shows "not found"
	s.DB.W.Exec(`UPDATE attachments SET updated_at=0 WHERE id=?`, id)
	if art := article(renderPage(t, s, u.ID, n.ID)); !strings.Contains(art, "Image not found · coming") {
		t.Fatal(art)
	}
	os.WriteFile(s.notePath(u.ID, n.ID), bytes.Repeat([]byte("a"), MaxBodyBytes+1), 0o600)
	if _, err := s.PreparePage(ctx, u.ID, n.ID, ""); err == nil {
		t.Fatal("an oversized note.txt was read")
	}
	os.Remove(s.notePath(u.ID, n.ID))
	if _, err := s.PreparePage(ctx, u.ID, n.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing text: %v", err)
	}
}

// The layout values copied from the app stay the app's.
func TestPageLayoutMatchesApp(t *testing.T) {
	app, err := os.ReadFile("../../web/src/app.css")
	if err != nil {
		t.Skip("web sources not available")
	}
	page, _ := os.ReadFile("../pageassets/page.css")
	rule := func(css []byte, sel string) string {
		i := bytes.Index(css, []byte("\n"+sel+" {"))
		if i < 0 {
			i = bytes.Index(css, []byte(sel+" {"))
		}
		if i < 0 {
			t.Fatalf("no %q rule", sel)
		}
		j := bytes.IndexByte(css[i:], '}')
		return string(css[i : i+j])
	}
	decl := func(block, prop string) string {
		m := regexp.MustCompile(`(?:^|[\s;{])` + regexp.QuoteMeta(prop) + `:\s*([^;]+);`).FindStringSubmatch(block)
		if m == nil {
			return ""
		}
		return strings.TrimSuffix(strings.TrimSpace(m[1]), " !important")
	}
	pairs := []struct {
		app, page string
		props     []string
	}{
		{".cm-title-line", ".title", []string{"font-size", "font-weight", "letter-spacing", "line-height"}},
		{".editor-host .cm-editor", "body", []string{"font-size", "line-height"}},
		{".cm-sec-head", ".sec-head", []string{"font-weight", "font-size", "line-height", "letter-spacing", "color"}},
		{".cm-sec-bar", ".sec-body::before", []string{"width", "border-radius", "background", "opacity"}},
		{".hl", ".hl", []string{"background", "color", "border-radius", "padding"}},
		{".cm-img-frame img", ".img-frame img", []string{"max-height", "object-fit"}},
		{".cm-img-caption", ".img figcaption", []string{"font-size", "color"}},
		{".page", ".page", []string{"max-width"}},
	}
	for _, p := range pairs {
		a, b := rule(app, p.app), rule(page, p.page)
		for _, prop := range p.props {
			if decl(a, prop) == "" || decl(a, prop) != decl(b, prop) {
				t.Errorf("%s %s: app %q, page %q", p.page, prop, decl(a, prop), decl(b, prop))
			}
		}
	}
	if !strings.Contains(rule(page, ".sec-body::before"), "left: -14px") || !strings.Contains(rule(app, ".cm-sec-bar"), "margin-left: -14px") {
		t.Error("section bar offset")
	}
}
