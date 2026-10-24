package core

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"homelab-notes/internal/fsx"
)

type NoteMeta struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Preview         string    `json:"preview"`
	Pinned          bool      `json:"pinned"`
	TrashedAt       *int64    `json:"trashed_at"`
	CreatedAt       int64     `json:"created_at"`
	UpdatedAt       int64     `json:"updated_at"`
	SizeBytes       int64     `json:"size_bytes"`
	SHA256          string    `json:"sha256"`
	TagIDs          []string  `json:"tag_ids"`
	AttachmentCount int       `json:"attachment_count"`
	Snippet         []Segment `json:"snippet,omitempty"`
}

type Note struct {
	NoteMeta
	Body        string       `json:"body"`
	Attachments []Attachment `json:"attachments"`
}

type SaveResult struct {
	SHA256    string `json:"sha256"`
	UpdatedAt int64  `json:"updated_at"`
	Title     string `json:"title"`
	Preview   string `json:"preview"`
	SizeBytes int64  `json:"size_bytes"`
	// Attachments is the note's full list, sent only when the save copied images
	// pasted from other notes into this note's folder.
	Attachments []Attachment `json:"attachments,omitempty"`
}

const noteCols = `n.id,n.title,n.preview,n.pinned,n.trashed_at,n.created_at,n.updated_at,n.size_bytes,n.content_sha256,
 (SELECT COUNT(*) FROM attachments a WHERE a.note_id=n.id AND a.state='ready')`

func scanMeta(sc interface{ Scan(...any) error }) (NoteMeta, error) {
	var m NoteMeta
	var trashed sql.NullInt64
	err := sc.Scan(&m.ID, &m.Title, &m.Preview, &m.Pinned, &trashed, &m.CreatedAt, &m.UpdatedAt, &m.SizeBytes, &m.SHA256, &m.AttachmentCount)
	if trashed.Valid {
		m.TrashedAt = &trashed.Int64
	}
	m.TagIDs = []string{}
	return m, err
}

// CreateNote writes the note's folder and file first, then the row (a crash
// leaves an orphan folder that `doctor` reports, never a row without a file).
func (s *Store) CreateNote(ctx context.Context, uid, body string) (*NoteMeta, error) {
	body, err := NormalizeBody(body)
	if err != nil {
		return nil, err
	}
	if err := s.ensureUserDirs(uid); err != nil {
		return nil, err
	}
	id := NewID()
	if err := os.Mkdir(s.noteDir(uid, id), fsx.DirMode); err != nil {
		return nil, err
	}
	if err := fsx.WriteAtomic(s.notePath(uid, id), []byte(body)); err != nil {
		os.RemoveAll(s.noteDir(uid, id))
		return nil, err
	}
	t := now()
	m := NoteMeta{ID: id, Title: DeriveTitle(body), Preview: DerivePreview(body), CreatedAt: t, UpdatedAt: t,
		SizeBytes: int64(len(body)), SHA256: fsx.HashBytes([]byte(body)), TagIDs: []string{}}
	_, err = s.DB.W.ExecContext(ctx, `INSERT INTO notes(id,user_id,title,preview,created_at,updated_at,size_bytes,content_sha256) VALUES(?,?,?,?,?,?,?,?)`,
		m.ID, uid, m.Title, m.Preview, t, t, m.SizeBytes, m.SHA256)
	if err != nil {
		os.RemoveAll(s.noteDir(uid, id))
		return nil, err
	}
	s.enqueueIndex(id)
	s.writeMeta(ctx, uid, id)
	if n, err := s.queueCopies(ctx, uid, id, "", body); err != nil {
		s.Log.Warn("queue image copies", "note", id, "err", err)
	} else if n > 0 {
		if _, err := s.runPending(ctx, uid, id); err != nil {
			s.Log.Warn("image copy", "note", id, "err", err)
		}
		m.AttachmentCount = s.countReady(ctx, id)
	}
	return &m, nil
}

