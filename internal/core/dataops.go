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
		if errors.Is(err, fs.ErrNotExist) {
			return nil // purged while we walked
		} else if err != nil {
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
	Missing     []manifestMissing `json:"missing,omitempty"` // a one-note archive's files it couldn't include (import ignores it)
}

type manifestMissing struct {
	Ref      string `json:"ref"`
	Filename string `json:"filename"`
	Reason   string `json:"reason"`
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
			ap := folder + "/files/" + uniqueFileName(usedFiles, exportFileName(a.Filename))
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

// uniqueFileName picks name, or "name (2).ext", … not used yet in a folder
// (compared case-insensitively: the export may be unpacked on macOS/Windows).
func uniqueFileName(used map[string]bool, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	cand := name
	for i := 2; used[strings.ToLower(cand)]; i++ {
		cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
	}
	used[strings.ToLower(cand)] = true
	return cand
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
	return notReserved(s)
}

// notReserved prefixes names Windows can't create (CON, NUL, COM1, …, also with an extension).
func notReserved(name string) string {
	base := strings.ToUpper(strings.TrimSpace(strings.SplitN(name, ".", 2)[0]))
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return "_" + name
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
		return "_" + name
	}
	return name
}

// Import creates notes for a user from either an export (a .zip, a note's .tar.gz, or
// an unpacked folder containing manifest.json — tags, pins, dates and attachments are
// restored) or any folder of .txt files (searched recursively).
func (s *Store) Import(ctx context.Context, uid, path string) (int, error) {
	var fsys fs.FS
	if isTarGz(path) {
		dir, err := s.unpackTarGz(path)
		if dir != "" {
			defer os.RemoveAll(dir)
		}
		if err != nil {
			return 0, err
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return 0, err
		}
		defer root.Close()
		fsys = root.FS()
	} else if strings.EqualFold(filepath.Ext(path), ".zip") {
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
			return 0, fmt.Errorf("%s is not a folder, .zip or .tar.gz", path)
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
	// Pass 1: every note and its images. Refs are mapped once for the whole export,
	// so a note that only shows another note's image follows that image's new ref.
	type pending struct {
		mn   manifestNote
		id   string
		sha  string
		text string
	}
	var notes []pending
	remap := map[string]string{} // export ref → ref in this account
	inExport := map[string]bool{}
	for _, mn := range man.Notes {
		body, err := readRegular(fsys, mn.File, MaxBodyBytes)
		if err != nil {
			return fail(mn.Title, err)
		}
		m, err := s.CreateNote(ctx, uid, "")
		if err != nil {
			return fail(mn.Title, err)
		}
		for _, ma := range mn.Attachments {
			f, err := openRegular(fsys, ma.File)
			if err != nil {
				return fail(mn.Title, err)
			}
			want := ma.Ref
			if r, ok := remap[ma.Ref]; ok {
				want = r
			}
			a, err := s.importAttachment(ctx, uid, m.ID, f, ma.Filename, want)
			f.Close()
			if err != nil {
				return fail(mn.Title, err)
			}
			inExport[ma.Ref] = true
			if _, ok := remap[ma.Ref]; !ok {
				remap[ma.Ref] = a.Ref
			}
		}
		notes = append(notes, pending{mn: mn, id: m.ID, sha: m.SHA256, text: string(body)})
	}
	// Pass 2: the text, with markers rewritten through the export-wide mapping.
	for _, p := range notes {
		mn, text := p.mn, p.text
		for _, ref := range markerRefs(text) {
			to, ok := remap[ref]
			if !ok && !inExport[ref] && s.exists(ctx, `SELECT 1 FROM ref_content WHERE user_id=? AND ref=?`, uid, ref) {
				// an image the export doesn't carry, whose ref means something else here:
				// leave it "not found" rather than show the wrong picture
				to, ok = NewRef(), true
			}
			if ok && to != ref {
				text = strings.ReplaceAll(text, "[image:"+ref+"]", "[image:"+to+"]")
			}
		}
		if _, err := s.SaveBody(ctx, uid, p.id, text, p.sha); err != nil {
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
		if _, err := s.PatchNote(ctx, uid, p.id, NotePatch{Pinned: &pinned, Trashed: &trashed, TagIDs: &tagIDs}); err != nil {
			return fail(mn.Title, err)
		}
		created, _ := time.Parse(time.RFC3339, mn.CreatedAt)
		updated, _ := time.Parse(time.RFC3339, mn.UpdatedAt)
		if !created.IsZero() && !updated.IsZero() {
			s.DB.W.ExecContext(ctx, `UPDATE notes SET created_at=?, updated_at=? WHERE id=? AND user_id=?`,
				created.UnixMilli(), updated.UnixMilli(), p.id, uid)
			s.writeMeta(ctx, uid, p.id)
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

func isTarGz(path string) bool {
	p := strings.ToLower(path)
	return strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz")
}

// unpackTarGz unpacks a .tar.gz into a new folder under data/tmp (removed by the
// caller): only folders and regular files, each name a plain relative path, written
// through os.Root so nothing can land outside it. Links and devices are skipped.
func (s *Store) unpackTarGz(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	defer gz.Close()
	tmp := filepath.Join(s.Dir, "tmp")
	if err := os.MkdirAll(tmp, fsx.DirMode); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(tmp, "import-")
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return dir, err
	}
	defer root.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return dir, nil
		} else if err != nil {
			return dir, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "./"), "/")
		if name == "" || name == "." {
			continue
		}
		if !filepath.IsLocal(name) || strings.ContainsRune(name, 0) {
			return dir, fmt.Errorf("%s: unsafe name %q", filepath.Base(path), hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, fsx.DirMode); err != nil {
				return dir, err
			}
		case tar.TypeReg:
			if d := filepath.Dir(name); d != "." {
				if err := root.MkdirAll(d, fsx.DirMode); err != nil {
					return dir, err
				}
			}
			out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fsx.FileMode)
			if err != nil {
				return dir, fmt.Errorf("%s: %w", name, err)
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return dir, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
}

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
