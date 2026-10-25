# Homelab Notes — Spec v2

Self-hosted, private, multi-user **plain-text** notes web app for a homelab. Each user's notes are
private. Content lives as files under `data/`; SQLite holds metadata. Runs via Docker Compose.

v2 incorporates round-1 review from team beta — see `REVIEWS.md`.

**Owner decisions (round 1):** keep resumable (tus) uploads · free-space guard default 1 GiB ·
listen on **all interfaces, port 4444** by default.

## 1. Requirements (owner — fixed)

1. Web app, self-hosted in a homelab.
2. Notes are **plain text** (not Markdown, not rich text).
3. **SQLite** database tracks metadata.
4. Note text, images and files are saved as files under **`data/`**.
5. UI: modern, fresh, elegant, minimal — and **colorful**.
6. Notes can have **tags**.
7. **Paste screenshots** into notes.
8. **Attach any kind of file**, **no size limit**.
9. Users are **added via the command line** only.
10. Notes are **private per user**. No sharing, no co-editing.
11. Deployed with **Docker Compose**.

## 2. Architecture

```
Browser — Svelte 5 SPA + CodeMirror 6
   │  HTTP :4444 on all interfaces (optional reverse proxy for TLS)
   ▼
Go binary  `notes`
   ├── serve      embedded SPA, JSON API, tus upload endpoint, janitor
   ├── CLI        user … | backup | export | import | reindex | doctor | gc | healthcheck
   ├── SQLite     data/db/notes.sqlite   metadata (authoritative for metadata)
   ├── SQLite     data/db/search.sqlite  full-text index (derived, disposable)
   └── Files      data/users/<uid>/…     note bodies + attachment bytes (authoritative for content)
```

| Layer | Choice | Notes |
|---|---|---|
| Backend + CLI | Go 1.27 (pinned), one static binary | Same binary is server and CLI |
| HTTP | stdlib `net/http` + Go 1.22 routing | No framework |
| DB driver | `modernc.org/sqlite` (pinned) | Pure Go, no CGO. **Build-time check:** FTS5 + `trigram` tokenizer present. Fallback: `mattn/go-sqlite3` behind `database/sql`. |
| Migrations | Embedded SQL, `schema_version` table, applied at startup | |
| Uploads | **Hand-rolled minimal tus 1.0** (core + creation + creation-with-upload + termination) | Not `tusd`. ~300 lines. |
| Passwords | Argon2id, PHC string (m=64 MiB, t=3, p=1) | Params stored per hash so they can change |
| Frontend | **Svelte 5 + Vite** SPA, TypeScript, tiny router (~4 routes) | No SvelteKit — no SSR needed |
| Editor | CodeMirror 6, block widget decorations | |
| Styling | Hand-written CSS, design tokens (OKLCH) first | No UI kit; self-hosted fonts, no CDN |
| Container | Alpine, multi-stage, multi-arch (amd64 + arm64) | Runs as `PUID:PGID` via entrypoint + `su-exec` |

**Single instance only.** One server process per `data/`. `data/` must be on a **local filesystem**
(not NFS/SMB — breaks SQLite locking and rename atomicity).

## 3. Data layout & source of truth

### 3.1 Layout

One folder per note: everything that belongs to a note sits together, so a lost database can be
rebuilt from the folders (§3.4) and a person can read the data folder directly.

```
data/
├── layout                      # "2" once the folder uses this layout (§3.3)
├── db/
│   ├── notes.sqlite            # metadata (+ -wal, -shm)
│   ├── search.sqlite           # FTS index — delete anytime, `notes reindex` rebuilds
│   └── layout.lock             # flock held while a process checks/converts the layout
├── tmp/
│   └── <uid>/<upload-id>       # in-progress uploads, per user, GC after 24 h
│   └── <uid>/purge-<note-id>/  # a note folder being deleted forever (§7.6)
├── backups/                    # default target of `notes backup`; pre-layout-v2-*.tar.gz (§3.3)
├── lost+found/                 # where doctor, the conversion and `user rm` set things aside
└── users/
    └── <user-uuid>/
        ├── user.json                   # mirror of the account (never the password)
        └── notes/
            └── <note-uuid>/
                ├── note.txt            # body — UTF-8, LF line endings
                ├── note.txt.bak        # previous version (one generation)
                ├── meta.json           # mirror of the note's database row (§3.4)
                └── files/              # created on the first image
                    └── <ref>.<ext>     # raw bytes, named after the marker's ref
```

- IDs: UUIDv7. No path is ever built from user input: folder names are UUIDs, file names are a
  validated ref plus an extension chosen from a fixed table by the sniffed content type
  (`png jpg gif webp bmp ico pdf zip gz mp3 wav ogg mp4 webm txt`, anything else `bin`).
- Permissions: dirs `0700`, files `0600`, owned by the container UID.
- `tmp/` and `users/` are on the same filesystem (required for atomic rename; checked at startup).

### 3.2 What is authoritative

The database is the source of truth for everything except the note text, which stays
file-authoritative (the file is written first, and reconcile copies file → database), so a crash
between the two writes can never lose the latest edit.

| Data | Authority | If lost |
|---|---|---|
| Note body | `note.txt` | Gone (restore from backup) |
| Attachment bytes | `files/<ref>.<ext>` | Copied back from another note with the same image (doctor), else gone |
| Title, size, sha256 of a note | Derived from `note.txt` | Recomputed on startup reconcile |
| Tags, tag colors, note↔tag, pinned, trash state, dates, attachment filename/mime | **`notes.sqlite`**, mirrored in `meta.json` | `notes rebuild-db` (§3.4) |
| Users (name, disabled) | **`notes.sqlite`**, mirrored in `user.json` | `notes rebuild-db`; passwords must be set again |
| Passwords, sessions, audit log | `notes.sqlite` only | Back up the DB |
| Search index | Derived | `notes reindex` |

### 3.3 Converting an older data folder

Data folders from before this layout (`notes/<id>.txt`, `files/<attachment-id>`) are converted
once, automatically, by `notes serve` before it starts listening (or ahead of time with
`notes migrate-layout`). The conversion:

1. Takes `data/db/layout.lock` (an exclusive `flock`), so two processes never convert at once.
2. Checks free space: the backup (size of `users/` + the database twice) + every image copy it will
   make + `NOTES_MIN_FREE`. Not enough → exits with the numbers; nothing has changed.
3. Writes **one** backup of the untouched folder and old schema, `backups/pre-layout-v2-<ts>.tar.gz`,
   verifies it can be read, and records it in `backups/pre-layout-v2.done` (a resumed run doesn't make
   another; it is never pruned).
4. Applies migration 002 (§4.2).
5. Moves each note's `.txt`/`.bak` into its folder, and each image into its note's `files/`: the
   extension is sniffed and stored on the row *before* the rename, so a crash in between resumes.
   Renames never overwrite: if both copies exist and differ, the old one goes to `lost+found/`.
6. Gives every note that shows another note's image its own copy (§6.3).
7. Writes every `meta.json` and `user.json`.
8. Only then moves what is left in the old places (files with no row) to `lost+found/layout-v2-<ts>/`.
9. Marks the layout done (`settings.layout_version`, `data/layout`).

Every step can be repeated, so a crash or `SIGTERM` part-way is finished by the next start. A
copy that fails stays queued (§6.3) and never stops the conversion. Image hashes are taken from the
files, not from the old rows. The backup record is trusted only if the archive it names still
opens; an archive written just before a crash is adopted. A trashed note whose text was already
gone (an interrupted purge) gets no folder, so reconcile finishes the purge.

If the database has **no users but the folder still has old-layout notes** (a lost database), every
command refuses (`ErrLostDBOldLayout`): the old layout keeps tags, pins and image links only in the
database, so converting would strand the notes. Restore `db/notes.sqlite` from a backup first.
Folders of accounts the database doesn't know are left to doctor and don't block the layout check. Until the
folder is converted, other commands refuse to run (they would misread it), except `backup` and
`user passwd|list|disable|enable`, which only touch the database or copy files verbatim. Restoring an
old backup later is converted again on the next start. Going back to an older version of the app
means restoring the pre-layout backup.

### 3.4 Mirrors and `notes rebuild-db`

`meta.json` is rewritten right after every change to its note (create, save, pin, trash, tags, image
added/renamed/removed/copied, hash computed). Tag renames, recolours and deletes touch many notes:
those notes are queued in `meta_dirty` in the same transaction and rewritten in the background, by
the hourly janitor and at startup. The JSON is canonical (fixed field order, tags sorted by name,
images by creation time, integer-millisecond times), so different bytes always mean "out of date".

```json
{"version":1,"id":"…","created_at":1792878064254,"updated_at":1792878064254,"pinned":true,"trashed_at":null,
 "tags":[{"name":"work","color":"teal"}],
 "attachments":[{"ref":"hespb6kga2","ext":"png","filename":"screenshot.png","mime":"image/png",
                 "size_bytes":123,"sha256":"…","created_at":1792878064264}]}
```

`notes rebuild-db` refills a new, empty database from the folders: users (with their `disabled`
flag and a password that never verifies — set one with `notes user passwd`), tags by name, notes
(pinned, trash, dates), images, then the search index. Nothing read from the files is used as a
path: user and note ids must equal their folder names, refs and extensions are validated, file
paths are rebuilt from them, and stored types are re-sniffed.

## 4. Database schema

### 4.1 Connection setup (every connection)

`PRAGMA foreign_keys=ON; journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000;`
One writer connection (serialized by a mutex) + a small read pool.

### 4.2 `notes.sqlite`

Timestamps are integer unix milliseconds, UTC.

