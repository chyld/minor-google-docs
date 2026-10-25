package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// readArchive prepares and writes a note's archive and returns its entries (name → bytes;
// folders end in "/").
func readArchive(t *testing.T, s *Store, uid, id string) (*NoteArchive, map[string][]byte, []string) {
	t.Helper()
	a, err := s.PrepareNoteArchive(context.Background(), uid, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, err := a.WriteTo(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(buf.Len()) {
		t.Fatalf("WriteTo said %d, wrote %d", n, buf.Len())
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string][]byte{}
	var order []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = b
		order = append(order, h.Name)
		if h.Typeflag == tar.TypeReg && h.Mode != 0o644 || h.Typeflag == tar.TypeDir && h.Mode != 0o755 {
			t.Fatalf("%s: mode %o", h.Name, h.Mode)
		}
	}
	return a, got, order
}

func archiveManifest(t *testing.T, entries map[string][]byte) manifestNote {
	t.Helper()
	var man struct {
		Version int            `json:"version"`
		Notes   []manifestNote `json:"notes"`
	}
	if err := json.Unmarshal(entries["manifest.json"], &man); err != nil || man.Version != 2 || len(man.Notes) != 1 {
		t.Fatalf("manifest: %v %s", err, entries["manifest.json"])
	}
	return man.Notes[0]
}

func TestArchiveContents(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "")
	img := uploadPNG(t, s, u.ID, n.ID, pngBytes)
	uploadBytes(t, s, u.ID, n.ID, "Report.pdf", []byte("%PDF-1.4 one"))
	uploadBytes(t, s, u.ID, n.ID, "report.PDF", []byte("%PDF-1.4 two"))
	uploadBytes(t, s, u.ID, n.ID, "CON.txt", []byte("reserved"))
	uploadBytes(t, s, u.ID, n.ID, `a:b?"c.txt`, []byte("unsafe"))
	other, _ := s.CreateNote(ctx, u.ID, "Other")
	borrowed := uploadPNG(t, s, u.ID, other.ID, append([]byte("\x89PNG\r\n\x1a\n"), 4, 4, 4))
	gone := NewRef()
	body := "Rack\n[image:" + img.Ref + "] front\n[image:" + borrowed.Ref + "] borrowed\n[image:" + gone + "] lost photo\n"
	s.Cfg.MinFree = 1 << 62 // keep the borrowed image in the other note only
	if _, err := s.SaveBody(ctx, u.ID, n.ID, body, s.mustSHA(t, u.ID, n.ID)); err != nil {
		t.Fatal(err)
	}
	s.Cfg.MinFree = 0
	tag, _ := s.CreateTag(ctx, u.ID, "homelab", "teal")
	pin := true
	s.PatchNote(ctx, u.ID, n.ID, NotePatch{Pinned: &pin, TagIDs: &[]string{tag.ID}})
	// the raw bytes are kept, not cleaned like the web page's text
	raw := []byte(strings.Replace(body, "Rack\n", "Rack\r\n\x00", 1))
	os.WriteFile(s.notePath(u.ID, n.ID), raw, 0o600)

	a, got, order := readArchive(t, s, u.ID, n.ID)
	dir := "notes/Rack/"
	if a.Filename() != "Rack.tar.gz" || a.Missing != 1 {
		t.Fatalf("filename %q, missing %d", a.Filename(), a.Missing)
	}
	if !bytes.Equal(got[dir+"note.txt"], raw) {
		t.Fatalf("note.txt: %q", got[dir+"note.txt"])
	}
	for name, want := range map[string]string{
		"shot.png": string(pngBytes), "Report.pdf": "%PDF-1.4 one", "report (2).PDF": "%PDF-1.4 two",
		"_CON.txt": "reserved", "a_b__c.txt": "unsafe",
	} {
		if string(got[dir+"files/"+name]) != want {
			t.Errorf("files/%s: %q", name, got[dir+"files/"+name])
		}
	}
	// the borrowed image is in too, under its own name (de-duplicated against shot.png)
	if string(got[dir+"files/shot (2).png"]) != string(append([]byte("\x89PNG\r\n\x1a\n"), 4, 4, 4)) {
		t.Fatalf("borrowed image: %v", order)
	}
	if order[len(order)-1] != "manifest.json" || !strings.Contains(string(got["MISSING.txt"]), "lost photo  [image:"+gone+"]") {
		t.Fatalf("order %v, MISSING.txt %q", order, got["MISSING.txt"])
	}
	for name := range got {
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) ||
			!(strings.HasPrefix(name, dir) || name == "notes/" || name == "manifest.json" || name == "MISSING.txt") {
			t.Fatalf("unexpected entry %q", name)
		}
		if strings.HasSuffix(name, ".bak") {
			t.Fatal("the backup copy is never included")
		}
	}
	mn := archiveManifest(t, got)
	if mn.File != dir+"note.txt" || mn.Folder != "notes/Rack" || !mn.Pinned || mn.Trashed || len(mn.Tags) != 1 || mn.TagColors["homelab"] != "teal" ||
		len(mn.Attachments) != 6 || len(mn.Missing) != 1 || mn.Missing[0].Ref != gone || mn.Missing[0].Reason != "not found" {
		t.Fatalf("manifest %+v", mn)
	}
	for _, ma := range mn.Attachments {
		if _, ok := got[ma.File]; !ok {
			t.Fatalf("manifest names %q, not in the archive", ma.File)
		}
	}
	var meta metaFile
	if err := json.Unmarshal(got[dir+"meta.json"], &meta); err != nil || len(meta.Attachments) != 6 || meta.TrashedAt != nil || !meta.Pinned {
		t.Fatalf("meta.json: %v %+v", err, meta)
	}
}

