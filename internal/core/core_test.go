package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"homelab-notes/internal/config"
	"homelab-notes/internal/fsx"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	cfg := &config.Config{DataDir: t.TempDir(), TrashDays: 30, BackupKeep: 3}
	s, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func newUser(t *testing.T, s *Store, name string) *User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSaveProtocol(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, err := s.CreateNote(ctx, u.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.SaveBody(ctx, u.ID, m.ID, "Title\r\nbody", m.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Title" {
		t.Fatalf("title = %q", res.Title)
	}
	b, _ := os.ReadFile(s.notePath(u.ID, m.ID))
	if string(b) != "Title\nbody" {
		t.Fatalf("file not normalized: %q", b)
	}
	// stale base → conflict carrying the current body
	_, err = s.SaveBody(ctx, u.ID, m.ID, "other", m.SHA256)
	var c *ConflictError
	if !errors.As(err, &c) || c.CurrentBody != "Title\nbody" {
		t.Fatalf("want conflict, got %v", err)
	}
	// previous version kept as .bak
	if _, err := s.SaveBody(ctx, u.ID, m.ID, "v3", res.SHA256); err != nil {
		t.Fatal(err)
	}
	bak, _ := os.ReadFile(s.notePath(u.ID, m.ID) + ".bak")
	if string(bak) != "Title\nbody" {
		t.Fatalf(".bak = %q", bak)
	}
}

func TestConcurrentSavesOnlyOneWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, _ := s.CreateNote(ctx, u.ID, "start")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, conflicts := 0, 0
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.SaveBody(ctx, u.ID, m.ID, strings.Repeat("x", i+1), m.SHA256)
			mu.Lock()
			defer mu.Unlock()
			var c *ConflictError
			switch {
			case err == nil:
				ok++
			case errors.As(err, &c):
				conflicts++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || conflicts != 19 {
		t.Fatalf("ok=%d conflicts=%d, want 1/19", ok, conflicts)
	}
}

func TestReconcileFileWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, _ := s.CreateNote(ctx, u.ID, "old title\nold")
	// simulate a crash after rename, before the row update
	if err := fsx.WriteAtomic(s.notePath(u.ID, m.ID), []byte("new title\nrouter 10.0.0.1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNote(ctx, u.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n.Title != "new title" {
		t.Fatalf("title = %q", n.Title)
	}
	// a save based on the file's hash must succeed (no stale-hash 409 loop)
	if _, err := s.SaveBody(ctx, u.ID, m.ID, "x", n.SHA256); err != nil {
		t.Fatal(err)
	}
}

func TestSearchScopedAndEscaped(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := newUser(t, s, "alice")
	b := newUser(t, s, "bob")
	s.CreateNote(ctx, a.ID, "Router\nip 192.168.1.20 vlan 30")
	s.CreateNote(ctx, b.ID, "Bob secret\nip 192.168.1.99")
	if _, err := s.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	l, err := s.ListNotes(ctx, a.ID, ListParams{Q: "168.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || l.Items[0].Title != "Router" {
		t.Fatalf("alice search: %+v", l.Items)
	}
	if len(l.Items[0].Snippet) == 0 {
		t.Fatal("expected snippet")
	}
	for _, q := range []string{`"`, `a OR b`, `NEAR(`, `*`, `x"y`, `--`} {
		if _, err := s.ListNotes(ctx, a.ID, ListParams{Q: q}); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
}

func TestTrashAndPurgeRemovesFiles(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, _ := s.CreateNote(ctx, u.ID, "doomed")
	id, ref := NewID(), NewRef()
	if _, err := s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: ref, NoteID: m.ID, Filename: "a.bin", Length: 3}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AppendUpload(ctx, u.ID, id, 0, -1, bytes.NewReader([]byte("abc"))); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeNote(ctx, u.ID, m.ID, "test"); err == nil {
		t.Fatal("purge of non-trashed note should fail")
	}
	tr := true
	if _, err := s.PatchNote(ctx, u.ID, m.ID, NotePatch{Trashed: &tr}); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeNote(ctx, u.ID, m.ID, "test"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.notePath(u.ID, m.ID), s.filePath(u.ID, id)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists", p)
		}
	}
}

func TestUploadOwnershipAndResume(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := newUser(t, s, "alice")
	b := newUser(t, s, "bob")
	m, _ := s.CreateNote(ctx, a.ID, "n")
	id := NewID()
	if _, err := s.CreateUpload(ctx, b.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: m.ID, Filename: "x", Length: 6}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob attaching to alice's note: %v", err)
	}
	if _, err := s.CreateUpload(ctx, a.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: m.ID, Filename: "x.png", Length: 6}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AppendUpload(ctx, b.ID, id, 0, -1, strings.NewReader("abc")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob appending: %v", err)
	}
	if _, off, err := s.AppendUpload(ctx, a.ID, id, 0, -1, strings.NewReader("abc")); err != nil || off != 3 {
		t.Fatalf("chunk1 off=%d err=%v", off, err)
	}
	var mm *OffsetMismatchError
	if _, _, err := s.AppendUpload(ctx, a.ID, id, 0, -1, strings.NewReader("def")); !errors.As(err, &mm) || mm.Actual != 3 {
		t.Fatalf("want offset mismatch at 3, got %v", err)
	}
	att, _, err := s.AppendUpload(ctx, a.ID, id, 3, -1, strings.NewReader("def"))
	if err != nil || att.State != "ready" {
		t.Fatalf("finalize: %v %+v", err, att)
	}
	// a non-image claiming .png must not be served inline
	if att.Inline || att.Mime == "image/png" {
		t.Fatalf("fake png treated as image: %+v", att)
	}
}

