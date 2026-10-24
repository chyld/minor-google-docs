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
	"sort"
	"strings"

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

// queueCopies records markers that body adds (compared with prev) and that the
// note has no attachment for. Returns how many were queued.
func (s *Store) queueCopies(ctx context.Context, uid, noteID, prev, body string) (int, error) {
	old := map[string]bool{}
	for _, r := range markerRefs(prev) {
		old[r] = true
	}
	n := 0
	for _, ref := range markerRefs(body) {
		if old[ref] {
			continue
		}
		res, err := s.DB.W.ExecContext(ctx, `INSERT OR IGNORE INTO pending_copies(note_id,user_id,ref,created_at)
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
// attachment in its note (conversion, doctor).
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

// runPending makes the copies a note is waiting for. Must be called without
// holding any note lock. Returns how many images were copied.
func (s *Store) runPending(ctx context.Context, uid, noteID string) (int, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT ref FROM pending_copies WHERE note_id=? AND user_id=?`, noteID, uid)
	if err != nil {
		return 0, err
	}
	refs, err := scanStrings(rows)
	if err != nil || len(refs) == 0 {
		return 0, err
	}
	body := s.readAnyBody(uid, noteID)
	present := map[string]bool{}
	for _, r := range markerRefs(body) {
		present[r] = true
	}
	copied := 0
	var errs []error
	for _, ref := range refs {
		done := true
		if present[ref] {
			err := s.copyRef(ctx, uid, ref, noteID)
			switch {
			case err == nil:
				copied++
			case errors.Is(err, errAlreadyHere), errors.Is(err, errNoSource), errors.Is(err, ErrNotFound):
				// nothing (more) to copy: the marker shows "image not found" if there's no image at all
			default:
				done = false // still uploading, out of space, I/O error: retried later
				if !errors.Is(err, errNotReady) {
					errs = append(errs, fmt.Errorf("copy %s into note %s: %w", ref, noteID, err))
				}
			}
		}
		if done {
			if _, err := s.DB.W.ExecContext(context.WithoutCancel(ctx), `DELETE FROM pending_copies WHERE note_id=? AND ref=?`, noteID, ref); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return copied, errors.Join(errs...)
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
			s.Log.Warn("image copy", "err", err)
		}
	}
}

// runAllPending retries every waiting copy (startup, janitor, conversion).
func (s *Store) runAllPending(ctx context.Context) error {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT DISTINCT user_id,note_id FROM pending_copies`)
	if err != nil {
		return err
	}
	var pairs [][2]string
	for rows.Next() {
		var p [2]string
		rows.Scan(&p[0], &p[1])
		pairs = append(pairs, p)
	}
	rows.Close()
	var errs []error
	for _, p := range pairs {
		if _, err := s.runPending(ctx, p[0], p[1]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// lockNotes takes the note locks in id order (so two copies in opposite
// directions can't deadlock) and returns the unlock.
func (s *Store) lockNotes(ids ...string) func() {
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
	a, err := scanAtt(s.DB.R.QueryRowContext(ctx, `SELECT `+attCols+` FROM attachments
 WHERE user_id=? AND ref=? AND state='ready' AND note_id!=? AND ext!='' ORDER BY created_at DESC, id LIMIT 1`, uid, ref, noteID))
	if errors.Is(err, sql.ErrNoRows) {
		if s.exists(ctx, `SELECT 1 FROM attachments WHERE user_id=? AND ref=? AND state='uploading'`, uid, ref) {
			return nil, errNotReady
		}
		return nil, errNoSource
	}
	return &a, err
}

// copyRef copies an image into dst's own folder: temp file, size and content
// check, durable rename, then the row (same ref, new id).
func (s *Store) copyRef(ctx context.Context, uid, ref, dst string) error {
	src, err := s.copySource(ctx, uid, ref, dst)
	if err != nil {
		return err
	}
	unlock := s.lockNotes(src.NoteID, dst)
	defer unlock()
	// re-check under the locks: the source may have been deleted, or a copy made meanwhile
	if src, err = s.copySource(ctx, uid, ref, dst); err != nil {
		return err
	}
	if _, err := s.noteMeta(ctx, s.DB.R, uid, dst); err != nil {
		return err
	}
	if err := s.ensureFilesDir(uid, dst); err != nil {
		return err
	}
	n := &Attachment{ID: NewID(), Ref: ref, NoteID: dst, Filename: src.Filename, Mime: src.Mime, Ext: src.Ext,
		SizeBytes: src.SizeBytes, State: "ready", CreatedAt: now()}
	sum, err := s.copyBytes(s.attPath(uid, src), s.attPath(uid, n), src.SizeBytes, s.refSHA(ctx, uid, ref))
	if err != nil {
		return err
	}
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO attachments(`+attCols+`,user_id,updated_at,sha256) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.ID, n.Ref, n.NoteID, n.Filename, n.Mime, n.Ext, n.SizeBytes, n.State, n.CreatedAt, uid, n.CreatedAt, sum); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE ref_content SET sha256=? WHERE user_id=? AND ref=? AND sha256=''`, sum, uid, ref); err != nil {
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

// guardLastCopy refuses to remove the last ready copy of ref (held by noteID)
// while another note still waits for its own copy. Caller holds noteID's lock.
func (s *Store) guardLastCopy(ctx context.Context, uid, noteID, ref string) error {
	if !s.exists(ctx, `SELECT 1 FROM pending_copies WHERE user_id=? AND ref=? AND note_id!=?`, uid, ref, noteID) {
		return nil
	}
	if s.exists(ctx, `SELECT 1 FROM attachments WHERE user_id=? AND ref=? AND state='ready' AND note_id!=?`, uid, ref, noteID) {
		return nil
	}
	return ErrCopyPending
}

// flushPendingFor completes waiting copies of the given refs before their source goes away.
func (s *Store) flushPendingFor(ctx context.Context, uid string, refs []string) {
	for _, ref := range refs {
		s.runPendingRef(ctx, uid, ref)
	}
}