// A file gone from disk is left out of the manifest (so the archive still imports),
// named in MISSING.txt and in the manifest's missing list.
func TestArchiveMissingFileStillImports(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	b := newUser(t, s, "bob")
	n, _ := s.CreateNote(ctx, u.ID, "")
	keep := uploadBytes(t, s, u.ID, n.ID, "keep.bin", []byte("kept"))
	lost := uploadBytes(t, s, u.ID, n.ID, "lost.bin", []byte("lost"))
	s.SaveBody(ctx, u.ID, n.ID, "Files", s.mustSHA(t, u.ID, n.ID))
	os.Remove(s.attPath(u.ID, lost))

	a, got, _ := readArchive(t, s, u.ID, n.ID)
	mn := archiveManifest(t, got)
	if a.Missing != 1 || len(mn.Attachments) != 1 || mn.Attachments[0].Ref != keep.Ref || len(mn.Missing) != 1 ||
		mn.Missing[0].Filename != "lost.bin" || mn.Missing[0].Reason != "unreadable" || !strings.Contains(string(got["MISSING.txt"]), "  lost.bin\n") ||
		strings.Contains(string(got["MISSING.txt"]), "[image:") { // a file, not an image the text shows
		t.Fatalf("missing %d, manifest %+v", a.Missing, mn)
	}
	path := writeArchiveFile(t, s, u.ID, n.ID)
	if c, err := s.Import(ctx, b.ID, path); err != nil || c != 1 {
		t.Fatalf("import: %d %v", c, err)
	}
}

