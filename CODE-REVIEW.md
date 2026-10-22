# Code Review — Round 1 (team beta)

All 8 reviewers: **ship after fixes**. Alpha checked every finding against the code before
acting on it. Status: ✅ fixed · 📝 spec/docs updated instead · ⏭ deferred.

| Reviewer | Focus | Findings |
|---|---|---|
| Claude Sonnet 5.5 | data consistency | 8 |
| GPT-6.1-Sol (high) | security | 6 |
| Grok 4.7 | uploads | 7 |
| Muse Wonder 1.0 | editor / autosave | 6 |
| DeepSeek V4.1 Flash | SQL / search | 7 |
| MiniMax M3.1 Flash (high) | Docker / CLI / ops | 10 |
| MiMo V2.6 Flash | UI / UX / a11y | 17 |
| GLM-5.3 Flash | spec compliance | 10 |

## Data & save protocol
| # | Finding | By | Status |
|---|---|---|---|
| D1 | Save not idempotent: retry after lost response → false 409; post-rename DB update cancellable | Sonnet | ✅ same-content save = success (repairs row); DB write uses `WithoutCancel` |
| D2 | Duplicate copies markers but not attachments → purge of original breaks copy | Sonnet | ✅ duplicate hard-links/copies attachments with new ids/refs and rewrites markers |
| D3 | `doctor --fix` races running server; ignores `.bak`; message says "never deletes" | Sonnet, MiniMax | ✅ skips files <10 min old, re-checks DB before move, restores from `.bak`, honest message |
| D4 | Reconcile deletes in-flight `*.tmp` | Sonnet | ✅ only `*.tmp` older than 10 min |
| D5 | Restore/purge not serialized; crash mid-purge leaves empty trashed ghost | Sonnet | ✅ trash/restore take the note lock; reconcile finishes purges of trashed notes with no file |
| D6 | Index update can store stale title | DeepSeek | ✅ indexer takes the note lock |
| D7 | Purge leaves FTS rows if unindex fails; never pruned | DeepSeek | ✅ unindex in one tx; reconcile prunes FTS orphans |
| D8 | Hourly janitor re-hashes every note (+ twice at startup) | DeepSeek | ✅ hourly janitor no longer reconciles; startup + `notes gc` do |

## Uploads
| # | Finding | By | Status |
|---|---|---|---|
| U1 | Cancel of single-request (≤8 MiB) upload doesn't DELETE; marker stays | Grok | ✅ client always DELETEs (with retry) and removes the marker line |
| U2 | Lost/failed create treated as ref collision → re-upload, orphan | Grok | ✅ create is idempotent for same id; client HEADs before minting a new ref |
| U3 | Finalize not retryable after rename (row stuck `uploading`) | Sonnet, Grok | ✅ HEAD/PATCH complete a half-finalized upload; client sends empty PATCH at full offset |
| U4 | GC can delete an upload that just completed | Grok | ✅ GC re-checks state & age under the lock; appends bump `updated_at` |
| U5 | Purge vs concurrent finalize can orphan a file | Grok | ✅ purge/delete take the attachment lock; finalize only marks rows still `uploading` |
| U6 | Free-space reserve checked once; one body can fill the disk | GPT, Grok | ✅ declared remainder checked up front; copy stops at the reserve (507, offset kept) |
| U7 | HEAD omits `Upload-Metadata` | Grok | ✅ echoed |
| U8 | sha256 computed after "ready", not before | GLM | ✅ kept async (hashing multi-GB before replying would stall clients/proxies); spec §7.3 updated |

## Security
| # | Finding | By | Status |
|---|---|---|---|
| S1 | Login racing a password reset can still mint a session | GPT | ✅ session insert is conditional on the verified hash + not disabled |
| S2 | Whitespace variants bypass per-username backoff | GPT | ✅ one canonical username everywhere |
| S3 | HTTPS accepts unprefixed (injectable) cookie | GPT | ✅ cookie name chosen by `isSecure` |
| S4 | Transfers have no inactivity deadline | GPT | ✅ sliding 2-min read/write deadlines refreshed on progress |
| S5 | Attachments cached a day, survive logout on shared profile | GPT | ✅ `private, no-cache` (revalidates through auth; 304 when unchanged) |
| S6 | CSP strings broader than spec | GLM | ✅ trimmed to spec |
| S7 | Attachment ids not required to be UUIDv7 | GLM | ✅ |

## Search & SQL
| # | Finding | By | Status |
|---|---|---|---|
| Q1 | Search ignores limit/cursor/sort, caps at 100 | DeepSeek | ✅ search hits go through the normal paginated/sorted list query |
| Q2 | Snippets read whole files (≤10 MiB each) per result | DeepSeek | ✅ snippets read at most 256 KiB |
| Q3 | Corrupt `search.sqlite` blocks startup | DeepSeek | ✅ moved aside and rebuilt automatically |
| Q4 | Malformed cursor → 500 | DeepSeek | ✅ validated → 400 |

