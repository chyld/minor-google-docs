package core

import (
	"context"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"homelab-notes/internal/fsx"
)

// Segment is a piece of a search snippet; Hit marks matched text. Snippets are
// returned as segments (not HTML) so the client never injects markup.
type Segment struct {
	Text string `json:"text"`
	Hit  bool   `json:"hit,omitempty"`
}

const snippetChunk = 256 << 10 // notes are scanned for a snippet in chunks of this size

// ftsPhrase escapes user input as a single FTS5 phrase.
func ftsPhrase(q string) string { return `"` + strings.ReplaceAll(q, `"`, `""`) + `"` }

// searchIDs returns ids of all the user's notes matching q in the body/title
// (FTS) or title / attachment file name (metadata DB). No cap: ids are cheap,
// and paging happens in the list query so every match is reachable.
func (s *Store) searchIDs(ctx context.Context, uid, q string) ([]string, error) {
	ids := map[string]bool{}
	if utf8.RuneCountInString(q) >= 3 {
		rows, err := s.DB.Search.QueryContext(ctx,
			`SELECT note_id FROM notes_fts WHERE notes_fts MATCH ? AND user_id=?`, ftsPhrase(q), uid)
		if err == nil {
			found, err := scanStrings(rows)
			if err != nil {
				return nil, err
			}
			for _, id := range found {
				ids[id] = true
			}
		} else {
			s.Log.Warn("search query failed", "err", err)
		}
	}
	like := "%" + escapeLike(q) + "%"
	rows, err := s.DB.R.QueryContext(ctx, `
SELECT id FROM notes WHERE user_id=? AND title LIKE ? ESCAPE '\'
UNION SELECT note_id FROM attachments WHERE user_id=? AND state='ready' AND filename LIKE ? ESCAPE '\'`, uid, like, uid, like)
	if err != nil {
		return nil, err
	}
	more, err := scanStrings(rows)
	if err != nil {
		return nil, err
	}
	for _, id := range more {
		ids[id] = true
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	return out, nil
}

// addSnippets fills Snippet for each item, scanning its file in chunks until
// the first match (so a hit deep in a large note still gets context).
func (s *Store) addSnippets(uid, q string, items []NoteMeta) {
	const overlap = 1 << 10 // carried between chunks so a match/context isn't cut
	buf := make([]byte, overlap+snippetChunk)
	for i := range items {
		f, err := os.Open(s.notePath(uid, items[i].ID))
		if err != nil {
			continue
		}
		carry := 0
		for {
			n, err := io.ReadFull(f, buf[carry:])
			text := StripMarkers(strings.ToValidUTF8(string(buf[:carry+n]), "")) // a cut mid-rune is dropped
			if seg := Snippet(text, q); seg != nil {
				items[i].Snippet = seg
				break
			}
			if err != nil { // EOF / short read: no more chunks
				break
			}
			carry = copy(buf, buf[carry+n-overlap:carry+n])
		}
		f.Close()
	}
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Snippet returns ~160 chars of text around the first case-insensitive match.
func Snippet(body, q string) []Segment {
	lb, lq := strings.ToLower(body), strings.ToLower(q)
	i := strings.Index(lb, lq)
	if i < 0 || len(lb) != len(body) { // lowercasing changed byte offsets; skip highlight
		return nil
	}
	start := max(0, i-60)
	for start > 0 && !utf8.RuneStart(body[start]) {
		start--
	}
	end := min(len(body), i+len(q)+100)
	for end < len(body) && !utf8.RuneStart(body[end]) {
		end++
	}
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return ' '
			}
			return r
		}, s)
	}
	var segs []Segment
	pre := clean(body[start:i])
	if start > 0 {
		pre = "…" + pre
	}
	segs = append(segs, Segment{Text: pre})
	// highlight every occurrence inside the window
	rest, lrest := body[i:end], lb[i:end]
	for {
		j := strings.Index(lrest, lq)
		if j < 0 {
			segs = append(segs, Segment{Text: clean(rest)})
			break
		}
		if j > 0 {
			segs = append(segs, Segment{Text: clean(rest[:j])})
		}
		segs = append(segs, Segment{Text: clean(rest[j : j+len(lq)]), Hit: true})
		rest, lrest = rest[j+len(lq):], lrest[j+len(lq):]
	}
	if end < len(body) {
		segs = append(segs, Segment{Text: "…"})
	}
	return segs
}