func writeArchiveFile(t *testing.T, s *Store, uid, id string) string {
	t.Helper()
	a, err := s.PrepareNoteArchive(context.Background(), uid, id)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), a.Filename())
	f, _ := os.Create(path)
	if _, err := a.WriteTo(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

func TestArchiveImportRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := newUser(t, s, "alice")
	b := newUser(t, s, "bob")
	n, _ := s.CreateNote(ctx, a.ID, "")
	img := uploadPNG(t, s, a.ID, n.ID, pngBytes)
	uploadBytes(t, s, a.ID, n.ID, "notes.pdf", []byte("%PDF-1.4"))
	other, _ := s.CreateNote(ctx, a.ID, "Other")
	borrowed := uploadPNG(t, s, a.ID, other.ID, append([]byte("\x89PNG\r\n\x1a\n"), 9, 9))
	body := "Rack\n[image:" + img.Ref + "] front\n[image:" + borrowed.Ref + "] back"
	s.Cfg.MinFree = 1 << 62
	s.SaveBody(ctx, a.ID, n.ID, body, s.mustSHA(t, a.ID, n.ID))
	s.Cfg.MinFree = 0
	tag, _ := s.CreateTag(ctx, a.ID, "homelab", "amber")
	pin := true
	s.PatchNote(ctx, a.ID, n.ID, NotePatch{Pinned: &pin, TagIDs: &[]string{tag.ID}})
	created := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	s.DB.W.Exec(`UPDATE notes SET created_at=? WHERE id=?`, created, n.ID)
	// bob already uses alice's image ref for something else → the marker is remapped
	bm, _ := s.CreateNote(ctx, b.ID, "")
	s.importAttachment(ctx, b.ID, bm.ID, strings.NewReader("x"), "clash.bin", img.Ref)

	path := writeArchiveFile(t, s, a.ID, n.ID)
	if !strings.HasSuffix(path, "Rack.tar.gz") {
		t.Fatal(path)
	}
	if c, err := s.Import(ctx, b.ID, path); err != nil || c != 1 {
		t.Fatalf("import: %d %v", c, err)
	}
	l, _ := s.ListNotes(ctx, b.ID, ListParams{Pinned: true})
	if len(l.Items) != 1 || l.Items[0].Title != "Rack" || len(l.Items[0].TagIDs) != 1 || l.Items[0].CreatedAt != created || l.Items[0].TrashedAt != nil {
		t.Fatalf("note not restored: %+v", l.Items)
	}
	got, _ := s.GetNote(ctx, b.ID, l.Items[0].ID)
	if len(got.Attachments) != 3 {
		t.Fatalf("attachments: %+v", got.Attachments)
	}
	names := map[string]bool{}
	for _, at := range got.Attachments {
		names[at.Filename] = true
		if at.Inline && !strings.Contains(got.Body, "[image:"+at.Ref+"]") {
			t.Fatalf("image %s has no marker in %q", at.Ref, got.Body)
		}
	}
	if !names["notes.pdf"] || strings.Contains(got.Body, "[image:"+img.Ref+"]") {
		t.Fatalf("names %v, body %q", names, got.Body)
	}
}

func TestArchiveRefusals(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	b := newUser(t, s, "bob")
	n, _ := s.CreateNote(ctx, u.ID, "Plan")
	if _, err := s.PrepareNoteArchive(ctx, b.ID, n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's note: %v", err)
	}
	trashed := true
	s.PatchNote(ctx, u.ID, n.ID, NotePatch{Trashed: &trashed})
	if _, err := s.PrepareNoteArchive(ctx, u.ID, n.ID); !errors.Is(err, ErrNoteTrashed) {
		t.Fatalf("trashed: %v", err)
	}
	trashed = false
	s.PatchNote(ctx, u.ID, n.ID, NotePatch{Trashed: &trashed})
	id := NewID()
	s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: n.ID, Filename: "big.bin", Length: 100})
	if _, err := s.PrepareNoteArchive(ctx, u.ID, n.ID); !errors.Is(err, ErrUploadPending) {
		t.Fatalf("upload running: %v", err)
	}
	s.DB.W.Exec(`DELETE FROM attachments WHERE id=?`, id)
	// note.txt as a symlink is not followed; as a FIFO it never blocks
	path := s.notePath(u.ID, n.ID)
	elsewhere := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(elsewhere, []byte("secret"), 0o600)
	os.Remove(path)
	os.Symlink(elsewhere, path)
	if _, err := s.PrepareNoteArchive(ctx, u.ID, n.ID); err == nil {
		t.Fatal("followed a symlinked note.txt")
	}
	os.Remove(path)
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skip("no fifo support")
	}
	done := make(chan error, 1)
	go func() { _, err := s.PrepareNoteArchive(ctx, u.ID, n.ID); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO note.txt was read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO note.txt hung the archive")
	}
}

