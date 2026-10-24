package core

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"homelab-notes/internal/config"
	"homelab-notes/internal/db"
	"homelab-notes/internal/fsx"
)

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 64)...)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// uploadPNG stores a ready image in a note and returns it.
func uploadPNG(t *testing.T, s *Store, uid, noteID string, data []byte) *Attachment {
	t.Helper()
	ctx := context.Background()
	id := NewID()
	if _, err := s.CreateUpload(ctx, uid, UploadMeta{ID: id, Ref: NewRef(), NoteID: noteID, Filename: "shot.png", Length: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.AppendUpload(ctx, uid, id, 0, -1, bytes.NewReader(data))
	if err != nil || a.State != "ready" {
		t.Fatalf("upload: %v %+v", err, a)
	}
	return a
}

func readyCount(t *testing.T, s *Store, noteID, ref string) int {
	t.Helper()
	var n int
	s.DB.R.QueryRow(`SELECT COUNT(*) FROM attachments WHERE note_id=? AND ref=? AND state='ready'`, noteID, ref).Scan(&n)
	return n
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- conversion ----

type legacyFixture struct {
	dir                  string
	uid, a, b, trashed   string
	attID, ref, orphanID string
}

// makeLegacy builds an old flat data folder: notes/<id>.txt and files/<att-id>,
// with schema version 1. Note b shows note a's image; one note is in the trash.
func makeLegacy(t *testing.T) legacyFixture {
	t.Helper()
	f := legacyFixture{dir: t.TempDir(), uid: NewID(), a: NewID(), b: NewID(), trashed: NewID(), attID: NewID(), ref: NewRef(), orphanID: NewID()}
	for _, d := range []string{"db", "tmp", "users", "backups", "lost+found"} {
		os.MkdirAll(filepath.Join(f.dir, d), 0o700)
	}
	d, err := db.Open(f.dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	ms := now()
	exec := func(q string, args ...any) {
		if _, err := d.W.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,created_at,updated_at) VALUES(?,?,?,?,?)`, f.uid, "alice", "x", ms, ms)
	notes := filepath.Join(f.dir, "users", f.uid, "notes")
	files := filepath.Join(f.dir, "users", f.uid, "files")
	os.MkdirAll(notes, 0o700)
	os.MkdirAll(files, 0o700)
	bodies := map[string]string{
		f.a:       "Owner\n[image:" + f.ref + "] shot.png",
		f.b:       "Borrower\n[image:" + f.ref + "] shot.png",
		f.trashed: "Old stuff",
	}
	for id, body := range bodies {
		var trashed any
		if id == f.trashed {
			trashed = ms
		}
		exec(`INSERT INTO notes(id,user_id,title,preview,trashed_at,created_at,updated_at,size_bytes,content_sha256) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, f.uid, DeriveTitle(body), "", trashed, ms, ms, len(body), fsx.HashBytes([]byte(body)))
		os.WriteFile(filepath.Join(notes, id+".txt"), []byte(body), 0o600)
	}
	os.WriteFile(filepath.Join(notes, f.a+".txt.bak"), []byte("Owner"), 0o600)
	exec(`INSERT INTO tags(id,user_id,name,color,created_at) VALUES(?,?,?,?,?)`, "t1", f.uid, "work", "teal", ms)
	exec(`INSERT INTO note_tags(note_id,tag_id,user_id) VALUES(?,?,?)`, f.a, "t1", f.uid)
	exec(`INSERT INTO attachments(id,ref,user_id,note_id,filename,mime,size_bytes,sha256,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		f.attID, f.ref, f.uid, f.a, "shot.png", "image/png", len(pngBytes), fsx.HashBytes(pngBytes), "ready", ms, ms)
	os.WriteFile(filepath.Join(files, f.attID), pngBytes, 0o600)
	os.WriteFile(filepath.Join(files, f.orphanID), []byte("no row"), 0o600) // an orphan
	d.Close()
	return f
}

func openDir(t *testing.T, dir string, mode OpenMode) (*Store, error) {
	t.Helper()
	cfg := &config.Config{DataDir: dir, TrashDays: 30, BackupKeep: 3}
	return OpenWith(context.Background(), cfg, quietLog(), mode)
}

func TestConversionMovesEverything(t *testing.T) {
	f := makeLegacy(t)
	// other commands refuse to touch the old layout, and change nothing
	if _, err := openDir(t, f.dir, ModeRefuse); !errors.Is(err, ErrOldLayout) {
		t.Fatalf("refuse mode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "users", f.uid, "notes", f.a+".txt")); err != nil {
		t.Fatal("refusing changed the folder")
	}
	// database-only commands can run on it
	ls, err := openDir(t, f.dir, ModeLegacy)
	if err != nil || !ls.Legacy {
		t.Fatalf("legacy mode: %v", err)
	}
	ls.Close()

	s, err := openDir(t, f.dir, ModeConvert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, id := range []string{f.a, f.b, f.trashed} {
		if _, err := os.Stat(s.notePath(f.uid, id)); err != nil {
			t.Fatalf("note %s not moved: %v", id, err)
		}
	}
	if b, _ := os.ReadFile(s.notePath(f.uid, f.a) + ".bak"); string(b) != "Owner" {
		t.Fatal(".bak not moved")
	}
	// the trashed note survived (it is not mistaken for an interrupted purge)
	if n, err := s.GetNote(ctx, f.uid, f.trashed); err != nil || n.TrashedAt == nil || n.Body != "Old stuff" {
		t.Fatalf("trashed note: %v %+v", err, n)
	}
	// the owner's image is now files/<ref>.png in its folder
	a, _ := s.getAtt(ctx, f.uid, f.attID)
	if a.Ext != "png" || !fileExists(filepath.Join(s.filesDir(f.uid, f.a), f.ref+".png")) {
		t.Fatalf("image not moved: %+v", a)
	}
	// the borrowing note got its own copy, same ref
	if readyCount(t, s, f.b, f.ref) != 1 || !fileExists(filepath.Join(s.filesDir(f.uid, f.b), f.ref+".png")) {
		t.Fatal("note b has no copy of the image it shows")
	}
	// mirrors
	var m metaFile
	if err := readJSONFile(s.metaPath(f.uid, f.a), &m); err != nil || len(m.Tags) != 1 || m.Tags[0].Color != "teal" || len(m.Attachments) != 1 {
		t.Fatalf("meta.json: %v %+v", err, m)
	}
	var u userFile
	if err := readJSONFile(s.userJSONPath(f.uid), &u); err != nil || u.Username != "alice" {
		t.Fatalf("user.json: %v %+v", err, u)
	}
	// leftovers: the old files/ folder is gone, its orphan kept in lost+found
	if fileExists(s.legacyFilesDir(f.uid)) {
		t.Fatal("old files folder still there")
	}
	found, _ := filepath.Glob(filepath.Join(s.Dir, "lost+found", "layout-v2-*", "users", f.uid, "files", f.orphanID))
	if len(found) != 1 {
		t.Fatal("orphan not kept in lost+found")
	}
	// one verified backup of the old layout, never pruned
	backups, _ := filepath.Glob(filepath.Join(s.Dir, "backups", "pre-layout-v2-*.tar.gz"))
	if len(backups) != 1 || verifyArchive(backups[0]) != nil {
		t.Fatalf("pre-conversion backup: %v", backups)
	}
	if !s.layoutDone() || !fileExists(s.layoutMarker()) {
		t.Fatal("layout not marked done")
	}
	// opening again changes nothing and makes no second backup
	s.Close()
	s2, err := openDir(t, f.dir, ModeRefuse)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if again, _ := filepath.Glob(filepath.Join(f.dir, "backups", "pre-layout-v2-*.tar.gz")); len(again) != 1 {
		t.Fatal("second backup taken")
	}
}

// A conversion that died part-way (some notes moved, an image renamed after its
// extension was stored) finishes on the next start, with no second backup.
func TestConversionResumes(t *testing.T) {
	f := makeLegacy(t)
	s, err := openDir(t, f.dir, ModeLegacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(context.Background(), ""); err != nil { // stand-in for the first run's backup
		t.Fatal(err)
	}
	s.Close()
	os.WriteFile(filepath.Join(f.dir, "backups", "pre-layout-v2.done"), []byte("x\n"), 0o600)
	// first run: schema migrated, one note moved, the image moved after its ext was stored
	d, _ := db.Open(f.dir, 0)
	d.W.Exec(`UPDATE attachments SET ext='png' WHERE id=?`, f.attID)
	d.Close()
	notes := filepath.Join(f.dir, "users", f.uid, "notes")
	os.MkdirAll(filepath.Join(notes, f.a, "files"), 0o700)
	os.Rename(filepath.Join(notes, f.a+".txt"), filepath.Join(notes, f.a, "note.txt"))
	os.Rename(filepath.Join(f.dir, "users", f.uid, "files", f.attID), filepath.Join(notes, f.a, "files", f.ref+".png"))

	s, err = openDir(t, f.dir, ModeConvert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{f.a, f.b, f.trashed} {
		if !fileExists(s.notePath(f.uid, id)) {
			t.Fatalf("note %s missing after resume", id)
		}
	}
	if readyCount(t, s, f.b, f.ref) != 1 {
		t.Fatal("copy not made after resume")
	}
	if b, _ := filepath.Glob(filepath.Join(f.dir, "backups", "pre-layout-v2-*.tar.gz")); len(b) != 0 {
		t.Fatal("resume took a second pre-layout backup")
	}
}

// An image whose only remaining copy was moved by an earlier run (ext not yet
// stored when the process died) is found again by its ref.
func TestConversionFindsMovedImage(t *testing.T) {
	f := makeLegacy(t)
	notes := filepath.Join(f.dir, "users", f.uid, "notes")
	os.MkdirAll(filepath.Join(notes, f.a, "files"), 0o700)
	os.Rename(filepath.Join(f.dir, "users", f.uid, "files", f.attID), filepath.Join(notes, f.a, "files", f.ref+".png"))
	s, err := openDir(t, f.dir, ModeConvert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.getAtt(context.Background(), f.uid, f.attID)
	if a.Ext != "png" {
		t.Fatalf("ext = %q", a.Ext)
	}
	if _, path, err := s.AttachmentFile(context.Background(), f.uid, f.attID); err != nil || !fileExists(path) {
		t.Fatalf("image lost: %v", err)
	}
}

func TestConversionNeedsSpace(t *testing.T) {
	f := makeLegacy(t)
	cfg := &config.Config{DataDir: f.dir, TrashDays: 30, BackupKeep: 3, MinFree: 1 << 62}
	if _, err := OpenWith(context.Background(), cfg, quietLog(), ModeConvert); err == nil || !strings.Contains(err.Error(), "free disk space") {
		t.Fatalf("expected a space error, got %v", err)
	}
	if fileExists(filepath.Join(f.dir, "users", f.uid, "notes", f.a)) {
		t.Fatal("conversion started without enough space")
	}
}

// ---- copies between notes ----

func TestPastedImageGetsItsOwnCopy(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	s.SaveBody(ctx, u.ID, a.ID, "A\n[image:"+img.Ref+"] shot.png", s.mustSHA(t, u.ID, a.ID))

	b, _ := s.CreateNote(ctx, u.ID, "B")
	res, err := s.SaveBody(ctx, u.ID, b.ID, "B\n[image:"+img.Ref+"] shot.png", b.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attachments) != 1 || res.Attachments[0].Ref != img.Ref || res.Attachments[0].NoteID != b.ID {
		t.Fatalf("save did not report the copy: %+v", res.Attachments)
	}
	// purge the original: B keeps its image
	tr := true
	s.PatchNote(ctx, u.ID, a.ID, NotePatch{Trashed: &tr})
	if err := s.PurgeNote(ctx, u.ID, a.ID, "test"); err != nil {
		t.Fatal(err)
	}
	got, err := s.AttachmentByRef(ctx, u.ID, img.Ref)
	if err != nil || got.NoteID != b.ID {
		t.Fatalf("ref after purge: %v %+v", err, got)
	}
	if _, path, err := s.AttachmentFile(ctx, u.ID, got.ID); err != nil {
		t.Fatal(err)
	} else if data, _ := os.ReadFile(path); !bytes.Equal(data, pngBytes) {
		t.Fatal("copy has different bytes")
	}
	// deleting the copy from B's attachments sticks: the next save doesn't bring it back
	if err := s.DeleteAttachment(ctx, u.ID, got.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBody(ctx, u.ID, b.ID, "B edited\n[image:"+img.Ref+"] shot.png", s.mustSHA(t, u.ID, b.ID)); err != nil {
		t.Fatal(err)
	}
	if readyCount(t, s, b.ID, img.Ref) != 0 {
		t.Fatal("deleted image came back")
	}
}

func (s *Store) mustSHA(t *testing.T, uid, id string) string {
	t.Helper()
	n, err := s.GetNote(context.Background(), uid, id)
	if err != nil {
		t.Fatal(err)
	}
	return n.SHA256
}

func TestDuplicateAndCreateCopyImages(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	s.SaveBody(ctx, u.ID, a.ID, "A\n[image:"+img.Ref+"]", s.mustSHA(t, u.ID, a.ID))
	d, err := s.DuplicateNote(ctx, u.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if readyCount(t, s, d.ID, img.Ref) != 1 {
		t.Fatal("duplicate has no copy")
	}
	c, _ := s.CreateNote(ctx, u.ID, "C\n[image:"+img.Ref+"]")
	if readyCount(t, s, c.ID, img.Ref) != 1 || c.AttachmentCount != 1 {
		t.Fatal("note created with a marker has no copy")
	}
}

// A marker pasted while its image is still uploading is copied when the upload finishes.
func TestCopyWaitsForUpload(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	b, _ := s.CreateNote(ctx, u.ID, "B")
	id, ref := NewID(), NewRef()
	if _, err := s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: ref, NoteID: a.ID, Filename: "big.png", Length: int64(len(pngBytes))}); err != nil {
		t.Fatal(err)
	}
	s.SaveBody(ctx, u.ID, b.ID, "B\n[image:"+ref+"]", b.SHA256)
	if !s.exists(ctx, `SELECT 1 FROM pending_copies WHERE note_id=? AND ref=?`, b.ID, ref) {
		t.Fatal("copy not queued")
	}
	if _, _, err := s.AppendUpload(ctx, u.ID, id, 0, -1, bytes.NewReader(pngBytes)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the copy", func() bool { return readyCount(t, s, b.ID, ref) == 1 })
}

// The last copy of an image can't be purged while another note waits for its own copy.
func TestPurgeWaitsForPendingCopy(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	b, _ := s.CreateNote(ctx, u.ID, "B")
	s.Cfg.MinFree = 1 << 62 // every copy fails for lack of space
	s.SaveBody(ctx, u.ID, b.ID, "B\n[image:"+img.Ref+"]", b.SHA256)
	if readyCount(t, s, b.ID, img.Ref) != 0 {
		t.Fatal("copy made despite no space")
	}
	tr := true
	s.PatchNote(ctx, u.ID, a.ID, NotePatch{Trashed: &tr})
	if err := s.PurgeNote(ctx, u.ID, a.ID, "test"); !errors.Is(err, ErrCopyPending) {
		t.Fatalf("purge: %v", err)
	}
	if n, err := s.EmptyTrash(ctx, u.ID); err != nil || n != 0 {
		t.Fatalf("empty trash: %d %v", n, err)
	}
	s.Cfg.MinFree = 0
	if err := s.PurgeNote(ctx, u.ID, a.ID, "test"); err != nil {
		t.Fatal(err) // the purge first finishes the copy, then deletes
	}
	if readyCount(t, s, b.ID, img.Ref) != 1 {
		t.Fatal("copy not made before the purge")
	}
}

// Two uploads racing for the same new ref in different notes: only one wins.
func TestRefIsOneImagePerUser(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	b, _ := s.CreateNote(ctx, u.ID, "B")
	ref := NewRef()
	errs := make(chan error, 2)
	for _, n := range []string{a.ID, b.ID} {
		go func() {
			_, err := s.CreateUpload(ctx, u.ID, UploadMeta{ID: NewID(), Ref: ref, NoteID: n, Filename: "x.png", Length: 3})
			errs <- err
		}()
	}
	e1, e2 := <-errs, <-errs
	if (e1 == nil) == (e2 == nil) || !(errors.Is(e1, ErrRefTaken) || errors.Is(e2, ErrRefTaken)) {
		t.Fatalf("both or neither won: %v / %v", e1, e2)
	}
}

// ---- meta.json ----

func TestMetaFollowsTagChanges(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "tagged")
	tag, _ := s.CreateTag(ctx, u.ID, "home", "sky")
	ids := []string{tag.ID}
	s.PatchNote(ctx, u.ID, n.ID, NotePatch{TagIDs: &ids})
	meta := func() metaFile {
		var m metaFile
		if err := readJSONFile(s.metaPath(u.ID, n.ID), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := meta(); len(m.Tags) != 1 || m.Tags[0].Name != "home" {
		t.Fatalf("after tagging: %+v", m.Tags)
	}
	name, color := "house", "mint"
	s.PatchTag(ctx, u.ID, tag.ID, TagPatch{Name: &name, Color: &color})
	if err := s.DrainMeta(ctx); err != nil {
		t.Fatal(err)
	}
	if m := meta(); m.Tags[0].Name != "house" || m.Tags[0].Color != "mint" {
		t.Fatalf("after rename: %+v", m.Tags)
	}
	s.DeleteTag(ctx, u.ID, tag.ID)
	s.DrainMeta(ctx)
	if m := meta(); len(m.Tags) != 0 {
		t.Fatalf("after delete: %+v", m.Tags)
	}
	// canonical: an up-to-date file is byte-identical to a fresh render
	want, _ := s.buildMeta(ctx, u.ID, n.ID)
	if cur, _ := os.ReadFile(s.metaPath(u.ID, n.ID)); !bytes.Equal(cur, want) {
		t.Fatal("meta.json not canonical")
	}
}

// ---- doctor ----

func TestDoctorUserRepairsAndNeverDeletes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	alice := newUser(t, s, "alice")
	bob := newUser(t, s, "bob")
	a, _ := s.CreateNote(ctx, alice.ID, "A")
	img := uploadPNG(t, s, alice.ID, a.ID, pngBytes)
	b, _ := s.CreateNote(ctx, alice.ID, "B\n[image:"+img.Ref+"]")
	if r, _ := s.DoctorUser(ctx, alice.ID, false); len(r.Problems) != 0 {
		t.Fatalf("clean account has problems: %v", r.Problems)
	}
	// B's copy vanished; A's file too, but there is no other copy for A once B's is gone
	bc, _ := scanAtt(s.DB.R.QueryRow(`SELECT `+attCols+` FROM attachments WHERE note_id=?`, b.ID))
	os.Remove(s.attPath(alice.ID, &bc))
	os.Remove(s.metaPath(alice.ID, a.ID))
	// bob has a stray file that alice's check must not touch
	bn, _ := s.CreateNote(ctx, bob.ID, "bob")
	s.ensureFilesDir(bob.ID, bn.ID)
	bobStray := filepath.Join(s.filesDir(bob.ID, bn.ID), "stray.bin")
	os.WriteFile(bobStray, []byte("x"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(bobStray, old, old)

	r, err := s.DoctorUser(ctx, alice.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Problems) != 2 || len(r.Fixed) != 2 {
		t.Fatalf("report: %+v", r)
	}
	for _, p := range append(r.Problems, r.Fixed...) {
		if strings.Contains(p, s.Dir) || strings.Contains(p, "users/") {
			t.Fatalf("web report shows a server path: %q", p)
		}
	}
	if !fileExists(s.attPath(alice.ID, &bc)) || !fileExists(s.metaPath(alice.ID, a.ID)) {
		t.Fatal("not repaired")
	}
	if !fileExists(bobStray) {
		t.Fatal("alice's check touched bob's files")
	}
	// a file with no other copy is reported, and its row is kept
	os.Remove(s.attPath(alice.ID, &bc))
	ia, _ := s.getAtt(ctx, alice.ID, img.ID)
	os.Remove(s.attPath(alice.ID, ia))
	r, _ = s.DoctorUser(ctx, alice.ID, true)
	if len(r.Problems) != 2 {
		t.Fatalf("missing images: %+v", r)
	}
	if readyCount(t, s, a.ID, img.Ref) != 1 || readyCount(t, s, b.ID, img.Ref) != 1 {
		t.Fatal("doctor deleted image rows")
	}
}

// ---- export / import / rebuild ----

func TestExportFoldersAndRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "Same")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	s.RenameAttachment(ctx, u.ID, img.ID, "note.txt") // must not clash with the note itself
	s.SaveBody(ctx, u.ID, a.ID, "Same\n[image:"+img.Ref+"]", s.mustSHA(t, u.ID, a.ID))
	b, _ := s.CreateNote(ctx, u.ID, "Same\n[image:"+img.Ref+"]") // same title, shared image
	tag, _ := s.CreateTag(ctx, u.ID, "pink one", "pink")
	ids := []string{tag.ID}
	s.PatchNote(ctx, u.ID, b.ID, NotePatch{TagIDs: &ids})

	var buf bytes.Buffer
	if err := s.Export(ctx, u.ID, &buf); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"notes/Same/note.txt", "notes/Same/meta.json", "notes/Same/files/note.txt",
		"notes/Same (2)/note.txt", "notes/Same (2)/files/note.txt", "manifest.json"} {
		if !names[want] {
			t.Fatalf("zip lacks %s: %v", want, names)
		}
	}
	zipPath := filepath.Join(t.TempDir(), "export.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0o600)
	other := newUser(t, s, "bob")
	if n, err := s.Import(ctx, other.ID, zipPath); err != nil || n != 3 { // + the welcome note
		t.Fatalf("import: %d %v", n, err)
	}
	tags, _ := s.ListTags(ctx, other.ID)
	if len(tags) != 1 || tags[0].Color != "pink" {
		t.Fatalf("tag colour lost: %+v", tags)
	}
	// both notes keep the shared ref (same content), each with its own copy
	var refs, rows int
	s.DB.R.QueryRow(`SELECT COUNT(DISTINCT ref), COUNT(*) FROM attachments WHERE user_id=?`, other.ID).Scan(&refs, &rows)
	if refs != 1 || rows != 2 {
		t.Fatalf("imported images: %d refs, %d rows", refs, rows)
	}
}

func TestRebuildDBFromFolders(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, TrashDays: 30, BackupKeep: 3}
	s, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "Kept\nbody")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	s.SaveBody(ctx, u.ID, a.ID, "Kept\nbody\n[image:"+img.Ref+"]", s.mustSHA(t, u.ID, a.ID))
	tag, _ := s.CreateTag(ctx, u.ID, "work", "teal")
	ids := []string{tag.ID}
	pin, tr := true, true
	s.PatchNote(ctx, u.ID, a.ID, NotePatch{TagIDs: &ids, Pinned: &pin})
	gone, _ := s.CreateNote(ctx, u.ID, "In the trash")
	s.PatchNote(ctx, u.ID, gone.ID, NotePatch{Trashed: &tr})
	before, _ := s.GetNote(ctx, u.ID, a.ID)
	s.Close()

	// lose the database
	os.RemoveAll(filepath.Join(dir, "db"))
	os.Remove(filepath.Join(dir, "layout"))
	s, err = Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rep, err := s.RebuildDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Users) != 1 || rep.Notes != 3 || rep.Images != 1 { // welcome note + 2
		t.Fatalf("rebuild: %+v", rep)
	}
	if _, err := s.RebuildDB(ctx); err == nil {
		t.Fatal("rebuild ran on a non-empty database")
	}
	after, err := s.GetNote(ctx, u.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != before.Body || !after.Pinned || after.CreatedAt != before.CreatedAt || len(after.Attachments) != 1 || len(after.TagIDs) != 1 {
		t.Fatalf("note differs after rebuild:\nbefore %+v\nafter  %+v", before, after)
	}
	if g, _ := s.GetNote(ctx, u.ID, gone.ID); g == nil || g.TrashedAt == nil {
		t.Fatal("trash state lost")
	}
	_, hash, _ := s.UserByName(ctx, "alice")
	if hash != noLogin {
		t.Fatal("rebuilt user can log in without a new password")
	}
	if hits, _ := s.ListNotes(ctx, u.ID, ListParams{Q: "Kept"}); len(hits.Items) != 1 {
		t.Fatal("search not rebuilt")
	}
}

// meta.json never trusts a stored path: a crafted file name is skipped.
func TestRebuildIgnoresCraftedPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, TrashDays: 30, BackupKeep: 3}
	uid, nid := NewID(), NewID()
	nd := filepath.Join(dir, "users", uid, "notes", nid)
	os.MkdirAll(filepath.Join(nd, "files"), 0o700)
	os.WriteFile(filepath.Join(dir, "users", uid, "user.json"), []byte(`{"version":1,"id":"`+uid+`","username":"eve","created_at":1}`), 0o600)
	os.WriteFile(filepath.Join(nd, "note.txt"), []byte("hi"), 0o600)
	os.WriteFile(filepath.Join(dir, "secret"), []byte("s"), 0o600)
	meta, _ := json.Marshal(metaFile{Version: 1, ID: nid, CreatedAt: 1, UpdatedAt: 1, Attachments: []metaAtt{
		{Ref: "../../../../secret", Ext: "png"}, {Ref: NewRef(), Ext: "../x"}}})
	os.WriteFile(filepath.Join(nd, "meta.json"), meta, 0o600)
	s, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rep, err := s.RebuildDB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Notes != 1 || rep.Images != 0 {
		t.Fatalf("rebuild: %+v", rep)
	}
}
