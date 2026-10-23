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

```
data/
├── db/
│   ├── notes.sqlite            # metadata (+ -wal, -shm)
│   └── search.sqlite           # FTS index — delete anytime, `notes reindex` rebuilds
├── tmp/
│   └── <uid>/<upload-id>       # in-progress uploads, per user, GC after 24 h
├── backups/                    # default target of `notes backup`
├── lost+found/                 # where `doctor --fix` quarantines orphans
└── users/
    └── <user-uuid>/
        ├── notes/
        │   ├── <note-uuid>.txt       # body — UTF-8, LF line endings
        │   └── <note-uuid>.txt.bak   # previous version (one generation)
        └── files/
            └── <attachment-uuid>     # raw bytes
```

- IDs: UUIDv7. No path is ever built from user input.
- Permissions: dirs `0700`, files `0600`, owned by the container UID.
- `tmp/` and `users/` are on the same filesystem (required for atomic rename; checked at startup).

### 3.2 What is authoritative

| Data | Authority | If lost |
|---|---|---|
| Note body | `.txt` file | Gone (restore from backup) |
| Attachment bytes | `files/<id>` | Gone |
| Title, size, sha256 of a note | Derived from the `.txt` | Recomputed on startup reconcile |
| Users, sessions, tags, tag colors, note↔tag, pinned, trash state, attachment filename/mime/ownership | **`notes.sqlite`** | **Not rebuildable** — back up the DB |
| Search index | Derived | `notes reindex` |

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

CREATE TABLE attachments(
  id TEXT PRIMARY KEY,                      -- client-generated UUIDv7, validated
  ref TEXT NOT NULL,                        -- 10-char base32 marker id (§6)
  user_id TEXT NOT NULL,
  note_id TEXT NOT NULL,
  filename TEXT NOT NULL,                   -- original name, display only
  mime TEXT NOT NULL,                       -- server-sniffed
  size_bytes INTEGER NOT NULL,
  sha256 TEXT,                              -- integrity, computed on finalize
  state TEXT NOT NULL,                      -- 'uploading' | 'ready' | 'deleting'
  created_at INTEGER NOT NULL,
  UNIQUE(user_id, ref),
  FOREIGN KEY(note_id, user_id) REFERENCES notes(id, user_id) ON DELETE CASCADE);
CREATE INDEX attachments_note ON attachments(note_id);

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
   `{current_body, current_sha256}`.
4. Copy current file to `<id>.txt.bak` (one generation).
5. Write `<id>.<random>.tmp` in the same `notes/` dir → fsync → rename to `<id>.txt` → fsync dir.
6. `BEGIN IMMEDIATE` → update `title, size_bytes, content_sha256, updated_at` → `COMMIT`.
7. Release lock. Respond `200 {sha256, updated_at, title}`.
8. Update `search.sqlite` (best effort, async).

### 5.2 Crash recovery (startup reconcile, also `notes doctor`)

For each note row: if file hash ≠ `content_sha256` → **file wins**, recompute derived fields.
For each `indexed` row: if sha ≠ note's sha → re-index. Remove stray `*.tmp` files.
Runs at startup, takes seconds at homelab scale.

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

- `ref` = 10 chars of random base32 (50 bits), unique per user (DB constraint). Generated
  client-side together with the attachment UUID; server rejects collisions (client retries).
- The caption after `]` is cosmetic; resolution is **by `ref` only**, scoped to the session user
  (copying a marker into another of your notes works).
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

fsync temp file → sniff mime from magic bytes → rename into `users/<uid>/files/<id>` →
fsync dir → `UPDATE … SET state='ready', mime WHERE state='uploading'`.
The client sees success only after the row is `ready`. Idempotent if retried: if the rename
landed but the row update didn't, `HEAD` (or an empty `PATCH` at the full offset) completes it.
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
- Trash note: attachments stay with the note; restore brings them back.
- Purge note: in one tx mark note + attachments deleting → unlink files → delete rows.
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
| `notes export <user> [dest]` | Zip: `notes/<title>.txt`, `attachments/<original name>`, `manifest.json` (tags, pins, dates, refs) |
| `notes import <user> <dir>` | Create notes from a folder of `.txt` files |
| `notes reindex` | Rebuild `search.sqlite` from `.txt` files |
| `notes doctor [--fix]` | Report DB↔disk mismatches, orphans, stray tmp, old user dirs. `--fix` is explicit; orphans move to `lost+found/`, never deleted. |
| `notes gc` | Run janitor once (trash purge, upload GC, session sweep) |
| `notes healthcheck` | Exit 0 if `/healthz` OK (for Docker) |