// A file that changed after the snapshot fails the write: deleted, grown, shrunk or
// replaced; never an archive with different bytes than its headers promise.
func TestArchiveFileChanges(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "N")
	data := bytes.Repeat([]byte("tar "), 4000)
	a := uploadBytes(t, s, u.ID, n.ID, "a.bin", data)
	path := s.attPath(u.ID, a)
	defer SetArchiveBeforeWriteForTest(nil)
	for name, change := range map[string]func(){
		"deleted": func() { os.Remove(path) },
		"grown": func() {
			f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			f.Write([]byte("more"))
			f.Close()
		},
		"shrunk": func() { os.Truncate(path, 10) },
		"replaced": func() {
			os.WriteFile(path+".new", data, 0o600)
			os.Rename(path+".new", path)
		},
		"symlinked": func() {
			os.WriteFile(path+".real", data, 0o600)
			os.Remove(path)
			os.Symlink(path+".real", path)
		},
	} {
		os.Remove(path)
		os.Remove(path + ".real")
		os.WriteFile(path, data, 0o600)
		SetArchiveBeforeWriteForTest(func(*NoteArchive) { change() })
		ar, err := s.PrepareNoteArchive(ctx, u.ID, n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ar.WriteTo(io.Discard); err == nil {
			t.Errorf("%s: the write didn't fail", name)
		}
	}
	// unchanged: fine
	os.Remove(path)
	os.WriteFile(path, data, 0o600)
	SetArchiveBeforeWriteForTest(nil)
	_, got, _ := readArchive(t, s, u.ID, n.ID)
	if !bytes.Equal(got["notes/N/files/a.bin"], data) {
		t.Fatal("bytes differ")
	}
}

// Names stay within file-system limits (255 bytes), even for emoji, and a note named
// like the archive's own files doesn't clash with them.
func TestArchiveNames(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, strings.Repeat("🦊", 80))
	long := strings.Repeat("é", 98) + ".png" // 200 bytes: the longest name an upload keeps
	uploadBytes(t, s, u.ID, n.ID, long, []byte("one"))
	uploadBytes(t, s, u.ID, n.ID, long, []byte("two"))
	ar, got, _ := readArchive(t, s, u.ID, n.ID)
	if len(ar.Filename()) > 255 || !utf8.ValidString(ar.Filename()) {
		t.Fatalf("download name %d bytes", len(ar.Filename()))
	}
	files := 0
	for name := range got {
		for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
			if len(part) > 255 || !utf8.ValidString(part) {
				t.Fatalf("component of %d bytes: %q", len(part), part)
			}
		}
		if strings.Contains(name, "/files/") && !strings.HasSuffix(name, "/") {
			files++
			if !strings.HasSuffix(name, ".png") {
				t.Fatalf("extension lost: %q", name)
			}
		}
	}
	if files != 2 {
		t.Fatalf("%d files", files)
	}
	m, _ := s.CreateNote(ctx, u.ID, "manifest.json")
	_, got, _ = readArchive(t, s, u.ID, m.ID)
	if _, ok := got["notes/manifest.json/note.txt"]; !ok || archiveManifest(t, got).Title != "manifest.json" {
		t.Fatal("a note named manifest.json")
	}
}

