// Package db opens the SQLite databases and applies migrations.
package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// DB bundles the metadata database (one writer, a read pool) and the
// disposable search index.
type DB struct {
	W      *sql.DB // single connection, BEGIN IMMEDIATE transactions
	R      *sql.DB // read pool
	Search *sql.DB
}

func dsn(path string, immediate bool) string {
	q := url.Values{}
	for _, p := range []string{"foreign_keys(1)", "journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)"} {
		q.Add("_pragma", p)
	}
	if immediate {
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

func Open(dataDir string) (*DB, error) {
	path := filepath.Join(dataDir, "db", "notes.sqlite")
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	r, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	spath := filepath.Join(dataDir, "db", "search.sqlite")
	s, err := openSearch(spath)
	if errors.Is(err, errDamaged) {
		// The index is derived: move a damaged one aside and start fresh (reconcile rebuilds it).
		bad := fmt.Sprintf("%s.corrupt-%d", spath, time.Now().Unix())
		slog.Warn("search index is damaged; moving it aside and rebuilding", "moved_to", bad, "err", err)
		for _, suf := range []string{"", "-wal", "-shm"} {
			os.Rename(spath+suf, bad+suf)
		}
		pruneCorrupt(spath, 2)
		s, err = openSearch(spath)
	}
	if err != nil {
		w.Close()
		r.Close()
		return nil, fmt.Errorf("search index: %w", err)
	}
	return &DB{W: w, R: r, Search: s}, nil
}

func (d *DB) Close() {
	d.Search.Close()
	d.R.Close()
	d.W.Close()
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version(version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var cur int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&cur); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil || v <= cur {
			continue
		}
		body, _ := migrations.ReadFile("migrations/" + e.Name())
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES(?)`, v); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

var errDamaged = errors.New("search index damaged")

// damaged reports SQLite errors that mean the file itself is bad (as opposed
// to e.g. a full disk or a read-only mount, where moving it aside won't help).
func damaged(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "malformed") || strings.Contains(m, "not a database") || strings.Contains(m, "corrupt")
}

// pruneCorrupt keeps only the newest `keep` quarantined copies of path.
func pruneCorrupt(path string, keep int) {
	old, _ := filepath.Glob(path + ".corrupt-*")
	var bases []string
	for _, f := range old {
		if !strings.HasSuffix(f, "-wal") && !strings.HasSuffix(f, "-shm") {
			bases = append(bases, f)
		}
	}
	sort.Strings(bases) // unix-time suffixes sort chronologically
	for i := 0; i+keep < len(bases); i++ {
		for _, suf := range []string{"", "-wal", "-shm"} {
			os.Remove(bases[i] + suf)
		}
	}
}

func openSearch(path string) (*sql.DB, error) {
	s, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	s.SetMaxOpenConns(1)
	if err := s.Ping(); err != nil {
		s.Close()
		if damaged(err) {
			return nil, fmt.Errorf("%w: %v", errDamaged, err)
		}
		return nil, err
	}
	var ok string
	if err := s.QueryRow(`PRAGMA quick_check`).Scan(&ok); err != nil || ok != "ok" {
		s.Close()
		if err != nil && !damaged(err) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v %s", errDamaged, err, ok)
	}
	_, err = s.Exec(`
CREATE VIRTUAL TABLE IF NOT EXISTS notes_fts USING fts5(
  title, body, note_id UNINDEXED, user_id UNINDEXED, tokenize='trigram');
CREATE TABLE IF NOT EXISTS indexed(note_id TEXT PRIMARY KEY, content_sha256 TEXT NOT NULL);`)
	if err != nil {
		s.Close()
		if damaged(err) {
			return nil, fmt.Errorf("%w: %v", errDamaged, err)
		}
		return nil, err
	}
	return s, nil
}
