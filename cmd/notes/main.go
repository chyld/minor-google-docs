// Command notes is the Homelab Notes server and admin CLI.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"homelab-notes/internal/config"
	"homelab-notes/internal/core"
	"homelab-notes/internal/server"
)

const usage = `notes — homelab notes server & admin CLI

Usage:
  notes serve                               run the web server (default)
  notes user add <name> [--password-stdin]  create a user
  notes user passwd <name> [--password-stdin]
  notes user list
  notes user disable <name> | enable <name>
  notes user rm <name> [--purge-data]
  notes backup [dest-dir]                   snapshot DB + files to a .tar.gz
  notes export <user> [file.zip]            export one user's notes (default: data/backups/)
  notes import <user> <file|folder>         restore an export (.zip), a note's .tar.gz,
                                            or create notes from *.txt files
  notes reindex                             rebuild the search index
  notes doctor [--fix]                      check files vs database
  notes gc                                  run maintenance once
  notes migrate-layout                      convert an old data folder to one folder per note
                                            (the server also does this on its first start)
  notes rebuild-db                          refill a new, empty database from the note folders
  notes healthcheck                         exit 0 if the server is healthy
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	if err := run(cmd, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "healthcheck":
		return healthcheck()
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if cmd != "serve" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cmd == "migrate-layout" || cmd == "serve" {
		log = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	store, err := core.OpenWith(ctx, cfg, log, openMode(cmd, args))
	if err != nil {
		return err
	}
	defer store.Close()
	store.SyncIndex = cmd != "serve"

	switch cmd {
	case "serve":
		return serve(ctx, cfg, store, log)
	case "user":
		return userCmd(ctx, store, args)
	case "backup":
		dest := ""
		if len(args) > 0 {
			dest = args[0]
		}
		out, err := store.Backup(ctx, dest)
		if err != nil {
			return err
		}
		fmt.Println("backup written:", out)
	case "export":
		if len(args) < 1 || len(args) > 2 {
			return errors.New("usage: notes export <user> [file.zip]")
		}
		u, _, err := store.UserByName(ctx, args[0])
		if err != nil {
			return fmt.Errorf("user %q: %w", args[0], err)
		}
		dest := filepath.Join(cfg.DataDir, "backups", fmt.Sprintf("export-%s-%s.zip", u.Username, core.Stamp()))
		if len(args) == 2 {
			dest = args[1]
		}
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if err := store.Export(ctx, u.ID, f); err != nil {
			f.Close()
			os.Remove(dest)
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		abs, _ := filepath.Abs(dest)
		fmt.Println("exported to", abs)
	case "import":
		if len(args) != 2 {
			return errors.New("usage: notes import <user> <export.zip | note.tar.gz | folder>")
		}
		u, _, err := store.UserByName(ctx, args[0])
		if err != nil {
			return fmt.Errorf("user %q: %w", args[0], err)
		}
		n, err := store.Import(ctx, u.ID, args[1])
		if rerr := store.Reconcile(ctx); rerr != nil && err == nil {
			err = rerr
		}
		if err != nil {
			return err
		}
		fmt.Printf("imported %d notes for %s\n", n, u.Username)
	case "reindex":
		n, err := store.Reindex(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("indexed %d notes\n", n)
	case "doctor":
		fix := len(args) > 0 && args[0] == "--fix"
		r, err := store.Doctor(ctx, fix)
		if err != nil {
			return err
		}
		if len(r.Problems) == 0 {
			fmt.Println("✓ no problems found")
			return nil
		}
		for _, p := range r.Problems {
			fmt.Println("•", p)
		}
		for _, f := range r.Fixed {
			fmt.Println("fixed:", f)
		}
		if !fix {
			fmt.Println("\nrun `notes doctor --fix` to repair:\n  • stray files older than 10 minutes move to data/lost+found (never deleted)\n  • notes with a missing file are restored from .bak, or recreated empty\n  • missing images are copied back from another note that has the same image\n  • waiting image copies are finished; meta.json and user.json are rewritten")
		}
	case "gc":
		if err := store.JanitorOnce(ctx); err != nil {
			return err
		}
		if err := store.Reconcile(ctx); err != nil {
			return err
		}
		fmt.Println("maintenance complete")
	case "migrate-layout":
		fmt.Println("✓ the data folder uses one folder per note")
	case "rebuild-db":
		rep, err := store.RebuildDB(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("restored %d users, %d notes, %d images\n", len(rep.Users), rep.Notes, rep.Images)
		for _, w := range rep.Warnings {
			fmt.Println("•", w)
		}
		if len(rep.Users) > 0 {
			fmt.Println("\nPasswords are not kept in the note folders. Set one for each user:")
			for _, u := range rep.Users {
				fmt.Printf("  notes user passwd %s\n", u)
			}
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

// openMode says what a command may do with a data folder still in the old layout:
// the server and migrate-layout convert it; database-only commands and backups
// run on it as is; everything else refuses until it is converted.
func openMode(cmd string, args []string) core.OpenMode {
	switch cmd {
	case "serve", "migrate-layout":
		return core.ModeConvert
	case "backup":
		return core.ModeLegacy
	case "user":
		if len(args) > 0 && (args[0] == "passwd" || args[0] == "list" || args[0] == "disable" || args[0] == "enable") {
			return core.ModeLegacy
		}
	}
	return core.ModeRefuse
}

func serve(ctx context.Context, cfg *config.Config, store *core.Store, log *slog.Logger) error {
	if err := store.Reconcile(ctx); err != nil {
		log.Error("startup reconcile", "err", err)
	}
	go store.RunMetaWriter(ctx)
	srv, err := server.New(cfg, store, log)
	if err != nil {
		return err
	}
	go store.RunIndexer(ctx)
	go store.RunJanitor(ctx, time.Hour, srv.Limiter().Sweep)
	go func() {
		if err := store.JanitorOnce(ctx); err != nil {
			log.Error("janitor", "err", err)
		}
	}()
	return srv.ListenAndServe(ctx)
}

func healthcheck() error {
	addr := os.Getenv("NOTES_ADDR")
	if addr == "" {
		addr = ":4444"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	} else if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1" + strings.TrimPrefix(addr, "0.0.0.0")
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func userCmd(ctx context.Context, store *core.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: notes user add|passwd|list|disable|enable|rm ...")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("user "+sub, flag.ContinueOnError)
	stdin := fs.Bool("password-stdin", false, "read the password from stdin")
	purge := fs.Bool("purge-data", false, "permanently delete the user's files")
	// allow flags after the positional name
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	need := func(n int) error {
		if len(pos) != n {
			return fmt.Errorf("usage: notes user %s <name>", sub)
		}
		return nil
	}
	switch sub {
	case "add":
		if err := need(1); err != nil {
			return err
		}
		pw, err := readPassword(*stdin, true)
		if err != nil {
			return err
		}
		u, err := store.CreateUser(ctx, pos[0], pw)
		if err != nil {
			return err
		}
		fmt.Printf("✓ created user %s\n", u.Username)
	case "passwd":
		if err := need(1); err != nil {
			return err
		}
		u, _, err := store.UserByName(ctx, pos[0])
		if err != nil {
			return fmt.Errorf("user %q: %w", pos[0], err)
		}
		pw, err := readPassword(*stdin, true)
		if err != nil {
			return err
		}
		if err := store.SetPassword(ctx, u.ID, pw, "cli"); err != nil {
			return err
		}
		fmt.Printf("✓ password changed for %s (all sessions signed out)\n", u.Username)
	case "list":
		users, err := store.ListUsers(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "USERNAME\tCREATED\tNOTES\tSTORAGE\tSTATUS")
		for _, u := range users {
			status := "active"
			if u.Disabled {
				status = "disabled"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", u.Username, time.UnixMilli(u.CreatedAt).Format("2006-01-02"), u.Notes, humanBytes(u.StorageBytes), status)
		}
		tw.Flush()
	case "disable", "enable":
		if err := need(1); err != nil {
			return err
		}
		if err := store.SetDisabled(ctx, pos[0], sub == "disable"); err != nil {
			return fmt.Errorf("user %q: %w", pos[0], err)
		}
		fmt.Printf("✓ %sd %s\n", sub, pos[0])
	case "rm":
		if err := need(1); err != nil {
			return err
		}
		if _, _, err := store.UserByName(ctx, pos[0]); err != nil {
			return fmt.Errorf("user %q: %w", pos[0], err)
		}
		if *purge {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return errors.New("--purge-data needs an interactive terminal (docker compose exec -it ...)")
			}
			fmt.Printf("This permanently deletes every note and file of %q.\nType the username to confirm: ", pos[0])
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) != strings.ToLower(strings.TrimSpace(pos[0])) {
				return errors.New("confirmation did not match; nothing deleted")
			}
		}
		moved, err := store.RemoveUser(ctx, pos[0], *purge)
		if err != nil {
			return err
		}
		fmt.Printf("✓ removed user %s\n", pos[0])
		if moved != "" {
			fmt.Printf("  their files were kept at %s\n", moved)
		}
	default:
		return fmt.Errorf("unknown user command %q", sub)
	}
	return nil
}

func readPassword(fromStdin, confirm bool) (string, error) {
	if fromStdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("no terminal: use `docker compose exec -it ...` or --password-stdin")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if confirm {
		fmt.Fprint(os.Stderr, "Repeat password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
	}
	return string(a), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