```sql
CREATE TABLE users(
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,            -- ^[a-z][a-z0-9_-]{0,31}$
  password_hash TEXT NOT NULL,              -- Argon2id PHC string
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE sessions(
  token_hash BLOB PRIMARY KEY,              -- SHA-256 of 256-bit random token
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,            -- updated at most once per hour
  expires_at INTEGER NOT NULL,              -- idle expiry
  absolute_expires_at INTEGER NOT NULL,     -- hard cap
  user_agent TEXT);                         -- display only
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_exp  ON sessions(expires_at);

CREATE TABLE notes(
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title TEXT NOT NULL,                      -- derived (§5.4)
  pinned INTEGER NOT NULL DEFAULT 0,
  trashed_at INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  size_bytes INTEGER NOT NULL,              -- derived
  content_sha256 TEXT NOT NULL,             -- derived; cache of the file's hash
  UNIQUE(id, user_id));
CREATE INDEX notes_list ON notes(user_id, trashed_at, pinned, updated_at);

CREATE TABLE tags(
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,        -- trimmed, NFC, 1–40 chars, no commas
  color TEXT NOT NULL,                      -- palette key, e.g. "coral"
  UNIQUE(user_id, name), UNIQUE(id, user_id));

CREATE TABLE note_tags(
  note_id TEXT NOT NULL, tag_id TEXT NOT NULL, user_id TEXT NOT NULL,
  PRIMARY KEY(note_id, tag_id),
  FOREIGN KEY(note_id, user_id) REFERENCES notes(id, user_id) ON DELETE CASCADE,
  FOREIGN KEY(tag_id, user_id)  REFERENCES tags(id, user_id)  ON DELETE CASCADE);
CREATE INDEX note_tags_tag ON note_tags(tag_id);

CREATE TABLE attachments(                  -- as of migration 002 (§3.3)
  id TEXT PRIMARY KEY,                      -- client-generated UUIDv7, validated (copies: server UUIDv7)
  ref TEXT NOT NULL,                        -- 10-char base32 marker id (§6); same in every copy
  user_id TEXT NOT NULL,
  note_id TEXT NOT NULL,
  filename TEXT NOT NULL,                   -- original name, display only
  mime TEXT NOT NULL,                       -- server-sniffed
  ext TEXT NOT NULL DEFAULT '',             -- disk extension, stored before the file moves ('' = not yet)
  size_bytes INTEGER NOT NULL,
  sha256 TEXT,                              -- integrity, computed after finalize
  state TEXT NOT NULL,                      -- 'uploading' | 'ready' | 'deleting'
  created_at INTEGER NOT NULL,
  UNIQUE(note_id, ref),                     -- an image at most once per note
  FOREIGN KEY(note_id, user_id) REFERENCES notes(id, user_id) ON DELETE CASCADE);
CREATE INDEX attachments_note ON attachments(note_id);
CREATE INDEX attachments_ref ON attachments(user_id, ref);

CREATE TABLE ref_content(                   -- one ref = one image's content per user (§6.1)
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  ref TEXT NOT NULL, sha256 TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(user_id, ref));
CREATE TABLE pending_copies(                -- markers waiting for their copy (§6.3)
  note_id TEXT NOT NULL, user_id TEXT NOT NULL, ref TEXT NOT NULL, created_at INTEGER NOT NULL,
  PRIMARY KEY(note_id, ref),
  FOREIGN KEY(note_id, user_id) REFERENCES notes(id, user_id) ON DELETE CASCADE);
CREATE TABLE meta_dirty(note_id TEXT PRIMARY KEY REFERENCES notes(id) ON DELETE CASCADE);  -- §3.4
CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);                          -- layout_version

CREATE TABLE audit_log(
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL, actor TEXT NOT NULL,  -- 'cli' | user id
  event TEXT NOT NULL, detail TEXT);        -- login ok/fail, user add/rm/passwd, purge
```

Composite FKs enforce that a note's tags and attachments always belong to the same user.
Every API query additionally filters `WHERE user_id = :session_user`.

### 4.3 `search.sqlite` (derived, disposable)

```sql
CREATE VIRTUAL TABLE notes_fts USING fts5(
  title, body,
  note_id UNINDEXED, user_id UNINDEXED,
  tokenize = 'trigram');                    -- substring search: IPs, paths, hostnames
CREATE TABLE indexed(note_id TEXT PRIMARY KEY, content_sha256 TEXT NOT NULL);
```

- Body is indexed with marker lines stripped.
- Queries: user input is escaped into a quoted FTS5 phrase (never raw `MATCH`). Queries < 3 chars
  fall back to title `LIKE` in `notes.sqlite`. Invalid query → empty result, not 500.
- Results always re-filtered against `notes.sqlite` (owner + not trashed) before returning.
- Index failures never fail a save; they are logged and repaired by reconcile.

## 5. Note save protocol

### 5.1 Server (`PUT /api/notes/{id}/body` with `{body, base_sha256}`)

1. Validate: valid UTF-8, no NUL, ≤ 10 MiB; normalize CRLF → LF.
2. Acquire the **per-note lock** (in-process mutex keyed by note id).
3. Hash the **current file on disk**. If ≠ `base_sha256` → release, return **409** with
   `{current_body, current_sha256}`. If it already equals the new body (a retry), it's a success;
   markers added over `note.txt.bak` are queued again for copying (§6.3).
4. Queue copies for image markers the new text adds (§6.3), before the file changes.
5. Copy the current file to `note.txt.bak` (one generation).
6. Write `note.txt.<random>.tmp` in the note's folder → fsync → rename to `note.txt` → fsync dir.
7. `BEGIN IMMEDIATE` → update `title, size_bytes, content_sha256, updated_at` → `COMMIT`; rewrite `meta.json`.
8. Release lock; make the queued copies (§6.3). Respond `200 {sha256, updated_at, title}`.
9. Update `search.sqlite` (best effort, async).

### 5.2 Crash recovery (startup reconcile, also `notes doctor`)

For each note row: if file hash ≠ `content_sha256` → **file wins**, recompute derived fields (and
`meta.json`). For each `indexed` row: if sha ≠ note's sha → re-index. Remove stray `*.tmp` files
older than 10 minutes. A trashed note whose folder is gone is an interrupted purge: finish it, and
delete leftover `tmp/<uid>/purge-<id>` folders. Then rewrite queued `meta.json` files and retry
waiting image copies (§6.3). Runs at startup, takes seconds at homelab scale.

### 5.3 Client

- Debounced autosave (800 ms idle) + on blur / `visibilitychange` / `beforeunload`.
- **One save in flight per note**; each save's `base_sha256` = previous response's `sha256`.
- Every keystroke batch is also written to **IndexedDB drafts**; cleared on successful save.
  On load, a draft newer than the server version offers "restore draft".
- On **409**: autosave pauses. Inline bar shows a diff of mine vs. server with
  **Keep mine** (overwrite using the new base), **Use theirs**, **Save mine as a new note**.
  Nothing is discarded silently (and `.bak` exists).

### 5.4 Title derivation

First non-empty line that is **not** a marker line, trimmed, truncated to 120 chars on a
grapheme boundary. Empty → `Untitled`.

## 6. Plain text + inline screenshots

### 6.1 Marker grammar

A marker is an **entire line** matching:

```
^\[image:([a-z2-7]{10})\](?: (.*))?$
```

```
Server rack wiring today:
[image:k7m2q9xh4t] rack-2026-10-21.png
Need to relabel port 12.
```

- `ref` = 10 chars of random base32 (50 bits). Generated client-side together with the attachment
  UUID. **One ref is one image's content per user**: a new upload claims its ref in `ref_content`
  in the same transaction as its row, so a ref used before (even by a deleted image) is refused and
  the client retries with a new one. Copies of an image in other notes keep the ref (§6.3).
- The caption after `]` is cosmetic; resolution is **by `ref` only**, scoped to the session user
  (copying a marker into another of your notes works, §6.3).
- Anything not matching exactly (e.g. leading spaces) is ordinary text. No escaping needed.
- Only `[image:…]` markers exist. Non-image files live in the attachment panel only.

### 6.2 Editor behavior (CodeMirror 6)

- Marker line → `Decoration.replace({block: true})` widget showing the image (max-width capped,
  rounded, caption on hover, click → lightbox). `atomicRanges` so the cursor skips it;
  Backspace/Delete removes the whole line.
- Unresolvable ref → muted dashed placeholder "image not found".
- Paste/drop image flow: client generates `id` + `ref` → inserts marker immediately with an
  inline progress placeholder → uploads → placeholder becomes the image. Typing never blocks.
- Removing a marker doesn't delete the attachment; the panel flags it as "not shown in note".
- If a shown image fails to load (its copy was deleted meanwhile), the widget looks the ref up once
  more before showing it as broken.

### 6.3 An image used in several notes

Pasting a marker into another note gives that note its own **real copy** of the image, in its own
folder, with the same ref, so the text never changes and deleting either note never breaks the other.

- **When:** a save (or a note created with text) records the markers it *adds*, compared with the
  previous text, that the note has no image for, in `pending_copies`, before the file is written.
  After the save the server copies them (outside the note lock) and the response carries the note's
  new attachment list, so the editor and the attachment count update at once. Only added markers
  are queued, so deleting a copy from the attachments panel stays deleted.
- **How:** both notes are locked in id order; the bytes go to a temp file in the target's `files/`,
  the size and the hash (against `ref_content`) are checked, then a durable rename and the row.
  Disk space is leased like an upload's.
