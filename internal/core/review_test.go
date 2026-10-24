package core

// Regression tests for the beta code review of the note-folders change.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"homelab-notes/internal/db"
)

// A save adding a marker races the background copy run after an upload: the
// request must never be dropped, and the save reports the copy either way.
func TestSaveRacesPendingCopy(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	for i := range 25 {
		b, _ := s.CreateNote(ctx, u.ID, "B")
		var wg sync.WaitGroup
		var res *SaveResult
		wg.Add(2)
		go func() {
			defer wg.Done()
			res, _ = s.SaveBody(ctx, u.ID, b.ID, "B\n[image:"+img.Ref+"]", b.SHA256)
		}()
		go func() {
			defer wg.Done()
			s.runPendingRef(ctx, u.ID, img.Ref)
		}()
		wg.Wait()
		s.runAllPending(ctx)
		if readyCount(t, s, b.ID, img.Ref) != 1 {
			t.Fatalf("run %d: copy lost", i)
		}
		if res == nil || len(res.Attachments) != 1 {
			t.Fatalf("run %d: save didn't report the copy: %+v", i, res)
		}
	}
}

// Two copies deleted at once while a third note waits: exactly one delete wins.
func TestConcurrentDeletesKeepLastCopy(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	b, _ := s.CreateNote(ctx, u.ID, "B\n[image:"+img.Ref+"]")
	bc, _ := scanAtt(s.DB.R.QueryRow(`SELECT `+attCols+` FROM attachments WHERE note_id=?`, b.ID))
	s.Cfg.MinFree = 1 << 62 // the third note can't get its copy
	c, _ := s.CreateNote(ctx, u.ID, "C\n[image:"+img.Ref+"]")
	if readyCount(t, s, c.ID, img.Ref) != 0 {
		t.Fatal("setup: copy made")
	}
	errs := make(chan error, 2)
	for _, id := range []string{img.ID, bc.ID} {
		go func() { errs <- s.DeleteAttachment(ctx, u.ID, id) }()
	}
	e1, e2 := <-errs, <-errs
	if (e1 == nil) == (e2 == nil) || !(errors.Is(e1, ErrCopyPending) || errors.Is(e2, ErrCopyPending)) {
		t.Fatalf("want one success and one copy_pending: %v / %v", e1, e2)
	}
	var ready int
	s.DB.R.QueryRow(`SELECT COUNT(*) FROM attachments WHERE ref=? AND state='ready'`, img.Ref).Scan(&ready)
	if ready != 1 {
		t.Fatalf("%d ready copies left", ready)
	}
}

func TestDuplicateCopiesPanelOnlyFiles(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A has a file in its panel only")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	d, err := s.DuplicateNote(ctx, u.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	tr := true
	s.PatchNote(ctx, u.ID, a.ID, NotePatch{Trashed: &tr})
	if err := s.PurgeNote(ctx, u.ID, a.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if readyCount(t, s, d.ID, img.Ref) != 1 {
		t.Fatal("the duplicate lost the panel-only file")
	}
}

// Finalize keeps the uploaded bytes when the note's folder can't be written for
// a reason other than a purge, so the upload can still complete.
func TestFinalizeKeepsBytesOnFolderError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "N")
	id := NewID()
	s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: n.ID, Filename: "x.png", Length: int64(len(pngBytes))})
	os.Chmod(s.noteDir(u.ID, n.ID), 0o500)
	_, _, err := s.AppendUpload(ctx, u.ID, id, 0, -1, bytes.NewReader(pngBytes))
	os.Chmod(s.noteDir(u.ID, n.ID), 0o700)
	if err == nil {
		t.Fatal("expected the finalize to fail")
	}
	if !fileExists(s.tmpPath(u.ID, id)) {
		t.Fatal("finalize deleted the only copy of the upload")
	}
	a, _, err := s.AppendUpload(ctx, u.ID, id, int64(len(pngBytes)), -1, bytes.NewReader(nil))
	if err != nil || a.State != "ready" {
		t.Fatalf("retry: %v %+v", err, a)
	}
}

// A stale hash from the old database must not stop the conversion (or the server).
func TestConversionSurvivesStaleHash(t *testing.T) {
	f := makeLegacy(t)
	d, _ := db.Open(f.dir, 1)
	d.W.Exec(`UPDATE attachments SET sha256='0000' WHERE id=?`, f.attID)
	d.Close()
	s, err := openDir(t, f.dir, ModeConvert)
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	defer s.Close()
	if readyCount(t, s, f.b, f.ref) != 1 {
		t.Fatal("copy not made")
	}
	if got := s.refSHA(context.Background(), f.uid, f.ref); got == "0000" || got == "" {
		t.Fatalf("hash not taken from the file: %q", got)
	}
}

