# Notes — a private, plain-text notebook for your homelab

A colorful, minimal, self-hosted notes web app.

- **Plain text.** Every note is a `.txt` file on your server.
- **Screenshots inline.** Paste (Ctrl+V) and the image appears right in the note.
- **Any file, any size.** Resumable uploads; the disk is the only limit.
- **Tags** with their own colors, **full-text search** (finds IPs, paths, hostnames), trash, pinning.
- **Private per user.** Users are created from the command line only.
- One Go binary + SQLite (metadata only) + a `data/` folder. Runs with Docker Compose.

## Quick start

```sh
docker compose up -d --build
docker compose exec -it notes notes user add yourname    # prompts for a password
```

Open **http://<server-ip>:4444** and sign in.

## Where your data lives

```
data/
├── db/notes.sqlite        metadata: users, tags, pins, trash state, attachment names
├── db/search.sqlite       search index — safe to delete, rebuilt automatically
├── users/<id>/notes/<note>/   one folder per note:
│     note.txt               the note (plain UTF-8 text) + note.txt.bak, the previous version
│     meta.json              its tags, pin, trash state, dates and image names
│     files/<ref>.png        its images and attachments, named after the marker in the text
├── backups/               output of `notes backup`
└── lost+found/            anything `notes doctor --fix` or `user rm` set aside
```

Screenshots appear in the note text as a single marker line, so the `.txt` stays readable:

```
Server rack wiring today:
[image:k7m2q9xh4t] rack-2026-10-21.png
Need to relabel port 12.
```

Each note's folder holds everything about it. An image pasted into a second note gets its own
copy in that note's folder, so deleting one note never breaks another.

**Back up both the files and the database** (`notes backup` does both). The note folders mirror
tags, pins and file names in `meta.json`, so if only the database is lost, `notes rebuild-db`
brings it back; passwords and sessions live only in the database.

Upgrading from a version that kept all notes in one folder: the first start converts the data
folder (after writing `data/backups/pre-layout-v2-*.tar.gz`). For a big folder you can run
`docker compose run --rm notes migrate-layout` first.

## Command line

Run any of these with `docker compose exec -it notes notes …`:

| Command | |
|---|---|
| `user add <name>` | create a user (`--password-stdin` for scripts) |
| `user passwd <name>` | reset a password (signs the user out everywhere) |
| `user list` | users, note counts, storage |
| `user disable <name>` / `enable <name>` | lock or unlock an account |
| `user rm <name> [--purge-data]` | remove a user; without `--purge-data` their files move to `lost+found/`. `--purge-data` asks you to type the name, so run it with `docker compose exec -it` |
| `backup [dir]` | consistent snapshot → `data/backups/notes-<time>.tar.gz` (safe while running) |
| `export <user> [file.zip]` | a folder per note (text, details, images) + `manifest.json` (default: `data/backups/`); also **Export my notes** in the app |
| `import <user> <zip\|dir>` | restore an export (tags, pins, dates, files), or create notes from `.txt` files |
| `reindex` | rebuild search |
| `doctor [--fix]` | check files ↔ database; `--fix` moves stray files to `lost+found/`, restores missing notes from `.bak`, copies missing images back from another note. Users can run their own check from **Account & data → Check my notes** |
| `migrate-layout` | convert an older data folder to one folder per note (the server also does this on its first start) |
| `rebuild-db` | refill a new, empty database from the note folders, then `user passwd` each user |
| `gc` | run maintenance now (trash purge, stale uploads, sessions) — the server already does this hourly |

`doctor --fix` and `gc` can run while the server is up (they re-check before changing
anything and leave files younger than 10 minutes alone), but for a big repair,
`docker compose stop` first is the calm option.

**Keep a copy off the box.** `data/backups/` lives on the same disk as your notes. Point
`notes backup` somewhere else from cron, e.g. a mounted NAS share:

```sh
# crontab: 03:00 every night
0 3 * * * cd /path/to/notes && docker compose exec -T notes notes backup /backups-nas
```