## Ops / Docker / CLI
| # | Finding | By | Status |
|---|---|---|---|
| O1 | Restored (root-owned) files never re-owned → crash loop | MiniMax | ✅ entrypoint re-owns any file not owned by PUID |
| O2 | README restore recipe fails / merges / keeps stale WAL | MiniMax | ✅ rewritten |
| O3 | export → import not a round trip | MiniMax | ✅ `import` accepts an export zip/folder (tags, pins, dates, attachments, markers) or plain `.txt` folder |
| O4 | Relative paths fail (read-only `/`) | MiniMax | ✅ `WORKDIR /data` |
| O5 | Same-second backups collide | MiniMax | ✅ unique names |
| O6 | Caps comment wrong; no stop grace period | MiniMax, GLM | ✅ comment/spec corrected; `stop_grace_period: 30s` |
| O7 | Healthcheck doesn't touch the DB | MiniMax | ✅ `/healthz` pings SQLite |
| O8 | Partial import gives no count | MiniMax | ✅ |
| O9 | `export` dest should be optional | GLM | ✅ defaults to `data/backups/` |

## Editor / client save
| # | Finding | By | Status |
|---|---|---|---|
| E1 | Typing during conflict not drafted → lost on switch/close | Muse | ✅ drafts always written; flush always drafts |
| E2 | One global `saving` flag drops the next note's autosave | Muse | ✅ per-note save queue (`lib/saver.ts`) |
| E3 | Switching mid-save fires duplicate save → self-409 | Muse, Sonnet | ✅ flush chains onto the in-flight save |
| E4 | Trash/Duplicate don't await the pending save | Muse | ✅ awaited |
| E5 | Keystrokes during load overwritten | Muse | ✅ draft resolved before the doc is shown; editor read-only while loading |
| E6 | Full-doc rescans per keystroke | Muse | ✅ decorations mapped unless marker/title lines change; counts debounced |
| E7 | Title cut on rune not grapheme boundary | GLM | ✅ never splits combining marks / ZWJ / variation selectors |

## UI / UX / a11y
| # | Finding | By | Status |
|---|---|---|---|
| A1 | Escape doesn't close Account/Shortcuts until focused | MiMo | ✅ |
| A2 | `c` creates notes from anywhere (behind modals) | MiMo, GLM | ✅ only when the note list has focus |
| A3 | No focus trap / background not inert | MiMo | ✅ |
| A4 | Menus: no Escape, no focus | MiMo | ✅ |
| A5–A6, A15 | Contrast: dark search marks, light `--muted`, gradient text | MiMo | ✅ |
| A7 | Mobile save status color-only, not announced | MiMo | ✅ |
| A8 | Closed mobile drawer still focusable | MiMo | ✅ |
| A9 | Tag/search inputs <16px on mobile (iOS zoom) | MiMo | ✅ |
| A10 | Touch targets <40px | MiMo | ✅ |
| A11 | Combobox ARIA missing | MiMo | ✅ |
| A12 | Palette active index stale | MiMo | ✅ |
| A13 | Trash filter breaks sort-menu scrim | MiMo | ✅ |
| A14 | `?` lacks preventDefault; Ctrl+\ should toggle sidebar; global Ctrl+S | MiMo, GLM | ✅ |
| A16 | Mobile: no FAB, attachments not bottom sheet, counts hidden | MiMo, GLM | ✅ |
| A17 | Tray/progress lack roles | MiMo | ✅ |
| A18 | No list virtualization / hover actions | GLM | ✅ `content-visibility` + hover pin/trash |

## Tests
| # | Finding | By | Status |
|---|---|---|---|
| T1 | No Vitest for save queue / drafts | GLM | ✅ `saver.test.ts` |
| T2 | No Playwright E2E | GLM | ⏸ deferred — flows verified by hand in Chromium (palette, menus, shortcuts, paste, cancel, mobile); Go + vitest suites cover the protocols |

---

# Code Review — Round 2 (team beta re-reviews the round-1 fixes)

Each reviewer re-checked their round-1 findings against the fixed code and hunted for
regressions. All verdicts: **ship after fixes**. Alpha verified each new finding in the
code before fixing it. Codex's first attempt was refused by its provider's safety filter and
re-run as a correctness review; MiniMax's first run returned no report and was re-run.

## Round-1 fixes that turned out partial
| # | What was missing | By | Status |
|---|---|---|---|
| D3 | `doctor --fix` could recreate a note mid-purge; `.bak` restore didn't refresh metadata | Sonnet | ✅ trashed notes are left to reconcile; restore re-checks the row and updates title/size/hash/index |
| D7 | `unindex` ignored per-statement errors | Sonnet | ✅ errors roll back and are logged |
| E4 | Duplicate didn't check the save result | Muse, Sonnet | ✅ refuses while unsaved/conflicted |
| E7 | Titles could split flag emoji / Hangul jamo | Muse | ✅ regional-indicator pairs and jamo/Mc handled |
| U1 | Cancel only edited the note on screen | Grok | ✅ markers removed in any note (open editor, background saver, or saved copy) |
| U3 | Recovery HEAD raced the final PATCH and could delete a finished file | Grok, Codex | ✅ HEAD/create take the attachment lock; `markReady` treats "already ready" as success |
| A7, A10, A17 | Mobile status still color-only; tiny buttons <40 px; panel progress bars unlabeled | MiMo | ✅ status word shown; 40 px hit areas; `role=progressbar` |
| S4 | Rejected requests had no deadline | Codex | ✅ deadlines set before CSRF/auth; accepted transfers slide |
| Q1 | Search silently capped at 1000 candidates; re-saved notes fell off | DeepSeek | ✅ no cap (ids passed via `json_each`), paged in one query |

