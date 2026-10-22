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

// Reconcile repairs derived metadata from the files (file wins) and brings the
// search index up to date. Runs at startup, after `notes import` and in
// `notes gc` — not hourly (the janitor's IndexSweep covers index drift).
func (s *Store) Reconcile(ctx context.Context) error {
	// Snapshot the index before the notes: a note created in between then looks
	// "not indexed yet" (harmless) rather than "indexed but gone" (would unindex it).
	indexed := map[string]string{}
	irows, err := s.DB.Search.QueryContext(ctx, `SELECT note_id,content_sha256 FROM indexed`)
	if err != nil {
		return err
	}
	for irows.Next() {
		var id, sha string
		irows.Scan(&id, &sha)
		indexed[id] = sha
	}
	irows.Close()

	rows, err := s.DB.R.QueryContext(ctx, `SELECT id,user_id,content_sha256,trashed_at IS NOT NULL FROM notes`)
	if err != nil {
		return err
	}
	type row struct {
		id, uid, sha string
		trashed      bool
	}
	var notes []row
	var ghosts [][2]string
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.uid, &r.sha, &r.trashed); err != nil {
			rows.Close()
			return err
		}
		notes = append(notes, r)
	}
	rows.Close()

	repaired, reindexed := 0, 0
	for _, n := range notes {
		unlock := s.locks.Lock("note:" + n.id)
		b, err := os.ReadFile(s.notePath(n.uid, n.id))
		if err != nil {
			unlock()
			if errors.Is(err, fs.ErrNotExist) {
				if n.trashed {
					ghosts = append(ghosts, [2]string{n.uid, n.id})
				} else {
					s.Log.Warn("note file missing (run `notes doctor`)", "note", n.id)
				}
				continue
			}
			return err
		}
		sha := fsx.HashBytes(b)
		if sha != n.sha {
			body := string(b)
			_, err := s.DB.W.ExecContext(ctx, `UPDATE notes SET title=?,preview=?,size_bytes=?,content_sha256=? WHERE id=?`,
				DeriveTitle(body), DerivePreview(body), len(b), sha, n.id)
			if err != nil {
				unlock()
				return err
			}
			repaired++
		}
		unlock()
		if indexed[n.id] != sha {
			if err := s.indexNote(ctx, n.id); err != nil {
				return err
			}
			reindexed++
		}
		delete(indexed, n.id)
	}
	gone := func(id string) bool { // re-check: the note may have been created after our snapshot
		return !s.exists(ctx, `SELECT 1 FROM notes WHERE id=?`, id)
	}
	for id := range indexed {
		if gone(id) {
			s.unindex(id)
		}
	}
	// FTS rows whose bookkeeping row is gone (crash mid-unindex)
	if frows, err := s.DB.Search.QueryContext(ctx, `SELECT DISTINCT note_id FROM notes_fts`); err == nil {
		live := map[string]bool{}
		for _, n := range notes {
			live[n.id] = true
		}
		var dead []string
		for frows.Next() {
			var id string
			frows.Scan(&id)
			if !live[id] {
				dead = append(dead, id)
			}
		}
		frows.Close()
		for _, id := range dead {
			if gone(id) {
				s.unindex(id)
			}
		}
	}
	// stray temp files from interrupted saves (old ones only: young ones may be mid-rename)
	matches, _ := filepath.Glob(filepath.Join(s.Dir, "users", "*", "notes", "*.tmp"))
	removed := 0
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && time.Since(st.ModTime()) > 10*time.Minute {
			os.Remove(m)
			removed++
		}
	}
	// finish purges interrupted after the files were deleted
	for _, g := range ghosts {
		// a missing notes folder means a mis-mounted volume, not an interrupted purge
		if _, err := os.Stat(filepath.Join(s.Dir, "users", g[0], "notes")); err != nil {
			s.Log.Warn("notes folder missing; not finishing purges (check the data volume)", "user", g[0])
			continue
		}
		if err := s.PurgeNote(ctx, g[0], g[1], "reconcile"); err != nil {
			s.Log.Warn("finishing interrupted purge", "note", g[1], "err", err)
		}
	}
	if repaired+reindexed+removed > 0 {
		s.Log.Info("reconcile", "repaired", repaired, "reindexed", reindexed, "stray_tmp_removed", removed)
	}
	return nil
}

// RunJanitor performs periodic maintenance until ctx is cancelled.
func (s *Store) RunJanitor(ctx context.Context, every time.Duration, extra func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.JanitorOnce(ctx); err != nil {
				s.Log.Error("janitor", "err", err)
			}
			if extra != nil {
				extra()
			}
		}
	}
}