- **Not possible yet** (source still uploading, disk full): the request stays in `pending_copies`
  and is retried when that upload finishes, by the hourly janitor, at startup, and by doctor. The
  image still shows meanwhile (refs resolve user-wide). A marker whose image exists nowhere yet
  (its upload not registered yet) waits 24 h before it is dropped; one whose every copy lost its
  file is dropped at once (doctor reports the missing image).
- **Race-free:** a waiting request is checked against the note's text, copied and removed while
  holding the note locks, so a save adding the marker again can't slip in between.
- **Never lost:** deleting forever (or deleting from the panel) the last copy of an image that another
  note still waits for first finishes that copy; if it can't, it is refused with `409 copy_pending`
  ("an image in this note is still being copied to another note"). The check and the switch to
  `deleting` are one immediate transaction, so two deletes (or a delete and a paste) can't both
  pass it. Empty trash keeps such notes and says how many (`{deleted, kept}`); the janitor retries.
- `POST /api/notes/{id}/duplicate` gives the duplicate its own copy of every file, including ones
  only in the attachments panel.
- `GET /api/refs/{ref}` returns the newest ready copy (all copies have the same content).

## 7. Uploads & attachments

### 7.1 Protocol

Hand-rolled tus 1.0 at `/api/uploads/`: `POST` (creation, `Upload-Length` or
`Upload-Defer-Length`, `Upload-Metadata: id, ref, note_id, filename`), `HEAD`, `PATCH`,
`DELETE` (termination). `creation-with-upload` makes small pastes a single request.
Client chunk size 8 MiB (fits behind Cloudflare tunnels / default proxies).

### 7.2 Ownership

- Creation requires a session + CSRF checks; validates `note_id` belongs to the user and isn't
  trashed; validates `id` (UUIDv7) and `ref` uniqueness.
- Inserts `attachments` row with `state='uploading'`. Temp bytes at `data/tmp/<uid>/<id>`.
- Every `HEAD`/`PATCH`/`DELETE` re-checks `attachments.user_id = session user`. Foreign → 404.

### 7.3 Finalize (on last byte, before responding)

fsync temp file → sniff mime and the disk extension from magic bytes → store both on the
still-`uploading` row → rename into the note's `files/<ref>.<ext>` (never recreating a note folder
that was purged meanwhile) → fsync dir → `UPDATE … SET state='ready' WHERE state='uploading'`.
The client sees success only after the row is `ready`. Idempotent if retried: if the rename
landed but the row update didn't, `HEAD` (or an empty `PATCH` at the full offset) finds the file
from the stored extension and completes it. Finishing an upload also runs any copies waiting
for that image (§6.3).
**sha256 is computed asynchronously** after `ready` (and backfilled by the janitor), so a
multi-GB upload doesn't stall the final response behind a full re-read of the file; it is an
integrity record, not part of the upload handshake.

### 7.4 Serving `GET /api/attachments/{id}`

- Authorized by `user_id`; foreign or missing → identical 404.
- `http.ServeContent` → **Range support** (resumable downloads, video seeking).
- **Inline only** if sniffed type ∈ {png, jpeg, gif, webp}. Everything else (incl. SVG, HTML,
  XML, PDF, text) → `Content-Disposition: attachment`.
- All responses: `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: sandbox; default-src 'none'`, RFC 5987 `filename*` with
  CR/LF/quotes/path separators stripped.
- No in-app PDF or text/code viewer in v1.

### 7.5 "No size limit" — protecting the disk

No per-file or per-user cap. Uploads are streamed (bounded memory). New uploads (and PATCHes)
are refused with **507** when free space on `data/` < `NOTES_MIN_FREE` (default 1 GiB, `0`
disables). Free space and per-user usage shown in the UI and `/healthz`-adjacent CLI output.

### 7.6 Lifecycle

- Delete one attachment: `state='deleting'` → unlink file → delete row (idempotent; janitor retries).
- Trash note: the folder stays; restore brings everything back.
- Purge note: finish copies other notes wait for (§6.3) → mark attachments `deleting` (a late upload
  can't land) → rename the note folder to `tmp/<uid>/purge-<id>` in one step → delete the rows →
  remove that folder. A crash after the rename is finished by reconcile.
- Uploads in `state='uploading'` older than 24 h: janitor deletes temp file + row.

## 8. Users, auth & security

### 8.1 CLI (same binary)

```
docker compose exec -it notes notes user add <name>          # password via TTY, echo off
docker compose exec -T  notes notes user add <name> --password-stdin
docker compose exec -it notes notes user passwd <name>       # revokes all sessions
docker compose exec    notes notes user list                 # name, created, notes, storage
docker compose exec    notes notes user disable|enable <name>
docker compose exec -it notes notes user rm <name> [--purge-data]   # revokes sessions;
                                                             # purge requires retyping name
```
Passwords: min 10 chars, never accepted in argv. All CLI user ops go to `audit_log`.

### 8.2 Sessions

- 256-bit random token in cookie; DB stores SHA-256 only.
- Cookie: `HttpOnly; SameSite=Lax; Path=/`; when secure: `Secure` + `__Host-` prefix.
- Rotated on login. Idle expiry 7 days (sliding), **absolute** 30 days.
- Revoked on `passwd`, `user rm`, `user disable`, in-app password change, "log out other sessions".
- Janitor sweeps expired sessions.

### 8.3 Login

- Same generic error for bad user / bad password; **dummy Argon2 verify** for unknown users.
- Rate limit: per-username exponential backoff (no hard lockout) + per-client-IP limit.
- Argon2 verifies capped at `max(1, NumCPU/2)` concurrent; excess → 429.
- Limiter is in-memory (resets on restart — acceptable for v1).

### 8.4 Reverse proxy trust

`NOTES_TRUSTED_PROXIES` (CIDR list, default empty). Only requests from these honor
`X-Forwarded-For` / `X-Forwarded-Proto`. `NOTES_SECURE_COOKIES=auto|true|false` (default `auto`:
secure when the request arrived over TLS or via trusted proxy with `proto=https`).

### 8.5 Request hardening

- CSRF: `SameSite=Lax` + **`Origin` / `Sec-Fetch-Site` same-origin check** on every mutating
  route (incl. login, logout, all tus methods) + `X-Requested-With` header. No CORS headers, ever.
  No GET mutates.
- `ReadHeaderTimeout` 10 s, `MaxHeaderBytes` 64 KiB, `http.MaxBytesReader` on JSON routes
  (1 MiB; note body route 10 MiB). No global `WriteTimeout`; upload/download routes use
  per-request deadlines via `http.ResponseController`.
- Headers (SPA): `Content-Security-Policy: default-src 'self'; script-src 'self';
  style-src 'self' 'unsafe-inline'; img-src 'self' blob:; object-src 'none'; base-uri 'self';
  frame-ancestors 'none'` (`unsafe-inline` styles required by CodeMirror),
  `Referrer-Policy: no-referrer`, `Permissions-Policy` denying camera/mic/geo,
  `Cross-Origin-Opener-Policy: same-origin`.
- IDOR: every lookup scoped by `user_id`; foreign IDs → 404. Covered by tests (§13).
- Logs: never note bodies, passwords, cookies, tokens.
- Out of scope, stated: 2FA/TOTP, encryption at rest (use disk encryption).

## 9. Ops

| Command | What it does |
|---|---|
| `notes serve` | Server + janitor (default command) |
| `notes backup [dest]` | `VACUUM INTO` the DB, then tar `users/` → `notes-<ts>.tar.gz` (skips `tmp/`, `*.tmp`, `search.sqlite`). Safe while running. Keeps last N (`NOTES_BACKUP_KEEP`). |
| `notes export <user> [dest]` | Zip with one folder per note (§9.3) |
| `notes import <user> <zip\|dir>` | Restore an export (old or new zip layout, read through `manifest.json`), or create notes from `.txt` files |
| `notes reindex` | Rebuild `search.sqlite` from the note files |
| `notes doctor [--fix]` | Every user's check (§9.2) plus folders of deleted users, stray uploads and disk space. `--fix` is explicit; nothing is deleted. |
| `notes gc` | Run janitor once (trash purge, upload GC, session sweep, meta.json queue, waiting copies) |
| `notes migrate-layout` | Convert an older data folder now (§3.3); `serve` also does it on start |
| `notes rebuild-db` | Refill a new, empty database from the note folders (§3.4) |
| `notes healthcheck` | Exit 0 if `/healthz` OK (for Docker) |

- **Janitor** (in `serve`, hourly): purge notes trashed > `NOTES_TRASH_DAYS` (default 30, `0` = never),
  GC stale uploads, sweep sessions, retry `deleting` rows. Every purge logged + audited.
- `/healthz`: no version/user info. Checks that the server can answer (`SELECT 1` on the
  metadata DB, no write lock) and that `data/tmp` is writable (a tiny temp file is created and
  removed) — a read-only or full volume makes it 503, which is the point: the app can't save.
- Restore: stop, put `data/` back (or untar backup), delete `search.sqlite`, start (reindexes; an
  old-layout backup is converted, §3.3). Lost only the database: with the server stopped, move `db/`
  aside, run `notes rebuild-db` (it opens a new, empty database itself), set passwords, start.

### 9.2 Check my notes (per-user doctor)

**Account & data → Check my notes** (also in the command palette) runs a check of the signed-in
user's notes only, on the server in the background (`POST /api/me/doctor {fix}` starts it, one at
a time per user; `GET /api/me/doctor` polls; it isn't tied to the request, so a large account
can't hit the request deadline). It lists problems in plain words, naming notes by title and
never showing server paths; **Repair** runs it again with `fix`. It checks:

- a missing (or emptied) notes folder: reported as "is the data volume mounted?" and nothing is
  repaired, so a check never writes empty notes onto the wrong disk;
- a note without its text: restored from `.bak`, else recreated empty — never for a trashed note
  (that is an interrupted purge, finished automatically), and re-checked under the note lock so a
  save made meanwhile is never overwritten;
- an image whose file is missing: copied back from another note with the same image, else reported;
- an image a note shows but doesn't have: copied in (§6.3), or reported as "no longer exists";
- copies still waiting; an image file whose bytes differ from its ref's content (hashed);
- `meta.json` / `user.json` missing or out of date: rewritten;
- files and folders the database doesn't know (including old-layout leftovers): moved to
  `lost+found/doctor-<ts>/` if older than 10 minutes.

Repairs never delete anything, and never remove database rows. The CLI `notes doctor` runs the same
check for every user (with paths and ids in its messages) plus the checks that span users. A folder
of an account the database doesn't know that holds notes is reported and left in place (a lost
database: `notes rebuild-db` needs it).