// A copy that keeps failing stays queued; the conversion still finishes.
func TestConversionFinishesWhenACopyFails(t *testing.T) {
	f := makeLegacy(t)
	s, err := openDir(t, f.dir, ModeLegacy)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	cfgDir := f.dir
	// make the owner's image unreadable as a copy source after it is moved: simulate
	// by giving the borrower's folder a non-directory "files"
	os.MkdirAll(filepath.Join(cfgDir, "users", f.uid, "notes", f.b), 0o700)
	os.WriteFile(filepath.Join(cfgDir, "users", f.uid, "notes", f.b, "files"), []byte("not a folder"), 0o600)
	s, err = openDir(t, cfgDir, ModeConvert)
	if err != nil {
		t.Fatalf("one failed copy stopped the conversion: %v", err)
	}
	defer s.Close()
	if !s.layoutDone() {
		t.Fatal("layout not marked done")
	}
	if !s.exists(context.Background(), `SELECT 1 FROM pending_copies WHERE note_id=?`, f.b) {
		t.Fatal("the failed copy isn't queued for a retry")
	}
}

func TestLostDatabaseOnOldLayoutRefuses(t *testing.T) {
	f := makeLegacy(t)
	os.RemoveAll(filepath.Join(f.dir, "db"))
	for _, mode := range []OpenMode{ModeConvert, ModeRefuse} {
		if _, err := openDir(t, f.dir, mode); !errors.Is(err, ErrLostDBOldLayout) {
			t.Fatalf("mode %d: %v", mode, err)
		}
	}
	if !fileExists(filepath.Join(f.dir, "users", f.uid, "notes", f.a+".txt")) || fileExists(filepath.Join(f.dir, "layout")) {
		t.Fatal("the old folder was touched or marked converted")
	}
}

func TestStrayUserFolderDoesNotBlockStartup(t *testing.T) {
	dir := t.TempDir()
	s, err := openDir(t, dir, ModeConvert)
	if err != nil {
		t.Fatal(err)
	}
	newUser(t, s, "alice")
	s.Close()
	os.MkdirAll(filepath.Join(dir, "users", NewID(), "files"), 0o700) // a folder of no user, old style
	s, err = openDir(t, dir, ModeRefuse)
	if err != nil {
		t.Fatalf("a stray folder blocked the commands: %v", err)
	}
	s.Close()
}

// The archive was written but the run died before recording it: it is adopted.
func TestConversionAdoptsUnrecordedBackup(t *testing.T) {
	f := makeLegacy(t)
	s, _ := openDir(t, f.dir, ModeLegacy)
	if _, err := s.backup(context.Background(), "", "pre-layout-v2-", false); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := openDir(t, f.dir, ModeConvert)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if b, _ := filepath.Glob(filepath.Join(f.dir, "backups", "pre-layout-v2-*.tar.gz")); len(b) != 1 {
		t.Fatalf("backups: %v", b)
	}
}

// An old backup restored over a converted folder is converted again with a backup of its own.
func TestReconversionTakesItsOwnBackup(t *testing.T) {
	f := makeLegacy(t)
	s, err := openDir(t, f.dir, ModeConvert)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// an old-layout note file reappears (as after restoring an old backup on top)
	os.WriteFile(filepath.Join(f.dir, "users", f.uid, "notes", f.trashed+".txt"), []byte("Old stuff"), 0o600)
	d, _ := db.Open(f.dir, 0)
	d.W.Exec(`DELETE FROM settings`)
	d.Close()
	s, err = openDir(t, f.dir, ModeConvert)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if b, _ := filepath.Glob(filepath.Join(f.dir, "backups", "pre-layout-v2-*.tar.gz")); len(b) != 2 {
		t.Fatalf("want a second backup, have %v", b)
	}
}

func TestDoctorWebMovesWithoutPaths(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "N")
	s.ensureFilesDir(u.ID, n.ID)
	stray := filepath.Join(s.filesDir(u.ID, n.ID), "stray.bin")
	os.WriteFile(stray, []byte("x"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(stray, old, old)
	r, err := s.DoctorUser(ctx, u.ID, true)
	if err != nil || len(r.Fixed) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	for _, line := range append(r.Problems, r.Fixed...) {
		if strings.Contains(line, "users/") || strings.Contains(line, "lost+found") || strings.Contains(line, s.Dir) {
			t.Fatalf("server path in the web report: %q", line)
		}
	}
}

func TestDoctorNeverOverwritesASave(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "fresh text")
	if s.restoreNoteText(ctx, u.ID, n.ID, []byte("older")) {
		t.Fatal("restored over an existing note.txt")
	}
	if b, _ := os.ReadFile(s.notePath(u.ID, n.ID)); string(b) != "fresh text" {
		t.Fatalf("text = %q", b)
	}
}

