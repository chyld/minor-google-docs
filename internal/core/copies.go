package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"homelab-notes/internal/fsx"
)

// An image marker pasted into another note gets its own copy in that note's
// folder (SPEC §6.3). The copy keeps the ref, so the note text never changes;
// ref_content guarantees one ref is one image's content for a user.
//
// pending_copies holds markers still waiting for their copy. Saves queue only
// markers they add, so an image deleted from a note's attachments stays deleted.

var (
	errAlreadyHere = errors.New("already in this note")
	errNotReady    = errors.New("source still uploading")
	errNoSource    = errors.New("image not found")
	errSourceLost  = errors.New("every copy of the image lost its file")
	ErrCopyPending = errors.New("an image in this note is still being copied to another note; try again in a moment")
)

// markerRefs returns the refs of the image markers in body, without repeats.
func markerRefs(body string) []string {
	var out []string
	seen := map[string]bool{}
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.HasPrefix(line, "[image:") {
			continue
		}
		if m := MarkerRe.FindStringSubmatch(line); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// execer is a *sql.DB or *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// queueCopies records markers that body adds (compared with prev) and that the
// note has no attachment for. Returns how many were queued.
func (s *Store) queueCopies(ctx context.Context, uid, noteID, prev, body string) (int, error) {
	return queueCopiesTx(ctx, s.DB.W, uid, noteID, prev, body)
}

func queueCopiesTx(ctx context.Context, x execer, uid, noteID, prev, body string) (int, error) {
	old := map[string]bool{}
	for _, r := range markerRefs(prev) {
		old[r] = true
	}
	n := 0
	for _, ref := range markerRefs(body) {
		if old[ref] {
			continue
		}
		res, err := x.ExecContext(ctx, `INSERT OR IGNORE INTO pending_copies(note_id,user_id,ref,created_at)
SELECT ?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM attachments WHERE note_id=? AND ref=? AND state IN ('ready','uploading'))`,
			noteID, uid, ref, now(), noteID, ref)
		if err != nil {
			return n, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return n, nil
}

// queueMissingCopies queues every marker of every note of a user that has no
// attachment in its note (conversion, rebuild, doctor).
func (s *Store) queueMissingCopies(ctx context.Context, uid string) error {
	ids, err := s.noteIDs(ctx, uid)
	if err != nil {
		return err
	}
	for _, id := range ids {
		b, err := os.ReadFile(s.notePath(uid, id))
		if err != nil {
			continue
		}
		if _, err := s.queueCopies(ctx, uid, id, "", string(b)); err != nil {
			return err
		}
	}
	return nil
}

// A marker whose image exists nowhere stays queued this long (its upload may not
// have been registered yet when the note was saved), then it is dropped.
const pendingGiveUp = 24 * time.Hour

// runPending makes the copies a note is waiting for. Must be called without
// holding any note lock. Returns how many waiting markers now have their copy
// in the note (copied now, or found already there).
func (s *Store) runPending(ctx context.Context, uid, noteID string) (int, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT ref,created_at FROM pending_copies WHERE note_id=? AND user_id=?`, noteID, uid)
	if err != nil {
		return 0, err
	}
	type item struct {
		ref string
		at  int64
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ref, &it.at); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	satisfied := 0
	var errs []error
	for _, it := range items {
		ok, err := s.settleCopy(ctx, uid, noteID, it.ref, time.UnixMilli(it.at))
		if ok {
			satisfied++
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("copy %s into note %s: %w", it.ref, noteID, err))
		}
	}
	return satisfied, errors.Join(errs...)
}

// settleCopy resolves one waiting marker. Everything that decides about the
// request — the note's text, the source, the copy and removing the request — runs
// under the note locks, so a save adding the marker again can't slip in between.
func (s *Store) settleCopy(ctx context.Context, uid, dst, ref string, queuedAt time.Time) (bool, error) {
	for range 3 {
		src, _ := s.copySource(ctx, uid, ref, dst) // a peek, to know which notes to lock
		locks := []string{dst}
		if src != nil {
			locks = append(locks, src.NoteID)
		}
		unlock := s.lockNotes(locks...)
		ok, retry, err := s.settleLocked(ctx, uid, dst, ref, queuedAt, src)
		unlock()
		if !retry {
			return ok, err
		}
	}
	return false, nil // the source keeps moving: next run
}

func (s *Store) settleLocked(ctx context.Context, uid, dst, ref string, queuedAt time.Time, peeked *Attachment) (ok, retry bool, err error) {
	drop := func() error {
		_, err := s.DB.W.ExecContext(context.WithoutCancel(ctx), `DELETE FROM pending_copies WHERE note_id=? AND ref=?`, dst, ref)
		return err
	}
	body, err := os.ReadFile(s.notePath(uid, dst))
	if err != nil {
		return false, false, nil // can't read the text now (purge in progress, I/O): keep it for later
	}
	if !slices.Contains(markerRefs(string(body)), ref) {
		return false, false, drop() // the marker was removed again
	}
	src, err := s.copySource(ctx, uid, ref, dst)
	switch {
	case errors.Is(err, errAlreadyHere):
		return true, false, drop()
	case errors.Is(err, errNotReady):
		return false, false, nil
	case errors.Is(err, errSourceLost):
		return false, false, drop() // nothing left to copy from; it would block deleting forever
	case errors.Is(err, errNoSource):
		if time.Since(queuedAt) > pendingGiveUp {
			return false, false, drop() // shows as "image not found"
		}
		return false, false, nil
	case err != nil:
		return false, false, err
	}
	if peeked == nil || peeked.NoteID != src.NoteID {
		return false, true, nil // locked the wrong source note: try again
	}
	if err := s.copyLocked(ctx, uid, src, dst); errors.Is(err, errAlreadyHere) {
		return true, false, drop()
	} else if err != nil {
		return false, false, err
	}
	return true, false, drop()
}

// runPendingRef completes the copies waiting for one image (its upload just finished).
func (s *Store) runPendingRef(ctx context.Context, uid, ref string) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT note_id FROM pending_copies WHERE user_id=? AND ref=?`, uid, ref)
	if err != nil {
		return
	}
	ids, _ := scanStrings(rows)
	for _, id := range ids {
		if _, err := s.runPending(ctx, uid, id); err != nil {
			s.Log.Warn("image copy (retried later)", "err", err)
		}
	}
}

// runAllPending retries every waiting copy (startup, janitor, conversion).
// Failures are logged and stay queued; they never fail the caller.
func (s *Store) runAllPending(ctx context.Context) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT DISTINCT user_id,note_id FROM pending_copies`)
	if err != nil {
		s.Log.Warn("image copies", "err", err)
		return
	}
	var pairs [][2]string
	for rows.Next() {
		var p [2]string
		rows.Scan(&p[0], &p[1])
		pairs = append(pairs, p)
	}
	rows.Close()
	for _, p := range pairs {
		if _, err := s.runPending(ctx, p[0], p[1]); err != nil {
			s.Log.Warn("image copy (retried later)", "err", err)
		}
	}
}

// lockNotes takes the note locks in id order (so two copies in opposite
// directions can't deadlock) and returns the unlock.
func (s *Store) lockNotes(ids ...string) func() {
	ids = slices.Clone(ids)
	sort.Strings(ids)
	var unlocks []func()
	last := ""
	for _, id := range ids {
		if id == last {
			continue
		}
		last = id
		unlocks = append(unlocks, s.locks.Lock("note:"+id))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

// copySource picks the newest ready copy of ref outside noteID.
func (s *Store) copySource(ctx context.Context, uid, ref, noteID string) (*Attachment, error) {
	if s.exists(ctx, `SELECT 1 FROM attachments WHERE note_id=? AND user_id=? AND ref=? AND state IN ('ready','uploading')`, noteID, uid, ref) {
		return nil, errAlreadyHere
	}
	if s.exists(ctx, `SELECT 1 FROM attachments WHERE note_id=? AND user_id=? AND ref=? AND state='deleting'`, noteID, uid, ref) {
		return nil, errNotReady // a copy being removed from this note: wait until it's gone
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT `+attCols+` FROM attachments
 WHERE user_id=? AND ref=? AND state='ready' AND note_id!=? AND ext!='' ORDER BY created_at DESC, id`, uid, ref, noteID)
	if err != nil {
		return nil, err
	}
	var cands []Attachment
	for rows.Next() {
		a, err := scanAtt(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, a)
	}
	rows.Close()
	for i := range cands {
		if fileExists(s.attPath(uid, &cands[i])) {
			return &cands[i], nil
		}
	}
	if len(cands) > 0 {
		// copies are listed but none has its file: nothing can be copied (doctor reports it)
		s.Log.Warn("image copy: no copy of the image has its file", "ref", ref)
		return nil, errSourceLost
	}
	if s.exists(ctx, `SELECT 1 FROM attachments WHERE user_id=? AND ref=? AND state='uploading'`, uid, ref) {
		return nil, errNotReady
	}
	return nil, errNoSource
}

