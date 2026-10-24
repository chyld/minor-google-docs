package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"homelab-notes/internal/fsx"
)

// DoctorReport lists problems found and (with fix) what was repaired.
type DoctorReport struct {
	Problems []string `json:"problems"`
	Fixed    []string `json:"fixed"`
}

func (r *DoctorReport) problem(f string, a ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(f, a...))
}
func (r *DoctorReport) fixed(f string, a ...any) { r.Fixed = append(r.Fixed, fmt.Sprintf(f, a...)) }

// young files may belong to an operation still in progress (the server may be running)
const doctorGrace = 10 * time.Minute

// DoctorUser checks one user's notes, images and mirror files against the
// database (SPEC §9.2). With fix, it repairs what it safely can. It never
// deletes: stray files move to lost+found, and database rows are never removed.
func (s *Store) DoctorUser(ctx context.Context, uid string, fix bool) (*DoctorReport, error) {
	r := &DoctorReport{Problems: []string{}, Fixed: []string{}}
	err := s.doctorUser(ctx, uid, fix, r, "doctor-"+time.Now().UTC().Format("20060102T150405Z"), false)
	return r, err
}

// Doctor runs DoctorUser for every user plus the checks that span users
// (folders of deleted users, stray uploads, disk space). CLI only.
func (s *Store) Doctor(ctx context.Context, fix bool) (*DoctorReport, error) {
	r := &DoctorReport{Problems: []string{}, Fixed: []string{}}
	batch := "doctor-" + time.Now().UTC().Format("20060102T150405Z")
	uids, err := s.allUserIDs(ctx)
	if err != nil {
		return nil, err
	}
	users := map[string]bool{}
	for _, uid := range uids {
		users[uid] = true
		if err := s.doctorUser(ctx, uid, fix, r, batch, true); err != nil {
			return r, err
		}
	}
	entries, _ := os.ReadDir(filepath.Join(s.Dir, "users"))
	for _, e := range entries {
		if users[e.Name()] {
			continue
		}
		r.problem("folder users/%s belongs to no user", e.Name())
		if fix {
			uid := e.Name()
			s.doctorMove(r, filepath.Join(s.Dir, "users", uid), batch, func() bool {
				return !s.exists(ctx, `SELECT 1 FROM users WHERE id=?`, uid)
			})
		}
	}
	tmpDirs, _ := os.ReadDir(filepath.Join(s.Dir, "tmp"))
	for _, d := range tmpDirs {
		if !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(s.Dir, "tmp", d.Name()))
		for _, f := range files {
			if strings.HasPrefix(f.Name(), "purge-") {
				continue // an interrupted purge: reconcile and the janitor finish it
			}
			fid := f.Name()
			if !s.exists(ctx, `SELECT 1 FROM attachments WHERE id=? AND state='uploading'`, fid) {
				r.problem("stray upload tmp/%s/%s", d.Name(), fid)
				if fix {
					s.doctorMove(r, filepath.Join(s.Dir, "tmp", d.Name(), fid), batch, func() bool {
						return !s.exists(ctx, `SELECT 1 FROM attachments WHERE id=? AND state='uploading'`, fid)
					})
				}
			}
		}
	}
	if free, err := fsx.FreeBytes(s.Dir); err == nil && s.Cfg.MinFree > 0 && free < s.Cfg.MinFree {
		r.problem("free disk space %d bytes is below NOTES_MIN_FREE (%d); uploads are refused", free, s.Cfg.MinFree)
	}
	return r, nil
}

// doctorMove quarantines an orphan unless it is young or no longer an orphan.
func (s *Store) doctorMove(r *DoctorReport, path, batch string, stillOrphan func() bool) {
	st, err := os.Stat(path)
	if err != nil || time.Since(st.ModTime()) < doctorGrace || !stillOrphan() {
		return
	}
	if dst, err := s.quarantine(path, batch); err == nil {
		rel, _ := filepath.Rel(s.Dir, dst)
		r.fixed("moved a stray file to %s", rel)
	}
}

type doctorNote struct {
	id, title string
	trashed   bool
}