func (s *Store) JanitorOnce(ctx context.Context) error {
	var errs []error
	if s.Cfg.TrashDays > 0 {
		due, err := s.trashPurgeDue(ctx, s.Cfg.TrashDays)
		errs = append(errs, err)
		for _, p := range due {
			// ErrNotFound: purged meanwhile (e.g. by the server while `notes gc` runs) — done either way
			if err := s.PurgeNote(ctx, p[0], p[1], "janitor"); !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
			}
		}
	}
	if n, err := s.GCUploads(ctx, 24*time.Hour); err != nil || n > 0 {
		errs = append(errs, err)
		if n > 0 {
			s.Log.Info("janitor: removed stale uploads", "count", n)
		}
	}
	_, err := s.SweepSessions(ctx)
	errs = append(errs, err)
	errs = append(errs, s.finishDeletingAttachments(ctx, "", ""))
	// hash attachments whose background hash didn't run
	rows, err := s.DB.R.QueryContext(ctx, `SELECT user_id,id FROM attachments WHERE state='ready' AND sha256 IS NULL LIMIT 50`)
	if err == nil {
		var pairs [][2]string
		for rows.Next() {
			var p [2]string
			rows.Scan(&p[0], &p[1])
			pairs = append(pairs, p)
		}
		rows.Close()
		for _, p := range pairs {
			s.fillSHA(ctx, p[0], p[1])
		}
	}
	errs = append(errs, err)
	errs = append(errs, s.IndexSweep(ctx))
	return errors.Join(errs...)
}

// ---- doctor ----

type DoctorReport struct {
	Problems []string
	Fixed    []string
}