### 9.3 Export

**Export my notes** (account menu, command palette, and Account & data) downloads
`GET /api/export`, also `notes export`: a zip with one folder per note, `notes/<title>/` (trashed
notes under `trash/`), each holding `note.txt`, `meta.json` and `files/` with every image under
its original name. Folder names are made unique once per note (`Untitled`, `Untitled (2)`) and
file names once per folder, comparing case-insensitively. `manifest.json` (version 2) lists every
note's folder, file, tags with their colours, pins, dates and images (ref, name, file), and is what
import reads, so old and new zips both import. Importing keeps an image's ref when it is free, or
when the same ref already holds the same content (a copy); otherwise it picks a new ref and
rewrites the marker. A single note can be downloaded as a `.tar.gz` in the same layout (§24);
`notes import` takes that too, unpacking it into a new folder under `data/tmp/` first (folders
and regular files only, every name a plain relative path, written through `os.Root`; links and
devices skipped; the folder is removed afterwards).

### Config (env)

`NOTES_ADDR` (`:4444`), `NOTES_DATA_DIR` (`/data`), `NOTES_TRUSTED_PROXIES`, `NOTES_SECURE_COOKIES`
(`auto`), `NOTES_TRASH_DAYS` (30), `NOTES_MIN_FREE` (1GiB), `NOTES_BACKUP_KEEP` (7),
`PUID`/`PGID` (1000).

## 10. API (JSON under `/api`, session auth)

Errors: `{"error": {"code": "conflict", "message": "…"}}` with proper HTTP status.
Lists use keyset pagination: `?limit=50&cursor=<opaque>` → `{items, next_cursor}`.

```
POST   /api/login                POST /api/logout          POST /api/logout-others
GET    /api/me                   PUT  /api/me/password     GET  /api/me/storage
GET    /api/me/doctor            POST /api/me/doctor {fix}  (§9.2)
GET    /api/notes?q=&tag=&untagged=&pinned=&trash=&has_attachments=&sort=&limit=&cursor=
         (q present → FTS results with escaped highlight snippets)
POST   /api/notes                          → {id, sha256}  (empty body)
GET    /api/notes/{id}                     → meta + body + sha256 + tags + attachments
PUT    /api/notes/{id}/body                {body, base_sha256} → 200 (+attachments when images were copied, §6.3) | 409
PATCH  /api/notes/{id}                     {pinned?, tag_ids?, trashed?}
POST   /api/notes/{id}/duplicate           (copies body and gives the copy its own images, §6.3)
DELETE /api/notes/{id}                     permanent; only when trashed (409 copy_pending, §6.3)
POST   /api/trash/empty
GET    /api/tags                           → with counts
POST   /api/tags   PATCH /api/tags/{id}   DELETE /api/tags/{id}
POST|HEAD|PATCH|DELETE /api/uploads/…     tus
GET    /api/attachments/{id}               download/inline (§7.4)
PATCH  /api/attachments/{id}               {filename}
DELETE /api/attachments/{id}
GET    /api/export                         zip of the current user's data
GET    /api/notes/{id}/html[?estimate=1&theme=]  one note as a self-contained web page (§23)
GET    /api/notes/{id}/archive[?check=1]   one note as a .tar.gz with all its files (§24)
GET    /healthz
```

Contract frozen as shared TypeScript types + Go structs **before** parallel implementation.

## 11. Features (v1)

**Notes** — create · edit · autosave with status dot · IndexedDB drafts · conflict diff bar ·
title from first line · pin · duplicate · trash / restore / permanent delete / empty trash ·
"purges in N days" chip · undo toast after trash · sort (updated, created, title) ·
word & char count · insert timestamp shortcut.

**Tags** — many per note · inline create with autocomplete · color auto-assigned by hashing
the name into a 12-color palette, user-changeable · rename / recolor / delete · sidebar with
counts · click to filter, multi-select = AND · "Untagged" filter.

**Search** — Ctrl+K palette (note titles, tags, actions: new note, toggle theme, trash, sidebar,
log out) · full-text substring search with highlighted snippets · filters combine
(tags, pinned, has attachments, trash) · also matches attachment filenames.

**Screenshots & attachments** — paste / drag-drop images → inline · attach any file, any size,
resumable · upload tray (progress, cancel, retry) · attachment panel (type-colored icon, name,
size, rename, download, delete, "not shown in note" flag) · image lightbox (←/→ through a
note's images, zoom-to-fit) · range downloads.

**Account** — login · change password · log out other sessions · storage used + disk free.

**Data** — export zip (UI + CLI) · import folder (CLI) · backup (CLI) · reindex · doctor.

## 12. UI / visual design

- **Layout:** sidebar 248 px (220 px on narrow screens; collapses to a 60 px icon rail) · note list
  340 px (drag to resize, 260–560 px, remembered; can be hidden) · editor fluid. Focus mode
  (Ctrl+Shift+F) hides both side panes. See §18. Mobile: list → editor push navigation with back
  button, tags as a horizontal chip scroller, attachments as a bottom sheet, floating
  new-note button, PWA manifest (installable, no offline).
- **Surfaces:** tinted neutrals, never pure black/white (light canvas ≈ `#f6f3eb`, ink `#1c2430`;
  dark canvas ≈ `#16141f`). Trash view desaturates the note cards and the open note.
- **Color system:** 12 tag hues defined once in OKLCH with matched lightness; each has
  light/dark chip bg (`color-mix` ~16%) + text pair verified ≥ 4.5:1. Hue drives chip,
  note-card 3 px accent stripe, sidebar dot. One brand gradient: logo, empty states, login
  mesh; surfaces carrying white text (primary button, new-note button, badges) use a deeper
  version of it so the text meets AA. Everything else flat.
- **Type:** self-hosted Inter (UI) · editor proportional 16–17 px / 1.65, ~68 ch measure,
  no gutter, soft wrap; mono toggle (JetBrains Mono). First line styled as a large title.
- **Note list:** cards — title, one preview line (marker lines skipped), relative time,
  tag dots, pin/attachment icons; hover reveals pin/trash actions without layout shift;
  skeleton loading. Not virtualized: cards use `content-visibility: auto`, which keeps
  thousands of cards cheap to render and is simpler than windowing for homelab-sized lists.
- **Editor chrome:** none while typing. Save status = 6 px dot (amber saving, green saved,
  red error) plus a short word ("Saved", "Saving…") and an `aria-live` announcement; never
  a toast for autosave. Drop overlay with gradient dashed border.
- **Motion:** 120–160 ms, transform/opacity only, honors `prefers-reduced-motion`.
- **Empty states:** soft gradient orb + one line + shortcut hint. First login seeds a
  "Welcome" note explaining shortcuts and image paste.
- **Login:** single card on a gradient mesh, no sign-up link; if no users exist, shows the
  `notes user add` command.
- **Keyboard:** Ctrl+K palette, Alt+N new (browsers reserve Ctrl+N), Ctrl+S save now,
  Ctrl+\ sidebar rail, Ctrl+Shift+\ notes list, Ctrl+Shift+F focus mode, Ctrl+Shift+H highlight, Ctrl+B / Ctrl+U bold / underline, Ctrl+/ raw text, `/` search, `?` cheat sheet (outside
  text fields). `c` (new note) and `j`/`k`/Enter only while the note list has focus.
- **Accessibility:** visible focus rings, WCAG AA contrast, state never by color alone,
  `aria-live` save status, 40 px touch targets, 16 px inputs (no iOS zoom).

## 13. Testing

- Go: `t.TempDir()` fixtures; save protocol incl. 409 race (concurrent saves), crash between
  rename and commit → reconcile; purge ordering; upload resume / cancel / finalize / GC;
  **cross-user IDOR suite** (user B gets 404 for every user A id on every route); FTS escaping;
  marker parsing; CSRF/Origin checks; tus over HTTP (create, repeat create, HEAD metadata,
  offset mismatch, foreign ids, empty final PATCH, DELETE); concurrent finalize recovery;
  shared free-space leases; search with >1000 hits; password change vs. admin reset.
- Frontend: Vitest for marker parser, save queue (chaining, conflicts, retries, idempotent
  retry), stale-draft cleanup, and the upload client (cancel mid-create, `ref_taken`
  resume vs. re-mint, completing a half-finalized upload) against a scripted fake server.
  Draft restore on load (IndexedDB) is covered manually; it needs a browser.
- E2E (Playwright, against `docker compose up`): login → create → type → paste image →
  reload → image renders → tag → search finds it → upload 2 GB file with simulated drop +
  resume → export zip.

## 14. Docker Compose

```yaml
services:
  notes:
    build: .
    image: homelab-notes:latest
    ports: ["4444:4444"]                # all interfaces (owner decision)
    volumes: ["./data:/data"]
    environment:
      PUID: 1000
      PGID: 1000
      NOTES_TRUSTED_PROXIES: ""
    read_only: true
    tmpfs: ["/tmp"]
    cap_drop: [ALL]
    cap_add: [CHOWN, DAC_OVERRIDE, FOWNER, SETUID, SETGID]  # entrypoint fixes ownership, su-exec drops to PUID
    security_opt: ["no-new-privileges:true"]
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "notes", "healthcheck"]
      interval: 30s
```

Shipped with `docs/reverse-proxy.md`: Caddy + nginx examples (`client_max_body_size 0`,
`proxy_request_buffering off`, long timeouts, trusted proxy setting, HSTS).

## 15. Out of scope (v1)

Sharing, co-editing, version history (beyond one `.bak`), Markdown, in-app PDF/text viewers,
mobile apps, OAuth/SSO/2FA, encryption at rest, admin web UI, API tokens / quick-capture
(`notes push`, v1.1), per-user quotas, multiple server instances.

## 16. Build plan

1. **Alpha:** scaffold repo, migrations, frozen API contract (Go + TS types), Dockerfile/compose,
   design tokens.
2. **Beta (parallel, one owner each):** (a) notes API + save protocol + reconcile · (b) auth,
   sessions, CLI user mgmt, security middleware · (c) tus uploads + attachment serving + janitor ·
   (d) search index + backup/export/import/doctor · (e) frontend shell, layout, theme, tags,
   list, palette · (f) editor: CodeMirror, markers, paste flow, drafts, conflict bar.
3. **Alpha:** integrate, `docker compose up`, run E2E.
4. **Beta:** cross-review each piece by a different model.

## 17. Editing around inline images

Images are text lines `[image:<ref>] caption` drawn as block widgets (`web/src/lib/editor/markers.ts`).
The editing rules live in `web/src/lib/editor/images.ts`. The model: an image is a block; the cursor
only ever sits on text lines; an image is reached as an **image selection** (its whole line selected,
outlined in the accent colour); image lines change only as whole lines.

**17.1 Geometry.** Block widgets carry no vertical margin (CodeMirror's height map cannot see it — that
was the cause of arrow keys skipping lines); spacing is padding inside `.cm-img`. Each widget watches its
own size with a `ResizeObserver` and calls `view.requestMeasure()`, so late image loads never leave the
height map stale. Natural image sizes are cached per ref for `aspect-ratio`/`estimatedHeight`.
`drawSelection({drawRangeCursor:false})` draws the caret, so it is visible wherever the cursor is.

**17.2 Guard** (`transactionFilter`, user transactions only). For every image line a change touches:
- removed as part of a larger deletion → allowed; exactly its hidden text removed (cut) → one newline
  goes with it, so no blank line is left;
- exactly its text replaced by text (typing, IME commit, mobile insert, Alt-T, paste over a selected
  image) → the image is kept and the text goes on a new line after it;
- text replacing a range that ends at the image's start or begins at its end (typing over a
  triple-clicked line above it) → a newline is added so the text keeps its own line;
