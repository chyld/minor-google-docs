// Client side of the save protocol (SPEC §5.3), one NoteSaver per note:
// - at most one save in flight per note; each save's base is the previous result's sha
// - every edit is mirrored to a draft (IndexedDB) — including while a conflict is open
// - flush() waits for the in-flight save and then saves anything newer
// - a 409 pauses autosave until the user resolves it; network errors retry with backoff
import type { SaveResult } from './types';

export type SaveStatus = 'saved' | 'dirty' | 'saving' | 'error' | 'conflict';

export interface Conflict {
  body: string;
  sha: string;
  reason: 'conflict' | 'draft';
}

export interface SaverDeps {
  save(noteId: string, body: string, base: string, keepalive?: boolean): Promise<SaveResult>;
  putDraft(key: string, body: string, base: string): void;
  deleteDraft(key: string): void;
}

export interface SaverState {
  status: SaveStatus;
  detail: string;
  conflict: Conflict | null;
}

type HttpError = Error & { status?: number; code?: string; details?: any };

export class NoteSaver {
  baseSha: string;
  savedBody: string;
  pending: string;
  status: SaveStatus = 'saved';
  detail = '';
  conflict: Conflict | null = null;

  onChange: (s: SaverState) => void = () => {};
  onSaved: (r: SaveResult) => void = () => {};

  private inflight: Promise<void> | null = null;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private draftTimer: ReturnType<typeof setTimeout> | undefined;
  private retryDelay = 2000;
  private disposed = false;

  constructor(
    readonly noteId: string,
    readonly draftKey: string,
    body: string,
    sha: string,
    private deps: SaverDeps,
    readonly autosaveMs = 800,
  ) {
    this.baseSha = sha;
    this.savedBody = body;
    this.pending = body;
  }

  get dirty() {
    return this.pending !== this.savedBody;
  }

  get busy() {
    return this.inflight !== null || this.timer !== undefined;
  }

  private emit() {
    this.onChange({ status: this.status, detail: this.detail, conflict: this.conflict });
  }

  /** The user typed: remember it locally right away and schedule a save. */
  edit(body: string) {
    this.pending = body;
    clearTimeout(this.draftTimer);
    this.draftTimer = setTimeout(() => this.writeDraft(), 250);
    if (this.conflict) return; // keep drafting; saving waits for the user's decision
    this.status = 'dirty';
    this.emit();
    this.schedule(this.autosaveMs);
  }

  /** Mirror unsaved text to the draft store — and drop the draft once there's nothing unsaved
   * (e.g. typed then deleted before autosave), so a stale draft can't come back later. */
  writeDraft() {
    clearTimeout(this.draftTimer);
    this.draftTimer = undefined;
    if (this.dirty || this.conflict) this.deps.putDraft(this.draftKey, this.pending, this.baseSha);
    else this.deps.deleteDraft(this.draftKey);
  }

  schedule(ms: number) {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.timer = undefined;
      void this.flush();
    }, ms);
  }

  /** Save now (after any in-flight save). Resolves when everything typed so far is saved or failed. */
  async flush(): Promise<void> {
    clearTimeout(this.timer);
    this.timer = undefined;
    while (this.inflight) await this.inflight;
    if (this.disposed && !this.dirty) return;
    if (this.conflict || !this.dirty) {
      if (!this.conflict) {
        clearTimeout(this.draftTimer);
        this.draftTimer = undefined;
        this.deps.deleteDraft(this.draftKey);
        if (this.status !== 'saved') {
          this.status = 'saved';
          this.emit();
        }
      }
      return;
    }
    this.inflight = this.saveOnce();
    try {
      await this.inflight;
    } finally {
      this.inflight = null;
    }
    if (this.dirty && !this.conflict && this.status !== 'error') await this.flush();
  }

  private async saveOnce() {
    const body = this.pending;
    this.status = 'saving';
    this.emit();
    try {
      const r = await this.deps.save(this.noteId, body, this.baseSha);
      this.baseSha = r.sha256;
      this.savedBody = body;
      this.retryDelay = 2000;
      this.detail = '';
      this.onSaved(r);
      if (this.pending === body) {
        clearTimeout(this.draftTimer);
        this.draftTimer = undefined;
        this.deps.deleteDraft(this.draftKey);
        this.status = 'saved';
      } else {
        this.status = 'dirty';
      }
    } catch (e) {
      const err = e as HttpError;
      if (err.status === 409 && err.code === 'conflict') {
        this.conflict = { body: err.details.current_body, sha: err.details.current_sha256, reason: 'conflict' };
        this.status = 'conflict';
        this.writeDraft();
      } else if (err.status === 400 || err.status === 413 || err.status === 404) {
        this.status = 'error';
        this.detail = err.message;
        this.writeDraft();
      } else {
        this.status = 'error';
        this.detail = 'Offline — retrying. Your text is safe in this browser.';
        this.writeDraft();
        if (!this.disposed) {
          const delay = this.retryDelay;
          this.retryDelay = Math.min(this.retryDelay * 2, 30000);
          setTimeout(() => {
            if (this.status === 'error' && !this.disposed) {
              this.status = 'dirty';
              void this.flush();
            }
          }, delay);
        }
      }
    }
    this.emit();
  }

  /** Show a conflict without a failed save (draft newer than a changed server copy). */
  setConflict(c: Conflict) {
    this.conflict = c;
    this.status = 'conflict';
    this.emit();
  }

  /** Overwrite the other version with ours. */
  keepMine() {
    if (!this.conflict) return Promise.resolve();
    this.baseSha = this.conflict.sha;
    this.savedBody = this.conflict.body;
    this.conflict = null;
    this.status = 'dirty';
    this.emit();
    return this.flush();
  }

  /** Take the other version; returns its text for the editor. */
  useTheirs(): string {
    const c = this.conflict!;
    this.conflict = null;
    this.baseSha = c.sha;
    this.savedBody = c.body;
    this.pending = c.body;
    clearTimeout(this.draftTimer);
    this.deps.deleteDraft(this.draftKey);
    this.status = 'saved';
    this.emit();
    return c.body;
  }

  /** Best effort when the page is really going away (pagehide): keepalive save + draft.
   * Not on beforeunload — the user may still choose to stay, and a save we don't
   * hear back from would leave our base sha stale (→ a false conflict). */
  unload() {
    this.writeDraft();
    if (this.dirty && !this.conflict && !this.inflight) {
      this.deps.save(this.noteId, this.pending, this.baseSha, true).catch(() => {});
    }
  }

  dispose() {
    this.disposed = true;
    clearTimeout(this.timer);
    if (this.dirty || this.conflict) this.writeDraft();
    else clearTimeout(this.draftTimer);
  }
}