func TestMarkersAndTitles(t *testing.T) {
	cases := map[string]bool{
		"[image:abcdefghij] shot.png": true,
		"[image:abcdefghij]":          true,
		" [image:abcdefghij]":         false,
		"[image:ABCDEFGHIJ]":          false,
		"[image:abcdefgh] x":          false,
		"[image:abcdefghij]x":         false,
	}
	for in, want := range cases {
		if IsMarker(in) != want {
			t.Errorf("IsMarker(%q) = %v", in, !want)
		}
	}
	if got := DeriveTitle("\n[image:abcdefghij] a.png\n  Real title  \nbody"); got != "Real title" {
		t.Errorf("title = %q", got)
	}
	if got := DeriveTitle(""); got != "Untitled" {
		t.Errorf("empty title = %q", got)
	}
	if _, err := NormalizeBody("bad\x00"); err == nil {
		t.Error("NUL accepted")
	}
	if _, err := NormalizeBody(string([]byte{0xff})); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
}

func TestBackupAndDoctor(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	s.CreateNote(ctx, u.ID, "keep me")
	out, err := s.Backup(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Fatalf("backup missing: %v", err)
	}
	orphan := s.filePath(u.ID, NewID())
	os.WriteFile(orphan, []byte("orphan"), 0o600)
	r, err := s.Doctor(ctx, false)
	if err != nil || len(r.Problems) != 1 {
		t.Fatalf("doctor problems=%v err=%v", r.Problems, err)
	}
	if r, _ = s.Doctor(ctx, true); len(r.Fixed) != 0 {
		t.Fatalf("doctor moved a brand-new file: %+v", r)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(orphan, old, old)
	if r, _ = s.Doctor(ctx, true); len(r.Fixed) != 1 {
		t.Fatalf("doctor fix: %+v", r)
	}
	if r, _ = s.Doctor(ctx, false); len(r.Problems) != 0 {
		t.Fatalf("still problems: %v", r.Problems)
	}
}

func TestRemoveUserKeepsFiles(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	moved, err := s.RemoveUser(ctx, "alice", false)
	if err != nil || moved == "" {
		t.Fatalf("moved=%q err=%v", moved, err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("files not kept")
	}
	if _, _, err := s.UserByName(ctx, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatal("user still exists")
	}
	_ = u
}

func upload(t *testing.T, s *Store, uid, noteID, name, content string) *Attachment {
	t.Helper()
	ctx := context.Background()
	id, ref := NewID(), NewRef()
	if _, err := s.CreateUpload(ctx, uid, UploadMeta{ID: id, Ref: ref, NoteID: noteID, Filename: name, Length: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.AppendUpload(ctx, uid, id, 0, -1, strings.NewReader(content))
	if err != nil || a.State != "ready" {
		t.Fatalf("upload: %v %+v", err, a)
	}
	return a
}

func TestSaveIsIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, _ := s.CreateNote(ctx, u.ID, "v1")
	if _, err := s.SaveBody(ctx, u.ID, m.ID, "v2", m.SHA256); err != nil {
		t.Fatal(err)
	}
	// retry with the stale base after a "lost response": same body → success, not 409
	if _, err := s.SaveBody(ctx, u.ID, m.ID, "v2", m.SHA256); err != nil {
		t.Fatalf("retry of an applied save: %v", err)
	}
}

func TestDuplicateKeepsImagesAfterOriginalPurged(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, _ := s.CreateNote(ctx, u.ID, "")
	a := upload(t, s, u.ID, m.ID, "pic.png", "\x89PNG\r\n\x1a\nfake")
	body := "Title\n[image:" + a.Ref + "] pic.png"
	if _, err := s.SaveBody(ctx, u.ID, m.ID, body, m.SHA256); err != nil {
		t.Fatal(err)
	}
	d, err := s.DuplicateNote(ctx, u.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	tr := true
	s.PatchNote(ctx, u.ID, m.ID, NotePatch{Trashed: &tr})
	if err := s.PurgeNote(ctx, u.ID, m.ID, "test"); err != nil {
		t.Fatal(err)
	}
	dn, err := s.GetNote(ctx, u.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dn.Attachments) != 1 || !strings.Contains(dn.Body, "[image:"+dn.Attachments[0].Ref+"]") {
		t.Fatalf("duplicate lost its image: %+v %q", dn.Attachments, dn.Body)
	}
	if _, path, err := s.AttachmentFile(ctx, u.ID, dn.Attachments[0].ID); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(path); err != nil {
		t.Fatal("duplicate's file missing")
	}
}

func TestFinalizeResumesAfterCrash(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, _ := s.CreateNote(ctx, u.ID, "n")
	id := NewID()
	s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: m.ID, Filename: "f.bin", Length: 3})
	os.WriteFile(s.tmpPath(u.ID, id), []byte("abc"), 0o600)
	// crash after the rename, before the row update
	if err := os.Rename(s.tmpPath(u.ID, id), s.filePath(u.ID, id)); err != nil {
		t.Fatal(err)
	}
	a, off, err := s.UploadOffset(ctx, u.ID, id)
	if err != nil || off != 3 || a.State != "ready" {
		t.Fatalf("resume finalize: off=%d err=%v a=%+v", off, err, a)
	}
	// create is idempotent for the same id
	if _, err := s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: a.Ref, NoteID: m.ID, Filename: "f.bin", Length: 3}); err != nil {
		t.Fatalf("repeat create: %v", err)
	}
}