// ---- index maintenance ----

func (s *Store) enqueueIndex(id string) {
	if s.SyncIndex {
		if err := s.indexNote(context.Background(), id); err != nil {
			s.Log.Warn("index update failed", "note", id, "err", err)
		}
		return
	}
	select {
	case s.indexCh <- id:
	default: // queue full: the hourly IndexSweep picks it up
		s.Log.Warn("index queue full; note will be indexed by the next sweep", "note", id)
	}
}

// IndexSweep re-indexes notes whose indexed hash differs from the metadata
// hash (a missed queue slot or a failed index write). It compares hashes only,
// without reading note files, so it is cheap enough to run hourly.
func (s *Store) IndexSweep(ctx context.Context) error {
	rows, err := s.DB.Search.QueryContext(ctx, `SELECT note_id, content_sha256 FROM indexed`)
	if err != nil {
		return err
	}
	indexed := map[string]string{}
	for rows.Next() {
		var id, sha string
		if err := rows.Scan(&id, &sha); err != nil {
			rows.Close()
			return err
		}
		indexed[id] = sha
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = s.DB.R.QueryContext(ctx, `SELECT id, content_sha256 FROM notes`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id, sha string
		if err := rows.Scan(&id, &sha); err != nil {
			rows.Close()
			return err
		}
		if indexed[id] != sha {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range stale {
		if err := s.indexNote(ctx, id); err != nil {
			s.Log.Warn("index sweep failed", "note", id, "err", err)
		}
	}
	return nil
}

// RunIndexer processes queued index updates until ctx is cancelled.
func (s *Store) RunIndexer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.indexCh:
			if err := s.indexNote(ctx, id); err != nil {
				s.Log.Warn("index update failed", "note", id, "err", err)
			}
		}
	}
}

func (s *Store) indexNote(ctx context.Context, id string) error {
	unlock := s.locks.Lock("note:" + id) // title and body read as one consistent snapshot
	defer unlock()
	var uid, title string
	err := s.DB.R.QueryRowContext(ctx, `SELECT user_id,title FROM notes WHERE id=?`, id).Scan(&uid, &title)
	if err != nil {
		if notFoundIfNoRows(err) == ErrNotFound {
			s.unindex(id)
			return nil
		}
		return err
	}
	body, err := s.readBody(uid, id)
	if err != nil {
		return err
	}
	sha := fsx.HashBytes([]byte(body))
	tx, err := s.DB.Search.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM notes_fts WHERE note_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO notes_fts(title,body,note_id,user_id) VALUES(?,?,?,?)`, title, StripMarkers(body), id, uid); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO indexed(note_id,content_sha256) VALUES(?,?) ON CONFLICT(note_id) DO UPDATE SET content_sha256=excluded.content_sha256`, id, sha); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) unindex(id string) {
	err := func() error {
		tx, err := s.DB.Search.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`DELETE FROM notes_fts WHERE note_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM indexed WHERE note_id=?`, id); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		s.Log.Warn("unindex failed (reconcile will retry)", "note", id, "err", err)
	}
}

func (s *Store) purgeSearchUser(uid string) {
	s.DB.Search.Exec(`DELETE FROM indexed WHERE note_id IN (SELECT note_id FROM notes_fts WHERE user_id=?)`, uid)
	s.DB.Search.Exec(`DELETE FROM notes_fts WHERE user_id=?`, uid)
}

// Reindex rebuilds the whole search index from the .txt files.
func (s *Store) Reindex(ctx context.Context) (int, error) {
	if _, err := s.DB.Search.ExecContext(ctx, `DELETE FROM notes_fts; DELETE FROM indexed;`); err != nil {
		return 0, err
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT id FROM notes`)
	if err != nil {
		return 0, err
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.indexNote(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