// Import unpacks a .tar.gz only inside its own folder: names leaving it are refused,
// links are skipped.
func TestImportTarGzSafety(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	mk := func(entries ...*tar.Header) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, h := range entries {
			tw.WriteHeader(h)
			if h.Typeflag == tar.TypeReg {
				tw.Write(make([]byte, h.Size))
			}
		}
		tw.Close()
		gz.Close()
		p := filepath.Join(t.TempDir(), "x.tar.gz")
		os.WriteFile(p, buf.Bytes(), 0o600)
		return p
	}
	outside := filepath.Join(s.Dir, "escaped.txt")
	for _, name := range []string{"../escaped.txt", "/abs.txt", "a/../../escaped.txt"} {
		if _, err := s.Import(ctx, u.ID, mk(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: 3, Mode: 0o644})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a file escaped the import folder")
	}
	// a symlink entry is skipped, so a file written "through" it lands nowhere outside
	p := mk(&tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: s.Dir, Mode: 0o777},
		&tar.Header{Typeflag: tar.TypeReg, Name: "link/escaped.txt", Size: 3, Mode: 0o644})
	s.Import(ctx, u.ID, p)
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("wrote through a symlink")
	}
	if left, _ := filepath.Glob(filepath.Join(s.Dir, "tmp", "import", "*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// The CLI indexes on the spot (SyncIndex): saving a note, and so importing one, must
// not wait on the note lock it already holds.
func TestImportWithSyncIndexDoesNotHang(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := newUser(t, s, "alice")
	b := newUser(t, s, "bob")
	n, _ := s.CreateNote(ctx, a.ID, "")
	s.SaveBody(ctx, a.ID, n.ID, "Rack wiring", s.mustSHA(t, a.ID, n.ID))
	path := writeArchiveFile(t, s, a.ID, n.ID)
	s.SyncIndex = true
	done := make(chan error, 1)
	go func() {
		_, err := s.Import(ctx, b.ID, path)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("import hung (saving under the note lock indexes under it again)")
	}
	if l, _ := s.ListNotes(ctx, b.ID, ListParams{Q: "wiring"}); len(l.Items) != 1 {
		t.Fatalf("not indexed: %+v", l.Items)
	}
}

// Raw text an editor or a damaged disk left (CRLF, NUL, invalid UTF-8) is kept in the
// archive but cleaned on import, so the archive imports and its markers still resolve.
func TestArchiveRawTextImports(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := newUser(t, s, "alice")
	b := newUser(t, s, "bob")
	n, _ := s.CreateNote(ctx, a.ID, "")
	img := uploadPNG(t, s, a.ID, n.ID, pngBytes)
	s.SaveBody(ctx, a.ID, n.ID, "Raw\n[image:"+img.Ref+"]", s.mustSHA(t, a.ID, n.ID))
	os.WriteFile(s.notePath(a.ID, n.ID), []byte("Raw\x00 \xff\r\n[image:"+img.Ref+"]\r\n"), 0o600)
	// bob already uses the ref for other content: the marker must follow the new ref
	bm, _ := s.CreateNote(ctx, b.ID, "")
	s.importAttachment(ctx, b.ID, bm.ID, strings.NewReader("other"), "clash.bin", img.Ref)
	if c, err := s.Import(ctx, b.ID, writeArchiveFile(t, s, a.ID, n.ID)); err != nil || c != 1 {
		t.Fatalf("import: %d %v", c, err)
	}
	l, _ := s.ListNotes(ctx, b.ID, ListParams{})
	var got *Note
	for _, it := range l.Items {
		if it.ID != bm.ID && strings.HasPrefix(it.Title, "Raw") {
			got, _ = s.GetNote(ctx, b.ID, it.ID)
		}
	}
	if got == nil || len(got.Attachments) != 1 || got.Body != "Raw �\n[image:"+got.Attachments[0].Ref+"]\n" || got.Attachments[0].Ref == img.Ref {
		t.Fatalf("imported %+v", got)
	}
}

// A manifest naming a file the archive doesn't hold creates nothing.
func TestImportBrokenManifestCreatesNothing(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	before, _ := s.ListNotes(ctx, u.ID, ListParams{})
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "notes", "N"), 0o700)
	os.WriteFile(filepath.Join(dir, "notes", "N", "note.txt"), []byte("N"), 0o600)
	man := `{"version":2,"notes":[{"id":"x","file":"notes/N/note.txt","title":"N","tags":[],"pinned":false,"trashed":false,
	 "created_at":"","updated_at":"","attachments":[{"ref":"aaaaaaaaaa","filename":"gone.bin","file":"notes/N/files/gone.bin","mime":"","size_bytes":1}]}]}`
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(man), 0o600)
	if _, err := s.Import(ctx, u.ID, dir); err == nil {
		t.Fatal("imported a manifest with a missing file")
	}
	if after, _ := s.ListNotes(ctx, u.ID, ListParams{}); len(after.Items) != len(before.Items) {
		t.Fatalf("%d notes left behind", len(after.Items)-len(before.Items))
	}
}

func tarGz(t *testing.T, name string, write func(tw *tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write(tw)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func addEntry(tw *tar.Writer, name, body string) {
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(body)), Mode: 0o644})
	tw.Write([]byte(body))
}

