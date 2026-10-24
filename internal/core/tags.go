package core

import (
	"context"
	"hash/fnv"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Palette is the fixed set of tag colors; the CSS defines each key in OKLCH.
var Palette = []string{"coral", "orange", "amber", "lime", "green", "mint", "teal", "sky", "blue", "violet", "magenta", "pink"}

type Tag struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Count int    `json:"count"`
}

func colorFor(name string) string {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(name)))
	return Palette[h.Sum32()%uint32(len(Palette))]
}

func normalizeTagName(name string) (string, error) {
	name = strings.Join(strings.Fields(norm.NFC.String(name)), " ")
	if name == "" || utf8.RuneCountInString(name) > 40 {
		return "", Invalid("tag names must be 1–40 characters")
	}
	if strings.ContainsAny(name, ",") {
		return "", Invalid("tag names cannot contain commas")
	}
	return name, nil
}

func validColor(c string) bool { return slices.Contains(Palette, c) }

func (s *Store) ListTags(ctx context.Context, uid string) ([]Tag, error) {
	rows, err := s.DB.R.QueryContext(ctx, `
SELECT t.id,t.name,t.color,
  (SELECT COUNT(*) FROM note_tags nt JOIN notes n ON n.id=nt.note_id WHERE nt.tag_id=t.id AND n.trashed_at IS NULL)
FROM tags t WHERE t.user_id=? ORDER BY t.name`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tag{}
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTag creates a tag, or returns the existing one with the same name.
func (s *Store) CreateTag(ctx context.Context, uid, name, color string) (*Tag, error) {
	name, err := normalizeTagName(name)
	if err != nil {
		return nil, err
	}
	if color == "" {
		color = colorFor(name)
	} else if !validColor(color) {
		return nil, Invalid("unknown color")
	}
	t := &Tag{ID: NewID(), Name: name, Color: color}
	_, err = s.DB.W.ExecContext(ctx, `INSERT INTO tags(id,user_id,name,color,created_at) VALUES(?,?,?,?,?) ON CONFLICT(user_id,name) DO NOTHING`,
		t.ID, uid, t.Name, t.Color, now())
	if err != nil {
		return nil, err
	}
	err = s.DB.W.QueryRowContext(ctx, `SELECT id,name,color FROM tags WHERE user_id=? AND name=?`, uid, name).Scan(&t.ID, &t.Name, &t.Color)
	return t, err
}

type TagPatch struct {
	Name  *string `json:"name"`
	Color *string `json:"color"`
}

func (s *Store) PatchTag(ctx context.Context, uid, id string, p TagPatch) (*Tag, error) {
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if p.Name != nil {
		name, err := normalizeTagName(*p.Name)
		if err != nil {
			return nil, err
		}
		res, err := tx.Exec(`UPDATE tags SET name=? WHERE id=? AND user_id=?`, name, id, uid)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return nil, Invalid("a tag with that name already exists")
			}
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrNotFound
		}
	}
	if p.Color != nil {
		if !validColor(*p.Color) {
			return nil, Invalid("unknown color")
		}
		res, err := tx.Exec(`UPDATE tags SET color=? WHERE id=? AND user_id=?`, *p.Color, id, uid)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrNotFound
		}
	}
	// every note with this tag mirrors its name and colour in meta.json
	if err := markTagNotesDirty(tx, id); err != nil {
		return nil, err
	}
	var t Tag
	if err := tx.QueryRow(`SELECT id,name,color FROM tags WHERE id=? AND user_id=?`, id, uid).Scan(&t.ID, &t.Name, &t.Color); err != nil {
		return nil, notFoundIfNoRows(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.kickMeta()
	return &t, nil
}

func (s *Store) DeleteTag(ctx context.Context, uid, id string) error {
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// queue the notes before the delete cascades their note_tags rows away
	if err := markTagNotesDirty(tx, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM tags WHERE id=? AND user_id=?`, id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.kickMeta()
	return nil
}
