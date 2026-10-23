package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"homelab-notes/internal/auth"
	"homelab-notes/internal/fsx"
)

// Two status checks racing to complete a half-finalized upload must not delete it.
func TestConcurrentRecoveryKeepsFile(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	n, _ := s.CreateNote(ctx, u.ID, "")
	id := NewID()
	if _, err := s.CreateUpload(ctx, u.ID, UploadMeta{ID: id, Ref: NewRef(), NoteID: n.ID, Filename: "a.bin", Length: 3}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.tmpPath(u.ID, id), []byte("abc"), 0o600)
	// crash after the rename, before the row was marked ready
	if err := fsx.MoveDurable(s.tmpPath(u.ID, id), s.filePath(u.ID, id)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, off, err := s.UploadOffset(ctx, u.ID, id)
			if err == nil && off != 3 {
				err = errors.New("wrong offset")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(s.filePath(u.ID, id)); err != nil {
		t.Fatalf("finished upload lost its file: %v", err)
	}
	a, _ := s.getAtt(ctx, u.ID, id)
	if a.State != "ready" {
		t.Fatalf("state %s", a.State)
	}
}

// Concurrent uploads must not each pass the free-space check against the same bytes.
func TestSpaceLeasesAreShared(t *testing.T) {
	s := newStore(t)
	free, err := fsx.FreeBytes(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Cfg.MinFree = free - 100<<20 // 100 MiB of headroom
	a, err := s.reserve(60 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.reserve(60 << 20); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("second upload should not fit alongside the first: %v", err)
	}
	a.release()
	b, err := s.reserve(60 << 20)
	if err != nil {
		t.Fatalf("space not returned after release: %v", err)
	}
	b.release()
}

// Every match is reachable by paging, including a note that was just re-saved
// (re-indexing used to push it past a 1000-hit cap).
func TestSearchHasNoHiddenCap(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 1050 notes")
	}
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	var last *NoteMeta
	for range 1050 {
		m, err := s.CreateNote(ctx, u.ID, "zzzcommon\nbody")
		if err != nil {
			t.Fatal(err)
		}
		last = m
	}
	s.Reindex(ctx)
	full, _ := s.GetNote(ctx, u.ID, last.ID)
	if _, err := s.SaveBody(ctx, u.ID, last.ID, "zzzcommon\nedited", full.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := s.indexNote(ctx, last.ID); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	cur := ""
	for {
		l, err := s.ListNotes(ctx, u.ID, ListParams{Q: "zzzcommon", Limit: 200, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range l.Items {
			seen[it.ID] = true
		}
		if l.NextCursor == "" {
			break
		}
		cur = l.NextCursor
	}
	if len(seen) != 1050 || !seen[last.ID] {
		t.Fatalf("found %d of 1050 (re-saved note found: %v)", len(seen), seen[last.ID])
	}
}

func TestSearchRejectsNUL(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	s.CreateNote(ctx, u.ID, "hello")
	l, err := s.ListNotes(ctx, u.ID, ListParams{Q: "\x00abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 0 {
		t.Fatalf("NUL query matched %d notes", len(l.Items))
	}
}

func TestSnippetForDeepMatch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	body := "Big\n" + strings.Repeat("filler text ", 30000) + "needle-at-the-end here"
	m, err := s.CreateNote(ctx, u.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	items := []NoteMeta{{ID: m.ID}}
	s.addSnippets(u.ID, "needle-at-the-end", items)
	if items[0].Snippet == nil {
		t.Fatal("no snippet for a match past the first chunk")
	}
}

func TestTitleKeepsFlags(t *testing.T) {
	title := DeriveTitle(strings.Repeat("a", 118) + "🇬🇧 and more text")
	r := []rune(strings.TrimSuffix(title, "…"))
	ri := 0
	for _, c := range r {
		if isRegionalIndicator(c) {
			ri++
		}
	}
	if ri%2 != 0 {
		t.Fatalf("split a flag: %q", title)
	}
}

// A password change racing with an admin reset must not undo the reset.
func TestChangePasswordLosesToReset(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	oldHash, _ := s.PasswordHash(ctx, u.ID)
	tok, err := s.CreateSessionVerified(ctx, u.ID, oldHash, "test")
	if err != nil {
		t.Fatal(err)
	}
	sessHash := auth.HashToken(tok)
	// admin resets between our verification and our update
	if err := s.SetPassword(ctx, u.ID, "admin chose this one", "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangePassword(ctx, u.ID, oldHash, "user wanted this", sessHash, "test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("change after reset: %v", err)
	}
	now, _ := s.PasswordHash(ctx, u.ID)
	if !auth.VerifyPassword("admin chose this one", now) {
		t.Fatal("admin reset was overwritten")
	}
	// the normal path still works and rotates the session
	tok2, err := s.CreateSessionVerified(ctx, u.ID, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	newTok, err := s.ChangePassword(ctx, u.ID, now, "user wanted this", auth.HashToken(tok2), "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, tok2); err == nil {
		t.Fatal("old session survived the change")
	}
	if _, err := s.SessionUser(ctx, newTok); err != nil {
		t.Fatalf("new session invalid: %v", err)
	}
}

func TestImportRefusesEscapingSymlinkAndHugeNotes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")

	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("not yours"), 0o600)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "notes"), 0o700)
	os.WriteFile(filepath.Join(dir, "notes", "a.txt"), []byte("A"), 0o600)
	os.Symlink(secret, filepath.Join(dir, "steal.bin"))
	man, _ := json.Marshal(map[string]any{"notes": []map[string]any{{
		"id": NewID(), "file": "notes/a.txt", "title": "A",
		"attachments": []map[string]any{{"ref": NewRef(), "filename": "steal.bin", "file": "steal.bin"}},
	}}})
	os.WriteFile(filepath.Join(dir, "manifest.json"), man, 0o600)
	if _, err := s.Import(ctx, u.ID, dir); err == nil {
		t.Fatal("import followed a symlink out of the folder")
	}

	big := t.TempDir()
	os.WriteFile(filepath.Join(big, "huge.txt"), make([]byte, MaxBodyBytes+1), 0o600)
	if n, err := s.Import(ctx, u.ID, big); err == nil || n != 0 {
		t.Fatalf("oversized note imported: n=%d err=%v", n, err)
	}
}

// doctor must not resurrect a trashed note whose file is gone (interrupted purge).
func TestDoctorLeavesInterruptedPurgeAlone(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "alice")
	m, _ := s.CreateNote(ctx, u.ID, "bye")
	tr := true
	if _, err := s.PatchNote(ctx, u.ID, m.ID, NotePatch{Trashed: &tr}); err != nil {
		t.Fatal(err)
	}
	os.Remove(s.notePath(u.ID, m.ID))
	os.Remove(s.notePath(u.ID, m.ID) + ".bak")
	if _, err := s.Doctor(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.notePath(u.ID, m.ID)); err == nil {
		t.Fatal("doctor recreated a note that was being purged")
	}
}

func TestHighlightFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/highlights.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Names []string
		Strip []struct{ In, Out string }
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fx.Names, ",") != strings.Join(HighlightNames, ",") {
		t.Fatalf("names differ from the shared fixture: %v", HighlightNames)
	}
	for _, c := range fx.Strip {
		if got := StripHighlights(c.In); got != c.Out {
			t.Errorf("StripHighlights(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestHighlightsInTitlesAndPreviews(t *testing.T) {
	body := "=={amber}Server== notes\n=={coral} ==\nIP =={sky-deep}10.0.0.1== and more"
	if got := DeriveTitle(body); got != "Server notes" {
		t.Fatalf("title %q", got)
	}
	if got := DerivePreview(body); got != "IP 10.0.0.1 and more" {
		t.Fatalf("preview %q", got)
	}
	if got := SearchText("[image:abcdefghij] x.png\n=={mint}admin=="); got != "admin" {
		t.Fatalf("search text %q", got)
	}
}

func TestSectionsSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/sections.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Headings []struct{ Line, Color, Name string }
		Strip    []struct{ In, Out string }
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.Headings {
		color, name, ok := ParseSection(c.Line)
		if ok != (c.Color != "") || color != c.Color || name != c.Name {
			t.Errorf("ParseSection(%q) = %q, %q, %v; want %q, %q", c.Line, color, name, ok, c.Color, c.Name)
		}
	}
	for _, c := range fx.Strip {
		if got := StripSections(c.In); got != c.Out {
			t.Errorf("StripSections(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestSectionsInTitlesPreviewsAndSearch(t *testing.T) {
	body := "## {teal} Router\n## {sky} Network\nIP =={amber}10.0.0.1==\n## Plain stays"
	if got := DeriveTitle(body); got != "Router" {
		t.Fatalf("title %q", got)
	}
	if got := DerivePreview(body); got != "Network IP 10.0.0.1 ## Plain stays" {
		t.Fatalf("preview %q", got)
	}
	if got := SearchText(body); got != "Router\nNetwork\nIP 10.0.0.1\n## Plain stays" {
		t.Fatalf("search text %q", got)
	}
	if got := StripHighlights("## {teal} =={amber}x=="); got != "## {teal} =={amber}x==" {
		t.Fatalf("a heading line is never a highlight: %q", got)
	}
}
