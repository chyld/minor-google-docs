// Package config loads runtime settings from environment variables.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

type SecureMode int

const (
	SecureAuto SecureMode = iota
	SecureOn
	SecureOff
)

type Config struct {
	Addr           string
	DataDir        string
	TrustedProxies []netip.Prefix
	SecureCookies  SecureMode
	TrashDays      int
	MinFree        int64
	BackupKeep     int
}

func Load() (*Config, error) {
	c := &Config{
		Addr:       env("NOTES_ADDR", ":4444"),
		DataDir:    env("NOTES_DATA_DIR", "/data"),
		TrashDays:  30,
		MinFree:    1 << 30,
		BackupKeep: 7,
	}
	var err error
	if c.TrustedProxies, err = parseProxies(os.Getenv("NOTES_TRUSTED_PROXIES")); err != nil {
		return nil, err
	}
	switch strings.ToLower(env("NOTES_SECURE_COOKIES", "auto")) {
	case "auto":
		c.SecureCookies = SecureAuto
	case "true", "1", "yes":
		c.SecureCookies = SecureOn
	case "false", "0", "no":
		c.SecureCookies = SecureOff
	default:
		return nil, fmt.Errorf("NOTES_SECURE_COOKIES must be auto, true or false")
	}
	if v := os.Getenv("NOTES_TRASH_DAYS"); v != "" {
		if c.TrashDays, err = strconv.Atoi(v); err != nil || c.TrashDays < 0 {
			return nil, fmt.Errorf("NOTES_TRASH_DAYS: invalid value %q", v)
		}
	}
	if v := os.Getenv("NOTES_MIN_FREE"); v != "" {
		if c.MinFree, err = ParseSize(v); err != nil {
			return nil, fmt.Errorf("NOTES_MIN_FREE: %w", err)
		}
	}
	if v := os.Getenv("NOTES_BACKUP_KEEP"); v != "" {
		if c.BackupKeep, err = strconv.Atoi(v); err != nil || c.BackupKeep < 1 {
			return nil, fmt.Errorf("NOTES_BACKUP_KEEP: invalid value %q", v)
		}
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("NOTES_TRUSTED_PROXIES: %w", err)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("NOTES_TRUSTED_PROXIES: %w", err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// ParseSize parses sizes like "1GiB", "500MB", "0", "1048576".
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	units := []struct {
		suf  string
		mult int64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suf) {
			mult = u.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suf))
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}