(mount that folder into the container in `compose.yaml`, e.g. `- /mnt/nas/notes:/backups-nas`).

Users can also export their own data from **Account & data** in the app.

A single note can be saved as one self-contained web page (⋯ → **Save as web page**): its text,
sections, highlights and images as in the app, plus every attached file, which can be saved back
from the page. It opens offline in any browser and prints cleanly.

### Restore

A restore **replaces** the data folder. Restore into a fresh one:

```sh
docker compose down
sudo mv data data.old                              # keep the old one until you're happy
sudo mkdir data && sudo tar -xzf data.old/backups/notes-<time>.tar.gz -C data
docker compose up -d                               # fixes ownership, rebuilds search
```

`sudo` because the container keeps `data/` private to its own user (`PUID`, mode 0700); if your
host user *is* `PUID`, you can drop it.

The backup holds `db/notes.sqlite` (a consistent snapshot) and `users/`. Leftover `-wal`/`-shm`
files from a live database never end up in it.

### If only the database is lost

Every note's folder mirrors its details (`meta.json`) and every user's folder its account
(`user.json`), so `notes rebuild-db` can rebuild a lost or broken `db/notes.sqlite` from `users/`:

```sh
docker compose down
sudo mv data/db data/db.broken                     # keep it until you're happy
docker compose run --rm notes rebuild-db           # lists the users it restored
docker compose run --rm notes user passwd alice   # once per user listed
docker compose up -d
```

What comes back: users (and whether they're disabled), every note with its text, title, pin,
trash state and dates, tags with their colours, images and attachments with their original
names, and search. What doesn't: **passwords** (never stored in the note folders, so set them
again as above), sign-in sessions, the audit log, and tags that aren't on any note.

`rebuild-db` only fills an empty database; it refuses if one with users or notes is there. It
trusts nothing in the folders as a path. If you only have `.txt` files (no note folders), use
`notes import` instead. A full `notes backup` is still the better safety net: it also keeps
passwords and the audit log.

### Moving notes between accounts or servers

`notes export alice` writes a zip to `data/backups/`. `notes import bob <that.zip>` recreates the
notes with their tags, pins, dates and attachments, and remaps image markers if needed.
`import` also accepts any folder of `.txt` files.

## Configuration

Set these in `compose.yaml` under `environment:`.

| Variable | Default | |
|---|---|---|
| `NOTES_ADDR` | `:4444` | listen address (all interfaces) |
| `PUID` / `PGID` | `1000` | owner of `data/` |
| `NOTES_TRUSTED_PROXIES` | *(empty)* | IPs/CIDRs of reverse proxies allowed to set `X-Forwarded-*` |
| `NOTES_SECURE_COOKIES` | `auto` | `auto` / `true` / `false` |
| `NOTES_TRASH_DAYS` | `30` | auto-delete trashed notes after N days (`0` = never) |
| `NOTES_MIN_FREE` | `1GiB` | refuse new uploads when free disk drops below this (`0` = off) |
| `NOTES_BACKUP_KEEP` | `7` | backups kept by `notes backup` |

`data/` must be on a **local disk**. Don't put it on an NFS or SMB share, because SQLite locking breaks there.

Plain HTTP on your LAN works. To use HTTPS, put a reverse proxy in front: see
[docs/reverse-proxy.md](docs/reverse-proxy.md).

## Keyboard

`Ctrl K` palette · `Alt N` new note · `/` search · `Ctrl S` save now · `Ctrl F` find in note ·
`Alt T` insert timestamp · `Ctrl Shift F` focus mode · `j`/`k` move through the list · `?` all shortcuts

## Development

```sh
cd web && npm install && npm run build && cd ..   # builds into internal/webui/dist
go test ./... && (cd web && npm test && npm run check)
NOTES_DATA_DIR=./data go run ./cmd/notes serve
# live frontend: (cd web && npm run dev) → http://localhost:5173 (proxies /api to :4444)
```

The design spec is in [SPEC.md](SPEC.md), and the design review notes are in [REVIEWS.md](REVIEWS.md).