func TestGCDoesNotEatFreshUploads(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, _ := s.CreateNote(ctx, u.ID, "n")
	id := NewID()
	s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: m.ID, Filename: "f", Length: 10})
	if n, _ := s.GCUploads(ctx, 24*time.Hour); n != 0 {
		t.Fatal("fresh upload collected")
	}
	if n, _ := s.GCUploads(ctx, -time.Second); n != 1 {
		t.Fatal("stale upload not collected")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := newUser(t, s, "alice")
	b := newUser(t, s, "bob")
	m, _ := s.CreateNote(ctx, a.ID, "")
	att := upload(t, s, a.ID, m.ID, "pic.png", "\x89PNG\r\n\x1a\nfake")
	s.SaveBody(ctx, a.ID, m.ID, "Rack\n[image:"+att.Ref+"] pic.png", m.SHA256)
	tag, _ := s.CreateTag(ctx, a.ID, "homelab", "")
	pin := true
	s.PatchNote(ctx, a.ID, m.ID, NotePatch{Pinned: &pin, TagIDs: &[]string{tag.ID}})
	// bob already uses alice's ref → import must remap the marker
	bm, _ := s.CreateNote(ctx, b.ID, "")
	s.importAttachment(ctx, b.ID, bm.ID, strings.NewReader("x"), "clash.bin", att.Ref)

	zipPath := filepath.Join(t.TempDir(), "export.zip")
	f, _ := os.Create(zipPath)
	if err := s.Export(ctx, a.ID, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	n, err := s.Import(ctx, b.ID, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 { // welcome note + Rack
		t.Fatalf("imported %d", n)
	}
	l, _ := s.ListNotes(ctx, b.ID, ListParams{Pinned: true})
	if len(l.Items) != 1 || l.Items[0].Title != "Rack" || len(l.Items[0].TagIDs) != 1 {
		t.Fatalf("pinned/tagged note not restored: %+v", l.Items)
	}
	got, _ := s.GetNote(ctx, b.ID, l.Items[0].ID)
	if len(got.Attachments) != 1 || got.Attachments[0].Ref == att.Ref || !strings.Contains(got.Body, "[image:"+got.Attachments[0].Ref+"]") {
		t.Fatalf("attachment/marker not remapped: %+v %q", got.Attachments, got.Body)
	}
}

func TestSearchPaginatesAndBadCursor(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	for range 7 {
		s.CreateNote(ctx, u.ID, "needle note\nbody")
	}
	s.Reindex(ctx)
	seen := map[string]bool{}
	cur := ""
	for {
		l, err := s.ListNotes(ctx, u.ID, ListParams{Q: "needle", Limit: 3, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range l.Items {
			if seen[it.ID] {
				t.Fatal("duplicate across pages")
			}
			seen[it.ID] = true
		}
		if l.NextCursor == "" {
			break
		}
		cur = l.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("paged %d of 7", len(seen))
	}
	var inv *InvalidError
	if _, err := s.ListNotes(ctx, u.ID, ListParams{Cursor: "eyJwIjowLCJ2Ijp7ImEiOjF9LCJpZCI6IngifQ"}); !errors.As(err, &inv) {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestTitleKeepsGraphemes(t *testing.T) {
	family := "👨\u200d👩\u200d👧"
	title := DeriveTitle(strings.Repeat("a", 117) + family) // naive cut would end on a ZWJ
	if strings.ContainsRune(title, '\u200d') && !strings.Contains(title, family) {
		t.Fatalf("split a ZWJ sequence: %q", title)
	}
}

func TestReconcileFinishesInterruptedPurge(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, _ := s.CreateNote(ctx, u.ID, "ghost")
	tr := true
	s.PatchNote(ctx, u.ID, m.ID, NotePatch{Trashed: &tr})
	os.Remove(s.notePath(u.ID, m.ID)) // crash after files deleted, before the row
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNote(ctx, u.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost note still there: %v", err)
	}
}