- anything else (joining it with a non-empty neighbour, partial edits, indent, text glued to either end)
  → the transaction is refused. Joins with an *empty* neighbour line are fine.
Replace-all skips image lines (`changeFilter`). Pasted text that starts/ends with a marker line gets that
line on its own (`clipboardInputFilter`). Our own structural edits carry the `imageOp` annotation.

**17.3 Selection.** A cursor that lands on an image line becomes that image's selection when a person
caused it (click, keys, delete) and moves to the nearest text line otherwise (programmatic edits, upload
cancel). A range ending inside an image's hidden text grows to the whole line — except search matches,
which stay as they are so Find/Replace can step past a caption; the first editing key on such a match
selects the image instead of editing (so Backspace after Find never deletes it). New states
(`setDoc`) start on text; an update listener corrects paths that bypass filters (undo/redo).

**17.4 Keys** (one `Prec.highest` keydown handler; edits are no-ops in read-only notes).

| Key | Cursor on a text line | Image selected |
|---|---|---|
| ↑ / ↓ | Leaving the line towards an adjacent image selects it; otherwise normal | Previous/next image is selected, or the cursor goes to the end/start of the text line |
| ← / → | At line start/end next to an image: select it | Same as ↑ / ↓ |
| Backspace / Delete | At the boundary of a non-empty line: select the image (no delete). Empty line: removed normally | Delete the image (+ one newline); cursor to the nearest text line |
| Held Backspace/Delete (key repeat) | Never selects or deletes an image | Never deletes |
| Enter / Shift+Enter | normal | New empty line below / above the image |
| Space | normal | Open the image |
| Escape | normal | Cursor to the next text line |
| Typing / IME / mobile input | normal | First a new line is made after the image, then the input lands there (Alt+key shortcuts don't count as typing, except on macOS) |
| Mobile Backspace (input event) | normal | Delete the image |

**17.5 Mouse.** Clicking an image selects it; clicking the selected image opens the lightbox (on `click`,
not `mousedown`, so the release can't hit the lightbox scrim). Shift-click extends the selection to the
image. In the trash (read-only) a single click opens it, and images are keyboard-focusable buttons.
Focus never moves into the widget, so closing the lightbox returns focus to the editor.

**17.6 Inserting images** (paste/drop/attach). After the image block the cursor is on a text line: an
empty line the images were pasted on stays below them; otherwise the start of the following text line,
or a new empty line at the end of the note / before another image. Never above the title. With an image
selected, new images go after it. The attach button keeps the cursor the user placed (end of note if none).

**17.7 Known limitations.** Captions (file names) are not editable. Notes already corrupted by the old bug
are not repaired automatically. Mobile is verified in desktop Chromium only.

**17.8 Tests.** `web/src/lib/editor/images.test.ts` (state-level rules). `web/e2e/images.mjs` drives a
real browser against a running server (`PW=<playwright> BASE=… USER_NAME=… PASS=… node web/e2e/images.mjs`).

## 18. Collapsible panels and page width

Desktop only (≥ 861 px); phones keep one pane at a time and ignore these settings. State lives in
`app` (`web/src/lib/state.svelte.ts`), persisted by `web/src/lib/layout.ts` (`notes.sidebarCollapsed`,
`notes.listCollapsed`, `notes.pageWidth`; the old `notes.sidebarHidden` migrates to the rail once).

- **Sidebar → icon rail (60 px).** Toggle in the sidebar header or Ctrl+\ (matched by physical key).
  The rail keeps New note, Jump to (Ctrl+K), the five views, the account menu and the theme toggle, all
  with tooltips; tags are tucked away. An active tag filter shows as a tag icon with the tag's dot
  (clicking it expands the sidebar), so a filter is never invisible.
- **Notes list → hidden (0 px).** Toggle in the list header, Ctrl+Shift+\, or the "Show notes list"
  button that replaces it in the note bar (and on the empty screen). Hidden panes are `inert` and
  `visibility:hidden`; the grid keeps its columns so the resizer stays in place. Picking a view or a tag,
  or `/` (search), brings the list back; Ctrl+K jumping leaves it hidden. Its width is remembered.
- Picking a view on a phone never changes the desktop list setting. Ctrl+\ also matches the `\`
  character, so layouts that type it with AltGr work.
- **Page width** (note ⋯ menu, Ctrl+K): Auto (default: 760 px, or 1100 px while the list is hidden or in
  focus mode) · Normal 760 px · Wide 1100 px · Full (up to 1600 px). Shown as `data-width` on `.shell`.
- **Focus mode** hides both panes without touching these settings; Esc restores exactly what was there.
- Panels snap (no animation), so text rewraps once; CodeMirror re-measures on resize.

Tests: `web/src/lib/layout.test.ts`, `web/e2e/panels.mjs`.

## 19. Text highlights

Stored in the note text, so they survive export/backup/conflicts and can't drift:
`=={name}highlighted words==`. Grammar (single line, one definition in `web/src/lib/highlight.ts` and
`internal/core/highlight.go`, both tested against `internal/core/testdata/highlights.json`):
`==\{(<name>)\}([^\n]+?)==` with names longest-first (each quoted in the Go regexp). A name is a
**style set**: an optional colour, then `b` (bold), then `u` (underline), joined by `+` in that order —
`coral`, `b`, `u`, `b+u`, `coral+b`, `sky-deep+b+u`. The 24 colours are the 12 tag hues (`coral` …
`pink`), each plain (light tint) and `-deep` (strong); with the style flags that makes 99 names, in a
fixed order shared by both languages (per colour: `c`, `c+b`, `c+u`, `c+b+u`; then `b`, `u`, `b+u`).
Colours and style names are separate lists: sections, the swatches and the remembered last colour
only ever use the 24 colours (`## {b} x` is not a section). Unknown names (`=={u+b}`, `=={bu}`,
`=={bold}`), `a == b`, unterminated or multi-line text stay plain. A note that already contained one
of the new names literally (e.g. text about this syntax) is read as formatted since this version. Image lines are never highlights. No escapes. The TS parser is hand-written and linear
(a lazy regex rescans the line for every unclosed opener); Go's RE2 is linear too. No length cap.

- **Writing.** A highlight's text never contains `==` (it would end early), never ends with `=` and is
  never only whitespace; when applying, those characters simply stay uncoloured. So for any apply/remove,
  the text without delimiters is unchanged (property-tested). Every written line is re-parsed and must
  read back as exactly the intended text and colours (a stray, unclosed `=={coral}` earlier on the line
  would otherwise be completed by the new `==`); if not, that line is left alone. Emoji are never split.
- **Drawing** (`web/src/lib/editor/highlights.ts`). An index of all highlights (`StateField`, changed
  lines rescanned) drives viewport decorations: coloured text, delimiters hidden as atomic inline
  replaces. When the selection touches a highlight its delimiters show dimmed (not atomic), so it is
  always visible whether typing lands inside (before the closing `==`) or outside. Nothing reveals or
  hides while the mouse button is down. The cursor never rests inside a delimiter. Next to an image,
  the text edge of a highlight that opens/closes the line counts as the line edge (Backspace/arrows
  reach the image as on any line).
- **Edits keep them well-formed** (`transactionFilter`, user edits): an edit crossing a delimiter skips
  it; Backspace/Delete on a delimiter removes the highlight but keeps its text; typing over part of a
  delimiter is refused. A highlight whose text no longer parses the same is rewritten: a line break
  inside (Enter, paste) closes and reopens it on each line, `==` typed inside splits it, and one left
  with no text (or only spaces) loses its delimiters; `=` typed at the very end goes after the closer
  (it could not be part of the text). The cursor ends after what was typed. Replace-all never edits
  delimiters (`changeFilter`). Registered before the image rules, so image edits (a dropped image line)
  are shaped first and highlights then close/reopen around them.
- **Apply / remove** (one undo step, `input.highlight`): per text line of the main selection, trimmed to
  its non-whitespace extent; parts of other highlights are split (outside parts keep their colour);
  touching same-colour runs merge; idempotent. With just a cursor in a highlight's text (not merely
  next to it): recolour/remove it.
  Image lines, blank lines and read-only notes are skipped. Afterwards the coloured text is selected
  (with just a cursor, the cursor stays between the same two characters).
