// Package core implements notes, tags, attachments, users and sessions on top
// of the files under data/ (content) and SQLite (metadata).
package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"homelab-notes/internal/config"
	"homelab-notes/internal/db"
	"homelab-notes/internal/fsx"
)

var (
	ErrNotFound = errors.New("not found")
	ErrNoSpace  = errors.New("not enough free disk space")
)

// InvalidError is a client error with a user-facing message.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func Invalid(format string, a ...any) error { return &InvalidError{fmt.Sprintf(format, a...)} }

// ConflictError is returned when a save's base hash doesn't match the file.
type ConflictError struct {
	CurrentBody   string
	CurrentSHA256 string
}

func (e *ConflictError) Error() string { return "note changed since it was loaded" }

type Store struct {
	Cfg *config.Config
	DB  *db.DB
	Dir string
	Log *slog.Logger

	locks   keyedMutex
	indexCh chan string
	space   spaceLeases

	// SyncIndex indexes on the spot instead of queueing. CLI commands set it:
	// they have no background indexer, so queued updates would be lost.
	SyncIndex bool

	// Legacy is set when the data folder was opened in its old layout (ModeLegacy):
	// only database-only commands and backups may run.
	Legacy bool

	metaCh chan struct{} // wakes the meta.json writer
}

// Open opens the data folder, converting an old layout first (see OpenWith).
func Open(cfg *config.Config, log *slog.Logger) (*Store, error) {
	return OpenWith(context.Background(), cfg, log, ModeConvert)
}

// OpenWith opens the data folder. mode decides what happens when it still uses
// the old flat layout (SPEC §3.3).
func OpenWith(ctx context.Context, cfg *config.Config, log *slog.Logger, mode OpenMode) (*Store, error) {
	dir := cfg.DataDir
	for _, d := range []string{"db", "tmp", "users", "backups", "lost+found"} {
		if err := os.MkdirAll(filepath.Join(dir, d), fsx.DirMode); err != nil {
			return nil, err
		}
	}
	if same, err := fsx.SameFS(filepath.Join(dir, "tmp"), filepath.Join(dir, "users")); err != nil || !same {
		return nil, fmt.Errorf("data/tmp and data/users must be on the same filesystem")
	}
	unlock, err := lockLayout(dir, log)
	if err != nil {
		return nil, err
	}
	defer unlock()
	d, err := db.Open(dir, 1) // the conversion backs up the old schema before migrating
	if err != nil {
		return nil, err
	}
	s := &Store{Cfg: cfg, DB: d, Dir: dir, Log: log, indexCh: make(chan string, 1024), metaCh: make(chan struct{}, 1)}
	if err := s.settle(ctx, mode); err != nil {
		d.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.DB.Close() }

// Ping checks that the metadata DB answers and the data dir is writable.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.DB.R.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Join(s.Dir, "tmp"), ".health-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// ---- ids & time ----

func NewID() string { return uuid.Must(uuid.NewV7()).String() }

func ValidID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

var refEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewRef returns a 10-char random base32 marker id (50 bits).
func NewRef() string {
	b := make([]byte, 7)
	rand.Read(b)
	return refEnc.EncodeToString(b)[:10]
}

func ValidRef(r string) bool {
	if len(r) != 10 {
		return false
	}
	for _, c := range r {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

func now() int64 { return time.Now().UnixMilli() }

// ---- audit ----

func (s *Store) Audit(actor, event, detail string) {
	if _, err := s.DB.W.Exec(`INSERT INTO audit_log(at,actor,event,detail) VALUES(?,?,?,?)`, now(), actor, event, detail); err != nil {
		s.Log.Error("audit write failed", "err", err)
	}
	s.Log.Info("audit", "actor", actor, "event", event, "detail", detail)
}

// ---- keyed mutex ----

type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*kmEntry
}

type kmEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*kmEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &kmEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// ---- helpers ----

func notFoundIfNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func placeholders(n int) string {
	if n == 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