func TestDoctorUnmountedVolume(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	s.CreateNote(ctx, u.ID, "N")
	moved := s.notesDir(u.ID) + ".away"
	os.Rename(s.notesDir(u.ID), moved)
	r, _ := s.DoctorUser(ctx, u.ID, true)
	if len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "mounted") || len(r.Fixed) != 0 {
		t.Fatalf("%+v", r)
	}
	if fileExists(s.notesDir(u.ID)) {
		t.Fatal("doctor recreated notes on the wrong volume")
	}
}

func TestDoctorChecksRealContent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	waitFor(t, "the hash", func() bool { return s.refSHA(ctx, u.ID, img.Ref) != "" })
	cur, _ := s.getAtt(ctx, u.ID, img.ID)
	os.WriteFile(s.attPath(u.ID, cur), []byte("tampered"), 0o600)
	r, _ := s.DoctorUser(ctx, u.ID, false)
	if len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "different content") {
		t.Fatalf("%+v", r)
	}
}

// An old export (borrowers carry only the marker) imported into an account where
// the ref already means another image: the borrower gets the exported image.
func TestImportFollowsRemappedRefs(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	alice := newUser(t, s, "alice")
	bob := newUser(t, s, "bob")
	// bob already has a different image under the ref alice's export will use
	bn, _ := s.CreateNote(ctx, bob.ID, "bob's")
	other := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{9}, 64)...)
	bobImg := uploadPNG(t, s, bob.ID, bn.ID, other)

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a"), 0o700)
	os.WriteFile(filepath.Join(dir, "a", "note.txt"), []byte("Owner\n[image:"+bobImg.Ref+"]"), 0o600)
	os.WriteFile(filepath.Join(dir, "a", "shot.png"), pngBytes, 0o600)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("Borrower\n[image:"+bobImg.Ref+"]"), 0o600)
	man, _ := json.Marshal(map[string]any{"notes": []map[string]any{
		{"id": NewID(), "file": "a/note.txt", "title": "Owner", "attachments": []map[string]any{
			{"ref": bobImg.Ref, "filename": "shot.png", "file": "a/shot.png"}}},
		{"id": NewID(), "file": "b.txt", "title": "Borrower", "attachments": []map[string]any{}},
	}})
	os.WriteFile(filepath.Join(dir, "manifest.json"), man, 0o600)
	_ = alice
	if _, err := s.Import(ctx, bob.ID, dir); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListNotes(ctx, bob.ID, ListParams{Q: "Borrower"})
	if len(list.Items) != 1 {
		t.Fatal("borrower not imported")
	}
	n, _ := s.GetNote(ctx, bob.ID, list.Items[0].ID)
	if strings.Contains(n.Body, "[image:"+bobImg.Ref+"]") {
		t.Fatal("borrower still points at bob's unrelated image")
	}
	if len(n.Attachments) != 1 {
		t.Fatalf("borrower has %d images", len(n.Attachments))
	}
	_, path, _ := s.AttachmentFile(ctx, bob.ID, n.Attachments[0].ID)
	if b, _ := os.ReadFile(path); !bytes.Equal(b, pngBytes) {
		t.Fatal("borrower shows the wrong image")
	}
}

func TestRebuildKeepsWhatItCannotRestore(t *testing.T) {
	dir := t.TempDir()
	s, _ := openDir(t, dir, ModeConvert)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	a, _ := s.CreateNote(ctx, u.ID, "A")
	img := uploadPNG(t, s, u.ID, a.ID, pngBytes)
	s.SaveBody(ctx, u.ID, a.ID, "A\n[image:"+img.Ref+"]", s.mustSHA(t, u.ID, a.ID))
	b, _ := s.CreateNote(ctx, u.ID, "B\n[image:"+img.Ref+"]")
	cur, _ := s.getAtt(ctx, u.ID, img.ID)
	s.Close()
	os.Remove(s.attPath(u.ID, cur)) // A's file is lost; B's copy survives
	os.RemoveAll(filepath.Join(dir, "db"))
	s, err := openDir(t, dir, ModeRefuse)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RebuildDB(ctx); err != nil {
		t.Fatal(err)
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "lost+found", "rebuild-*", "users", u.ID, "notes", a.ID, "meta.json"))
	if len(kept) != 1 {
		t.Fatal("original meta.json of the note with a missing image not kept")
	}
	// A shows the image: it is copied back from B
	if readyCount(t, s, a.ID, img.Ref) != 1 || readyCount(t, s, b.ID, img.Ref) != 1 {
		t.Fatal("copy not made again after the rebuild")
	}
}