func (s *Store) doctorUser(ctx context.Context, uid string, fix bool, r *DoctorReport, batch string, cli bool) error {
	label := func(n doctorNote) string {
		t := n.title
		if t == "" {
			t = "Untitled"
		}
		if cli {
			return fmt.Sprintf("“%s” (%s)", t, n.id)
		}
		return "“" + t + "”"
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT id,title,trashed_at IS NOT NULL FROM notes WHERE user_id=? ORDER BY updated_at DESC`, uid)
	if err != nil {
		return err
	}
	var notes []doctorNote
	byID := map[string]doctorNote{}
	for rows.Next() {
		var n doctorNote
		if err := rows.Scan(&n.id, &n.title, &n.trashed); err != nil {
			rows.Close()
			return err
		}
		notes = append(notes, n)
		byID[n.id] = n
	}
	rows.Close()

	// 1. every note has its folder and text
	for _, n := range notes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(s.notePath(uid, n.id)); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if n.trashed {
			r.problem("%s is in the trash and its files are gone: a permanent delete was interrupted (finished automatically)", label(n))
			continue
		}
		bak := s.notePath(uid, n.id) + ".bak"
		b, berr := os.ReadFile(bak)
		if berr == nil {
			r.problem("%s lost its text; the previous version can be restored", label(n))
		} else {
			r.problem("%s lost its text and has no previous version", label(n))
			b = []byte{}
		}
		if fix && s.restoreNoteText(ctx, uid, n.id, b) {
			if berr == nil {
				r.fixed("restored the previous version of %s", label(n))
			} else {
				r.fixed("recreated %s as an empty note", label(n))
			}
		}
	}

	// 2. every image row has its file; a missing one is copied back from another note
	arows, err := s.DB.R.QueryContext(ctx, `SELECT `+attCols+` FROM attachments WHERE user_id=? AND state='ready'`, uid)
	if err != nil {
		return err
	}
	var atts []Attachment
	for arows.Next() {
		a, err := scanAtt(arows)
		if err != nil {
			arows.Close()
			return err
		}
		atts = append(atts, a)
	}
	arows.Close()
	expected := map[string]map[string]bool{} // note id → file names in files/
	for _, a := range atts {
		if expected[a.NoteID] == nil {
			expected[a.NoteID] = map[string]bool{}
		}
		expected[a.NoteID][a.Ref+"."+a.Ext] = true
		if a.Ext != "" && fileExists(s.attPath(uid, &a)) {
			continue
		}
		n := byID[a.NoteID]
		r.problem("%s: the image “%s” is missing from its folder", label(n), a.Filename)
		if fix {
			if s.restoreImage(ctx, uid, &a) {
				r.fixed("copied the image “%s” back into %s from another note", a.Filename, label(n))
			}
		}
	}
	// rows still being uploaded or deleted keep their files too
	if orows, err := s.DB.R.QueryContext(ctx, `SELECT note_id,ref,ext FROM attachments WHERE user_id=? AND state!='ready' AND ext!=''`, uid); err == nil {
		for orows.Next() {
			var note, ref, ext string
			orows.Scan(&note, &ref, &ext)
			if expected[note] == nil {
				expected[note] = map[string]bool{}
			}
			expected[note][ref+"."+ext] = true
		}
		orows.Close()
	}

	// 3. images shown by a note that aren't in its folder
	for _, n := range notes {
		body, err := os.ReadFile(s.notePath(uid, n.id))
		if err != nil {
			continue
		}
		for _, ref := range markerRefs(string(body)) {
			if s.exists(ctx, `SELECT 1 FROM attachments WHERE note_id=? AND ref=? AND state IN ('ready','uploading')`, n.id, ref) ||
				s.exists(ctx, `SELECT 1 FROM pending_copies WHERE note_id=? AND ref=?`, n.id, ref) {
				continue
			}
			if !s.exists(ctx, `SELECT 1 FROM attachments WHERE user_id=? AND ref=? AND state='ready'`, uid, ref) {
				r.problem("%s shows an image that no longer exists (it appears as “image not found”)", label(n))
				continue
			}
			r.problem("%s shows an image from another note that isn't copied into its own folder", label(n))
			if fix {
				if _, err := s.queueCopies(ctx, uid, n.id, "", "[image:"+ref+"]"); err == nil {
					if k, err := s.runPending(ctx, uid, n.id); err == nil && k > 0 {
						r.fixed("copied an image into %s", label(n))
					}
				}
			}
		}
	}

	// 4. copies still waiting
	var pending int
	s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_copies WHERE user_id=?`, uid).Scan(&pending)
	if pending > 0 {
		r.problem("%d image copies between notes are still waiting", pending)
		if fix {
			if err := s.runAllPendingFor(ctx, uid); err == nil {
				s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_copies WHERE user_id=?`, uid).Scan(&pending)
				if pending == 0 {
					r.fixed("finished the waiting image copies")
				}
			}
		}
	}

	// 5. one ref, one content
	crows, err := s.DB.R.QueryContext(ctx, `SELECT ref FROM attachments WHERE user_id=? AND state='ready' AND sha256 IS NOT NULL
 GROUP BY ref HAVING COUNT(DISTINCT sha256) > 1`, uid)
	if err == nil {
		refs, _ := scanStrings(crows)
		for _, ref := range refs {
			r.problem("the image %s has different content in different notes", ref)
		}
	}

	// 6. mirrors (meta.json, user.json)
	for _, n := range notes {
		if _, err := os.Stat(s.noteDir(uid, n.id)); err != nil {
			continue
		}
		want, err := s.buildMeta(ctx, uid, n.id)
		if err != nil {
			continue
		}
		if cur, err := os.ReadFile(s.metaPath(uid, n.id)); err != nil || string(cur) != string(want) {
			r.problem("%s: its details file (meta.json) is missing or out of date", label(n))
			if fix && s.writeMetaErr(ctx, uid, n.id) == nil {
				r.fixed("rewrote the details file of %s", label(n))
			}
		}
	}
	if want, err := s.buildUserJSON(ctx, uid); err == nil {
		if cur, err := os.ReadFile(s.userJSONPath(uid)); err != nil || string(cur) != string(want) {
			r.problem("the account file (user.json) is missing or out of date")
			if fix && s.writeUserJSON(ctx, uid) == nil {
				r.fixed("rewrote the account file")
			}
		}
	}

	// 7. files and folders on disk that the database doesn't know
	stray := func(path, what string, stillOrphan func() bool) {
		if st, err := os.Stat(path); err == nil && strings.HasSuffix(path, ".tmp") && time.Since(st.ModTime()) < doctorGrace {
			return // a save or copy in progress
		}
		if cli {
			rel, _ := filepath.Rel(s.Dir, path)
			r.problem("%s: %s", what, rel)
		} else {
			r.problem("%s", what)
		}
		if fix {
			s.doctorMove(r, path, batch, stillOrphan)
		}
	}
	if _, err := os.Stat(s.legacyFilesDir(uid)); err == nil {
		stray(s.legacyFilesDir(uid), "a folder left over from the old storage layout", func() bool { return true })
	}
	entries, _ := os.ReadDir(s.notesDir(uid))
	for _, e := range entries {
		p := filepath.Join(s.notesDir(uid), e.Name())
		id := e.Name()
		if !e.IsDir() {
			stray(p, "a stray file next to the note folders", func() bool { return true })
			continue
		}
		if _, ok := byID[id]; !ok {
			stray(p, "a note folder that belongs to no note", func() bool { return !s.exists(ctx, `SELECT 1 FROM notes WHERE id=?`, id) })
			continue
		}
		inner, _ := os.ReadDir(p)
		for _, f := range inner {
			switch f.Name() {
			case "note.txt", "note.txt.bak", "meta.json":
				continue
			case "files":
				if f.IsDir() {
					continue
				}
			}
			stray(filepath.Join(p, f.Name()), "a stray file in "+label(byID[id])+"’s folder", func() bool { return true })
		}
		files, _ := os.ReadDir(filepath.Join(p, "files"))
		for _, f := range files {
			name := f.Name()
			if expected[id][name] {
				continue
			}
			ref, ext, _ := strings.Cut(name, ".")
			stray(filepath.Join(p, "files", name), "a file in "+label(byID[id])+"’s folder that belongs to no image", func() bool {
				return !s.exists(ctx, `SELECT 1 FROM attachments WHERE note_id=? AND ref=? AND ext=?`, id, ref, ext)
			})
		}
	}
	return nil
}

// restoreNoteText rewrites a missing note.txt, unless the note was trashed or
// purged meanwhile (an interrupted purge must not be resurrected).
func (s *Store) restoreNoteText(ctx context.Context, uid, id string, body []byte) bool {
	unlock := s.locks.Lock("note:" + id)
	defer unlock()
	if !s.exists(ctx, `SELECT 1 FROM notes WHERE id=? AND user_id=? AND trashed_at IS NULL`, id, uid) {
		return false
	}
	if err := os.MkdirAll(s.noteDir(uid, id), fsx.DirMode); err != nil {
		return false
	}
	if err := fsx.WriteAtomic(s.notePath(uid, id), body); err != nil {
		return false
	}
	text := string(body)
	s.DB.W.ExecContext(ctx, `UPDATE notes SET title=?,preview=?,size_bytes=?,content_sha256=? WHERE id=? AND user_id=?`,
		DeriveTitle(text), DerivePreview(text), len(body), fsx.HashBytes(body), id, uid)
	s.enqueueIndex(id)
	s.writeMeta(ctx, uid, id)
	return true
}

// restoreImage copies a missing image file back from another note's copy.
func (s *Store) restoreImage(ctx context.Context, uid string, a *Attachment) bool {
	src, err := scanAtt(s.DB.R.QueryRowContext(ctx, `SELECT `+attCols+` FROM attachments
 WHERE user_id=? AND ref=? AND state='ready' AND id!=? AND ext!='' ORDER BY created_at DESC`, uid, a.Ref, a.ID))
	if err != nil || !fileExists(s.attPath(uid, &src)) {
		return false
	}
	unlock := s.lockNotes(src.NoteID, a.NoteID)
	defer unlock()
	if s.ensureFilesDir(uid, a.NoteID) != nil {
		return false
	}
	dst := *a
	dst.Ext = src.Ext
	sum, err := s.copyBytes(s.attPath(uid, &src), s.attPath(uid, &dst), src.SizeBytes, s.refSHA(ctx, uid, a.Ref))
	if err != nil {
		return false
	}
	s.DB.W.ExecContext(ctx, `UPDATE attachments SET ext=?, sha256=?, updated_at=? WHERE id=?`, dst.Ext, sum, now(), a.ID)
	s.writeMeta(ctx, uid, a.NoteID)
	return true
}

func (s *Store) runAllPendingFor(ctx context.Context, uid string) error {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT DISTINCT note_id FROM pending_copies WHERE user_id=?`, uid)
	if err != nil {
		return err
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if _, err := s.runPending(ctx, uid, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
