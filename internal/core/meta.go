package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"homelab-notes/internal/fsx"
)

// meta.json mirrors a note's database row so the data folder can rebuild a lost
// database (SPEC §3.4). The database stays authoritative; the mirror is rewritten
// after every change. Field order, sorting and integer-ms times make the bytes
// canonical, so "different bytes" always means "out of date".
type metaFile struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	CreatedAt   int64     `json:"created_at"`
	UpdatedAt   int64     `json:"updated_at"`
	Pinned      bool      `json:"pinned"`
	TrashedAt   *int64    `json:"trashed_at"`
	Tags        []metaTag `json:"tags"`
	Attachments []metaAtt `json:"attachments"`
}

type metaTag struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type metaAtt struct {
	Ref       string `json:"ref"`
	Ext       string `json:"ext"`
	Filename  string `json:"filename"`
	Mime      string `json:"mime"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	CreatedAt int64  `json:"created_at"`
}

type userFile struct {
	Version   int    `json:"version"`
	ID        string `json:"id"`
	Username  string `json:"username"`
	Disabled  bool   `json:"disabled"`
	CreatedAt int64  `json:"created_at"`
}

// buildMeta renders a note's meta.json from the database.
func (s *Store) buildMeta(ctx context.Context, uid, id string) ([]byte, error) {
	m := metaFile{Version: 1, ID: id, Tags: []metaTag{}, Attachments: []metaAtt{}}
	var trashed sql.NullInt64
	err := s.DB.R.QueryRowContext(ctx, `SELECT created_at,updated_at,pinned,trashed_at FROM notes WHERE id=? AND user_id=?`, id, uid).
		Scan(&m.CreatedAt, &m.UpdatedAt, &m.Pinned, &trashed)
	if err != nil {
		return nil, notFoundIfNoRows(err)
	}
	if trashed.Valid {
		m.TrashedAt = &trashed.Int64
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT t.name,t.color FROM tags t JOIN note_tags nt ON nt.tag_id=t.id WHERE nt.note_id=? AND t.user_id=? ORDER BY t.name, t.id`, id, uid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t metaTag
		if err := rows.Scan(&t.Name, &t.Color); err != nil {
			rows.Close()
			return nil, err
		}
		m.Tags = append(m.Tags, t)
	}
	rows.Close()
	rows, err = s.DB.R.QueryContext(ctx, `SELECT ref,ext,filename,mime,size_bytes,COALESCE(sha256,''),created_at FROM attachments
 WHERE note_id=? AND user_id=? AND state='ready' ORDER BY created_at, id`, id, uid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a metaAtt
		if err := rows.Scan(&a.Ref, &a.Ext, &a.Filename, &a.Mime, &a.SizeBytes, &a.SHA256, &a.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		m.Attachments = append(m.Attachments, a)
	}
	rows.Close()
	b, err := json.MarshalIndent(m, "", "  ")
	return append(b, '\n'), err
}

// writeMeta rewrites a note's meta.json. Failures are remembered in meta_dirty
// and retried by the background writer, so callers never fail because of it.
func (s *Store) writeMeta(ctx context.Context, uid, id string) {
	if err := s.writeMetaErr(ctx, uid, id); err != nil {
		s.Log.Warn("meta.json write failed; will retry", "note", id, "err", err)
		s.DB.W.ExecContext(context.WithoutCancel(ctx), `INSERT OR IGNORE INTO meta_dirty(note_id) SELECT id FROM notes WHERE id=?`, id)
		s.kickMeta()
	}
}

func (s *Store) writeMetaErr(ctx context.Context, uid, id string) error {
	if s.Legacy {
		return nil
	}
	unlock := s.locks.Lock("meta:" + id) // leaf lock: two writers can't land an older snapshot last
	defer unlock()
	b, err := s.buildMeta(ctx, uid, id)
	if errors.Is(err, ErrNotFound) {
		return nil // purged meanwhile
	} else if err != nil {
		return err
	}
	if st, err := os.Stat(s.noteDir(uid, id)); err != nil || !st.IsDir() {
		return nil // no folder (purge in progress): nothing to mirror into
	}
	if cur, err := os.ReadFile(s.metaPath(uid, id)); err == nil && string(cur) == string(b) {
		return nil
	}
	return fsx.WriteAtomic(s.metaPath(uid, id), b)
}

func (s *Store) buildUserJSON(ctx context.Context, uid string) ([]byte, error) {
	u := userFile{Version: 1, ID: uid}
	err := s.DB.R.QueryRowContext(ctx, `SELECT username,disabled,created_at FROM users WHERE id=?`, uid).Scan(&u.Username, &u.Disabled, &u.CreatedAt)
	if err != nil {
		return nil, notFoundIfNoRows(err)
	}
	b, err := json.MarshalIndent(u, "", "  ")
	return append(b, '\n'), err
}

// writeUserJSON mirrors the account (never its password) for rebuild-db.
func (s *Store) writeUserJSON(ctx context.Context, uid string) error {
	if s.Legacy {
		return nil
	}
	b, err := s.buildUserJSON(ctx, uid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.userDir(uid), fsx.DirMode); err != nil {
		return err
	}
	if cur, err := os.ReadFile(s.userJSONPath(uid)); err == nil && string(cur) == string(b) {
		return nil
	}
	return fsx.WriteAtomic(s.userJSONPath(uid), b)
}

// markTagNotesDirty queues meta.json rewrites for every note carrying a tag.
// Call inside the transaction that changes the tag (before a delete cascades).
func markTagNotesDirty(tx *sql.Tx, tagID string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO meta_dirty(note_id) SELECT note_id FROM note_tags WHERE tag_id=?`, tagID)
	return err
}

func (s *Store) kickMeta() {
	select {
	case s.metaCh <- struct{}{}:
	default:
	}
}

// DrainMeta rewrites every queued meta.json. Each queue entry is removed before
// its file is rendered, so a tag change landing meanwhile queues the note again
// instead of being lost; a failed write puts the entry back. One failing note
// doesn't hold up the others.
func (s *Store) DrainMeta(ctx context.Context) error {
	failed := map[string]bool{}
	var errs []error
	for {
		rows, err := s.DB.R.QueryContext(ctx, `SELECT n.user_id,n.id FROM meta_dirty d JOIN notes n ON n.id=d.note_id LIMIT 200`)
		if err != nil {
			return err
		}
		var batch [][2]string
		for rows.Next() {
			var p [2]string
			rows.Scan(&p[0], &p[1])
			if !failed[p[1]] {
				batch = append(batch, p)
			}
		}
		rows.Close()
		if len(batch) == 0 {
			return errors.Join(errs...)
		}
		for _, p := range batch {
			if _, err := s.DB.W.ExecContext(ctx, `DELETE FROM meta_dirty WHERE note_id=?`, p[1]); err != nil {
				return err
			}
			if err := s.writeMetaErr(ctx, p[0], p[1]); err != nil {
				failed[p[1]] = true
				errs = append(errs, err)
				s.DB.W.ExecContext(context.WithoutCancel(ctx), `INSERT OR IGNORE INTO meta_dirty(note_id) SELECT id FROM notes WHERE id=?`, p[1])
			}
		}
	}
}

// RunMetaWriter drains the meta.json queue whenever something is queued, and
// once a minute as a backstop, until ctx is cancelled.
func (s *Store) RunMetaWriter(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.metaCh:
		case <-t.C:
		}
		if err := s.DrainMeta(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("meta.json writer", "err", err)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return !errors.Is(err, fs.ErrNotExist)
}
