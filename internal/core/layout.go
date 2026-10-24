package core

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"homelab-notes/internal/fsx"
)

// Data layout (SPEC §3): one folder per note.
//
//	users/<uid>/user.json
//	users/<uid>/notes/<note-id>/note.txt, note.txt.bak, meta.json
//	users/<uid>/notes/<note-id>/files/<ref>.<ext>
//
// Every name is a UUID, a ref or an extension from extByType — never user input.

func (s *Store) userDir(uid string) string  { return filepath.Join(s.Dir, "users", uid) }
func (s *Store) notesDir(uid string) string { return filepath.Join(s.Dir, "users", uid, "notes") }
func (s *Store) noteDir(uid, id string) string {
	return filepath.Join(s.Dir, "users", uid, "notes", id)
}
func (s *Store) notePath(uid, id string) string { return filepath.Join(s.noteDir(uid, id), "note.txt") }
func (s *Store) metaPath(uid, id string) string {
	return filepath.Join(s.noteDir(uid, id), "meta.json")
}
func (s *Store) filesDir(uid, id string) string { return filepath.Join(s.noteDir(uid, id), "files") }
func (s *Store) userJSONPath(uid string) string { return filepath.Join(s.userDir(uid), "user.json") }

// attPath is where an attachment's bytes live once its extension is known.
func (s *Store) attPath(uid string, a *Attachment) string {
	return filepath.Join(s.filesDir(uid, a.NoteID), a.Ref+"."+a.Ext)
}
func (s *Store) tmpPath(uid, id string) string { return filepath.Join(s.Dir, "tmp", uid, id) }

// the old flat layout, only read by the conversion
func (s *Store) legacyNotePath(uid, id string) string {
	return filepath.Join(s.notesDir(uid), id+".txt")
}
func (s *Store) legacyFilesDir(uid string) string { return filepath.Join(s.userDir(uid), "files") }

func (s *Store) ensureUserDirs(uid string) error {
	if err := os.MkdirAll(s.notesDir(uid), fsx.DirMode); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(s.Dir, "tmp", uid), fsx.DirMode)
}