- **Bold and underline** (`applyStyle`, one undo step): **toggle** — decided once for the whole
  selection over its visible characters (images and headings skipped): if all have it, it comes off
  everywhere, otherwise it goes on everywhere; trimmed to the non-whitespace extent when adding, never
  when removing; emoji never split. **Colour** changes only the colour of each character (bold and
  underline stay; "Remove colour" keeps them too). **Clear formatting** removes colour, bold and
  underline. A cursor in a span's text acts on that whole span; in plain text nothing happens (there is
  no "bold the next characters"). Drawn with `.fmt-b` (700) / `.fmt-u` (underline, from-font thickness);
  the tint (`.hl`, `--h`) only when there is a colour; in the title line bold keeps the title's 760.
  Ctrl+B / Ctrl+U (Cmd on a Mac) toggle; in the editor they are always swallowed (so Firefox's bookmarks
  sidebar and view-source don't open), also while an input method is composing (then they do nothing),
  and do nothing in raw view, read-only notes, on images and headings. Ctrl+Shift+B / U are left to the
  browser. Ctrl+U replaces CodeMirror's rarely used "undo selection". A bare cursor keeps its place in
  the text after a change. Characters a highlight can never carry (an "=" of "==" inside it, a trailing
  "=") don't count for the toggle, so "a == b" switches on and off. A selection that crosses a section
  heading formats the text around it (the heading is skipped). Palette: Bold, Underline, Clear
  formatting (not for read-only notes).
- **UI.** Note-bar highlighter button (enabled with a selection or a cursor in a highlight) opens a
  12×2 grid (6 columns on phones, pinned to the bottom) with Bold, Underline (pressed: on / mixed / off)
  and Clear formatting above it, and "Remove colour" when the selection has one; these toggles keep the
  menu open, colours close it. The section colour pickers use the same grid without the formatting row.
  Desktop also gets a small bar above a selection (fixed position, kept inside the note's scroll area,
  below the selection near the top; its menu kept inside the window): Bold, Underline, last colour,
  colours, Remove colour, Clear formatting. The bold/underline state is only worked out while one of
  these menus is showing, over at most 200,000 characters of line text (beyond that the buttons show
  "mixed").
- **New rules, old notes.** Titles, previews and search text are derived with versioned rules
  (`deriveVersion` in `maintenance.go`); when they change (as with bold/underline), startup reconcile
  re-derives every note once.
  Arrow keys follow the visible columns. Ctrl+Shift+H opens the
  grid; arrows move, Enter applies, Esc returns to the editor. The grid starts on the current or last
  colour, so Ctrl+Shift+H, Enter repeats it. Menus keep the editor's selection (mousedown prevented).
- **Colours.** Background tints `oklch(--hl-l --hl-c hue)` with an ink colour per theme; light and
  strong text pairs are unit-tested at ≥ 4.5:1 for every hue in both themes. Forced-colours mode uses
  `Mark`/`MarkText`. The swatches carry names (tooltip, `aria-label`).
- **Clipboard.** Copy/cut/drag build their text from positions: `text/plain` without delimiters (Slack,
  email), and in `text/x-notes-raw` the same range with balanced delimiters (a highlight cut by the
  selection is closed/reopened at the cut). Paste and drop in this app prefer the raw text (keeping any
  line padding the image rules added) — unless pasting into a highlight, where the plain text takes that
  highlight's colour. The last-used colour is validated before use.
- **Elsewhere.** Titles, previews (strip → trim → skip blank → truncate), the search index and snippets
  (`core.SearchText`; snippet chunks end at a line break so no delimiter is cut in half) and the footer
  counts all ignore delimiters. Export, backups and the conflict view
  show the raw text. Ctrl+F matches raw text, so a word highlighted mid-way may not be found there
  (server search finds it).

Tests: `internal/core` (fixture, titles/previews), `web/src/lib/highlight.test.ts` (grammar, round-trip
property, contrast), `web/src/lib/editor/highlights.test.ts` (apply/remove, edit rules, clipboard),
`web/e2e/highlights.mjs` (browser); bold/underline: `web/src/lib/editor/format.test.ts` (toggles, cursor,
`==`, edit rules with combined names), `web/e2e/format.mjs` (browser, incl. a Firefox shortcut check).

## 20. Raw text view

A note-bar button (`</>`, Ctrl+/, the ⋯ menu's "Raw text", and the command palette) switches the editor
between the rendered note and its raw text: the stored file exactly, in the monospace font: `[image:…]`
lines, `=={name}…==` delimiters and the title line are plain, editable text.

- **One editor.** The rendering extensions (`markers`, `highlights`, `imageEditing`) sit in a CodeMirror
  compartment that holds `rawEditing()` instead in raw mode, so switching keeps the text, cursor and
  undo history. Raw edits are saved like any other. Switching back (and any undo/redo in the rendered
  view) re-runs the rendered view's selection rules: a cursor inside image text selects the image or
  moves off it, and none rests in a hidden delimiter. A Find match (search panel open) keeps its exact
  range, as when Find selected it, so the next Backspace selects the image rather than deleting it. That re-check is not "the user placed the cursor":
  attachments still go to the end of a note nobody clicked in (§17.6).
- **Edit rules off, two safety nets on.** Typing and deleting are never reshaped: delimiters can be typed
  or deleted one character at a time, and copy gives the text as shown. A broken highlight or marker is
  just text again in the rendered view. But search-and-replace never rewrites an `[image:ref]` or a
  highlight's delimiters (a replace-all would cut images loose; captions and highlighted text are fine),
  and text copied in the rendered view pastes with its highlights (`text/x-notes-raw`).
- **No colour tools.** The highlight button, floating bar, Ctrl+Shift+H and Ctrl+B / Ctrl+U do nothing in raw mode;
  switching with the colours open (or focus on their button) closes them and returns focus to the note. Pasted or dropped images
  still upload and insert their marker line. Footer counts include the delimiters (what is on screen).
- **Shortcut.** Ctrl+/ goes by the character typed (`e.key === '/'`), so it follows the layout (Ctrl+Shift+7
  on German keys, AltGr layouts too) and never takes Ctrl+- or Ctrl+?; ignored while held (key repeat),
  in other text boxes (search, tags, the find field), behind modals, or when the editor already handled the key.
- **Screen readers and focus.** Switching is announced ("Raw text" / "Rendered note"); the button keeps
  focus in the note on desktop.
- **Phones.** Below 560 px the note-bar button is hidden (the bar must fit at 320 px) and "Raw text" is
  in the ⋯ menu; the raw (and monospace) font stays 17 px so iOS doesn't zoom on focus. Trashed notes on phones have no
  ⋯ menu, so the raw toggle is desktop-only there.
- **Remembered.** The choice applies to every note and is kept in `localStorage` (`notes.raw`).
  Works on trashed (read-only) notes too, for viewing.

Tests: `web/src/lib/editor/raw.test.ts` (replace-all safety net), `web/e2e/raw.mjs` (browser: switching,
layouts, attachments, clipboard, counts, undo, focus, trashed notes, phone toolbar).

## 21. End of note

A small asterism (`⁂`, three dots in a triangle, an inline SVG so it looks the same on every device)
is drawn centred under the last line of every note with text, with a faded hairline either side running
out to the edges of the text column, each ending in a small diamond, so it's clear nothing more follows, for example after a final image or
blank lines.

- **Not text.** It sits outside the editor (`.note-end` in `NoteView.svelte`), is `aria-hidden`, and can't
  be selected, copied, searched or saved.
- **Colour.** Neutral grey (`--muted`, faded; the hairlines fainter than the dots) in both themes, whatever the note's tags, so it never
  competes with section or tag colours. Full-strength `GrayText` in forced-colours mode.
- **Click.** A plain left click (or tap) on it, or on the empty space under the note in line with the
  text (anywhere down to the bottom of the scroll area, not the side margins), puts the cursor at the
  end of the note: `focus(true)`, so in the rendered view a note ending with an image selects that image
  and typing starts a new line under it; in raw text the cursor goes to the very end of the text. Only
  a click that also *started* there counts (press and release within a few pixels): a drag-selection
  from the text that ends below keeps its selection. Shift+click extends the selection to the end;
  Ctrl/Alt/Meta clicks are left alone.
  Nothing happens on trashed (read-only) notes (`cursor: default` there). On a phone a tap there works
  the same and brings up the keyboard, as tapping under the text does in other notes apps. The keyboard
  equivalent is Ctrl+End (Cmd+End / Cmd+↓ on a Mac).
- **When.** Every note with non-blank text, rendered or raw, read-only or not, desktop and phone; hidden
  on an empty note (the placeholder shows there).

Tests: `web/e2e/endmark.mjs` (browser).

## 22. Sections

A note can be divided into named, coloured sections. Design reviewed by the beta team before building.

- **Grammar.** A line `## {colour} Name` starts a section. `colour` is one of the 24 highlight names and
  is required. `Name` must not be blank, meaning it can't be only spaces and tabs (the same rule in Go
  and TS; other Unicode spaces count as text). It is matched with `[^\n]*`, so it may contain any
  other character. The line must start at column 0 with exactly `## {`. The section
  runs to the next heading or the end of the note (flat, no nesting). Anything else stays ordinary text:
  `## Plain`, `## {see below}`, `#`/`###`, indented lines, an unknown colour. Existing notes are never
  reinterpreted. The title line (§5.4) is never a section. Text before the first section belongs to no
  section. Heading lines are never highlights (like image lines). Same grammar in
  `internal/core/section.go` and `web/src/lib/heading.ts`, both tested against
  `internal/core/testdata/sections.json`.
- **Look.**
  - The heading shows only the name: semibold, 1.2em, in the hue (`--sec-l`/`--sec-c`, contrast-tested
    ≥ 4.5:1 per hue in both themes against `--bg` and `--surface`). There's no horizontal rule: the
    vertical bar carries the colour. Space above comes from padding, so the height map stays exact.
  - Body lines keep normal ink, with no tint, so same-hue highlights stay visible.
  - A 3px bar in the hue runs down the body, from the line after the heading to the last non-blank
    line. It is drawn as a CodeMirror layer, so it continues past image widgets.
  - `## {colour} ` is hidden and atomic. It shows dimmed when the selection touches the heading. It is
    frozen while the mouse is down, like highlight delimiters.
  - "Strong" (`-deep`) colours differ by heading text only: deeper ink (`--sec-deep-l`/`--sec-deep-c`,
    also contrast-tested) and heavier weight. Every bar is the same 3px.
  - Forced colours: `CanvasText` heading and bar.
  - A heading on the title line is not a section. The editor shows it as the title with its markup;
    the server's derived title strips the markup. This is accepted: insertion never puts one there.
- **Heading controls.**
  - An arrow before the name collapses or opens the section.
  - Both are 22px buttons at the heading's ink colour. Coarse pointers get a 40px invisible hit area
    that doesn't change the line height.
  - A ⋯ button after it (shown on hover or when the cursor is on the heading; always on touch screens)
    opens a menu: Colour… (the 24-swatch grid; "Remove section" there too), Move up, Move down,
    Collapse/Show, and Remove section (keeps text).
- **Adding.**
  - The note-bar "Add section" button (desktop), the ⋯ menu (also on phones) and the palette open the
    colour grid. The last-used section colour is focused.
  - Picking a colour inserts `## {colour} New section` after the cursor's line, with "New section"
    selected for typing. With the cursor on a heading, it goes after that section's text, never into
    its body or a collapsed part. The button, ⋯ item and palette entry are offered only when the note is
    editable and has a title line (a note of only images has none yet). It goes after a selected image. It is never on or above the title line. A
    blank line becomes the heading.
  - There is no keyboard shortcut (Ctrl+Shift+S is the browser's Save As). Typing `## ` never creates a
    section by itself; a completed valid line simply is one.
- **Edit rules** (filters registered before highlights/images, so those shape an edit first):
  - The cursor never rests inside `## {colour} `. ArrowLeft from the name's start goes to the line
    above. Home lands at the name's start. A range may start at the line start only if it reaches
    beyond the heading: Shift+Home selects just the name, so typing over it renames the section.
  - Backspace at the name start, or any deletion reaching into the markup, removes all of it: the
    heading becomes text and the name stays. Typing inside the markup lands at the name's start.
  - Enter at the name start opens a line above. Enter at the end starts the body. Enter mid-name moves
    the rest into the body.
  - Backspace at the first body line's start, Delete at the heading's end, and Delete at the end of a
    non-blank line just above a heading move the cursor instead of merging lines.
  - Indenting (Tab, Ctrl+]) skips heading lines. Text inserted at a heading's line start goes to
    the name's start, unless it is whole lines inserted above the heading.
  - These rules apply to every change in an edit: several cursors, or an edit that the highlight
    rules split in two. With several cursors, a deletion that would merge a body (or the line above)
    into a heading is dropped.
  - A heading whose name is deleted loses its markup, even by replace-all.
  - The image rules run first, so Delete at a heading's end when the body starts with an image is
    refused by them. Nothing changes, and the cursor doesn't move across. This is accepted.
  - Add, recolour, remove and move are each one undo step.
  - Replace-all never rewrites the markup, in the rendered view or in raw text (§20). Raw text exempts
    the title line, which is never a section.
- **Moving.** Move up/down swaps a section's text (heading to its last non-blank line) with its
  neighbour's. The blank lines between and after them stay where they are. Undo and redo of a move put
  collapsed sections back (`invertedEffects` replays the folds). The text before the first section never moves. The cursor, in either of the two
  swapped sections, and their collapsed state travel with them.
- **Collapsing.**
  - Folds the body (heading end → last non-blank line) behind a "⋯ N lines" button.
  - Kept per note in this browser (`notes.folds.<user>:<note>`, section indexes), never in the file.
    Restored when the note opens and when leaving raw text.
  - Kept by the editor itself (`setDoc(text, ro, key)`): restored when the note opens and when
    leaving raw text, and never overwritten by the reconfigure in between.
  - The cursor or any selection reaching into the body opens it, including a Find match that ends at
    the body's last character. The saved state follows. A cursor at the heading's end leaves it
    collapsed. An edit at a collapsed body's start (Enter at the heading's end) opens it, so the fold
    never drifts off its heading. Removing a collapsed section opens it first. Raw text shows
    everything.
  - Saved by section index. Adding or removing sections elsewhere (raw text, another browser) can shift
    which ones reopen collapsed. This is accepted.
