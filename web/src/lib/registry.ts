// Resolves inline-image marker refs to attachments and tracks in-flight uploads,
// so editor widgets can render previews and progress without re-rendering the doc.
import { api } from './api';
import type { Attachment } from './types';

export type RefState =
  | { kind: 'loading' }
  | { kind: 'pending'; url: string; progress: number; error?: string }
  | { kind: 'ready'; att: Attachment }
  | { kind: 'missing' };

type Listener = (s: RefState) => void;

const states = new Map<string, RefState>();
const listeners = new Map<string, Set<Listener>>();
const inflight = new Map<string, Promise<void>>();

function emit(ref: string, s: RefState) {
  states.set(ref, s);
  for (const l of listeners.get(ref) ?? []) l(s);
}

export const registry = {
  get(ref: string): RefState {
    const s = states.get(ref);
    if (s) return s;
    void this.resolve(ref);
    return { kind: 'loading' };
  },

  subscribe(ref: string, fn: Listener): () => void {
    let set = listeners.get(ref);
    if (!set) listeners.set(ref, (set = new Set()));
    set.add(fn);
    fn(this.get(ref));
    return () => set!.delete(fn);
  },

  resolve(ref: string): Promise<void> {
    if (states.has(ref)) return Promise.resolve();
    let p = inflight.get(ref);
    if (!p) {
      p = api
        .byRef(ref)
        .then((att) => emit(ref, { kind: 'ready', att }))
        .catch(() => emit(ref, { kind: 'missing' }))
        .finally(() => inflight.delete(ref));
      inflight.set(ref, p);
    }
    return p;
  },

  /** Seed from a loaded note's attachment list. */
  seed(atts: Attachment[]) {
    for (const a of atts) if (states.get(a.ref)?.kind !== 'pending') emit(a.ref, { kind: 'ready', att: a });
  },

  pending(ref: string, url: string) {
    emit(ref, { kind: 'pending', url, progress: 0 });
  },

  progress(ref: string, progress: number, error?: string) {
    const s = states.get(ref);
    if (s?.kind === 'pending') emit(ref, { ...s, progress, error });
  },

  ready(att: Attachment) {
    const s = states.get(att.ref);
    emit(att.ref, { kind: 'ready', att });
    // keep the preview URL alive until the real image has had time to load
    if (s?.kind === 'pending') setTimeout(() => URL.revokeObjectURL(s.url), 10_000);
  },

  forget(ref: string) {
    const s = states.get(ref);
    if (s?.kind === 'pending') URL.revokeObjectURL(s.url);
    emit(ref, { kind: 'missing' });
  },
};