// ensureFilesDir creates a note's files/ folder, but never the note folder
// itself: a note purged meanwhile must not come back as an empty folder.
func (s *Store) ensureFilesDir(uid, noteID string) error {
	if st, err := os.Stat(s.noteDir(uid, noteID)); err != nil || !st.IsDir() {
		return ErrNotFound
	}
	if err := os.Mkdir(s.filesDir(uid, noteID), fsx.DirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// extByType maps a sniffed content type (http.DetectContentType) to the file
// extension used on disk. Anything else is stored as .bin.
var extByType = map[string]string{
	"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp",
	"image/bmp": "bmp", "image/x-icon": "ico", "application/pdf": "pdf", "application/zip": "zip",
	"application/x-gzip": "gz", "audio/mpeg": "mp3", "audio/wave": "wav", "application/ogg": "ogg",
	"video/mp4": "mp4", "video/webm": "webm", "text/plain": "txt",
}

func sniffExt(head []byte) string {
	t := strings.TrimSpace(strings.SplitN(http.DetectContentType(head), ";", 2)[0])
	if e, ok := extByType[t]; ok {
		return e
	}
	return "bin"
}

func validExt(e string) bool {
	if e == "bin" {
		return true
	}
	for _, v := range extByType {
		if v == e {
			return true
		}
	}
	return false
}

func readHead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return head[:n], nil
}

// ---- opening: which layout is on disk ----

// OpenMode says what Open may do when the data folder still uses the old layout.
type OpenMode int

const (
	// ModeConvert converts an old data folder before returning (serve, migrate-layout).
	ModeConvert OpenMode = iota
	// ModeRefuse returns ErrOldLayout instead.
	ModeRefuse
	// ModeLegacy opens an old data folder as is, for commands that only use the
	// database or copy files verbatim (backup, user passwd/list/disable/enable).
	ModeLegacy
)

var ErrOldLayout = errors.New("the data folder still uses the old layout: start the server once, or run `notes migrate-layout`")

const layoutVersion = "2"

func (s *Store) layoutMarker() string { return filepath.Join(s.Dir, "layout") }

// layoutDone reports whether the database says the conversion finished.
func (s *Store) layoutDone() bool {
	var v string
	return s.DB.W.QueryRow(`SELECT value FROM settings WHERE key='layout_version'`).Scan(&v) == nil && v == layoutVersion
}

// legacyOnDisk reports whether any old-layout note file or files/ folder is present
// (e.g. an old backup was restored over a converted folder).
func (s *Store) legacyOnDisk() bool {
	users, _ := os.ReadDir(filepath.Join(s.Dir, "users"))
	for _, u := range users {
		if !u.IsDir() {
			continue
		}
		if _, err := os.Stat(s.legacyFilesDir(u.Name())); err == nil {
			return true
		}
		entries, _ := os.ReadDir(s.notesDir(u.Name()))
		for _, e := range entries {
			if e.Type().IsRegular() && (strings.HasSuffix(e.Name(), ".txt") || strings.HasSuffix(e.Name(), ".txt.bak")) {
				return true
			}
		}
	}
	return false
}

// lockLayout takes an exclusive lock on the data folder for the layout check
// and conversion, so two processes never convert at once.
func lockLayout(dir string, log interface{ Warn(string, ...any) }) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "db", "layout.lock"), os.O_RDWR|os.O_CREATE, fsx.FileMode)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		log.Warn("another notes process is checking or converting the data folder; waiting")
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// settle brings the store to the current layout and schema, or reports why not.
func (s *Store) settle(ctx context.Context, mode OpenMode) error {
	done := s.layoutDone()
	legacy := s.legacyOnDisk()
	if done && !legacy {
		return s.DB.Migrate()
	}
	var users int
	if err := s.DB.W.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		return err
	}
	if users == 0 && !legacy {
		// a fresh install (or a lost database over converted folders): nothing to convert
		if err := s.DB.Migrate(); err != nil {
			return err
		}
		return s.markLayoutDone()
	}
	switch mode {
	case ModeConvert:
		return s.convert(ctx)
	case ModeLegacy:
		s.Legacy = true
		return nil
	}
	return ErrOldLayout
}