- **Section list.**
  - The note-bar "Sections" button (desktop; shown when there are sections) and the ⋯ menu's "Sections"
    list every section with its colour dot. Picking one puts the cursor on the heading, opening it if
    collapsed, and scrolls it to the top.
  - The palette has "Go to section: …" for each, and "Section options", which opens the menu of the
    section the cursor is in, for keyboard use. The heading's own buttons are mouse/touch only.
  - Section popovers close when another note opens. This is also the screen-reader navigation path:
    CodeMirror lines aren't real headings. Moving onto a heading, by keyboard or click, announces
    "Section, colour, name".
- **Elsewhere.**
  - Copy: other apps get the name (`text/plain`); this app keeps the heading (`text/x-notes-raw`),
    including when just the whole name is selected. Pasted at a line start it is a section again;
    mid-line it pastes as its name.
  - Title, preview, search index and snippets (`core.SearchText`) and footer counts drop the markup and
    keep the name.
  - Export and backups keep the raw text.
  - Highlights and formatting skip headings: a selection that touches one formats the text around it.
- **Phones.** The note-bar section buttons are hidden below 560 px. The ⋯ menu has "Add section" and
  "Sections". Popovers are pinned to the bottom of the screen.

Tests: `internal/core` (fixture, titles/previews/search), `web/src/lib/heading.test.ts` (fixture,
contrast), `web/src/lib/editor/sections.test.ts` (finding, adding, edit rules, moving, collapsing,
clipboard), `web/e2e/sections.mjs` (browser).

## 23. Save as web page

One note becomes **one self-contained `.html` file** with everything related to it: the text as the
editor shows it, its images and screenshots in place, and every attached file (PDF, zip, video, …),
which can be saved back from the page. No size limit. The page needs nothing from the network.

- **Where:** the note's ⋯ menu, a button beside Restore on trashed notes, and the palette ("Save note
  as web page"). The client remembers which note it was for, saves it first and refuses (with a
  message) if it still has unsaved changes, a conflict, a save error or an upload in progress. It asks
  `GET /api/notes/{id}/html?estimate=1` for the size and confirms above 100 MB (about the size, slow
  to open, tunnels may refuse), checks again for unsaved changes and uploads, sends a `HEAD` (so an
  error is shown, never saved as the page), then downloads `GET /api/notes/{id}/html?theme=…`.
- **Server** (`internal/core/htmlpage.go`): `PreparePage` authorizes the note (404 otherwise; trashed
  allowed), refuses with `409 upload_pending` while an upload to the note is running (from any tab;
  an upload quiet for 5 minutes no longer counts), and takes a snapshot first: the text (`note.txt`,
  at most 10 MiB, LF, valid UTF-8, no NUL; any length of line; missing → 404), tags (app order: by
  name), its ready attachments, and for every image the text shows but the note doesn't have (or
  whose file can't be read) the copy in another note, as the editor does. Every file is opened once
  to prove it can be read; one that can't is listed as "missing on the server" (or replaced by
  another note's copy, then not listed twice). Database errors fail the request, never a page that
  quietly lacks something. Size and
  WriteTo walk the same renderer, so the estimate, `Content-Length` and the bytes written are equal by
  construction. Files are read with their snapshot sizes, one open at a time (closed after their
  last block), never following a symlink or waiting on a FIFO; a file swapped for another since the
  snapshot (a different inode) is refused rather than read as a prefix; any read or write failure after the headers resets the connection
  (`http.ErrAbortHandler`), so the browser reports a failed download, never a page that looks
  complete but isn't. The zip export now does the same. `HEAD` answers the headers only.
