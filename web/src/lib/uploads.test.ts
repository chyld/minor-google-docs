import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// The upload client talks to the server through XMLHttpRequest (tus) plus
// fetch/api for DELETE and metadata. Both are faked here with a tiny scripted server.
const meta = vi.fn();
vi.mock('./api', () => ({ api: { attachmentMeta: (id: string) => meta(id) } }));
vi.mock('./registry', () => ({ registry: { pending() {}, progress() {}, ready() {}, forget() {} } }));

interface Req {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: Blob | null;
}
type Reply = { status: number; headers?: Record<string, string>; body?: string } | 'hang';
let handler: (r: Req) => Reply;
const requests: Req[] = [];

class FakeXHR {
  static DONE = 4;
  readyState = 0;
  status = 0;
  responseText = '';
  upload = { onprogress: null as ((e: { loaded: number }) => void) | null };
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;
  private req: Req = { method: '', url: '', headers: {}, body: null };
  private resHeaders: Record<string, string> = {};
  private loadend: (() => void)[] = [];
  open(method: string, url: string) {
    this.req = { method, url, headers: {}, body: null };
  }
  setRequestHeader(k: string, v: string) {
    this.req.headers[k] = v;
  }
  getResponseHeader(k: string) {
    return this.resHeaders[k] ?? null;
  }
  addEventListener(ev: string, fn: () => void) {
    if (ev === 'loadend') this.loadend.push(fn);
  }
  send(body: Blob | null) {
    this.req.body = body;
    requests.push(this.req);
    const reply = handler(this.req);
    if (reply === 'hang') return;
    setTimeout(() => {
      if (this.readyState === 4) return;
      this.readyState = 4;
      this.status = reply.status;
      this.resHeaders = reply.headers ?? {};
      this.responseText = reply.body ?? '';
      this.onload?.();
      this.loadend.forEach((f) => f());
    }, 1);
  }
  abort() {
    if (this.readyState === 4) return;
    this.readyState = 4;
    this.onabort?.();
    this.loadend.forEach((f) => f());
  }
}

const fetchCalls: { url: string; method: string }[] = [];
let deleteStatus = 204;

beforeEach(() => {
  requests.length = 0;
  fetchCalls.length = 0;
  deleteStatus = 204;
  meta.mockReset();
  vi.stubGlobal('XMLHttpRequest', FakeXHR);
  vi.stubGlobal('fetch', async (url: string, init: { method: string }) => {
    fetchCalls.push({ url, method: init.method });
    return { status: deleteStatus };
  });
  vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: () => 'blob:x' }));
});
afterEach(() => vi.unstubAllGlobals());

const until = async (cond: () => boolean) => {
  for (let i = 0; i < 200 && !cond(); i++) await new Promise((r) => setTimeout(r, 5));
  expect(cond()).toBe(true);
};

describe('upload client', () => {
  it('cancel during the create request waits for it, then DELETEs and drops the marker', async () => {
    const { uploads } = await import('./uploads.svelte');
    handler = () => 'hang'; // the create is in flight when the user cancels
    const canceled: string[] = [];
    const off = uploads.onCancel((_n, ref) => canceled.push(ref));
    const t = uploads.start(new Blob(['hello']), 'a.png', 'note-1', true);
    await until(() => requests.length === 1);
    await uploads.cancel(t.id);
    off();
    expect(canceled).toEqual([t.ref]);
    expect(fetchCalls).toEqual([{ url: `/api/uploads/${t.id}`, method: 'DELETE' }]);
  });

  it('ref_taken after a lost create response resumes the same upload (HEAD finds it)', async () => {
    const { uploads } = await import('./uploads.svelte');
    const changed: string[] = [];
    const off = uploads.onRefChange((_n, a, b) => changed.push(`${a}->${b}`));
    handler = (r) => {
      if (r.method === 'POST') return { status: 409, body: JSON.stringify({ error: { code: 'ref_taken', message: 'taken' } }) };
      if (r.method === 'HEAD') return { status: 200, headers: { 'Upload-Offset': '5' } };
      return { status: 500 };
    };
    meta.mockResolvedValue({ id: 'x', ref: 'r', filename: 'a.png' });
    const t = uploads.start(new Blob(['hello']), 'a.png', 'note-1', false);
    await until(() => t.status === 'done');
    off();
    expect(changed).toEqual([]); // same id and ref kept
    expect(requests.map((r) => r.method)).toEqual(['POST', 'HEAD']);
  });

  it('ref_taken by someone else mints a new ref and announces it', async () => {
    const { uploads } = await import('./uploads.svelte');
    const changed: string[] = [];
    const off = uploads.onRefChange((_n, a, b) => changed.push(`${a}->${b}`));
    let posts = 0;
    handler = (r) => {
      if (r.method === 'POST')
        return ++posts === 1
          ? { status: 409, body: JSON.stringify({ error: { code: 'ref_taken', message: 'taken' } }) }
          : { status: 201, headers: { 'Upload-Offset': '5' } };
      if (r.method === 'HEAD') return { status: 404 };
      return { status: 500 };
    };
    meta.mockResolvedValue({ id: 'x', ref: 'r', filename: 'a.png' });
    const t = uploads.start(new Blob(['hello']), 'a.png', 'note-1', false);
    const firstRef = t.ref;
    await until(() => t.status === 'done');
    off();
    expect(changed).toEqual([`${firstRef}->${t.ref}`]);
    expect(t.ref).not.toBe(firstRef);
  });

  it('completes a half-finalized upload with an empty PATCH at the full offset', async () => {
    const { uploads } = await import('./uploads.svelte');
    handler = (r) => {
      if (r.method === 'POST') return { status: 201, headers: { 'Upload-Offset': '5' } };
      if (r.method === 'PATCH') return { status: 204, headers: { 'Upload-Offset': '5' } };
      return { status: 500 };
    };
    meta.mockRejectedValueOnce(Object.assign(new Error('nf'), { status: 404 })).mockResolvedValue({ id: 'x', ref: 'r', filename: 'a.png' });
    const t = uploads.start(new Blob(['hello']), 'a.png', 'note-1', false);
    await until(() => t.status === 'done');
    const patch = requests.find((r) => r.method === 'PATCH')!;
    expect(patch.headers['Upload-Offset']).toBe('5');
    expect(patch.body?.size).toBe(0);
  });
});
