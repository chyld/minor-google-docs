package core

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"homelab-notes/internal/fsx"
)

var ErrRefTaken = errors.New("attachment id or ref already in use")

// InlineImageTypes are the only types ever rendered inline (sniffed by magic bytes).
var InlineImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

type Attachment struct {
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	NoteID    string `json:"note_id"`
	Filename  string `json:"filename"`
	Mime      string `json:"mime"`
	SizeBytes int64  `json:"size_bytes"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
	Inline    bool   `json:"inline"`
	Ext       string `json:"-"` // disk extension ("" until the type is sniffed at finalize)
}

const attCols = `id,ref,note_id,filename,mime,ext,size_bytes,state,created_at`

func scanAtt(sc interface{ Scan(...any) error }) (Attachment, error) {
	var a Attachment
	err := sc.Scan(&a.ID, &a.Ref, &a.NoteID, &a.Filename, &a.Mime, &a.Ext, &a.SizeBytes, &a.State, &a.CreatedAt)
	a.Inline = InlineImageTypes[a.Mime]
	return a, err
}

func SanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '_'
		case unicode.IsControl(r) || r == utf8.RuneError:
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	name = strings.Trim(name, ". ")
	for len(name) > 200 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" {
		return "file"
	}
	return name
}

type UploadMeta struct {
	ID, Ref, NoteID, Filename string
	Length                    int64 // -1 = deferred
}

// ValidAttachmentID requires a canonical UUIDv7 (client-generated).
func ValidAttachmentID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id && u.Version() == 7 && u.Variant() == uuid.RFC4122
}

// CreateUpload registers a new upload. It is idempotent: repeating the same
// id/ref/note (e.g. after a lost response) returns the existing upload.
func (s *Store) CreateUpload(ctx context.Context, uid string, m UploadMeta) (*Attachment, error) {
	if !ValidAttachmentID(m.ID) || !ValidRef(m.Ref) || !ValidID(m.NoteID) {
		return nil, Invalid("invalid upload metadata")
	}
	if m.Length < -1 {
		return nil, Invalid("invalid upload length")
	}
	unlock := s.locks.Lock("att:" + m.ID)
	defer unlock()
	if ex, err := s.getAtt(ctx, uid, m.ID); err == nil {
		if ex.Ref == m.Ref && ex.NoteID == m.NoteID && ex.State != "deleting" {
			return ex, nil
		}
		return nil, ErrRefTaken
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	l, err := s.reserve(m.Length)
	if err != nil {
		return nil, err
	}
	l.release() // just a check; bytes are leased when they arrive
	nm, err := s.noteMeta(ctx, s.DB.R, uid, m.NoteID)
	if err != nil {
		return nil, err
	}
	if nm.TrashedAt != nil {
		return nil, Invalid("cannot attach files to a note in the trash")
	}
	if err := s.ensureUserDirs(uid); err != nil {
		return nil, err
	}
	// A leftover temp file without a row (crash between create steps) is stale: replace it.
	f, err := os.OpenFile(s.tmpPath(uid, m.ID), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fsx.FileMode)
	if err != nil {
		return nil, err
	}
	f.Close()
	t := now()
	a := &Attachment{ID: m.ID, Ref: m.Ref, NoteID: m.NoteID, Filename: SanitizeFilename(m.Filename),
		Mime: "application/octet-stream", SizeBytes: m.Length, State: "uploading", CreatedAt: t}
	if err := s.insertNewRef(ctx, uid, a, ""); err != nil {
		os.Remove(s.tmpPath(uid, m.ID))
		return nil, err
	}
	return a, nil
}

// insertNewRef claims a's ref for a new image and inserts its row, atomically:
// a ref used anywhere by the user (even by a deleted image) is refused with
// ErrRefTaken, so one ref always means one image content.
func (s *Store) insertNewRef(ctx context.Context, uid string, a *Attachment, sha string) error {
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO ref_content(user_id,ref,sha256) VALUES(?,?,?) ON CONFLICT DO NOTHING`, uid, a.Ref, sha)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRefTaken
	}
	if err := insertAtt(tx, uid, a, sha); err != nil {
		return err
	}
	return tx.Commit()
}

