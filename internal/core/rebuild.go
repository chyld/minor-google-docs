package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"homelab-notes/internal/fsx"
)

// RebuildReport says what RebuildDB restored and what it skipped.
type RebuildReport struct {
	Users    []string // usernames restored (each needs `notes user passwd`)
	Notes    int
	Images   int
	Warnings []string
}

// noLogin is stored as the password of rebuilt accounts: it never verifies.
const noLogin = "!rebuilt-needs-a-new-password"

// RebuildDB recreates users, tags, notes and images from the data folder
// (user.json, meta.json, note.txt, files/) into an empty database (SPEC §3.4).
// Nothing in the files is trusted as a path: ids must match folder names,
// refs and extensions are validated, and file paths are rebuilt from them.
func (s *Store) RebuildDB(ctx context.Context) (*RebuildReport, error) {
	var n int
	if err := s.DB.R.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM users)+(SELECT COUNT(*) FROM notes)+(SELECT COUNT(*) FROM tags)`).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, errors.New("the database is not empty; rebuild-db only fills a new, empty database")
	}
	r := &RebuildReport{}
	warn := func(f string, a ...any) { r.Warnings = append(r.Warnings, fmt.Sprintf(f, a...)) }
	dirs, err := os.ReadDir(filepath.Join(s.Dir, "users"))
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		uid := d.Name()
		if !d.IsDir() || !ValidID(uid) {
			warn("skipped users/%s: not a user folder", uid)
			continue
		}
		var u userFile
		if err := readJSONFile(s.userJSONPath(uid), &u); err != nil {
			warn("skipped users/%s: no readable user.json (%v)", uid, err)
			continue
		}
		name, err := NormalizeUsername(u.Username)
		if err != nil || u.ID != uid {
			warn("skipped users/%s: user.json does not match its folder", uid)
			continue
		}
		t := now()
		if u.CreatedAt <= 0 {
			u.CreatedAt = t
		}
		if _, err := s.DB.W.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,disabled,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			uid, name, noLogin, u.Disabled, u.CreatedAt, t); err != nil {
			warn("skipped user %s: %v", name, err)
			continue
		}
		r.Users = append(r.Users, name)
		if err := s.rebuildNotes(ctx, uid, r, warn); err != nil {
			return r, err
		}
		s.Audit("cli", "user.rebuild", name)
	}
	if _, err := s.Reindex(ctx); err != nil {
		return r, err
	}
	return r, nil
}

func (s *Store) rebuildNotes(ctx context.Context, uid string, r *RebuildReport, warn func(string, ...any)) error {
	tagIDs := map[string]string{} // name → id
	tagColor := map[string]string{}
	entries, _ := os.ReadDir(s.notesDir(uid))
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !ValidID(id) {
			continue
		}
		body, err := os.ReadFile(s.notePath(uid, id))
		if err != nil {
			warn("skipped note folder %s: no note.txt", id)
			continue
		}
		var m metaFile
		if err := readJSONFile(s.metaPath(uid, id), &m); err != nil || m.ID != id {
			// text without its details: keep the text, with the folder's dates
			m = metaFile{ID: id}
			if st, err := os.Stat(s.notePath(uid, id)); err == nil {
				m.CreatedAt, m.UpdatedAt = st.ModTime().UnixMilli(), st.ModTime().UnixMilli()
			}
			warn("note %s had no readable meta.json; restored its text only", id)
		}
		text, err := NormalizeBody(string(body))
		if err != nil {
			warn("skipped note %s: %v", id, err)
			continue
		}
		if m.CreatedAt <= 0 {
			m.CreatedAt = now()
		}
		if m.UpdatedAt <= 0 {
			m.UpdatedAt = m.CreatedAt
		}
		tx, err := s.DB.W.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO notes(id,user_id,title,preview,pinned,trashed_at,created_at,updated_at,size_bytes,content_sha256) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			id, uid, DeriveTitle(text), DerivePreview(text), m.Pinned, m.TrashedAt, m.CreatedAt, m.UpdatedAt, len(body), fsx.HashBytes(body))
		if err != nil {
			tx.Rollback()
			warn("skipped note %s: %v", id, err)
			continue
		}
		for _, t := range m.Tags {
			name, err := normalizeTagName(t.Name)
			if err != nil {
				continue
			}
			tid, ok := tagIDs[name]
			if !ok {
				color := t.Color
				if !validColor(color) {
					color = colorFor(name)
				}
				tid = NewID()
				if _, err := tx.Exec(`INSERT INTO tags(id,user_id,name,color,created_at) VALUES(?,?,?,?,?)`, tid, uid, name, color, now()); err != nil {
					tx.Rollback()
					return err
				}
				tagIDs[name], tagColor[name] = tid, color
			} else if t.Color != tagColor[name] {
				warn("tag %q has different colours in different notes; kept %s", name, tagColor[name])
			}
			tx.Exec(`INSERT OR IGNORE INTO note_tags(note_id,tag_id,user_id) VALUES(?,?,?)`, id, tid, uid)
		}
		images := 0
		for _, a := range m.Attachments {
			if !ValidRef(a.Ref) || !validExt(a.Ext) {
				warn("note %s: skipped an image with an invalid name", id)
				continue
			}
			att := &Attachment{ID: NewID(), Ref: a.Ref, NoteID: id, Filename: SanitizeFilename(a.Filename), Mime: a.Mime,
				Ext: a.Ext, State: "ready", CreatedAt: a.CreatedAt}
			path := s.attPath(uid, att)
			st, err := os.Lstat(path)
			if err != nil || !st.Mode().IsRegular() {
				warn("note %s: the image %q is missing", id, a.Filename)
				continue
			}
			sum, err := fsx.HashFile(path)
			if err != nil {
				tx.Rollback()
				return err
			}
			if head, err := readHead(path); err == nil {
				att.Mime = detectMime(head, att.Filename) // never trust a stored type
			}
			att.SizeBytes = st.Size()
			if att.CreatedAt <= 0 {
				att.CreatedAt = now()
			}
			var have string
			if err := tx.QueryRow(`SELECT sha256 FROM ref_content WHERE user_id=? AND ref=?`, uid, a.Ref).Scan(&have); err == nil {
				if have != sum {
					warn("note %s: the image %q has different content than its copies elsewhere; skipped", id, a.Filename)
					continue
				}
			} else if _, err := tx.Exec(`INSERT INTO ref_content(user_id,ref,sha256) VALUES(?,?,?)`, uid, a.Ref, sum); err != nil {
				tx.Rollback()
				return err
			}
			if err := insertAtt(tx, uid, att, sum); err != nil {
				warn("note %s: image %q: %v", id, a.Filename, err)
				continue
			}
			images++
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		r.Notes++
		r.Images += images
		s.writeMeta(ctx, uid, id)
	}
	sort.Strings(r.Users)
	return nil
}

func readJSONFile(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