func (s *Store) countReady(ctx context.Context, id string) int {
	var n int
	s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE note_id=? AND state='ready'`, id).Scan(&n)
	return n
}

func (s *Store) readBody(uid, id string) (string, error) {
	b, err := os.ReadFile(s.notePath(uid, id))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil // reported by doctor; treat as empty so it can be re-saved
	}
	return string(b), err
}

func (s *Store) noteMeta(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, uid, id string) (NoteMeta, error) {
	m, err := scanMeta(q.QueryRowContext(ctx, `SELECT `+noteCols+` FROM notes n WHERE n.id=? AND n.user_id=?`, id, uid))
	return m, notFoundIfNoRows(err)
}

func (s *Store) GetNote(ctx context.Context, uid, id string) (*Note, error) {
	m, err := s.noteMeta(ctx, s.DB.R, uid, id)
	if err != nil {
		return nil, err
	}
	body, err := s.readBody(uid, id)
	if err != nil {
		return nil, err
	}
	m.SHA256 = fsx.HashBytes([]byte(body)) // the file is authoritative
	if err := s.fillTags(ctx, []*NoteMeta{&m}); err != nil {
		return nil, err
	}
	atts, err := s.ListAttachments(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	return &Note{NoteMeta: m, Body: body, Attachments: atts}, nil
}

// SaveBody implements the save protocol in SPEC §5.1. Images pasted from other
// notes are then copied into this note's folder (outside the note lock, since
// a copy also locks its source note).
func (s *Store) SaveBody(ctx context.Context, uid, id, body, base string) (*SaveResult, error) {
	res, err := s.saveBody(ctx, uid, id, body, base)
	if err != nil {
		return nil, err
	}
	n, err := s.runPending(ctx, uid, id)
	if err != nil {
		s.Log.Warn("image copy (retried later)", "note", id, "err", err)
	}
	if n > 0 {
		if atts, err := s.ListAttachments(ctx, uid, id); err == nil {
			res.Attachments = atts
		}
	}
	return res, nil
}

func (s *Store) saveBody(ctx context.Context, uid, id, body, base string) (*SaveResult, error) {
	body, err := NormalizeBody(body)
	if err != nil {
		return nil, err
	}
	unlock := s.locks.Lock("note:" + id)
	defer unlock()

	m, err := s.noteMeta(ctx, s.DB.R, uid, id)
	if err != nil {
		return nil, err
	}
	if m.TrashedAt != nil {
		return nil, Invalid("restore this note from the trash to edit it")
	}
	path := s.notePath(uid, id)
	cur, err := s.readBody(uid, id)
	if err != nil {
		return nil, err
	}
	curSHA := fsx.HashBytes([]byte(cur))
	sha := fsx.HashBytes([]byte(body))
	res := &SaveResult{SHA256: sha, Title: DeriveTitle(body), Preview: DerivePreview(body), SizeBytes: int64(len(body))}
	dbctx := context.WithoutCancel(ctx) // once the file is renamed, the row must follow even if the client left
	if sha == curSHA {
		// Already applied (e.g. a retry after a lost response): success regardless of base.
		res.UpdatedAt = m.UpdatedAt
		if m.SHA256 != sha {
			res.UpdatedAt = now()
			if _, err := s.DB.W.ExecContext(dbctx, `UPDATE notes SET title=?, preview=?, size_bytes=?, content_sha256=?, updated_at=? WHERE id=? AND user_id=?`,
				res.Title, res.Preview, res.SizeBytes, sha, res.UpdatedAt, id, uid); err != nil {
				return nil, err
			}
			s.enqueueIndex(id)
			s.writeMeta(dbctx, uid, id)
		}
		return res, nil
	}
	if curSHA != base {
		return nil, &ConflictError{CurrentBody: cur, CurrentSHA256: curSHA}
	}
	// queued before the file is written, so a crash can't lose the copy request;
	// a marker that doesn't end up in the saved text is dropped by runPending
	if _, err := s.queueCopies(ctx, uid, id, cur, body); err != nil {
		return nil, err
	}
	if cur != "" {
		if err := fsx.WriteAtomic(path+".bak", []byte(cur)); err != nil {
			return nil, err
		}
	}
	if err := fsx.WriteAtomic(path, []byte(body)); err != nil {
		return nil, err
	}
	res.UpdatedAt = now()
	_, err = s.DB.W.ExecContext(dbctx, `UPDATE notes SET title=?, preview=?, size_bytes=?, content_sha256=?, updated_at=? WHERE id=? AND user_id=?`,
		res.Title, res.Preview, res.SizeBytes, sha, res.UpdatedAt, id, uid)
	if err != nil {
		// The file is already written; startup reconcile repairs the row.
		s.Log.Error("note row update failed after file write", "note", id, "err", err)
		return nil, err
	}
	s.enqueueIndex(id)
	s.writeMeta(dbctx, uid, id)
	return res, nil
}

type NotePatch struct {
	Pinned  *bool     `json:"pinned"`
	TagIDs  *[]string `json:"tag_ids"`
	Trashed *bool     `json:"trashed"`
}

func (s *Store) PatchNote(ctx context.Context, uid, id string, p NotePatch) (*NoteMeta, error) {
	if p.Trashed != nil {
		// serialize with PurgeNote / SaveBody so restore can't race a purge
		unlock := s.locks.Lock("note:" + id)
		defer unlock()
	}
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := s.noteMeta(ctx, tx, uid, id); err != nil {
		return nil, err
	}
	if p.Pinned != nil {
		if _, err := tx.Exec(`UPDATE notes SET pinned=? WHERE id=? AND user_id=?`, *p.Pinned, id, uid); err != nil {
			return nil, err
		}
	}
	if p.Trashed != nil {
		var v any
		if *p.Trashed {
			v = now()
		}
		if _, err := tx.Exec(`UPDATE notes SET trashed_at=? WHERE id=? AND user_id=?`, v, id, uid); err != nil {
			return nil, err
		}
	}
	if p.TagIDs != nil {
		if _, err := tx.Exec(`DELETE FROM note_tags WHERE note_id=?`, id); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, tid := range *p.TagIDs {
			if seen[tid] {
				continue
			}
			seen[tid] = true
			var ok int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM tags WHERE id=? AND user_id=?`, tid, uid).Scan(&ok); err != nil {
				return nil, err
			}
			if ok == 0 {
				return nil, Invalid("unknown tag")
			}
			if _, err := tx.Exec(`INSERT INTO note_tags(note_id,tag_id,user_id) VALUES(?,?,?)`, id, tid, uid); err != nil {
				return nil, err
			}
		}
	}
	m, err := s.noteMeta(ctx, tx, uid, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.writeMeta(context.WithoutCancel(ctx), uid, id)
	if err := s.fillTags(ctx, []*NoteMeta{&m}); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) DuplicateNote(ctx context.Context, uid, id string) (*NoteMeta, error) {
	n, err := s.GetNote(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	// The copy gets its own images (same refs, real copies), so purging the
	// original can't break it.
	m, err := s.CreateNote(ctx, uid, n.Body)
	if err != nil {
		return nil, err
	}
	tags := n.TagIDs
	return s.PatchNote(ctx, uid, m.ID, NotePatch{TagIDs: &tags})
}

// PurgeNote permanently deletes a trashed note. Idempotent and crash-tolerant:
// attachments are marked 'deleting' first (so a late upload can't land), then
// the folder is renamed into tmp/ in one step; a crash after that is finished
// by reconcile (a trashed row without a folder is an interrupted purge).
func (s *Store) PurgeNote(ctx context.Context, uid, id, actor string) error {
	if refs, err := s.readyRefs(ctx, uid, id); err == nil {
		s.flushPendingFor(ctx, uid, refs) // notes waiting for one of these images copy it first
	}
	unlock := s.locks.Lock("note:" + id)
	defer unlock()
	m, err := s.noteMeta(ctx, s.DB.R, uid, id)
	if err != nil {
		return err
	}
	if m.TrashedAt == nil {
		return Invalid("only notes in the trash can be deleted permanently")
	}
	refs, err := s.readyRefs(ctx, uid, id)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := s.guardLastCopy(ctx, uid, id, ref); err != nil {
			return err
		}
	}
	if _, err := s.DB.W.ExecContext(ctx, `UPDATE attachments SET state='deleting', updated_at=? WHERE note_id=? AND user_id=?`, now(), id, uid); err != nil {
		return err
	}
	gone := filepath.Join(s.Dir, "tmp", uid, "purge-"+id)
	if _, err := os.Stat(s.noteDir(uid, id)); err == nil {
		if err := os.MkdirAll(filepath.Dir(gone), fsx.DirMode); err != nil {
			return err
		}
		os.RemoveAll(gone) // leftover of an earlier attempt
		if err := fsx.MoveDurable(s.noteDir(uid, id), gone); err != nil {
			return err
		}
	}
	if err := s.finishDeletingAttachments(ctx, uid, id); err != nil {
		return err
	}
	if _, err := s.DB.W.ExecContext(ctx, `DELETE FROM notes WHERE id=? AND user_id=?`, id, uid); err != nil {
		return err
	}
	if err := os.RemoveAll(gone); err != nil {
		s.Log.Warn("purge leftovers (removed later)", "note", id, "err", err)
	}
	s.unindex(id)
	s.Audit(actor, "note.purge", id)
	return nil
}