## New findings
| # | Finding | By | Status |
|---|---|---|---|
| R1 | Password change could undo an admin reset / leave a session after revocation | Codex | ✅ one transaction conditioned on old hash + live session; new session issued inside it |
| R2 | Concurrent uploads each passed the free-space check against the same bytes | Codex, Grok | ✅ process-wide leases + 64 MiB re-checks for all uploads |
| R3 | Import read whole files into memory (zip bombs); folder import followed symlinks out | Codex | ✅ reads capped (10 MiB note, 64 MiB manifest) on actual bytes; `os.OpenRoot`; regular files only |
| R4 | Malformed PHC (`t=0`) could panic and pin the Argon2 semaphore | Codex | ✅ parameter/salt/key bounds; release deferred |
| R5 | Origin check ignored scheme | Codex | ✅ https required when the request is secure |
| R6 | Stale draft resurrected deleted text | Sonnet | ✅ draft deleted whenever text equals the saved copy |
| R7 | "Stay" on the leave-page prompt caused a false conflict | Sonnet | ✅ beforeunload only drafts; keepalive save moved to `pagehide` |
| R8 | Re-opened note's saver could be disposed by an earlier release; stale text on reopen | Sonnet, Muse | ✅ release/claim tracking; load waits for the in-flight save |
| R9 | Opening a trashed note deleted its draft | Muse | ✅ kept (offered again after restore) |
| R10 | Duplicate marker lines survived cancel | Muse | ✅ all lines for the ref removed / rewritten |
| R11 | NUL in a search matched every note | DeepSeek | ✅ rejected (also invalid UTF-8, >1000 bytes) |
| R12 | No snippet for matches past 256 KiB | DeepSeek | ✅ chunked scan with overlap |
| R13 | Missed index updates never retried; unbounded goroutines when queue full | DeepSeek | ✅ hourly `IndexSweep` (hash compare, no file reads); queue drops instead of spawning |
| R14 | Any search-DB open error treated as corruption; quarantined copies piled up | DeepSeek | ✅ only real damage quarantined, logged, newest 2 kept |
| R15 | Export aborted mid-zip if a note/file was deleted meanwhile | Sonnet | ✅ skipped |
| R16 | Reconcile could un-index a just-created note; finished "purges" on a mis-mounted volume | Sonnet | ✅ index snapshot first + re-check; missing notes folder → skip |
| R17 | IME: Enter/Escape during composition triggered actions | MiMo | ✅ `isComposing` guards everywhere |
| R18 | Mobile drawer: no Escape, content behind focusable | MiMo | ✅ Escape closes; list/editor inert; focus moves in and back |
| R19 | Ctrl+K / Alt+N acted behind open dialogs; stacked dialogs fought over keys | MiMo | ✅ guarded; dialog stack — only the top one handles keys |
| R20 | Account dialog opened with focus on `<body>` | MiMo | ✅ focuses the password field |
| R21 | White text on the light end of the brand gradient < AA | MiMo | ✅ deeper gradient for text-bearing surfaces |
| R22 | Upload flow had no automated tests (client or HTTP) | GLM | ✅ `tus_test.go` (HTTP) + `uploads.test.ts` (client, scripted fake server) |
| R23 | Cache-Control not asserted; stale comments; SPEC §12/§13 drift | GLM | ✅ |
| R24 | Two exports in the same second collided | MiniMax | ✅ unique `Stamp()` shared with backups |
| R25 | Restore recipe failed for a host user ≠ PUID (data is 0700) | MiniMax | 📝 README uses `sudo` and says why |
| R26 | Notes created by CLI commands (welcome note) weren't indexed while the server ran | MiniMax | ✅ CLI indexes synchronously (`SyncIndex`) |
| R27 | SPEC said `/healthz` doesn't touch the DB/disk | MiniMax | 📝 SPEC §9 describes the real check |
| R28 | `notes gc` reported "not found" when the server purged the same note first | MiniMax | ✅ treated as done; README notes on running gc/doctor live |
| R29 | `user rm --purge-data` confirmation failed for names typed with spaces; TTY need undocumented | MiniMax | ✅ normalized compare; README says `exec -it` |
| R30 | Interrupted backups left `.partial` files; backups only on the same disk | MiniMax | ✅ stale partials pruned; 📝 README cron recipe for off-box backups |

Round-2 checks: `go vet`, `go test` (core + server, incl. 9 new core tests and the tus HTTP
suite), `svelte-check` 0 errors, Vitest 15/15. Re-tested in Chromium on the deployed container:
password change round trip, dialog focus/stacking, IME guards, cancel while the note is
not open, mobile drawer. CLI: `user add` against a running server → welcome note found by
search; two exports in the same second → two files.