func (s *Store) markLayoutDone() error {
	if _, err := s.DB.W.Exec(`INSERT INTO settings(key,value) VALUES('layout_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, layoutVersion); err != nil {
		return err
	}
	return fsx.WriteAtomic(s.layoutMarker(), []byte(layoutVersion+"\n"))
}

// ---- the one-time conversion (SPEC §3.3) ----

// convert moves an old flat data folder into one folder per note. Every step can
// be repeated: a crash or SIGTERM part-way is finished by the next start.
func (s *Store) convert(ctx context.Context) error {
	start := time.Now()
	s.Log.Info("layout: converting the data folder to one folder per note (one time)")
	if err := s.conversionBackup(ctx); err != nil {
		return err
	}
	if err := s.DB.Migrate(); err != nil {
		return err
	}
	uids, err := s.allUserIDs(ctx)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for _, uid := range uids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.convertUser(ctx, uid, stamp); err != nil {
			return fmt.Errorf("layout: user %s: %w", uid, err)
		}
	}
	// images used by notes other than the one that owns them get their own copy
	for _, uid := range uids {
		if err := s.queueMissingCopies(ctx, uid); err != nil {
			return err
		}
	}
	if err := s.runAllPending(ctx); err != nil {
		return err
	}
	for _, uid := range uids {
		if err := s.writeUserJSON(ctx, uid); err != nil {
			return err
		}
		ids, err := s.noteIDs(ctx, uid)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.writeMetaErr(ctx, uid, id); err != nil {
				return err
			}
		}
	}
	for _, uid := range uids {
		if err := s.moveLegacyLeftovers(ctx, uid, stamp); err != nil {
			return err
		}
	}
	if err := s.markLayoutDone(); err != nil {
		return err
	}
	s.Log.Info("layout: conversion finished", "users", len(uids), "took", time.Since(start).Round(time.Millisecond).String())
	return nil
}

// conversionBackup checks free space and takes one verified backup of the old
// layout (old schema too) before anything is changed. A resumed run reuses it.
func (s *Store) conversionBackup(ctx context.Context) error {
	doneFile := filepath.Join(s.Dir, "backups", "pre-layout-v2.done")
	copies, err := s.copyBytesNeeded(ctx)
	if err != nil {
		return err
	}
	need := copies + s.Cfg.MinFree
	_, statErr := os.Stat(doneFile)
	if statErr != nil {
		users := dirBytes(filepath.Join(s.Dir, "users"))
		dbBytes := dirBytes(filepath.Join(s.Dir, "db"))
		need += users + 2*dbBytes // archive of users/ + the DB snapshot and its copy in the archive
	}
	if free, err := fsx.FreeBytes(s.Dir); err == nil && free < need {
		return fmt.Errorf("layout: not enough free disk space to convert the data folder safely: need %d bytes, %d free (free some space, or lower NOTES_MIN_FREE)", need, free)
	}
	if statErr == nil {
		return nil
	}
	out, err := s.backup(ctx, "", "pre-layout-v2-", false)
	if err != nil {
		return fmt.Errorf("layout: backup before conversion: %w", err)
	}
	if err := verifyArchive(out); err != nil {
		return fmt.Errorf("layout: backup %s is unreadable: %w", out, err)
	}
	s.Log.Info("layout: backup written", "file", out)
	return fsx.WriteAtomic(doneFile, []byte(filepath.Base(out)+"\n"))
}

func verifyArchive(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		if _, err := tr.Next(); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
}

func dirBytes(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// copyBytesNeeded adds up the images that notes show but don't own yet.
func (s *Store) copyBytesNeeded(ctx context.Context) (int64, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT user_id,id FROM notes`)
	if err != nil {
		return 0, err
	}
	var notes [][2]string
	for rows.Next() {
		var p [2]string
		rows.Scan(&p[0], &p[1])
		notes = append(notes, p)
	}
	rows.Close()
	var total int64
	for _, n := range notes {
		body := s.readAnyBody(n[0], n[1])
		for _, ref := range markerRefs(body) {
			var own int
			var size int64
			err := s.DB.R.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM attachments WHERE note_id=? AND ref=?),
 COALESCE((SELECT MAX(size_bytes) FROM attachments WHERE user_id=? AND ref=? AND state='ready'),0)`,
				n[1], ref, n[0], ref).Scan(&own, &size)
			if err != nil {
				return 0, err
			}
			if own == 0 {
				total += size
			}
		}
	}
	return total, nil
}

// readAnyBody reads a note in either layout (conversion only).
func (s *Store) readAnyBody(uid, id string) string {
	if b, err := os.ReadFile(s.notePath(uid, id)); err == nil {
		return string(b)
	}
	b, _ := os.ReadFile(s.legacyNotePath(uid, id))
	return string(b)
}

func (s *Store) allUserIDs(ctx context.Context) ([]string, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT id FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

func (s *Store) noteIDs(ctx context.Context, uid string) ([]string, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT id FROM notes WHERE user_id=? ORDER BY id`, uid)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

func (s *Store) convertUser(ctx context.Context, uid, stamp string) error {
	if err := s.ensureUserDirs(uid); err != nil {
		return err
	}
	ids, err := s.noteIDs(ctx, uid)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := os.MkdirAll(s.noteDir(uid, id), fsx.DirMode); err != nil {
			return err
		}
		if err := s.moveInto(s.legacyNotePath(uid, id), s.notePath(uid, id), stamp); err != nil {
			return err
		}
		if err := s.moveInto(s.legacyNotePath(uid, id)+".bak", s.notePath(uid, id)+".bak", stamp); err != nil {
			return err
		}
	}
	// every state: an 'uploading' row may already have its bytes in files/ (crash during finalize)
	rows, err := s.DB.R.QueryContext(ctx, `SELECT `+attCols+` FROM attachments WHERE user_id=?`, uid)
	if err != nil {
		return err
	}
	var atts []Attachment
	for rows.Next() {
		a, err := scanAtt(rows)
		if err != nil {
			rows.Close()
			return err
		}
		atts = append(atts, a)
	}
	rows.Close()
	for i := range atts {
		a := &atts[i]
		src := filepath.Join(s.legacyFilesDir(uid), a.ID)
		_, srcErr := os.Stat(src)
		if a.Ext == "" {
			// the extension is stored before the bytes move, so a crash in between can resume
			if srcErr == nil {
				head, err := readHead(src)
				if err != nil {
					return err
				}
				a.Ext = sniffExt(head)
			} else if e := s.findMovedExt(uid, a); e != "" {
				a.Ext = e
			} else {
				continue // bytes missing: doctor reports it
			}
			if _, err := s.DB.W.ExecContext(ctx, `UPDATE attachments SET ext=? WHERE id=?`, a.Ext, a.ID); err != nil {
				return err
			}
		}
		if srcErr != nil {
			continue
		}
		if err := s.ensureFilesDir(uid, a.NoteID); err != nil {
			return err
		}
		if err := s.moveInto(src, s.attPath(uid, a), stamp); err != nil {
			return err
		}
	}
	return nil
}