func (s *Store) Doctor(ctx context.Context, fix bool) (*DoctorReport, error) {
	r := &DoctorReport{}
	add := func(f string, a ...any) { r.Problems = append(r.Problems, fmt.Sprintf(f, a...)) }
	quarantine := filepath.Join(s.Dir, "lost+found", "doctor-"+time.Now().UTC().Format("20060102T150405Z"))
	move := func(src string, stillOrphan func() bool) {
		// The server may be running: leave young files alone and re-check the DB first.
		if st, err := os.Stat(src); err != nil || time.Since(st.ModTime()) < 10*time.Minute || !stillOrphan() {
			return
		}
		rel, _ := filepath.Rel(s.Dir, src)
		dst := filepath.Join(quarantine, rel)
		if err := os.MkdirAll(filepath.Dir(dst), fsx.DirMode); err == nil {
			if err := os.Rename(src, dst); err == nil {
				r.Fixed = append(r.Fixed, "moved "+rel+" → "+dst)
			}
		}
	}

	users := map[string]bool{}
	urows, err := s.DB.R.QueryContext(ctx, `SELECT id FROM users`)
	if err != nil {
		return nil, err
	}
	uids, err := scanStrings(urows)
	if err != nil {
		return nil, err
	}
	for _, u := range uids {
		users[u] = true
	}
	notes := map[string]bool{}
	nrows, err := s.DB.R.QueryContext(ctx, `SELECT user_id||'/'||id FROM notes`)
	if err != nil {
		return nil, err
	}
	nids, _ := scanStrings(nrows)
	// restore writes the file and its derived metadata, unless the note was
	// trashed or purged meanwhile (a trashed note without a file is an
	// interrupted purge, which reconcile finishes — don't resurrect it).
	restore := func(uid, id string, body []byte, msg string) {
		if !s.exists(ctx, `SELECT 1 FROM notes WHERE id=? AND user_id=? AND trashed_at IS NULL`, id, uid) {
			return
		}
		if err := fsx.WriteAtomic(s.notePath(uid, id), body); err != nil {
			return
		}
		text := string(body)
		s.DB.W.ExecContext(ctx, `UPDATE notes SET title=?,preview=?,size_bytes=?,content_sha256=? WHERE id=? AND user_id=?`,
			DeriveTitle(text), DerivePreview(text), len(body), fsx.HashBytes(body), id, uid)
		s.enqueueIndex(id)
		r.Fixed = append(r.Fixed, msg)
	}
	for _, n := range nids {
		notes[n] = true
		parts := strings.SplitN(n, "/", 2)
		if _, err := os.Stat(s.notePath(parts[0], parts[1])); errors.Is(err, fs.ErrNotExist) {
			if s.exists(ctx, `SELECT 1 FROM notes WHERE id=? AND trashed_at IS NOT NULL`, parts[1]) {
				add("note %s is in the trash and has no file (an interrupted purge; finished on next start or `notes gc`)", parts[1])
				continue
			}
			bak := s.notePath(parts[0], parts[1]) + ".bak"
			if b, err := os.ReadFile(bak); err == nil {
				add("note %s has no file (previous version available in .bak)", parts[1])
				if fix {
					restore(parts[0], parts[1], b, "restored note "+parts[1]+" from its .bak")
				}
			} else {
				add("note %s has no file and no .bak", parts[1])
				if fix {
					restore(parts[0], parts[1], []byte{}, "recreated note "+parts[1]+" as an empty note")
				}
			}
		}
	}
	atts := map[string]string{}
	arows, err := s.DB.R.QueryContext(ctx, `SELECT user_id||'/'||id, state FROM attachments`)
	if err != nil {
		return nil, err
	}
	for arows.Next() {
		var k, st string
		arows.Scan(&k, &st)
		atts[k] = st
	}
	arows.Close()
	for k, st := range atts {
		parts := strings.SplitN(k, "/", 2)
		if st == "ready" {
			if _, err := os.Stat(s.filePath(parts[0], parts[1])); errors.Is(err, fs.ErrNotExist) {
				add("attachment %s has no file", parts[1])
				if fix {
					if _, err := s.DB.W.ExecContext(ctx, `DELETE FROM attachments WHERE id=? AND state='ready'`, parts[1]); err == nil {
						r.Fixed = append(r.Fixed, "removed the database row of missing attachment "+parts[1])
					}
				}
			}
		}
	}

	entries, _ := os.ReadDir(filepath.Join(s.Dir, "users"))
	for _, e := range entries {
		uid := e.Name()
		if !users[uid] {
			add("directory users/%s belongs to no user", uid)
			if fix {
				move(filepath.Join(s.Dir, "users", uid), func() bool { return !s.exists(ctx, `SELECT 1 FROM users WHERE id=?`, uid) })
			}
			continue
		}
		files, _ := os.ReadDir(filepath.Join(s.Dir, "users", uid, "notes"))
		for _, f := range files {
			name := f.Name()
			id := strings.TrimSuffix(strings.TrimSuffix(name, ".bak"), ".txt")
			if strings.HasSuffix(name, ".tmp") {
				id = strings.SplitN(name, ".", 2)[0]
			}
			if strings.HasSuffix(name, ".tmp") || !notes[uid+"/"+id] {
				add("orphan note file users/%s/notes/%s", uid, name)
				if fix {
					move(filepath.Join(s.Dir, "users", uid, "notes", name), func() bool { return !s.exists(ctx, `SELECT 1 FROM notes WHERE id=?`, id) })
				}
			}
		}
		files, _ = os.ReadDir(filepath.Join(s.Dir, "users", uid, "files"))
		for _, f := range files {
			if _, ok := atts[uid+"/"+f.Name()]; !ok {
				add("orphan attachment file users/%s/files/%s", uid, f.Name())
				if fix {
					fid := f.Name()
					move(filepath.Join(s.Dir, "users", uid, "files", fid), func() bool { return !s.exists(ctx, `SELECT 1 FROM attachments WHERE id=?`, fid) })
				}
			}
		}
	}
	tmpDirs, _ := os.ReadDir(filepath.Join(s.Dir, "tmp"))
	for _, d := range tmpDirs {
		files, _ := os.ReadDir(filepath.Join(s.Dir, "tmp", d.Name()))
		for _, f := range files {
			if atts[d.Name()+"/"+f.Name()] != "uploading" {
				add("stray upload tmp/%s/%s", d.Name(), f.Name())
				if fix {
					fid := f.Name()
					move(filepath.Join(s.Dir, "tmp", d.Name(), fid), func() bool {
						return !s.exists(ctx, `SELECT 1 FROM attachments WHERE id=? AND state='uploading'`, fid)
					})
				}
			}
		}
	}
	if free, err := fsx.FreeBytes(s.Dir); err == nil && s.Cfg.MinFree > 0 && free < s.Cfg.MinFree {
		add("free disk space %d bytes is below NOTES_MIN_FREE (%d); uploads are refused", free, s.Cfg.MinFree)
	}
	return r, nil
}

func (s *Store) exists(ctx context.Context, q string, args ...any) bool {
	var one int
	return s.DB.R.QueryRowContext(ctx, q, args...).Scan(&one) == nil
}