- **Janitor** (in `serve`, hourly): purge notes trashed > `NOTES_TRASH_DAYS` (default 30, `0` = never),
  GC stale uploads, sweep sessions, retry `deleting` rows. Every purge logged + audited.
- `/healthz`: no version/user info. Checks that the server can answer (`SELECT 1` on the
  metadata DB, no write lock) and that `data/tmp` is writable (a tiny temp file is created and
  removed) — a read-only or full volume makes it 503, which is the point: the app can't save.
- Restore: stop, put `data/` back (or untar backup), delete `search.sqlite`, start (reindexes).

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
GET    /api/notes?q=&tag=&untagged=&pinned=&trash=&has_attachments=&sort=&limit=&cursor=
         (q present → FTS results with escaped highlight snippets)
POST   /api/notes                          → {id, sha256}  (empty body)
GET    /api/notes/{id}                     → meta + body + sha256 + tags + attachments
PUT    /api/notes/{id}/body                {body, base_sha256} → 200 | 409
PATCH  /api/notes/{id}                     {pinned?, tag_ids?, trashed?}
POST   /api/notes/{id}/duplicate           (copies body; markers still resolve — same user)
DELETE /api/notes/{id}                     permanent; only when trashed
POST   /api/trash/empty
GET    /api/tags                           → with counts
POST   /api/tags   PATCH /api/tags/{id}   DELETE /api/tags/{id}
POST|HEAD|PATCH|DELETE /api/uploads/…     tus
GET    /api/attachments/{id}               download/inline (§7.4)
PATCH  /api/attachments/{id}               {filename}
DELETE /api/attachments/{id}
GET    /api/export                         zip of the current user's data
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
  Ctrl+\ sidebar rail, Ctrl+Shift+\ notes list, Ctrl+Shift+F focus mode, Ctrl+Shift+H highlight, Ctrl+/ raw text, `/` search, `?` cheat sheet (outside
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
`==\{(<name>)\}([^\n]+?)==` with names longest-first. 24 names: the 12 tag hues (`coral` … `pink`),
each plain (light tint) and `-deep` (strong). Unknown names, `a == b`, unterminated or multi-line text
stay plain. Image lines are never highlights. No escapes. The TS parser is hand-written and linear
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
  Image lines, blank lines and read-only notes are skipped. Afterwards the coloured text is selected.
- **UI.** Note-bar highlighter button (enabled with a selection or a cursor in a highlight) opens a
  12×2 grid (6 columns on phones, pinned to the bottom); "Remove highlight" first when inside one.
  Desktop also gets a small bar above a selection (fixed position, kept inside the note's scroll area,
  below the selection near the top; its menu kept inside the window): last colour, colours, remove.
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
`web/e2e/highlights.mjs` (browser).

## 20. Raw text view

A note-bar button (`</>`, Ctrl+/, also in the command palette) switches the editor between the rendered
note and its raw text: the stored file exactly, in the monospace font: `[image:…]` lines, `=={name}…==`
delimiters and the title line are plain, editable text.

- **One editor.** The rendering extensions (`markers`, `highlights`, `imageEditing`) sit in a CodeMirror
  compartment that is emptied in raw mode, so switching keeps the text, cursor and undo history. Raw
  edits are saved like any other; switching back re-runs the rendered view's selection rules (a cursor
  inside image text selects the image or moves off it; none rests in a hidden delimiter).
- **No rules in raw.** Nothing reshapes edits there: delimiters can be typed or deleted one character at
  a time, and copy/paste carry the text as-is. A broken highlight or marker is just text again in the
  rendered view.
- **No colour tools.** The highlight button, floating bar and Ctrl+Shift+H do nothing in raw mode.
  Pasted or dropped images still upload and insert their marker line.
- **Remembered.** The choice applies to every note and is kept in `localStorage` (`notes.raw`).
  Works on trashed (read-only) notes too, for viewing.

Tests: `web/e2e/raw.mjs` (browser).