- **Requests from other sites** are refused for the page and the export: `Sec-Fetch-Site` must be
  absent, `same-origin` or `none` (typed or bookmarked), so another site or a sibling subdomain can't
  make the browser download someone's notes.
- **Rendering** follows the editor: the title line is the first line that isn't an image and has text
  once highlights are stripped (JavaScript whitespace rules), shown in full; a title line shaped like a
  heading is shown as typed and isn't a section. Images above it stay above it. Plain text runs are
  `pre-wrap` blocks (every space, tab and blank line as typed). Highlights use the app's `.hl` / `--h`
  colours (`ParseHighlights`, checked against the shared fixture). Section headings show their name in
  the section colour; the 3px bar runs from the line after the heading to the last line with text (none
  for an empty section); all sections open. Image lines: this note's copy, else another note's; only
  png/jpeg/gif/webp are images (`InlineImageTypes`), anything else is "Not an image · name", none is
  "Image not found · caption". Captions only when there is one. The end mark when the note has text,
  then the word and character counts, then the attachments (thumbnail only for images the text doesn't
  show; flags for "not shown in the note" and "from another note"; size; Save).
- **Bytes:** every file is in the page once. Images up to 2 MB are `data:` URLs at their first
  appearance (they show even with scripts off; repeats are filled in by the script); everything else
  is stored after the script as `<script type="application/octet-stream">` base64 chunks of ≤ 4 MiB,
  decoded one chunk at a time into a Blob the first time it is shown or saved, after which the base64
  text is dropped. A file still costs about its size twice in browser memory (its base64 in the
  page, then its Blob). Save always downloads (`application/octet-stream`, the export's safe file
  name), never opens a file; a file that can't be read says so on its button. Images zoom with a
  click, Enter or Space.
- **Safety:** every piece of note text is HTML-escaped; no note data is ever in a script, style value,
  URL or event handler. One constant script (`internal/pageassets/page.js`); the page's CSP (meta tag,
  and as a header with `sandbox`) is `default-src 'none'; img-src data: blob:; style-src 'unsafe-inline';
  font-src data:; script-src 'sha256-…'; script-src-attr 'none'; base-uri 'none'; form-action 'none'`,
  with the hash computed from the script itself. A test asserts exactly one executable script and no
  event handler attribute. Without scripts the text and small images still show, with a line saying
  saving files needs JavaScript.
- **Look:** the note sits on a card (a standalone page has no app around it); inside, the app's
  colour tokens (`PageTokens`, compared with `web/src/app.css` and `colors.ts` by a Go test, as are
  the title, body, section, bar, highlight, image and caption rules), the app's Inter font (all its subsets, embedded once, ~290 KB; OFL), light or dark
  following the app's theme setting (system → the reader's system). Dates are shown in the reader's
  time zone. Print: light colours always, highlight/chip/bar colours kept, no buttons or counts,
  headings kept with what follows, images not split.
- Not carried into the page: raw text view, monospace font, page width (it is always the rendered
  note in the default column), and folded sections.

## 24. Download note as .tar.gz

One note becomes **one `.tar.gz`** with every file that belongs to it, ready to unpack anywhere
or to bring back with `notes import`. Where "Save as web page" (§23) is for reading, the archive
is for **keeping the files**: originals, under their own names, nothing re-encoded. Reviewed as a
plan by team beta (8 of 8, approve with changes; findings folded in).

**Owner decisions:** the format is `.tar.gz`; trashed notes can't be downloaded; `note.txt.bak`
is never included; the action lives in the ⋯ menu (no toolbar button); no size warning, however
big.

- **What belongs to the note:** its text (`note.txt`, the raw bytes), every **ready** attachment
  (images and other files, shown in the text or not), and, for an image the text shows that this
  note has no readable copy of, another of the same user's notes' copy (the editor's and §23's
  lookup). Not included: `note.txt.bak`, uploads still running (they make the request wait,
  below), stray files without a database row (`doctor` handles those), and anything of another
  user.
- **Where:** the note's ⋯ menu ("Download as .tar.gz", the `archive` icon) and the palette
  ("Download note as .tar.gz", only when the open note isn't in the trash). Trashed notes have no
  ⋯ menu and no button for it, and the server refuses them (`409 note_trashed`, "Restore the note
  to download it.").
- **What's inside** (the full export's layout, §9.3, for one note):
  ```
  notes/<title>/note.txt
  notes/<title>/meta.json
  notes/<title>/files/<name>    every included file, under its original name
  MISSING.txt                   only when something couldn't be included
  manifest.json                 version 2, one note: the importer reads only this
  ```
  - **`manifest.json` is built from what is in the archive**: the export's entry for one note,
    `file`/`folder`/`attachments[].file` equal to the entry paths, RFC 3339 dates, tags with
    colours, pin, `trashed: false`. A borrowed copy is listed under the marker's ref, so import
    gives the note its own copy and the marker keeps working. A file that couldn't be included is
    **not** listed in `attachments` (the importer stops on a listed file it can't open); it goes
    in the note's `missing` list (`ref`, `filename`, `reason`: `unreadable` or `not found`), which
    the importer ignores and the full export never writes.
  - **`meta.json`** is built from the same inventory (not reread from disk), so it never names a
    file the archive doesn't have.
  - **`MISSING.txt`** names, for people, the files missing on the server: an unreadable file, or
    an image the text shows that exists nowhere (by its caption), with its marker, and points to
    Check my notes. An own file replaced by another note's copy isn't listed.
  - Names: `safeName` for the folder, the export's file-name rules (`exportFileName`: Windows-unsafe
    characters → `_`, reserved names prefixed) and case-insensitive de-duplication across own and
    borrowed files (`name (2).png`, `uniqueFileName`, shared with the export), plus a **byte** cap
    (folder ≤ 200 bytes, file ≤ 230, cut on a rune boundary, the extension kept), so an all-emoji
    title still unpacks within 255-byte names. No entry is absolute or has `..`.
  - Tar entries are PAX (UTF-8 names, any size), files `0644`, folders `0755`, times from the note
    and the files; gzip at `BestSpeed` (attachments are mostly compressed already).
- **Download name:** `<title>.tar.gz` (`safeName`, capped like the folder), sent with
  `contentDisposition` (RFC 5987), `Content-Type: application/gzip`, `Cache-Control: no-store`.
- **Server** (`internal/core/notearchive.go`, shared parts in `notefiles.go`):
  - `PrepareNoteArchive(ctx, uid, id)` authorizes (404 otherwise), refuses a trashed note, refuses
    `409 upload_pending` while an upload to the note runs (5-minute quiet rule, as §23), reads
    `note.txt` with `openRegularFile` (no symlink, no FIFO; at most `MaxBodyBytes`, as the importer
    takes; missing → 404), keeps those raw bytes (markers are found in a cleaned copy), and
    resolves the files. Database errors fail the request.
  - **Shared with §23:** `resolveNoteFiles` (own readable / borrowed / own replaced by a copy /
    found nowhere), `NoteFile` (was `PageFile`), `openRegularFile`, `uploadPending`, `noteTags`;
    the page now also reads its text without following a symlink.
  - **Streaming, no `Content-Length`**, like the full export, through `slidingWrites` (a big
    archive outlives the 120 s write deadline). Each file is reopened with `openRegularFile`, must
    be the same inode (`os.SameFile`) and exactly the snapshot size, and is copied with
    `io.CopyN`; anything else, or any read or write failure, resets the connection
    (`http.ErrAbortHandler`), so the browser reports a failed download, never a short archive.
  - The full export's output is unchanged; only `uniqueFileName` and the manifest types are shared.
- **Handler:** `GET /api/notes/{id}/archive` (the mux answers `HEAD` too), behind `requireUser` and
  `crossSite` (only absent, `same-origin` or `none`). `?check=1` does the whole preparation and
  answers `{"missing": n}` or the error, so the client can show the server's message.
- **Client** (one `download` helper in `NoteView.svelte` for the page and the archive, each with its
  own words): remember the note; save first; refuse on unsaved changes, a conflict, a save error or
  an upload in progress ("…the archive includes every file"); `?check=1`; check again; start the
  download through the browser (`startDownload`: streamed to disk, never held in memory). When
  files are missing, a toast: "N files are missing on the server: see MISSING.txt in the archive."
  The 409 upload message is now neutral ("…the download includes every file").
- **Import:** `notes import <user> note.tar.gz` (also `.tgz`) unpacks safely (§9.3) and imports
  as an export: a new note with text, tags, pin, dates and files; markers remapped if needed.
- **Tests:** `internal/core/notearchive_test.go` (entries and names, raw text, borrowed and missing
  files, manifest and meta, import round trip incl. a remapped ref, refusals: other user, trashed,
  upload running, symlinked and FIFO `note.txt`; a file deleted, grown, shrunk, replaced or
  symlinked after the snapshot fails the write; byte caps; a note titled `manifest.json`; unsafe
  `.tar.gz` names on import); `internal/server/archive_test.go` (route, headers, HEAD, `?check=1`,
  404/403/409, mid-stream reset); `web/e2e/archive.mjs` (⋯ menu and palette, unpacked with `tar`,
  MISSING.txt and toast, unsaved change refused, trashed note: no button, no palette entry, 409).
- Not in v1: choosing which files go in, several notes at once (the full export does that),
  in-app import (the CLI does it), a size up front.
