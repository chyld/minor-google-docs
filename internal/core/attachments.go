package core

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
}

const attCols = `id,ref,note_id,filename,mime,size_bytes,state,created_at`

func scanAtt(sc interface{ Scan(...any) error }) (Attachment, error) {
	var a Attachment
	err := sc.Scan(&a.ID, &a.Ref, &a.NoteID, &a.Filename, &a.Mime, &a.SizeBytes, &a.State, &a.CreatedAt)
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
	if err := s.reserve(m.Length); err != nil {
		return nil, err
	}
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
	_, err = s.DB.W.ExecContext(ctx, `INSERT INTO attachments(`+attCols+`,user_id,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Ref, a.NoteID, a.Filename, a.Mime, a.SizeBytes, a.State, a.CreatedAt, uid, t)
	if err != nil {
		os.Remove(s.tmpPath(uid, m.ID))
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return nil, ErrRefTaken
		}
		return nil, err
	}
	return a, nil
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
		st, err := os.Stat(s.filePath(uid, id))
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

// reserve fails if writing n more bytes would dip below the free-space floor.
// n < 0 means "unknown"; only the floor itself is checked.
func (s *Store) reserve(n int64) error {
	if s.Cfg.MinFree <= 0 {
		return nil
	}
	free, err := fsx.FreeBytes(s.Dir)
	if err != nil {
		return err
	}
	if free < s.Cfg.MinFree || (n > 0 && free-n < s.Cfg.MinFree) {
		return ErrNoSpace
	}
	return nil
}

// guardedWriter re-checks the free-space floor every 64 MiB while streaming a
// body of unknown length, so one request can't fill the disk.
type guardedWriter struct {
	f          *os.File
	s          *Store
	check      bool
	sinceCheck int64
}

const freeCheckEvery = 64 << 20

func (w *guardedWriter) Write(p []byte) (int, error) {
	if w.check && w.sinceCheck >= freeCheckEvery {
		w.sinceCheck = 0
		if err := w.s.reserve(freeCheckEvery); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.sinceCheck += int64(n)
	return n, err
}

// AppendUpload appends a chunk at offset. Returns the attachment (State reports
// whether it was finalized) and the new offset.
func (s *Store) AppendUpload(ctx context.Context, uid, id string, offset int64, newLength int64, r io.Reader) (*Attachment, int64, error) {
	unlock := s.locks.Lock("att:" + id)
	defer unlock()
	a, cur, err := s.UploadOffset(ctx, uid, id)
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
	if err := s.reserve(remaining); err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(s.tmpPath(uid, id), os.O_WRONLY|os.O_APPEND, fsx.FileMode)
	if err != nil {
		return nil, 0, err
	}
	src := r
	if remaining >= 0 {
		src = io.LimitReader(r, remaining)
	}
	// a declared length was reserved up front; unknown lengths are checked as they stream
	gw := &guardedWriter{f: f, s: s, check: remaining < 0}
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

// finalize sniffs the type, moves bytes into users/<uid>/files and marks the row
// ready. Caller holds the attachment lock. Safe to repeat (see UploadOffset).
func (s *Store) finalize(ctx context.Context, uid string, a *Attachment) error {
	tmp := s.tmpPath(uid, a.ID)
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	f.Close()
	if err := fsx.MoveDurable(tmp, s.filePath(uid, a.ID)); err != nil {
		return err
	}
	return s.markReady(ctx, uid, a)
}

func (s *Store) markReady(ctx context.Context, uid string, a *Attachment) error {
	head := make([]byte, 512)
	f, err := os.Open(s.filePath(uid, a.ID))
	if err != nil {
		return err
	}
	n, _ := io.ReadFull(f, head)
	f.Close()
	a.Mime = detectMime(head[:n], a.Filename)
	res, err := s.DB.W.ExecContext(ctx, `UPDATE attachments SET state='ready', mime=?, size_bytes=?, updated_at=? WHERE id=? AND user_id=? AND state='uploading'`,
		a.Mime, a.SizeBytes, now(), a.ID, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// purged/canceled meanwhile: don't leave the bytes behind
		os.Remove(s.filePath(uid, a.ID))
		return ErrNotFound
	}
	a.State = "ready"
	a.Inline = InlineImageTypes[a.Mime]
	go s.fillSHA(context.WithoutCancel(ctx), uid, a.ID)
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
	sum, err := fsx.HashFile(s.filePath(uid, id))
	if err != nil {
		s.Log.Warn("attachment hash", "id", id, "err", err)
		return
	}
	s.DB.W.ExecContext(ctx, `UPDATE attachments SET sha256=? WHERE id=? AND user_id=?`, sum, id, uid)
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
	return a, s.filePath(uid, id), nil
}

// AttachmentByRef resolves an inline marker ref for the session user.
func (s *Store) AttachmentByRef(ctx context.Context, uid, ref string) (*Attachment, error) {
	a, err := scanAtt(s.DB.R.QueryRowContext(ctx, `SELECT `+attCols+` FROM attachments WHERE user_id=? AND ref=? AND state='ready'`, uid, ref))
	if err != nil {
		return nil, notFoundIfNoRows(err)
	}
	return &a, nil
}

// copyAttachment gives noteID its own copy (new id + ref) of a ready attachment.
func (s *Store) copyAttachment(ctx context.Context, uid, noteID string, a *Attachment) (*Attachment, error) {
	src := s.filePath(uid, a.ID)
	n := &Attachment{ID: NewID(), Ref: NewRef(), NoteID: noteID, Filename: a.Filename, Mime: a.Mime,
		SizeBytes: a.SizeBytes, State: "ready", CreatedAt: now(), Inline: a.Inline}
	dst := s.filePath(uid, n.ID)
	if err := os.Link(src, dst); err != nil { // hard link: instant, no extra space
		if err := copyFile(src, dst); err != nil {
			return nil, err
		}
	}
	for range 5 {
		_, err := s.DB.W.ExecContext(ctx, `INSERT INTO attachments(`+attCols+`,user_id,updated_at,sha256)
SELECT ?,?,?,?,?,?,?,?,?,?,sha256 FROM attachments WHERE id=?`,
			n.ID, n.Ref, n.NoteID, n.Filename, n.Mime, n.SizeBytes, n.State, n.CreatedAt, uid, n.CreatedAt, a.ID)
		if err == nil {
			return n, nil
		}
		if !strings.Contains(err.Error(), "UNIQUE") {
			os.Remove(dst)
			return nil, err
		}
		n.Ref = NewRef()
	}
	os.Remove(dst)
	return nil, ErrRefTaken
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fsx.FileMode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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
	return s.getAtt(ctx, uid, id)
}

func (s *Store) DeleteAttachment(ctx context.Context, uid, id string) error {
	unlock := s.locks.Lock("att:" + id)
	defer unlock()
	if _, err := s.getAtt(ctx, uid, id); err != nil {
		return err
	}
	if _, err := s.DB.W.ExecContext(ctx, `UPDATE attachments SET state='deleting', updated_at=? WHERE id=? AND user_id=?`, now(), id, uid); err != nil {
		return err
	}
	return s.removeAttachment(context.WithoutCancel(ctx), uid, id)
}

// removeAttachment unlinks bytes and the row. Caller holds the attachment lock.
func (s *Store) removeAttachment(ctx context.Context, uid, id string) error {
	for _, p := range []string{s.tmpPath(uid, id), s.filePath(uid, id)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	_, err := s.DB.W.ExecContext(ctx, `DELETE FROM attachments WHERE id=? AND user_id=?`, id, uid)
	return err
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