func (s *Store) readyRefs(ctx context.Context, uid, id string) ([]string, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT ref FROM attachments WHERE note_id=? AND user_id=? AND state='ready'`, id, uid)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

func (s *Store) EmptyTrash(ctx context.Context, uid string) (int, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT id FROM notes WHERE user_id=? AND trashed_at IS NOT NULL`, uid)
	if err != nil {
		return 0, err
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := s.PurgeNote(ctx, uid, id, uid)
		if errors.Is(err, ErrCopyPending) {
			continue // kept until its image has been copied; the next empty or the janitor gets it
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---- listing ----

type ListParams struct {
	Q              string
	TagIDs         []string
	Untagged       bool
	Pinned         bool
	Trash          bool
	HasAttachments bool
	Sort           string
	Limit          int
	Cursor         string
}

type NoteList struct {
	Items      []NoteMeta `json:"items"`
	NextCursor string     `json:"next_cursor"`
}

type cursor struct {
	P  int    `json:"p"`
	V  any    `json:"v"`
	ID string `json:"id"`
}

func (s *Store) ListNotes(ctx context.Context, uid string, p ListParams) (*NoteList, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	where := []string{"n.user_id=?"}
	args := []any{uid}
	if p.Trash {
		where = append(where, "n.trashed_at IS NOT NULL")
	} else {
		where = append(where, "n.trashed_at IS NULL")
	}
	if p.Pinned {
		where = append(where, "n.pinned=1")
	}
	if p.HasAttachments {
		where = append(where, "EXISTS(SELECT 1 FROM attachments a WHERE a.note_id=n.id AND a.state='ready')")
	}
	if p.Untagged {
		where = append(where, "NOT EXISTS(SELECT 1 FROM note_tags nt WHERE nt.note_id=n.id)")
	} else if len(p.TagIDs) > 0 {
		where = append(where, "(SELECT COUNT(*) FROM note_tags nt WHERE nt.note_id=n.id AND nt.tag_id IN ("+placeholders(len(p.TagIDs))+"))=?")
		for _, t := range p.TagIDs {
			args = append(args, t)
		}
		args = append(args, len(p.TagIDs))
	}

	q := strings.TrimSpace(p.Q)
	if q != "" {
		// NUL would truncate the LIKE pattern to "%" (match everything); such a
		// query can't match note text anyway.
		if strings.ContainsRune(q, 0) || !utf8.ValidString(q) || len(q) > 1000 {
			return &NoteList{Items: []NoteMeta{}}, nil
		}
		ids, err := s.searchIDs(ctx, uid, q)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return &NoteList{Items: []NoteMeta{}}, nil
		}
		idsJSON, _ := json.Marshal(ids)
		where = append(where, "n.id IN (SELECT value FROM json_each(?))")
		args = append(args, string(idsJSON))
	}

	col, asc := "n.updated_at", false
	switch p.Sort {
	case "created":
		col = "n.created_at"
	case "title":
		col, asc = "n.title COLLATE NOCASE", true
	}
	if p.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
		var c cursor
		if err != nil || json.Unmarshal(raw, &c) != nil || (c.P != 0 && c.P != 1) || !ValidID(c.ID) {
			return nil, Invalid("bad cursor")
		}
		var v any
		switch x := c.V.(type) {
		case float64:
			if asc {
				return nil, Invalid("bad cursor")
			}
			v = int64(x)
		case string:
			if !asc {
				return nil, Invalid("bad cursor")
			}
			v = x
		default:
			return nil, Invalid("bad cursor")
		}
		if asc {
			where = append(where, "(n.pinned<? OR (n.pinned=? AND ("+col+">? OR ("+col+"=? AND n.id>?))))")
		} else {
			where = append(where, "(n.pinned<? OR (n.pinned=? AND ("+col+"<? OR ("+col+"=? AND n.id<?))))")
		}
		args = append(args, c.P, c.P, v, v, c.ID)
	}
	dir := "DESC"
	if asc {
		dir = "ASC"
	}
	query := `SELECT ` + noteCols + ` FROM notes n WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY n.pinned DESC, ` + col + ` ` + dir + `, n.id ` + dir + ` LIMIT ?`
	args = append(args, p.Limit+1)
	items, err := s.queryMetas(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := &NoteList{Items: items}
	if len(items) > p.Limit {
		out.Items = items[:p.Limit]
		last := out.Items[p.Limit-1]
		c := cursor{ID: last.ID}
		if last.Pinned {
			c.P = 1
		}
		switch p.Sort {
		case "created":
			c.V = last.CreatedAt
		case "title":
			c.V = last.Title
		default:
			c.V = last.UpdatedAt
		}
		b, _ := json.Marshal(c)
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	if q != "" {
		s.addSnippets(uid, q, out.Items)
	}
	return out, s.fillTagsSlice(ctx, out.Items)
}

func (s *Store) queryMetas(ctx context.Context, query string, args ...any) ([]NoteMeta, error) {
	rows, err := s.DB.R.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []NoteMeta{}
	for rows.Next() {
		m, err := scanMeta(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

func (s *Store) fillTagsSlice(ctx context.Context, items []NoteMeta) error {
	ptrs := make([]*NoteMeta, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	return s.fillTags(ctx, ptrs)
}

func (s *Store) fillTags(ctx context.Context, items []*NoteMeta) error {
	if len(items) == 0 {
		return nil
	}
	idx := map[string]*NoteMeta{}
	args := make([]any, len(items))
	for i, m := range items {
		idx[m.ID] = m
		args[i] = m.ID
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT nt.note_id, nt.tag_id FROM note_tags nt JOIN tags t ON t.id=nt.tag_id
WHERE nt.note_id IN (`+placeholders(len(items))+`) ORDER BY t.name`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var nid, tid string
		if err := rows.Scan(&nid, &tid); err != nil {
			return err
		}
		idx[nid].TagIDs = append(idx[nid].TagIDs, tid)
	}
	return rows.Err()
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// TrashPurgeDue returns (user, note) pairs trashed longer than the retention.
func (s *Store) trashPurgeDue(ctx context.Context, days int) ([][2]string, error) {
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	rows, err := s.DB.R.QueryContext(ctx, `SELECT user_id,id FROM notes WHERE trashed_at IS NOT NULL AND trashed_at<?`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
