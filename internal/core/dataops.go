package core

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"homelab-notes/internal/fsx"
)

// Backup writes notes-<ts>.tar.gz into destDir: a consistent DB snapshot
// (VACUUM INTO, taken first) plus users/. Safe while the server runs; files
// newer than the snapshot are harmless, and reconcile fixes derived fields on restore.
func (s *Store) Backup(ctx context.Context, destDir string) (string, error) {
	return s.backup(ctx, destDir, "notes-", true)
}

// backup writes <prefix><ts>.tar.gz; only "notes-" archives are pruned.
func (s *Store) backup(ctx context.Context, destDir, prefix string, prune bool) (string, error) {
	if destDir == "" {
		destDir = filepath.Join(s.Dir, "backups")
	}
	if err := os.MkdirAll(destDir, fsx.DirMode); err != nil {
		return "", err
	}
	ts := Stamp()
	snap := filepath.Join(s.Dir, "tmp", "backup-"+ts+".sqlite")
	if _, err := s.DB.W.ExecContext(ctx, `VACUUM INTO ?`, snap); err != nil {
		return "", fmt.Errorf("db snapshot: %w", err)
	}
	defer os.Remove(snap)

	out := filepath.Join(destDir, prefix+ts+".tar.gz")
	f, err := os.OpenFile(out+".partial", os.O_WRONLY|os.O_CREATE|os.O_EXCL, fsx.FileMode)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	fail := func(err error) (string, error) {
		tw.Close()
		gz.Close()
		f.Close()
		os.Remove(out + ".partial")
		return "", err
	}
	if err := addFile(tw, snap, "db/notes.sqlite"); err != nil {
		return fail(err)
	}
	usersDir := filepath.Join(s.Dir, "users")
	err = filepath.WalkDir(usersDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.Dir, p)
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o700, ModTime: time.Now()})
		}
		if strings.HasSuffix(p, ".tmp") || !d.Type().IsRegular() {
			return nil
		}
		return addFile(tw, p, rel)
	})
	if err != nil {
		return fail(err)
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	f.Close()
	if err := fsx.MoveDurable(out+".partial", out); err != nil {
		return "", err
	}
	if prune {
		s.pruneBackups(destDir)
	}
	return out, nil
}

func addFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // deleted while walking
		}
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: st.Size(), ModTime: st.ModTime()}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, st.Size())
	return err
}

func (s *Store) pruneBackups(dir string) {
	// leftovers of interrupted backups
	partial, _ := filepath.Glob(filepath.Join(dir, "notes-*.tar.gz.partial"))
	for _, p := range partial {
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > 24*time.Hour {
			os.Remove(p)
		}
	}
	m, _ := filepath.Glob(filepath.Join(dir, "notes-*.tar.gz"))
	sort.Strings(m)
	for len(m) > s.Cfg.BackupKeep {
		os.Remove(m[0])
		m = m[1:]
	}
}

// ---- export ----

