// Keeps one NoteSaver per note alive while it has unsaved work, so switching
// away (and back) never drops or duplicates a save.
import { api } from './api';
import { drafts } from './drafts';
import { NoteSaver } from './saver';
import { stripMarker, swapRef } from './ids';
import { uploads } from './uploads.svelte';
import type { SaverDeps } from './saver';

const savers = new Map<string, NoteSaver>();
const released = new WeakSet<NoteSaver>();

export const saverDeps: SaverDeps = {
  save: (id, body, base, keepalive) => api.saveBody(id, body, base, keepalive),
  putDraft: (key, body, base) => void drafts.put({ key, body, base_sha256: base, at: Date.now() }),
  deleteDraft: (key) => void drafts.delete(key),
};

export function existingSaver(noteId: string): NoteSaver | undefined {
  return savers.get(noteId);
}

/** The editor is using this saver (again): it must not be disposed by an earlier release. */
export function claimSaver(s: NoteSaver) {
  released.delete(s);
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
  released.add(s);
  s.flush()
    .catch(() => {})
    .finally(() => {
      if (!released.has(s)) return; // re-opened meanwhile: the editor owns it again
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

export function draftAll() {
  for (const s of savers.values()) s.writeDraft();
}

/** The open editor registers here so marker changes for its note apply to the
 * screen; other notes are changed through their saver or on the server. */
interface LiveEditor {
  noteId: string;
  removeMarker(ref: string): void;
  replaceRef(oldRef: string, newRef: string): void;
}
let liveEditor: LiveEditor | null = null;

export function setLiveEditor(e: LiveEditor | null) {
  liveEditor = e;
}

async function rewriteNote(noteId: string, fn: (body: string) => string, live: (e: LiveEditor) => void) {
  if (liveEditor?.noteId === noteId) return live(liveEditor);
  const s = savers.get(noteId);
  if (s) {
    const next = fn(s.pending);
    if (next !== s.pending) {
      s.edit(next);
      void s.flush();
    }
    return;
  }
  // Not open and nothing pending: edit the saved copy. A 409 means it changed
  // elsewhere; that copy wins and the marker just shows "image not found".
  try {
    const n = await api.getNote(noteId);
    const next = fn(n.body);
    if (next !== n.body) await api.saveBody(noteId, next, n.sha256);
  } catch {
    /* best effort */
  }
}

// Refs canceled (null) or re-minted (new ref) this session. A trashed note can't be saved, so
// its markers are fixed up when it is restored (fixRetiredRefs).
const retiredRefs = new Map<string, string | null>();

/** Apply this session's canceled/re-minted refs to a body. */
export function fixRetiredRefs(body: string): string {
  for (const [ref, next] of retiredRefs) body = next ? swapRef(body, ref, next) : stripMarker(body, ref);
  return body;
}

// A canceled upload's marker goes away; a re-minted ref is rewritten — in
// whichever note it was pasted into, open or not.
uploads.onCancel((noteId, ref) => {
  retiredRefs.set(ref, null);
  void rewriteNote(noteId, (b) => stripMarker(b, ref), (e) => e.removeMarker(ref));
});
uploads.onRefChange((noteId, oldRef, newRef) => {
  retiredRefs.set(oldRef, newRef);
  void rewriteNote(noteId, (b) => swapRef(b, oldRef, newRef), (e) => e.replaceRef(oldRef, newRef));
});
