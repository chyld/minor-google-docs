package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"homelab-notes/internal/auth"
)

var usernameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Disabled  bool   `json:"disabled"`
	CreatedAt int64  `json:"created_at"`
}

type UserInfo struct {
	User
	Notes        int   `json:"notes"`
	StorageBytes int64 `json:"storage_bytes"`
}

func NormalizeUsername(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !usernameRe.MatchString(name) {
		return "", Invalid("username must match ^[a-z][a-z0-9_-]{0,31}$")
	}
	return name, nil
}

func (s *Store) CreateUser(ctx context.Context, name, password string) (*User, error) {
	name, err := NormalizeUsername(name)
	if err != nil {
		return nil, err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return nil, Invalid("%s", err.Error())
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &User{ID: NewID(), Username: name, CreatedAt: now()}
	if err := s.ensureUserDirs(u.ID); err != nil {
		return nil, err
	}
	_, err = s.DB.W.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at,updated_at) VALUES(?,?,?,?,?)`,
		u.ID, u.Username, hash, u.CreatedAt, u.CreatedAt)
	if err != nil {
		os.RemoveAll(s.userDir(u.ID))
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, Invalid("user %q already exists", name)
		}
		return nil, err
	}
	if _, err := s.CreateNote(ctx, u.ID, welcomeNote); err != nil {
		s.Log.Warn("welcome note", "err", err)
	}
	s.Audit("cli", "user.add", name)
	return u, nil
}

func (s *Store) UserByName(ctx context.Context, name string) (*User, string, error) {
	var u User
	var hash string
	err := s.DB.R.QueryRowContext(ctx, `SELECT id,username,disabled,created_at,password_hash FROM users WHERE username=?`,
		strings.ToLower(strings.TrimSpace(name))).Scan(&u.ID, &u.Username, &u.Disabled, &u.CreatedAt, &hash)
	if err != nil {
		return nil, "", notFoundIfNoRows(err)
	}
	return &u, hash, nil
}

func (s *Store) UserCount(ctx context.Context) (int, error) {
	var n int
	err := s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) ListUsers(ctx context.Context) ([]UserInfo, error) {
	rows, err := s.DB.R.QueryContext(ctx, `
SELECT u.id,u.username,u.disabled,u.created_at,
  (SELECT COUNT(*) FROM notes n WHERE n.user_id=u.id),
  (SELECT COALESCE(SUM(size_bytes),0) FROM notes n WHERE n.user_id=u.id) +
  (SELECT COALESCE(SUM(MAX(size_bytes,0)),0) FROM attachments a WHERE a.user_id=u.id)
FROM users u ORDER BY u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserInfo
	for rows.Next() {
		var u UserInfo
		if err := rows.Scan(&u.ID, &u.Username, &u.Disabled, &u.CreatedAt, &u.Notes, &u.StorageBytes); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPassword changes a password and revokes every session of the user.
func (s *Store) SetPassword(ctx context.Context, uid, password, actor string) error {
	if err := auth.ValidatePassword(password); err != nil {
		return Invalid("%s", err.Error())
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE users SET password_hash=?, updated_at=? WHERE id=?`, hash, now(), uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, uid); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.Audit(actor, "user.passwd", uid)
	return nil
}

func (s *Store) SetDisabled(ctx context.Context, name string, disabled bool) error {
	u, _, err := s.UserByName(ctx, name)
	if err != nil {
		return err
	}
	tx, err := s.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET disabled=?, updated_at=? WHERE id=?`, disabled, now(), u.ID); err != nil {
		return err
	}
	if disabled {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	ev := "user.enable"
	if disabled {
		ev = "user.disable"
	}
	s.Audit("cli", ev, u.Username)
	return nil
}

// RemoveUser deletes the account. Without purge, the user's files are moved
// to lost+found so nothing on disk is destroyed.
func (s *Store) RemoveUser(ctx context.Context, name string, purge bool) (string, error) {
	u, _, err := s.UserByName(ctx, name)
	if err != nil {
		return "", err
	}
	// Move files out of the way first: if we crash after this, the user row
	// still exists and `doctor` reports the missing files.
	src := s.userDir(u.ID)
	dst := filepath.Join(s.Dir, "lost+found", fmt.Sprintf("user-%s-%s", u.Username, time.Now().UTC().Format("20060102T150405Z")))
	moved := ""
	if _, err := os.Stat(src); err == nil {
		if err := os.Rename(src, dst); err != nil {
			return "", err
		}
		moved = dst
	}
	if _, err := s.DB.W.ExecContext(ctx, `DELETE FROM users WHERE id=?`, u.ID); err != nil {
		if moved != "" {
			os.Rename(dst, src)
		}
		return "", err
	}
	os.RemoveAll(s.tmpPath(u.ID, ""))
	s.purgeSearchUser(u.ID)
	if purge && moved != "" {
		if err := os.RemoveAll(moved); err != nil {
			return moved, err
		}
		moved = ""
	}
	s.Audit("cli", "user.rm", fmt.Sprintf("%s purge=%v", u.Username, purge))
	return moved, nil
}

// ---- sessions ----

const (
	SessionIdle     = 7 * 24 * time.Hour
	SessionAbsolute = 30 * 24 * time.Hour
	lastSeenEvery   = time.Hour
)

type Session struct {
	User      User
	TokenHash []byte
}

// CreateSessionVerified issues a session only if the account is still enabled and
// its password hash is still the one just verified (closes the login-vs-reset race).
func (s *Store) CreateSessionVerified(ctx context.Context, uid, verifiedHash, userAgent string) (string, error) {
	token, hash, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	t := time.Now()
	if len(userAgent) > 200 {
		userAgent = userAgent[:200]
	}
	res, err := s.DB.W.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,created_at,last_seen_at,expires_at,absolute_expires_at,user_agent)
SELECT ?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM users WHERE id=? AND password_hash=? AND disabled=0)`,
		hash, uid, t.UnixMilli(), t.UnixMilli(), t.Add(SessionIdle).UnixMilli(), t.Add(SessionAbsolute).UnixMilli(), userAgent, uid, verifiedHash)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return token, nil
}

func (s *Store) CreateSession(ctx context.Context, uid, userAgent string) (string, error) {
	token, hash, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	t := time.Now()
	if len(userAgent) > 200 {
		userAgent = userAgent[:200]
	}
	_, err = s.DB.W.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,created_at,last_seen_at,expires_at,absolute_expires_at,user_agent) VALUES(?,?,?,?,?,?,?)`,
		hash, uid, t.UnixMilli(), t.UnixMilli(), t.Add(SessionIdle).UnixMilli(), t.Add(SessionAbsolute).UnixMilli(), userAgent)
	return token, err
}