type manifestNote struct {
	ID          string            `json:"id"`
	File        string            `json:"file"`
	Folder      string            `json:"folder,omitempty"`
	Title       string            `json:"title"`
	Tags        []string          `json:"tags"`
	TagColors   map[string]string `json:"tag_colors,omitempty"`
	Pinned      bool              `json:"pinned"`
	Trashed     bool              `json:"trashed"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	Attachments []manifestAttach  `json:"attachments"`
}

type manifestAttach struct {
	Ref      string `json:"ref"`
	Filename string `json:"filename"`
	File     string `json:"file"`
	Mime     string `json:"mime"`
	Size     int64  `json:"size_bytes"`
}

// Export streams a zip of one user's notes: one folder per note
// (notes/<title>/ or trash/<title>/) holding note.txt, meta.json and files/ with
// each image under its original name, plus manifest.json, which import reads.
func (s *Store) Export(ctx context.Context, uid string, w io.Writer) error {
	tags, err := s.ListTags(ctx, uid)
	if err != nil {
		return err
	}
	tagByID := map[string]Tag{}
	for _, t := range tags {
		tagByID[t.ID] = t
	}
	var metas []NoteMeta
	for _, trash := range []bool{false, true} {
		cur := ""
		for {
			l, err := s.ListNotes(ctx, uid, ListParams{Trash: trash, Limit: 200, Cursor: cur})
			if err != nil {
				return err
			}
			metas = append(metas, l.Items...)
			if l.NextCursor == "" {
				break
			}
			cur = l.NextCursor
		}
	}
	zw := zip.NewWriter(w)
	// names are compared case-insensitively: the zip may be unpacked on macOS/Windows
	usedDirs := map[string]bool{}
	uniqueDir := func(p string) string {
		cand := p
		for i := 2; usedDirs[strings.ToLower(cand)]; i++ {
			cand = fmt.Sprintf("%s (%d)", p, i)
		}
		usedDirs[strings.ToLower(cand)] = true
		return cand
	}
	var manifest []manifestNote
	for _, m := range metas {
		root := "notes"
		if m.TrashedAt != nil {
			root = "trash"
		}
		b, err := os.ReadFile(s.notePath(uid, m.ID))
		if errors.Is(err, fs.ErrNotExist) {
			continue // purged while we were exporting
		} else if err != nil {
			return err
		}
		folder := uniqueDir(root + "/" + safeName(m.Title))
		name := folder + "/note.txt"
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.UnixMilli(m.UpdatedAt)})
		if err != nil {
			return err
		}
		if _, err := fw.Write(b); err != nil {
			return err
		}
		if mb, err := s.buildMeta(ctx, uid, m.ID); err == nil {
			fw, err := zw.CreateHeader(&zip.FileHeader{Name: folder + "/meta.json", Method: zip.Deflate, Modified: time.UnixMilli(m.UpdatedAt)})
			if err != nil {
				return err
			}
			if _, err := fw.Write(mb); err != nil {
				return err
			}
		}
		mn := manifestNote{ID: m.ID, File: name, Folder: folder, Title: m.Title, Pinned: m.Pinned, Trashed: m.TrashedAt != nil,
			CreatedAt: time.UnixMilli(m.CreatedAt).UTC().Format(time.RFC3339Nano), UpdatedAt: time.UnixMilli(m.UpdatedAt).UTC().Format(time.RFC3339Nano),
			Tags: []string{}, TagColors: map[string]string{}, Attachments: []manifestAttach{}}
		for _, id := range m.TagIDs {
			if t, ok := tagByID[id]; ok {
				mn.Tags = append(mn.Tags, t.Name)
				mn.TagColors[t.Name] = t.Color
			}
		}
		atts, err := s.ListAttachments(ctx, uid, m.ID)
		if err != nil {
			return err
		}
		usedFiles := map[string]bool{}
		for _, a := range atts {
			f, err := os.Open(s.attPath(uid, &a))
			if errors.Is(err, fs.ErrNotExist) {
				continue // deleted while we were exporting
			} else if err != nil {
				return err
			}
			fname := SanitizeFilename(a.Filename)
			ext := filepath.Ext(fname)
			base := strings.TrimSuffix(fname, ext)
			cand := fname
			for i := 2; usedFiles[strings.ToLower(cand)]; i++ {
				cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
			}
			usedFiles[strings.ToLower(cand)] = true
			ap := folder + "/files/" + cand
			fw, err := zw.CreateHeader(&zip.FileHeader{Name: ap, Method: zip.Store, Modified: time.UnixMilli(a.CreatedAt)})
			if err != nil {
				f.Close()
				return err
			}
			_, err = io.Copy(fw, f)
			f.Close()
			if err != nil {
				return err
			}
			mn.Attachments = append(mn.Attachments, manifestAttach{Ref: a.Ref, Filename: a.Filename, File: ap, Mime: a.Mime, Size: a.SizeBytes})
		}
		manifest = append(manifest, mn)
	}
	fw, err := zw.Create("manifest.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(fw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"version": 2, "exported_at": time.Now().UTC().Format(time.RFC3339), "notes": manifest}); err != nil {
		return err
	}
	return zw.Close()
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, s)
	s = strings.Trim(s, ". ")
	if len([]rune(s)) > 80 {
		s = string([]rune(s)[:80])
	}
	if s == "" {
		return "Untitled"
	}
	return s
}

// Import creates notes for a user from either an export (a .zip or an unzipped
// folder containing manifest.json — tags, pins, dates and attachments are
// restored) or any folder of .txt files (searched recursively).
func (s *Store) Import(ctx context.Context, uid, path string) (int, error) {
	var fsys fs.FS
	if strings.EqualFold(filepath.Ext(path), ".zip") {
		zr, err := zip.OpenReader(path)
		if err != nil {
			return 0, err
		}
		defer zr.Close()
		fsys = zr
	} else {
		st, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		if !st.IsDir() {
			return 0, fmt.Errorf("%s is not a folder or .zip", path)
		}
		// os.Root refuses symlinks (and ..) that lead outside the folder
		root, err := os.OpenRoot(path)
		if err != nil {
			return 0, err
		}
		defer root.Close()
		fsys = root.FS()
	}
	if mb, err := readRegular(fsys, "manifest.json", maxManifestBytes); err == nil {
		return s.importExport(ctx, uid, fsys, mb)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("manifest.json: %w", err)
	}
	n := 0
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".txt") {
			return err
		}
		b, err := readRegular(fsys, p, MaxBodyBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if _, err := s.CreateNote(ctx, uid, string(b)); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		n++
		return nil
	})
	if err != nil {
		return n, fmt.Errorf("import stopped after %d notes: %w", n, err)
	}
	return n, nil
}

func (s *Store) importExport(ctx context.Context, uid string, fsys fs.FS, manifestJSON []byte) (int, error) {
	var man struct {
		Notes []manifestNote `json:"notes"`
	}
	if err := json.Unmarshal(manifestJSON, &man); err != nil {
		return 0, fmt.Errorf("manifest.json: %w", err)
	}
	n := 0
	fail := func(title string, err error) (int, error) {
		return n, fmt.Errorf("import stopped after %d notes (at %q): %w", n, title, err)
	}
	for _, mn := range man.Notes {
		body, err := readRegular(fsys, mn.File, MaxBodyBytes)
		if err != nil {
			return fail(mn.Title, err)
		}
		m, err := s.CreateNote(ctx, uid, "")
		if err != nil {
			return fail(mn.Title, err)
		}
		text := string(body)
		for _, ma := range mn.Attachments {
			f, err := openRegular(fsys, ma.File)
			if err != nil {
				return fail(mn.Title, err)
			}
			a, err := s.importAttachment(ctx, uid, m.ID, f, ma.Filename, ma.Ref)
			f.Close()
			if err != nil {
				return fail(mn.Title, err)
			}
			if a.Ref != ma.Ref {
				text = strings.ReplaceAll(text, "[image:"+ma.Ref+"]", "[image:"+a.Ref+"]")
			}
		}
		if _, err := s.SaveBody(ctx, uid, m.ID, text, m.SHA256); err != nil {
			return fail(mn.Title, err)
		}
		var tagIDs []string
		for _, name := range mn.Tags {
			color := mn.TagColors[name]
			if !validColor(color) {
				color = ""
			}
			if t, err := s.CreateTag(ctx, uid, name, color); err == nil {
				tagIDs = append(tagIDs, t.ID)
			}
		}
		pinned, trashed := mn.Pinned, mn.Trashed
		if _, err := s.PatchNote(ctx, uid, m.ID, NotePatch{Pinned: &pinned, Trashed: &trashed, TagIDs: &tagIDs}); err != nil {
			return fail(mn.Title, err)
		}
		created, _ := time.Parse(time.RFC3339, mn.CreatedAt)
		updated, _ := time.Parse(time.RFC3339, mn.UpdatedAt)
		if !created.IsZero() && !updated.IsZero() {
			s.DB.W.ExecContext(ctx, `UPDATE notes SET created_at=?, updated_at=? WHERE id=? AND user_id=?`,
				created.UnixMilli(), updated.UnixMilli(), m.ID, uid)
			s.writeMeta(ctx, uid, m.ID)
		}
		n++
	}
	return n, nil
}

// importAttachment stores r as a ready attachment of noteID. It keeps wantRef
// when the ref is free, or when it already names this same content (a copy of
// an image shared between notes); otherwise it picks a new ref.
func (s *Store) importAttachment(ctx context.Context, uid, noteID string, r io.Reader, filename, wantRef string) (*Attachment, error) {
	l, err := s.reserve(-1)
	if err != nil {
		return nil, err
	}
	defer l.release()
	a := &Attachment{ID: NewID(), Ref: wantRef, NoteID: noteID, Filename: SanitizeFilename(filename),
		Mime: "application/octet-stream", State: "uploading", CreatedAt: now()}
	if err := s.ensureUserDirs(uid); err != nil {
		return nil, err
	}
	tmp := s.tmpPath(uid, a.ID)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fsx.FileMode)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(&guardedWriter{f: f, l: l}, h), r)
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	a.SizeBytes = size
	sum := hex.EncodeToString(h.Sum(nil))
	inserted := false
	if ValidRef(a.Ref) {
		if err := s.insertNewRef(ctx, uid, a, sum); err == nil {
			inserted = true
		} else if !errors.Is(err, ErrRefTaken) {
			os.Remove(tmp)
			return nil, err
		} else if s.refSHA(ctx, uid, a.Ref) == sum {
			// same image already known under this ref: another copy of it
			tx, err := s.DB.W.BeginTx(ctx, nil)
			if err != nil {
				os.Remove(tmp)
				return nil, err
			}
			err = insertAtt(tx, uid, a, sum)
			if err == nil {
				err = tx.Commit()
			}
			tx.Rollback()
			inserted = err == nil
		}
	}
	for i := 0; !inserted && i < 5; i++ {
		a.Ref = NewRef()
		if err := s.insertNewRef(ctx, uid, a, sum); err == nil {
			inserted = true
		} else if !errors.Is(err, ErrRefTaken) {
			os.Remove(tmp)
			return nil, err
		}
	}
	if !inserted {
		os.Remove(tmp)
		return nil, ErrRefTaken
	}
	unlock := s.locks.Lock("att:" + a.ID)
	defer unlock()
	if err := s.finalize(ctx, uid, a); err != nil {
		return nil, err
	}
	return a, nil
}

const maxManifestBytes = 64 << 20

// openRegular opens name only if it is a regular file (no devices, fifos or dirs).
func openRegular(fsys fs.FS, name string) (fs.File, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return f, nil
}

// readRegular reads a regular file, refusing anything larger than limit
// (checked on the bytes actually read, so compressed sizes can't lie).
func readRegular(fsys fs.FS, name string, limit int64) ([]byte, error) {
	f, err := openRegular(fsys, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d MiB", name, limit>>20)
	}
	return b, nil
}

// Stamp is a file-name timestamp that sorts by time and stays unique even for
// two backups/exports started in the same second.
func Stamp() string {
	return time.Now().UTC().Format("20060102T150405Z") + "-" + fsxRand()
}

func fsxRand() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}