// findMovedExt finds the extension of an attachment already moved by an earlier run.
func (s *Store) findMovedExt(uid string, a *Attachment) string {
	entries, _ := os.ReadDir(s.filesDir(uid, a.NoteID))
	for _, e := range entries {
		if name, ext, ok := strings.Cut(e.Name(), "."); ok && name == a.Ref && validExt(ext) {
			return ext
		}
	}
	return ""
}

// moveInto durably renames src to dst without ever overwriting: if both exist
// and differ, src is kept in lost+found and reported.
func (s *Store) moveInto(src, dst, stamp string) error {
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(dst); errors.Is(err, fs.ErrNotExist) {
		return fsx.MoveDurable(src, dst)
	}
	a, err := fsx.HashFile(src)
	if err != nil {
		return err
	}
	b, err := fsx.HashFile(dst)
	if err != nil {
		return err
	}
	if a == b {
		return os.Remove(src)
	}
	kept, err := s.quarantine(src, "layout-v2-"+stamp)
	if err != nil {
		return err
	}
	s.Log.Warn("layout: two different copies of a file; kept the old one aside", "kept", kept, "used", dst)
	return nil
}

// quarantine moves path (inside the data folder) to lost+found/<batch>/<same relative path>.
func (s *Store) quarantine(path, batch string) (string, error) {
	rel, err := filepath.Rel(s.Dir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("quarantine: %s is outside the data folder", path)
	}
	dst := filepath.Join(s.Dir, "lost+found", batch, rel)
	if err := os.MkdirAll(filepath.Dir(dst), fsx.DirMode); err != nil {
		return "", err
	}
	return dst, fsx.MoveDurable(path, dst)
}

// moveLegacyLeftovers runs last: anything still in the old places has no row.
func (s *Store) moveLegacyLeftovers(ctx context.Context, uid, stamp string) error {
	batch := "layout-v2-" + stamp
	if entries, err := os.ReadDir(s.legacyFilesDir(uid)); err == nil {
		for _, e := range entries {
			if s.exists(ctx, `SELECT 1 FROM attachments WHERE id=? AND state IN ('ready','deleting') AND ext!=''`, e.Name()) {
				return fmt.Errorf("attachment %s was not moved; not touching the old files folder", e.Name())
			}
			if _, err := s.quarantine(filepath.Join(s.legacyFilesDir(uid), e.Name()), batch); err != nil {
				return err
			}
		}
		if err := os.Remove(s.legacyFilesDir(uid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	entries, _ := os.ReadDir(s.notesDir(uid))
	for _, e := range entries {
		if e.Type().IsRegular() {
			if _, err := s.quarantine(filepath.Join(s.notesDir(uid), e.Name()), batch); err != nil {
				return err
			}
		}
	}
	return nil
}