func insertAtt(tx *sql.Tx, uid string, a *Attachment, sha string) error {
	var sum any
	if sha != "" {
		sum = sha
	}
	_, err := tx.Exec(`INSERT INTO attachments(`+attCols+`,user_id,updated_at,sha256) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Ref, a.NoteID, a.Filename, a.Mime, a.Ext, a.SizeBytes, a.State, a.CreatedAt, uid, a.CreatedAt, sum)
	if err != nil && (strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY")) {
		return ErrRefTaken
	}
	return err
}

func (s *Store) getAtt(ctx context.Context, uid, id string) (*Attachment, error) {
	a, err := scanAtt(s.DB.R.QueryRowContext(ctx, `SELECT `+attCols+` FROM attachments WHERE id=? AND user_id=?`, id, uid))
	if err != nil {
		return nil, notFoundIfNoRows(err)
	}
	return &a, nil
}

// UploadOffset returns the attachment and how many bytes have been received.
// It also completes an upload whose bytes were moved into place but whose row
// was never marked ready (crash or disconnect during finalize).
func (s *Store) UploadOffset(ctx context.Context, uid, id string) (*Attachment, int64, error) {
	unlock := s.locks.Lock("att:" + id)
	defer unlock()
	return s.uploadOffset(ctx, uid, id)
}

// uploadOffset is UploadOffset for callers that already hold the attachment lock.
func (s *Store) uploadOffset(ctx context.Context, uid, id string) (*Attachment, int64, error) {
	a, err := s.getAtt(ctx, uid, id)
	if err != nil {
		return nil, 0, err
	}
	switch a.State {
	case "ready":
		return a, a.SizeBytes, nil
	case "uploading":
		if st, err := os.Stat(s.tmpPath(uid, id)); err == nil {
			return a, st.Size(), nil
		}
		if a.Ext == "" {
			return nil, 0, ErrNotFound // never reached finalize
		}
		st, err := os.Stat(s.attPath(uid, a))
		if err != nil {
			return nil, 0, ErrNotFound
		}
		if a.SizeBytes < 0 || st.Size() == a.SizeBytes {
			a.SizeBytes = st.Size()
			if err := s.markReady(ctx, uid, a); err != nil {
				return nil, 0, err
			}
		}
		return a, st.Size(), nil
	}
	return nil, 0, ErrNotFound
}

// OffsetMismatchError tells the client where the upload actually is.
type OffsetMismatchError struct{ Actual int64 }

func (e *OffsetMismatchError) Error() string { return "upload offset mismatch" }

// spaceLeases tracks bytes that in-flight uploads have announced but not yet
// written, so concurrent uploads can't each pass the free-space check against
// the same free bytes and then fill the disk together.
type spaceLeases struct {
	mu  sync.Mutex
	out int64
}

const freeCheckEvery = 64 << 20

// lease holds n bytes (n < 0: unknown length) against the free-space floor.
type lease struct {
	s       *Store
	left    int64
	unknown bool
}

// reserve fails if writing n more bytes (on top of everything other uploads
// still have to write) would dip below the free-space floor. n < 0 = unknown.
func (s *Store) reserve(n int64) (*lease, error) {
	l := &lease{s: s, left: max(n, 0), unknown: n < 0}
	if s.Cfg.MinFree <= 0 {
		l.left = 0
		return l, nil
	}
	s.space.mu.Lock()
	defer s.space.mu.Unlock()
	free, err := fsx.FreeBytes(s.Dir)
	if err != nil {
		return nil, err
	}
	if free-s.space.out-l.need() < s.Cfg.MinFree {
		return nil, ErrNoSpace
	}
	s.space.out += l.left
	return l, nil
}

func (l *lease) need() int64 {
	if l.unknown {
		return freeCheckEvery
	}
	return l.left
}

// wrote marks n leased bytes as on disk (they now show up in free space).
func (l *lease) wrote(n int64) {
	l.s.space.mu.Lock()
	d := min(n, l.left)
	l.left -= d
	l.s.space.out -= d
	l.s.space.mu.Unlock()
}

// check re-verifies the floor mid-stream (other processes may be using space too).
func (l *lease) check() error {
	if l.s.Cfg.MinFree <= 0 {
		return nil
	}
	l.s.space.mu.Lock()
	defer l.s.space.mu.Unlock()
	free, err := fsx.FreeBytes(l.s.Dir)
	if err != nil {
		return err
	}
	extra := int64(0)
	if l.unknown {
		extra = freeCheckEvery
	}
	if free-l.s.space.out-extra < l.s.Cfg.MinFree {
		return ErrNoSpace
	}
	return nil
}

func (l *lease) release() {
	l.s.space.mu.Lock()
	l.s.space.out -= l.left
	l.left = 0
	l.s.space.mu.Unlock()
}

// guardedWriter re-checks the free-space floor every 64 MiB while streaming,
// so one request can't fill the disk.
type guardedWriter struct {
	f          *os.File
	l          *lease
	sinceCheck int64
}

func (w *guardedWriter) Write(p []byte) (int, error) {
	if w.sinceCheck >= freeCheckEvery {
		w.sinceCheck = 0
		if err := w.l.check(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.sinceCheck += int64(n)
	w.l.wrote(int64(n))
	return n, err
}

// AppendUpload appends a chunk at offset. Returns the attachment (State reports
// whether it was finalized) and the new offset.
func (s *Store) AppendUpload(ctx context.Context, uid, id string, offset int64, newLength int64, r io.Reader) (*Attachment, int64, error) {
	unlock := s.locks.Lock("att:" + id)
	defer unlock()
	a, cur, err := s.uploadOffset(ctx, uid, id)
	if err != nil {
		return nil, 0, err
	}
	if a.State == "ready" {
		if offset == a.SizeBytes {
			return a, cur, nil // idempotent completion
		}
		return nil, 0, &OffsetMismatchError{cur}
	}
	if cur != offset {
		return nil, 0, &OffsetMismatchError{cur}
	}
	if newLength >= 0 {
		if a.SizeBytes >= 0 && a.SizeBytes != newLength {
			return nil, 0, Invalid("upload length cannot change")
		}
		if newLength < cur {
			return nil, 0, Invalid("upload length smaller than offset")
		}
		if a.SizeBytes < 0 {
			if _, err := s.DB.W.ExecContext(ctx, `UPDATE attachments SET size_bytes=?, updated_at=? WHERE id=?`, newLength, now(), id); err != nil {
				return nil, 0, err
			}
			a.SizeBytes = newLength
		}
	}
	remaining := int64(-1)
	if a.SizeBytes >= 0 {
		remaining = a.SizeBytes - cur
	}
	l, err := s.reserve(remaining)
	if err != nil {
		return nil, 0, err
	}
	defer l.release()
	f, err := os.OpenFile(s.tmpPath(uid, id), os.O_WRONLY|os.O_APPEND, fsx.FileMode)
	if err != nil {
		return nil, 0, err
	}
	src := r
	if remaining >= 0 {
		src = io.LimitReader(r, remaining)
	}
	gw := &guardedWriter{f: f, l: l}
	n, copyErr := io.Copy(gw, src)
	syncErr := f.Sync()
	f.Close()
	cur += n
	dbctx := context.WithoutCancel(ctx)
	if n > 0 {
		s.DB.W.ExecContext(dbctx, `UPDATE attachments SET updated_at=? WHERE id=?`, now(), id)
	}
	if copyErr != nil {
		if errors.Is(copyErr, syscall.ENOSPC) || errors.Is(copyErr, ErrNoSpace) {
			return nil, cur, ErrNoSpace
		}
		// Client disconnects are normal for resumable uploads; bytes written so far are kept.
		return a, cur, copyErr
	}
	if syncErr != nil {
		return nil, cur, syncErr
	}
	if a.SizeBytes >= 0 && cur == a.SizeBytes {
		if err := s.finalize(dbctx, uid, a); err != nil {
			return nil, cur, err
		}
	}
	return a, cur, nil
}

// finalize sniffs the type, records the type and disk extension on the row, then
// moves the bytes into the note's files/ folder and marks the row ready. The
// extension is stored first so a crash after the move can still find the file.
// Caller holds the attachment lock. Safe to repeat (see UploadOffset).
func (s *Store) finalize(ctx context.Context, uid string, a *Attachment) error {
	tmp := s.tmpPath(uid, a.ID)
	head, err := readHead(tmp)
	if err != nil {
		return err
	}
	a.Mime, a.Ext = detectMime(head, a.Filename), sniffExt(head)
	res, err := s.DB.W.ExecContext(ctx, `UPDATE attachments SET mime=?, ext=?, updated_at=? WHERE id=? AND user_id=? AND state='uploading'`,
		a.Mime, a.Ext, now(), a.ID, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		os.Remove(tmp) // purged or canceled meanwhile
		return ErrNotFound
	}
	if err := s.ensureFilesDir(uid, a.NoteID); err != nil {
		// Only a purge or cancel (the row no longer 'uploading') makes the bytes
		// garbage. Any other failure keeps them for a retry or for doctor.
		var state string
		qerr := s.DB.W.QueryRowContext(ctx, `SELECT state FROM attachments WHERE id=? AND user_id=?`, a.ID, uid).Scan(&state)
		if errors.Is(qerr, sql.ErrNoRows) || (qerr == nil && state != "uploading") {
			os.Remove(tmp)
			return ErrNotFound
		}
		return err
	}
	if err := fsx.MoveDurable(tmp, s.attPath(uid, a)); err != nil {
		return err
	}
	return s.markReady(ctx, uid, a)
}

func (s *Store) markReady(ctx context.Context, uid string, a *Attachment) error {
	res, err := s.DB.W.ExecContext(ctx, `UPDATE attachments SET state='ready', size_bytes=?, updated_at=? WHERE id=? AND user_id=? AND state='uploading'`,
		a.SizeBytes, now(), a.ID, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var state string
		err := s.DB.W.QueryRowContext(ctx, `SELECT state FROM attachments WHERE id=? AND user_id=?`, a.ID, uid).Scan(&state)
		if err == nil && state == "ready" {
			a.State = "ready" // someone else finished it: that's success, keep the file
			a.Inline = InlineImageTypes[a.Mime]
			return nil
		}
		// purged/canceled meanwhile: don't leave the bytes behind
		os.Remove(s.attPath(uid, a))
		return ErrNotFound
	}
	a.State = "ready"
	a.Inline = InlineImageTypes[a.Mime]
	s.writeMeta(ctx, uid, a.NoteID)
	bg := context.WithoutCancel(ctx)
	go func() {
		s.fillSHA(bg, uid, a.ID)
		s.runPendingRef(bg, uid, a.Ref) // notes that were waiting for this image get their copy
	}()
	return nil
}

func detectMime(head []byte, filename string) string {
	sniffed := http.DetectContentType(head)
	if InlineImageTypes[sniffed] {
		return sniffed
	}
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); t != "" {
		if InlineImageTypes[strings.Split(t, ";")[0]] {
			return "application/octet-stream" // claims to be an image but isn't one
		}
		return t
	}
	return sniffed
}

func (s *Store) fillSHA(ctx context.Context, uid, id string) {
	a, err := s.getAtt(ctx, uid, id)
	if err != nil || a.State != "ready" {
		return
	}
	sum, err := fsx.HashFile(s.attPath(uid, a))
	if err != nil {
		s.Log.Warn("attachment hash", "id", id, "err", err)
		return
	}
	s.DB.W.ExecContext(ctx, `UPDATE attachments SET sha256=? WHERE id=? AND user_id=?`, sum, id, uid)
	if want := s.refSHA(ctx, uid, a.Ref); want == "" {
		s.DB.W.ExecContext(ctx, `UPDATE ref_content SET sha256=? WHERE user_id=? AND ref=? AND sha256=''`, sum, uid, a.Ref)
	} else if want != sum {
		s.Log.Warn("image content differs from its other copies (run `notes doctor`)", "ref", a.Ref, "note", a.NoteID)
	}
	s.writeMeta(ctx, uid, a.NoteID)
}

func (s *Store) ListAttachments(ctx context.Context, uid, noteID string) ([]Attachment, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT `+attCols+` FROM attachments WHERE note_id=? AND user_id=? AND state='ready' ORDER BY created_at`, noteID, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAtt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AttachmentFile returns a ready attachment and its path, authorized by user.
func (s *Store) AttachmentFile(ctx context.Context, uid, id string) (*Attachment, string, error) {
	a, err := s.getAtt(ctx, uid, id)
	if err != nil || a.State != "ready" {
		return nil, "", ErrNotFound
	}
	return a, s.attPath(uid, a), nil
}

// AttachmentByRef resolves an inline marker ref for the session user. Every copy
// of a ref has the same content; the newest one is returned.
func (s *Store) AttachmentByRef(ctx context.Context, uid, ref string) (*Attachment, error) {
	a, err := scanAtt(s.DB.R.QueryRowContext(ctx, `SELECT `+attCols+` FROM attachments WHERE user_id=? AND ref=? AND state='ready' ORDER BY created_at DESC, id LIMIT 1`, uid, ref))
	if err != nil {
		return nil, notFoundIfNoRows(err)
	}
	return &a, nil
}

func (s *Store) RenameAttachment(ctx context.Context, uid, id, name string) (*Attachment, error) {
	res, err := s.DB.W.ExecContext(ctx, `UPDATE attachments SET filename=?, updated_at=? WHERE id=? AND user_id=? AND state='ready'`,
		SanitizeFilename(name), now(), id, uid)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	a, err := s.getAtt(ctx, uid, id)
	if err == nil {
		s.writeMeta(ctx, uid, a.NoteID)
	}
	return a, err
}

func (s *Store) DeleteAttachment(ctx context.Context, uid, id string) error {
	a, err := s.getAtt(ctx, uid, id)
	if err != nil {
		return err
	}
	if a.State == "ready" {
		s.flushPendingFor(ctx, uid, []string{a.Ref}) // other notes waiting for this image copy it first
	}
	unlockNote := s.locks.Lock("note:" + a.NoteID)
	defer unlockNote()
	unlock := s.locks.Lock("att:" + id)
	defer unlock()
	if a, err = s.getAtt(ctx, uid, id); err != nil {
		return err
	}
	if err := s.fenceDeleting(ctx, uid, a.NoteID, id); err != nil {
		return err // ErrCopyPending → 409 copy_pending
	}
	if a.State != "ready" {
		// uploading (a cancel) or already deleting: fence those too
		if _, err := s.DB.W.ExecContext(ctx, `UPDATE attachments SET state='deleting', updated_at=? WHERE id=? AND user_id=?`, now(), id, uid); err != nil {
			return err
		}
	}
	return s.removeAttachment(context.WithoutCancel(ctx), uid, id)
}

// removeAttachment unlinks bytes and the row. Caller holds the attachment lock.
func (s *Store) removeAttachment(ctx context.Context, uid, id string) error {
	a, err := s.getAtt(ctx, uid, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	paths := []string{s.tmpPath(uid, id)}
	if a.Ext != "" {
		paths = append(paths, s.attPath(uid, a))
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if _, err := s.DB.W.ExecContext(ctx, `DELETE FROM attachments WHERE id=? AND user_id=?`, id, uid); err != nil {
		return err
	}
	s.writeMeta(ctx, uid, a.NoteID)
	return nil
}

// finishDeletingAttachments removes every 'deleting' attachment (of one note, or all if noteID is "").
func (s *Store) finishDeletingAttachments(ctx context.Context, uid, noteID string) error {
	q := `SELECT user_id,id FROM attachments WHERE state='deleting'`
	var args []any
	if noteID != "" {
		q += ` AND note_id=? AND user_id=?`
		args = append(args, noteID, uid)
	}
	rows, err := s.DB.R.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	var pairs [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			rows.Close()
			return err
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	for _, p := range pairs {
		unlock := s.locks.Lock("att:" + p[1]) // waits for an in-flight chunk/finalize
		err := s.removeAttachment(ctx, p[0], p[1])
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// GCUploads removes uploads idle for longer than maxAge.
func (s *Store) GCUploads(ctx context.Context, maxAge time.Duration) (int, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT user_id,id FROM attachments WHERE state='uploading'`)
	if err != nil {
		return 0, err
	}
	var ups [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			rows.Close()
			return 0, err
		}
		ups = append(ups, p)
	}
	rows.Close()
	cutoff := time.Now().Add(-maxAge)
	n := 0
	for _, u := range ups {
		unlock := s.locks.Lock("att:" + u[1])
		// re-check under the lock: a resume may have just finished it
		var state string
		var upd int64
		err := s.DB.R.QueryRowContext(ctx, `SELECT state,updated_at FROM attachments WHERE id=?`, u[1]).Scan(&state, &upd)
		last := time.UnixMilli(upd)
		if st, serr := os.Stat(s.tmpPath(u[0], u[1])); serr == nil && st.ModTime().After(last) {
			last = st.ModTime()
		}
		if err == nil && state == "uploading" && last.Before(cutoff) {
			// bytes already moved into the note (finalize crashed before marking it
			// ready): finish it instead of throwing the only copy away
			if a, err := s.getAtt(ctx, u[0], u[1]); err == nil && a.Ext != "" && fileExists(s.attPath(u[0], a)) {
				if _, _, err := s.uploadOffset(ctx, u[0], u[1]); err != nil {
					s.Log.Warn("finishing an interrupted upload", "id", u[1], "err", err)
				}
				unlock()
				continue
			}
			if err := s.removeAttachment(ctx, u[0], u[1]); err != nil {
				unlock()
				return n, err
			}
			n++
		}
		unlock()
	}
	return n, nil
}

type Storage struct {
	UsedBytes int64 `json:"used_bytes"`
	FreeBytes int64 `json:"free_bytes"`
	MinFree   int64 `json:"min_free_bytes"`
}

func (s *Store) StorageFor(ctx context.Context, uid string) (*Storage, error) {
	var st Storage
	err := s.DB.R.QueryRowContext(ctx, `SELECT
 (SELECT COALESCE(SUM(size_bytes),0) FROM notes WHERE user_id=?) +
 (SELECT COALESCE(SUM(MAX(size_bytes,0)),0) FROM attachments WHERE user_id=?)`, uid, uid).Scan(&st.UsedBytes)
	if err != nil {
		return nil, err
	}
	st.FreeBytes, _ = fsx.FreeBytes(s.Dir)
	st.MinFree = s.Cfg.MinFree
	return &st, nil
}