// SessionUser resolves a cookie token to its user, sliding the idle expiry.
func (s *Store) SessionUser(ctx context.Context, token string) (*Session, error) {
	hash := auth.HashToken(token)
	var sess Session
	var lastSeen, exp, abs int64
	err := s.DB.R.QueryRowContext(ctx, `
SELECT u.id,u.username,u.disabled,u.created_at,s.last_seen_at,s.expires_at,s.absolute_expires_at
FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=?`, hash).
		Scan(&sess.User.ID, &sess.User.Username, &sess.User.Disabled, &sess.User.CreatedAt, &lastSeen, &exp, &abs)
	if err != nil {
		return nil, notFoundIfNoRows(err)
	}
	t := time.Now()
	if t.UnixMilli() >= exp || t.UnixMilli() >= abs || sess.User.Disabled {
		s.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, hash)
		return nil, ErrNotFound
	}
	if t.Sub(time.UnixMilli(lastSeen)) > lastSeenEvery {
		newExp := min(t.Add(SessionIdle).UnixMilli(), abs)
		s.DB.W.ExecContext(ctx, `UPDATE sessions SET last_seen_at=?, expires_at=? WHERE token_hash=?`, t.UnixMilli(), newExp, hash)
	}
	sess.TokenHash = hash
	return &sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := s.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, hash)
	return err
}

func (s *Store) DeleteOtherSessions(ctx context.Context, uid string, keep []byte) (int64, error) {
	res, err := s.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND token_hash<>?`, uid, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) PasswordHash(ctx context.Context, uid string) (string, error) {
	var h string
	err := s.DB.R.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, uid).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}

func (s *Store) SweepSessions(ctx context.Context) (int64, error) {
	t := now()
	res, err := s.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=? OR absolute_expires_at<=?`, t, t)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const welcomeNote = `Welcome to your notes ✨

This is plain text — everything you type is saved as a .txt file on your server.

A few things to try:
• Paste a screenshot (Ctrl+V) — it appears right here in the note.
• Drag any file onto the editor to attach it. There is no size limit.
• Add tags below the title. Every tag gets its own color.
• Ctrl+K opens the command palette. Ctrl+N makes a new note.
• Press ? (outside the editor) to see every keyboard shortcut.

Your notes are private to your account. Delete this note whenever you like.
`