// What import refuses in a .tar.gz, and that a .tgz of a real archive works.
func TestImportTarGzRefusals(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "Good")
	good, _ := os.ReadFile(writeArchiveFile(t, s, u.ID, n.ID))
	put := func(name string, b []byte) string {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, b, 0o600)
		return p
	}
	if c, err := s.Import(ctx, u.ID, put("good.tgz", good)); err != nil || c != 1 {
		t.Fatalf(".tgz: %d %v", c, err)
	}
	badCRC := bytes.Clone(good)
	badCRC[len(badCRC)-8] ^= 0xff
	cases := map[string][]byte{
		"a damaged checksum": badCRC,
		"a cut-off archive":  good[:len(good)-8],
		"a backup (no manifest)": tarGz(t, "", func(tw *tar.Writer) {
			addEntry(tw, "users/u/notes/n/note.txt", "someone's note")
		}),
		"a name with ..": tarGz(t, "", func(tw *tar.Writer) { addEntry(tw, "notes/../manifest.json", "{}") }),
		"a name with ./": tarGz(t, "", func(tw *tar.Writer) { addEntry(tw, "./manifest.json", "{}") }),
		"an entry twice": tarGz(t, "", func(tw *tar.Writer) {
			addEntry(tw, "manifest.json", "{}")
			addEntry(tw, "manifest.json", "{}")
		}),
		"a sparse file": tarGz(t, "", func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "big", Size: 0, Mode: 0o644,
				PAXRecords: map[string]string{"GNU.sparse.major": "1", "GNU.sparse.minor": "0", "GNU.sparse.realsize": "100000000000"}})
		}),
	}
	for what, b := range cases {
		before, _ := s.ListNotes(ctx, u.ID, ListParams{})
		if c, err := s.Import(ctx, u.ID, put("x.tar.gz", b)); err == nil {
			t.Errorf("%s: imported %d", what, c)
		}
		if after, _ := s.ListNotes(ctx, u.ID, ListParams{}); len(after.Items) != len(before.Items) {
			t.Errorf("%s: created notes", what)
		}
	}
	// past the free-space floor, unpacking stops before writing
	s.Cfg.MinFree = 1 << 62
	if _, err := s.Import(ctx, u.ID, put("good.tar.gz", good)); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("free-space floor: %v", err)
	}
	s.Cfg.MinFree = 0
	// a cancelled import stops
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Import(cctx, u.ID, put("good.tar.gz", good)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(s.Dir, "tmp", "import", "*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// A killed import's folder: doctor doesn't call it a stray upload, the janitor removes it
// once it is a day old.
func TestImportLeftoversCleanedUp(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	newUser(t, s, "alice")
	left := filepath.Join(s.Dir, "tmp", "import", "123")
	os.MkdirAll(filepath.Join(left, "notes", "N"), 0o700)
	os.WriteFile(filepath.Join(left, "notes", "N", "note.txt"), []byte("x"), 0o600)
	r, err := s.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Problems {
		if strings.Contains(p, "import") {
			t.Fatalf("doctor: %s", p)
		}
	}
	s.removeImportLeftovers()
	if _, err := os.Stat(left); err != nil {
		t.Fatal("removed a fresh import folder")
	}
	old := time.Now().Add(-25 * time.Hour)
	os.Chtimes(left, old, old)
	s.removeImportLeftovers()
	if _, err := os.Stat(left); err == nil {
		t.Fatal("a day-old leftover stayed")
	}
}

// This note's copy is gone but another note has one: the copy goes in under the
// marker's ref, and the gone file isn't reported missing.
func TestArchiveSupersededCopy(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	b, _ := s.CreateNote(ctx, u.ID, "B\n[image:"+img.Ref+"] pic")
	bc, _ := scanAtt(s.DB.R.QueryRow(`SELECT `+attCols+` FROM attachments WHERE note_id=?`, b.ID))
	os.Remove(s.attPath(u.ID, &bc))
	ar, got, _ := readArchive(t, s, u.ID, b.ID)
	mn := archiveManifest(t, got)
	if ar.Missing != 0 || got["MISSING.txt"] != nil || len(mn.Attachments) != 1 || mn.Attachments[0].Ref != img.Ref ||
		!bytes.Equal(got[mn.Attachments[0].File], pngBytes) {
		t.Fatalf("missing %d, manifest %+v", ar.Missing, mn)
	}
}

func TestCapFileName(t *testing.T) {
	long := strings.Repeat("é", 150) // 300 bytes
	for in, check := range map[string]func(string) bool{
		"short.png":             func(s string) bool { return s == "short.png" },
		long + ".png":           func(s string) bool { return strings.HasSuffix(s, ".png") && len(s) <= archiveFileBytes },
		long:                    func(s string) bool { return len(s) <= archiveFileBytes && utf8.ValidString(s) },
		long + "." + long:       func(s string) bool { return len(s) <= archiveFileBytes && utf8.ValidString(s) }, // a long "extension" isn't kept
		strings.Repeat("🦊", 70): func(s string) bool { return len(s) <= archiveFileBytes && utf8.ValidString(s) && len(s)%4 == 0 },
	} {
		if got := capFileName(in); !check(got) {
			t.Errorf("capFileName(%d bytes) = %q (%d bytes)", len(in), got, len(got))
		}
	}
}
