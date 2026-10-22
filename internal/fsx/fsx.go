// Package fsx holds crash-safe filesystem helpers.
package fsx

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	DirMode  = 0o700
	FileMode = 0o600
)

// SyncDir fsyncs a directory so a rename or create inside it is durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// WriteAtomic writes data to path via a uniquely named temp file in the same
// directory: write → fsync → rename → fsync dir.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, filepath.Base(path)+"."+randHex(6)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return SyncDir(dir)
}

// MoveDurable renames src to dst (same filesystem) and fsyncs both dirs.
func MoveDurable(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	if err := SyncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	if filepath.Dir(src) != filepath.Dir(dst) {
		return SyncDir(filepath.Dir(src))
	}
	return nil
}

func HashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FreeBytes returns bytes available to unprivileged users on the filesystem holding path.
func FreeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// SameFS reports whether two paths live on the same device.
func SameFS(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
