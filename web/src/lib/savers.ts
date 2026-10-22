// Keeps one NoteSaver per note alive while it has unsaved work, so switching
// away (and back) never drops or duplicates a save.
import { api } from './api';
import { drafts } from './drafts';
import { NoteSaver } from './saver';
import type { SaverDeps } from './saver';

const savers = new Map<string, NoteSaver>();

export const saverDeps: SaverDeps = {
  save: (id, body, base, keepalive) => api.saveBody(id, body, base, keepalive),
  putDraft: (key, body, base) => void drafts.put({ key, body, base_sha256: base, at: Date.now() }),
  deleteDraft: (key) => void drafts.delete(key),
};

export function existingSaver(noteId: string): NoteSaver | undefined {
  return savers.get(noteId);
}

export function createSaver(noteId: string, key: string, body: string, sha: string): NoteSaver {
  savers.get(noteId)?.dispose();
  const s = new NoteSaver(noteId, key, body, sha, saverDeps);
  savers.set(noteId, s);
  return s;
}

/** The editor moved on from this note: finish its saves in the background. */
export function releaseSaver(s: NoteSaver, onError: () => void) {
  s.onChange = () => {};
  s.flush()
    .catch(() => {})
    .finally(() => {
      if (s.conflict || s.status === 'error') onError();
      if (!s.dirty && !s.conflict && !s.busy && savers.get(s.noteId) === s) {
        s.dispose();
        savers.delete(s.noteId);
      }
    });
}

export function forgetSaver(noteId: string) {
  savers.get(noteId)?.dispose();
  savers.delete(noteId);
}

export function anyUnsaved(): boolean {
  return [...savers.values()].some((s) => s.dirty || s.conflict);
}

export function unloadAll() {
  for (const s of savers.values()) s.unload();
}

export function flushAll(): Promise<void> {
  return Promise.all([...savers.values()].map((s) => s.flush())).then(() => {});
}
