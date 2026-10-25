package core

import (
	"context"
	"errors"
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

	// titles, previews and the search index are derived from the text by rules that can
	// change between versions (e.g. bold/underline made "=={b}…==" formatting): when they
	// did, re-derive everything once even though no note changed
	var stored string
	s.DB.R.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='derive_version'`).Scan(&stored)
	rederive := stored != deriveVersion
	repaired, reindexed := 0, 0
	for _, n := range notes {
		unlock := s.locks.Lock("note:" + n.id)
		b, err := os.ReadFile(s.notePath(n.uid, n.id))
		if err != nil {
			unlock()
			if errors.Is(err, fs.ErrNotExist) {
				// a trashed note whose folder is gone is an interrupted purge
				if _, derr := os.Stat(s.noteDir(n.uid, n.id)); n.trashed && errors.Is(derr, fs.ErrNotExist) {
					ghosts = append(ghosts, [2]string{n.uid, n.id})
				} else {
					s.Log.Warn("note file missing (run `notes doctor`)", "note", n.id)
				}
				continue
			}
			return err
		}
		sha := fsx.HashBytes(b)
		if sha != n.sha || rederive {
			body := string(b)
			_, err := s.DB.W.ExecContext(ctx, `UPDATE notes SET title=?,preview=?,size_bytes=?,content_sha256=? WHERE id=?`,
				DeriveTitle(body), DerivePreview(body), len(b), sha, n.id)
			if err != nil {
				unlock()
				return err
			}
			s.writeMeta(ctx, n.uid, n.id)
			repaired++
		}
		unlock()
		if indexed[n.id] != sha || rederive {
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
	// stray temp files from interrupted saves and copies (old ones only: young ones may be mid-rename)
	matches, _ := filepath.Glob(filepath.Join(s.Dir, "users", "*", "notes", "*", "*.tmp"))
	more, _ := filepath.Glob(filepath.Join(s.Dir, "users", "*", "notes", "*", "files", "*.tmp"))
	matches = append(matches, more...)
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
	if rederive {
		if _, err := s.DB.W.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('derive_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, deriveVersion); err != nil {
			return err
		}
		s.Log.Info("reconcile: titles, previews and search re-derived for this version", "notes", len(notes))
	}
	s.removePurgeLeftovers(ctx)
	s.removeImportLeftovers()
	// a crash between a database change and its meta.json write leaves the mirror
	// behind: bring every one up to date (unchanged files aren't rewritten)
	for _, n := range notes {
		if err := s.writeMetaErr(ctx, n.uid, n.id); err != nil {
			s.Log.Warn("meta.json", "note", n.id, "err", err)
		}
	}
	if err := s.DrainMeta(ctx); err != nil {
		s.Log.Warn("meta.json writer", "err", err)
	}
	s.runAllPending(ctx)
	if repaired+reindexed+removed > 0 {
		s.Log.Info("reconcile", "repaired", repaired, "reindexed", reindexed, "stray_tmp_removed", removed)
	}
	return nil
}

// removeImportLeftovers deletes what a killed `notes import` of a .tar.gz left in
// tmp/import/ (a day old: no import is still reading it).
func (s *Store) removeImportLeftovers() {
	dirs, _ := os.ReadDir(filepath.Join(s.Dir, "tmp", "import"))
	for _, d := range dirs {
		if st, err := d.Info(); err == nil && time.Since(st.ModTime()) > 24*time.Hour {
			os.RemoveAll(filepath.Join(s.Dir, "tmp", "import", d.Name()))
		}
	}
}

// removePurgeLeftovers deletes tmp/<uid>/purge-<id> folders whose note row is gone
// (a purge renames the folder there before deleting the row).
func (s *Store) removePurgeLeftovers(ctx context.Context) {
	matches, _ := filepath.Glob(filepath.Join(s.Dir, "tmp", "*", "purge-*"))
	for _, m := range matches {
		id := strings.TrimPrefix(filepath.Base(m), "purge-")
		if ValidID(id) && !s.exists(ctx, `SELECT 1 FROM notes WHERE id=?`, id) {
			os.RemoveAll(m)
		}
	}
}

// deriveVersion names the rules titles, previews and search text are derived with.
// Bump it whenever those rules change, so reconcile re-derives every note once.
const deriveVersion = "2" // 2: bold and underline (style-set highlight names)

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
			// ErrCopyPending: waits until another note has its copy of an image
			if err := s.PurgeNote(ctx, p[0], p[1], "janitor"); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrCopyPending) {
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
	s.removePurgeLeftovers(ctx)
	s.removeImportLeftovers()
	errs = append(errs, s.DrainMeta(ctx))
	s.runAllPending(ctx)
	return errors.Join(errs...)
}

func (s *Store) exists(ctx context.Context, q string, args ...any) bool {
	var one int
	return s.DB.R.QueryRowContext(ctx, q, args...).Scan(&one) == nil
}