// copyRef copies an image into dst's folder whether or not dst shows it
// (duplicating a note copies its panel-only files too).
func (s *Store) copyRef(ctx context.Context, uid, ref, dst string) error {
	for range 3 {
		src, err := s.copySource(ctx, uid, ref, dst)
		if err != nil {
			return err
		}
		unlock := s.lockNotes(src.NoteID, dst)
		cur, err := s.copySource(ctx, uid, ref, dst)
		if err == nil && cur.NoteID == src.NoteID {
			err = s.copyLocked(ctx, uid, cur, dst)
			unlock()
			return err
		}
		unlock()
		if err != nil {
			return err
		}
	}
	return errNotReady
}

// copyLocked copies src into dst's own folder: temp file, size and content
// check, durable rename, then the row (same ref, new id). Caller holds both note locks.
func (s *Store) copyLocked(ctx context.Context, uid string, src *Attachment, dst string) error {
	if _, err := s.noteMeta(ctx, s.DB.R, uid, dst); err != nil {
		return err
	}
	if err := s.ensureFilesDir(uid, dst); err != nil {
		return err
	}
	n := &Attachment{ID: NewID(), Ref: src.Ref, NoteID: dst, Filename: src.Filename, Mime: src.Mime, Ext: src.Ext,
		SizeBytes: src.SizeBytes, State: "ready", CreatedAt: now()}
	sum, err := s.copyBytes(s.attPath(uid, src), s.attPath(uid, n), src.SizeBytes, s.refSHA(ctx, uid, src.Ref))
	if err != nil {
		return err
	}
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertAtt(tx, uid, n, sum); errors.Is(err, ErrRefTaken) {
		return errAlreadyHere
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE ref_content SET sha256=? WHERE user_id=? AND ref=? AND sha256=''`, sum, uid, src.Ref); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.writeMeta(ctx, uid, dst)
	return nil
}

// refSHA is the known content hash of a ref ("" if not hashed yet).
func (s *Store) refSHA(ctx context.Context, uid, ref string) string {
	var sum string
	s.DB.R.QueryRowContext(ctx, `SELECT sha256 FROM ref_content WHERE user_id=? AND ref=?`, uid, ref).Scan(&sum)
	return sum
}

// copyBytes copies src to dst through a temp file next to dst, checking the size
// and (when known) the hash before the rename. An existing dst with the same
// content is kept; a different one is moved to lost+found first. Returns the hash.
func (s *Store) copyBytes(src, dst string, size int64, wantSHA string) (string, error) {
	l, err := s.reserve(size)
	if err != nil {
		return "", err
	}
	defer l.release()
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+"."+fsxRand()+".tmp")
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fsx.FileMode)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(&guardedWriter{f: out, l: l}, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && size >= 0 && n != size {
		err = fmt.Errorf("copied %d bytes, expected %d", n, size)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err == nil && wantSHA != "" && sum != wantSHA {
		err = errors.New("the image's content does not match its other copies")
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if cur, err := fsx.HashFile(dst); err == nil {
		if cur == sum {
			os.Remove(tmp)
			return sum, nil
		}
		if _, err := s.quarantine(dst, "copy-"+Stamp()); err != nil {
			os.Remove(tmp)
			return "", err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		os.Remove(tmp)
		return "", err
	}
	if err := fsx.MoveDurable(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return sum, nil
}

// fenceDeleting marks a note's attachments (or just one) 'deleting', refusing
// with ErrCopyPending if that would remove the last ready copy of an image that
// another note still waits for. Check and update are one immediate transaction,
// so they serialize with queueCopies and with other deletes.
func (s *Store) fenceDeleting(ctx context.Context, uid, noteID, attID string) error {
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := `SELECT ref FROM attachments WHERE note_id=? AND user_id=? AND state='ready'`
	args := []any{noteID, uid}
	if attID != "" {
		q += ` AND id=?`
		args = append(args, attID)
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	refs, err := scanStrings(rows)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		var waiting, others int
		err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM pending_copies WHERE user_id=? AND ref=? AND note_id!=?),
 (SELECT COUNT(*) FROM attachments WHERE user_id=? AND ref=? AND state='ready' AND note_id!=?)`,
			uid, ref, noteID, uid, ref, noteID).Scan(&waiting, &others)
		if err != nil {
			return err
		}
		if waiting > 0 && others == 0 {
			return ErrCopyPending
		}
	}
	u := `UPDATE attachments SET state='deleting', updated_at=? WHERE note_id=? AND user_id=?`
	uargs := []any{now(), noteID, uid}
	if attID != "" {
		u += ` AND id=?`
		uargs = append(uargs, attID)
	}
	if _, err := tx.ExecContext(ctx, u, uargs...); err != nil {
		return err
	}
	return tx.Commit()
}

// flushPendingFor completes waiting copies of the given refs before their source goes away.
func (s *Store) flushPendingFor(ctx context.Context, uid string, refs []string) {
	for _, ref := range refs {
		s.runPendingRef(ctx, uid, ref)
	}
}
