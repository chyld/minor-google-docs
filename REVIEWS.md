# Spec Review — Round 1 (team beta)

Reviewed: `SPEC.md` v1 draft. Result folded into `SPEC.md` v2.

## Verdicts

| # | Reviewer | Model | Verdict |
|---|---|---|---|
| 1 | Claude | `claude-sonnet-5-5` | approve with changes |
| 2 | Codex | `gpt-6.1-sol` (high) | **needs rework** |
| 3 | Grok | `grok-4.7` | approve with changes |
| 4 | Muse Code | `muse-wonder-1.0` | approve with changes |
| 5 | OpenCode | `deepseek-v4.1-flash-free` | approve with changes |
| 6 | OpenCode | `minimax-m3.1-flash-free` (high) | approve with changes |
| 7 | OpenCode | `mimo-v2.6-flash-free` | approve with changes |
| 8 | OpenCode | `glm-5.3-flash-free` | approve with changes |

Stack (Go + SQLite + files + CodeMirror 6) was endorsed by all 8. Nobody challenged the owner requirements.

## Consensus findings (raised by ≥ 5 of 8)

| Finding | Raised by | v2 resolution |
|---|---|---|
| Contentless FTS5 can't do snippets/highlight, can't delete rows easily, can't join TEXT UUIDs, isn't user-scoped | all 8 | Regular FTS5 table with `note_id`/`user_id` UNINDEXED, in a separate disposable `search.sqlite` (§4.3) |
| File↔DB write protocol undefined: crash window, missing dir fsync, fixed tmp name | all 8 | Numbered save protocol + startup reconcile (§5) |
| 409 check is a TOCTOU race; stale DB hash after crash → 409 forever | 7 | Per-note lock + `BEGIN IMMEDIATE`; base hash compared against the **file** (§5) |
| "Copy `data/`" backup is unsafe with live WAL; DB isn't rebuildable (tags, pins, filenames live only there) | 7 | `notes backup` command; honest source-of-truth table (§3.2, §9) |
| tus uploads not bound to user/note; no finalize; no temp GC | 7 | Upload records, owner binding, atomic finalize, 24 h GC (§7) |
| Same-origin serving of user files = XSS risk; drop PDF/text viewers | 7 | Magic-byte image allowlist inline; everything else download + sandbox CSP (§7.4) |
| Trusted-proxy config needed for rate limit + Secure cookie | 7 | `NOTES_TRUSTED_PROXIES` (§8) |
| 4-char short marker id collides; grammar needs strict rules | 7 | 10-char random `ref`, line-anchored grammar (§6) |
| Missing indexes, FK pragma, tag case rules | 6 | Schema v2 (§4) |
| Session rotation, revoke on passwd/rm, dummy Argon2, Argon2 concurrency cap | 7 | §8 |
| Pagination missing | 6 | Cursor pagination (§10) |
| Export / import missing | 6 | `notes export`, `notes import`, `/api/export` (§11) |
| Cut `[file:]` markers | 6 | Cut |
| Disk-full / no-limit protection | 7 | Free-space watermark (no per-file cap) (§7.5) |
| Background janitor for trash purge + temp GC | 5 | In-process janitor + `notes gc` (§9) |
| Title derivation must skip marker lines | 5 | §5.4 |

## Split decisions (alpha's call)

| Question | Positions | Decision |
|---|---|---|
| tus vs plain streaming POST | Keep tus: Sonnet, Codex, Muse, DeepSeek, MiniMax, GLM · Cut tus: Grok, MiMo | **Keep a hand-rolled minimal tus subset** (not `tusd`). Reverse proxies / Cloudflare tunnels cap single requests (~100 MB); chunking is the only way "no size limit" survives them. Small pastes use `creation-with-upload` = one request. |
| `tusd` library vs hand-rolled | Hand-roll: MiniMax, GLM, Sonnet (conditional) | Hand-roll (~300 lines); needed for owner binding, client-chosen ids, direct finalize into `data/users/…`. |
| SvelteKit vs plain Svelte + Vite | Plain: Sonnet, Codex, GLM, MiniMax · Keep: DeepSeek, MiMo | **Svelte 5 + Vite**, tiny router. No SSR is used. |
| FTS in main DB vs separate file | Separate: MiniMax · Main: Grok, DeepSeek | **Separate `search.sqlite`** — keeps "DB is metadata" literally true; safe to delete. |
| Marker id format | Full UUID: Grok · 8–12 chars: others | 10-char base32 random `ref`, DB-unique per user. Readable and collision-safe. |
| Command palette | Cut: Muse, MiMo · Keep: Sonnet, DeepSeek, MiniMax | Keep, scoped to note titles + tags + 5 actions. |
| Container base | Alpine + PUID/PGID: Grok, DeepSeek · distroless: none firmly | **Alpine** + entrypoint chown + `su-exec`; multi-arch amd64/arm64. |

## Rejected / corrected suggestions

- **"Sessions are forgeable without a server secret"** (DeepSeek) — not with 256-bit random tokens stored as SHA-256 hashes. No secret needed.
- **"DB is rebuildable from files"** (MiMo) — incorrect; tags, pins, attachment names, users live only in SQLite. Spec now says so explicitly.
- **"Lock down max upload size"** (MiMo, MiniMax `NOTES_MAX_UPLOAD_BYTES`) — conflicts with owner requirement 8. Replaced by a free-space watermark.
- **Separate media origin** (MiniMax option a) — too much homelab setup; sandbox CSP + download-only for non-images achieves the same.
- **Persist rate-limit counters in SQLite** (MiniMax) — overkill for v1; documented as in-memory.
- **PATCH-diff saves** (MiniMax) — deferred; 10 MiB body cap + debounce is enough.