func TestExportAvoidsWindowsReservedNames(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "CON")
	img := uploadPNG(t, s, u.ID, n.ID, pngBytes)
	s.RenameAttachment(ctx, u.ID, img.ID, `nul.png`)
	var buf bytes.Buffer
	if err := s.Export(ctx, u.ID, &buf); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["notes/_CON/note.txt"] || !names["notes/_CON/files/_nul.png"] {
		t.Fatalf("names: %v", names)
	}
}

// A tag change landing while its notes' meta.json is being rewritten isn't lost.
func TestDrainMetaRequeuesDuringWrite(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "tagged")
	tag, _ := s.CreateTag(ctx, u.ID, "one", "sky")
	ids := []string{tag.ID}
	s.PatchNote(ctx, u.ID, n.ID, NotePatch{TagIDs: &ids})
	for i := range 20 {
		var wg sync.WaitGroup
		color := Palette[i%len(Palette)]
		wg.Add(2)
		go func() { defer wg.Done(); s.PatchTag(ctx, u.ID, tag.ID, TagPatch{Color: &color}) }()
		go func() { defer wg.Done(); s.DrainMeta(ctx) }()
		wg.Wait()
		s.DrainMeta(ctx)
		var m metaFile
		readJSONFile(s.metaPath(u.ID, n.ID), &m)
		if len(m.Tags) != 1 || m.Tags[0].Color != color {
			t.Fatalf("run %d: meta.json colour %+v, want %s", i, m.Tags, color)
		}
	}
}

// A record pointing at a backup that is gone doesn't count: a new one is taken.
func TestConversionChecksRecordedBackup(t *testing.T) {
	f := makeLegacy(t)
	os.WriteFile(filepath.Join(f.dir, "backups", "pre-layout-v2.done"), []byte("pre-layout-v2-gone.tar.gz\n"), 0o600)
	s, err := openDir(t, f.dir, ModeConvert)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if b, _ := filepath.Glob(filepath.Join(f.dir, "backups", "pre-layout-v2-*.tar.gz")); len(b) != 1 {
		t.Fatalf("no backup taken: %v", b)
	}
}

// A trashed note whose text was already gone (an interrupted purge) is finished, not revived.
func TestConversionFinishesInterruptedPurge(t *testing.T) {
	f := makeLegacy(t)
	os.Remove(filepath.Join(f.dir, "users", f.uid, "notes", f.trashed+".txt"))
	s, err := openDir(t, f.dir, ModeConvert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNote(context.Background(), f.uid, f.trashed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("interrupted purge revived: %v", err)
	}
}

// After a lost database, doctor --fix leaves account folders for rebuild-db.
func TestDoctorKeepsFoldersOfUnknownAccounts(t *testing.T) {
	dir := t.TempDir()
	s, _ := openDir(t, dir, ModeConvert)
	u := newUser(t, s, "alice")
	s.Close()
	os.RemoveAll(filepath.Join(dir, "db"))
	s, err := openDir(t, dir, ModeRefuse)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "users", u.ID), old, old)
	r, _ := s.Doctor(context.Background(), true)
	if !fileExists(filepath.Join(dir, "users", u.ID, "user.json")) {
		t.Fatalf("the account folder was moved: %+v", r)
	}
	if len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "rebuild-db") {
		t.Fatalf("%+v", r)
	}
}

// An upload whose bytes were moved before a crash is finished by the janitor, not deleted.
func TestUploadGCFinishesMovedUpload(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "N")
	id := NewID()
	s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: n.ID, Filename: "x.png", Length: int64(len(pngBytes))})
	os.WriteFile(s.tmpPath(u.ID, id), pngBytes, 0o600)
	crashAfterMove(t, s, u.ID, id)
	s.DB.W.Exec(`UPDATE attachments SET updated_at=0 WHERE id=?`, id)
	if _, err := s.GCUploads(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	a, err := s.getAtt(ctx, u.ID, id)
	if err != nil || a.State != "ready" || !fileExists(s.attPath(u.ID, a)) {
		t.Fatalf("upload lost: %v %+v", err, a)
	}
}
