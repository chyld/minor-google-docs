// Minimal tus 1.0 client (creation, creation-with-upload, termination) with
// resume-on-failure. Small files go up in one request; large files in 8 MiB chunks.
import { api } from './api';
import { newRef, uuidv7 } from './ids';
import { registry } from './registry';
import type { Attachment } from './types';

const CHUNK = 8 * 1024 * 1024;
const BACKOFF = [1, 2, 4, 8, 15, 30, 30, 60];

export type UploadStatus = 'uploading' | 'retrying' | 'error' | 'done' | 'canceled';

export interface UploadTask {
  id: string;
  ref: string;
  noteId: string;
  filename: string;
  size: number;
  sent: number;
  status: UploadStatus;
  error: string;
  inline: boolean;
}

interface Internal {
  file: Blob;
  xhr: XMLHttpRequest | null;
  canceled: boolean;
  created: boolean;
}

type DoneFn = (task: UploadTask, att: Attachment) => void;
type RefChangeFn = (noteId: string, oldRef: string, newRef: string) => void;
type CancelFn = (noteId: string, ref: string) => void;

function b64(s: string): string {
  const bytes = new TextEncoder().encode(s);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

class HttpError extends Error {
  constructor(
    public status: number,
    public code = '',
    msg = `HTTP ${status}`,
  ) {
    super(msg);
  }
}

interface XhrResult {
  status: number;
  header: (n: string) => string | null;
  body: string;
}

function send(method: string, url: string, headers: Record<string, string>, body: Blob | null, onProgress: (n: number) => void, hold: (x: XMLHttpRequest) => void): Promise<XhrResult> {
  return new Promise((resolve, reject) => {
    const x = new XMLHttpRequest();
    hold(x);
    x.open(method, url);
    x.setRequestHeader('X-Requested-With', 'notes');
    x.setRequestHeader('Tus-Resumable', '1.0.0');
    for (const [k, v] of Object.entries(headers)) x.setRequestHeader(k, v);
    x.upload.onprogress = (e) => onProgress(e.loaded);
    x.onload = () => resolve({ status: x.status, header: (n) => x.getResponseHeader(n), body: x.responseText });
    x.onerror = () => reject(new HttpError(0, 'network', 'Network error'));
    x.onabort = () => reject(new HttpError(-1, 'aborted', 'Canceled'));
    x.send(body);
  });
}

function errorFrom(r: XhrResult): HttpError {
  try {
    const e = JSON.parse(r.body).error;
    return new HttpError(r.status, e.code, e.message);
  } catch {
    return new HttpError(r.status);
  }
}

class Uploads {
  tasks = $state<UploadTask[]>([]);
  private internals = new Map<string, Internal>();
  private doneFns = new Set<DoneFn>();
  private refFns = new Set<RefChangeFn>();
  private cancelFns = new Set<CancelFn>();

  active = $derived(this.tasks.filter((t) => t.status === 'uploading' || t.status === 'retrying').length);

  onDone(fn: DoneFn) {
    this.doneFns.add(fn);
    return () => this.doneFns.delete(fn);
  }

  onRefChange(fn: RefChangeFn) {
    this.refFns.add(fn);
    return () => this.refFns.delete(fn);
  }

  /** An upload was canceled: its marker should go. */
  onCancel(fn: CancelFn) {
    this.cancelFns.add(fn);
    return () => this.cancelFns.delete(fn);
  }

  /** Start uploading; returns the task (ref is usable in a marker immediately). */
  start(file: Blob, filename: string, noteId: string, inline: boolean): UploadTask {
    const task: UploadTask = { id: uuidv7(), ref: newRef(), noteId, filename, size: file.size, sent: 0, status: 'uploading', error: '', inline };
    this.tasks.push(task);
    this.internals.set(task.id, { file, xhr: null, canceled: false, created: false });
    if (inline) registry.pending(task.ref, URL.createObjectURL(file));
    void this.run(task.id);
    return this.find(task.id)!;
  }

  private find(id: string) {
    return this.tasks.find((t) => t.id === id);
  }

  private update(id: string, patch: Partial<UploadTask>) {
    const t = this.find(id);
    if (t) Object.assign(t, patch);
    if (t?.inline && (patch.sent !== undefined || patch.error !== undefined)) {
      registry.progress(t.ref, t.size ? t.sent / t.size : 1, t.status === 'error' ? t.error : undefined);
    }
  }

  private async run(id: string) {
    const t = this.find(id)!;
    const int = this.internals.get(id)!;
    let attempt = 0;
    while (!int.canceled) {
      try {
        if (!int.created) await this.create(t, int);
        while (t.sent < t.size && !int.canceled) await this.patch(t, int);
        if (int.canceled) return;
        const att = await this.meta(t, int);
        if (int.canceled) return;
        this.update(id, { status: 'done', sent: t.size });
        if (t.inline) registry.ready(att);
        for (const fn of this.doneFns) fn(t, att);
        setTimeout(() => this.dismiss(id), 3000);
        return;
      } catch (e) {
        if (int.canceled) return;
        const err = e as HttpError;
        if (err.code === 'ref_taken' && !int.created) {
          // Our own earlier create may have landed (response lost): resume it if so.
          if (await this.syncOffset(t).then(() => true, () => false)) {
            int.created = true;
            continue;
          }
          const oldRef = t.ref;
          this.update(id, { id: uuidv7(), ref: newRef() });
          this.internals.delete(id);
          this.internals.set(t.id, int);
          id = t.id;
          if (t.inline) {
            registry.forget(oldRef);
            registry.pending(t.ref, URL.createObjectURL(int.file));
          }
          for (const fn of this.refFns) fn(t.noteId, oldRef, t.ref);
          continue;
        }
        // 507 stops here too, but stays retryable from the tray once space is freed.
        const fatal = err.status === 404 || err.status === 400 || err.status === 403 || err.status === 507 || err.status === 401;
        if (fatal || attempt >= BACKOFF.length) {
          this.update(id, { status: 'error', error: err.status === 507 ? 'Server is low on disk space' : err.message });
          return;
        }
        this.update(id, { status: 'retrying', error: err.message });
        await new Promise((r) => setTimeout(r, BACKOFF[attempt++] * 1000));
        if (int.created) await this.syncOffset(t).catch(() => {});
        if (!int.canceled) this.update(id, { status: 'uploading', error: '' });
      }
    }
  }

  /** Metadata of the finished upload. If the final PATCH landed but finalizing
   * didn't, an empty PATCH at the full offset completes it server-side. */
  private async meta(t: UploadTask, int: Internal): Promise<Attachment> {
    try {
      return await api.attachmentMeta(t.id);
    } catch (e) {
      if ((e as HttpError).status !== 404) throw e;
    }
    const r = await send(
      'PATCH',
      `/api/uploads/${t.id}`,
      { 'Content-Type': 'application/offset+octet-stream', 'Upload-Offset': String(t.size) },
      new Blob([]),
      () => {},
      (x) => (int.xhr = x),
    );
    if (r.status !== 204) throw errorFrom(r);
    return api.attachmentMeta(t.id);
  }

  private async create(t: UploadTask, int: Internal) {
    const meta = [`id ${b64(t.id)}`, `ref ${b64(t.ref)}`, `note_id ${b64(t.noteId)}`, `filename ${b64(t.filename)}`].join(',');
    const headers: Record<string, string> = { 'Upload-Length': String(t.size), 'Upload-Metadata': meta };
    let body: Blob | null = null;
    if (t.size <= CHUNK) {
      body = int.file;
      headers['Content-Type'] = 'application/offset+octet-stream';
    }
    const r = await send('POST', '/api/uploads/', headers, body, (n) => this.update(t.id, { sent: n }), (x) => (int.xhr = x));
    if (r.status !== 201) throw errorFrom(r);
    int.created = true;
    this.update(t.id, { sent: Number(r.header('Upload-Offset') ?? 0) });
  }

  private async patch(t: UploadTask, int: Internal) {
    const start = t.sent;
    const chunk = int.file.slice(start, Math.min(t.size, start + CHUNK));
    const r = await send(
      'PATCH',
      `/api/uploads/${t.id}`,
      { 'Content-Type': 'application/offset+octet-stream', 'Upload-Offset': String(start) },
      chunk,
      (n) => this.update(t.id, { sent: start + n }),
      (x) => (int.xhr = x),
    );
    if (r.status === 409) {
      this.update(t.id, { sent: Number(r.header('Upload-Offset') ?? start) });
      return;
    }
    if (r.status !== 204) throw errorFrom(r);
    this.update(t.id, { sent: Number(r.header('Upload-Offset') ?? start + chunk.size) });
  }

  private async syncOffset(t: UploadTask) {
    const int = this.internals.get(t.id)!;
    const r = await send('HEAD', `/api/uploads/${t.id}`, {}, null, () => {}, (x) => (int.xhr = x));
    if (r.status !== 200) throw new HttpError(r.status);
    this.update(t.id, { sent: Number(r.header('Upload-Offset') ?? 0) });
  }

  retry(id: string) {
    const t = this.find(id);
    if (!t || t.status !== 'error') return;
    this.update(id, { status: 'uploading', error: '' });
    void this.run(id);
  }

  async cancel(id: string) {
    const t = this.find(id);
    const int = this.internals.get(id);
    if (!t || !int || int.canceled) return;
    int.canceled = true;
    const xhr = int.xhr;
    this.update(id, { status: 'canceled' });
    if (t.inline) registry.forget(t.ref);
    for (const fn of this.cancelFns) fn(t.noteId, t.ref);
    setTimeout(() => this.dismiss(id), 1500);
    // Let any in-flight request settle first: a create may land right after we abort.
    if (xhr && xhr.readyState !== XMLHttpRequest.DONE) {
      await new Promise<void>((r) => {
        xhr.addEventListener('loadend', () => r(), { once: true });
        xhr.abort();
      });
    }
    // Always DELETE — even if we never saw a 201, the server may have created it.
    // A 404 may mean "not created yet", so try again a little later.
    for (const wait of [0, 1000, 3000, 10000]) {
      if (wait) await new Promise((r) => setTimeout(r, wait));
      const status = await fetch(`/api/uploads/${t.id}`, { method: 'DELETE', headers: { 'Tus-Resumable': '1.0.0', 'X-Requested-With': 'notes' } }).then(
        (r) => r.status,
        () => 0,
      );
      if (status === 204 || (status >= 400 && status < 500 && status !== 404)) return;
    }
  }

  dismiss(id: string) {
    this.tasks = this.tasks.filter((t) => t.id !== id);
    this.internals.delete(id);
  }
}

export const uploads = new Uploads();
