import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { NoteSaver, type SaverDeps } from './saver';
import type { SaveResult } from './types';

class FakeServer {
  body = 'v0';
  sha = 'sha:v0';
  calls: { body: string; base: string }[] = [];
  fail: null | { status: number; code?: string } = null;
  delay = 0;
  inFlight = 0;
  maxInFlight = 0;

  deps(drafts: Map<string, string>): SaverDeps {
    return {
      save: async (_id, body, base) => {
        this.calls.push({ body, base });
        this.inFlight++;
        this.maxInFlight = Math.max(this.maxInFlight, this.inFlight);
        await new Promise((r) => setTimeout(r, this.delay));
        this.inFlight--;
        if (this.fail) {
          const e: any = new Error('fail');
          Object.assign(e, this.fail);
          throw e;
        }
        if (base !== this.sha && `sha:${body}` !== this.sha) {
          const e: any = new Error('conflict');
          Object.assign(e, { status: 409, code: 'conflict', details: { current_body: this.body, current_sha256: this.sha } });
          throw e;
        }
        this.body = body;
        this.sha = `sha:${body}`;
        return { sha256: this.sha, updated_at: 1, title: body, preview: '', size_bytes: body.length } as SaveResult;
      },
      putDraft: (k, b) => drafts.set(k, b),
      deleteDraft: (k) => drafts.delete(k),
    };
  }
}

describe('NoteSaver', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('debounces edits into one save and clears the draft', async () => {
    const srv = new FakeServer();
    const drafts = new Map<string, string>();
    const s = new NoteSaver('n', 'k', 'v0', 'sha:v0', srv.deps(drafts));
    s.edit('v1');
    s.edit('v12');
    await vi.advanceTimersByTimeAsync(300);
    expect(drafts.get('k')).toBe('v12');
    await vi.advanceTimersByTimeAsync(1000);
    expect(srv.calls).toEqual([{ body: 'v12', base: 'sha:v0' }]);
    expect(s.status).toBe('saved');
    expect(drafts.has('k')).toBe(false);
  });

  it('never has two saves in flight and chains the base sha', async () => {
    const srv = new FakeServer();
    srv.delay = 500;
    const s = new NoteSaver('n', 'k', 'v0', 'sha:v0', srv.deps(new Map()));
    s.edit('a');
    await vi.advanceTimersByTimeAsync(900); // save of "a" in flight
    s.edit('ab');
    const p = s.flush(); // e.g. switching notes mid-save
    await vi.advanceTimersByTimeAsync(2000);
    await p;
    expect(srv.maxInFlight).toBe(1);
    expect(srv.calls).toEqual([
      { body: 'a', base: 'sha:v0' },
      { body: 'ab', base: 'sha:a' },
    ]);
    expect(s.dirty).toBe(false);
  });

  it('keeps drafting while a conflict is open, and keepMine overwrites', async () => {
    const srv = new FakeServer();
    const drafts = new Map<string, string>();
    const s = new NoteSaver('n', 'k', 'v0', 'sha:v0', srv.deps(drafts));
    srv.body = 'other';
    srv.sha = 'sha:other';
    s.edit('mine');
    await vi.advanceTimersByTimeAsync(1000);
    expect(s.status).toBe('conflict');
    s.edit('mine, more');
    await vi.advanceTimersByTimeAsync(300);
    expect(drafts.get('k')).toBe('mine, more');
    expect(srv.calls.length).toBe(1); // autosave paused
    const p = s.keepMine();
    await vi.advanceTimersByTimeAsync(10);
    await p;
    expect(srv.body).toBe('mine, more');
    expect(s.status).toBe('saved');
  });

  it('useTheirs adopts the server text', async () => {
    const srv = new FakeServer();
    const s = new NoteSaver('n', 'k', 'v0', 'sha:v0', srv.deps(new Map()));
    srv.body = 'other';
    srv.sha = 'sha:other';
    s.edit('mine');
    await vi.advanceTimersByTimeAsync(1000);
    expect(s.useTheirs()).toBe('other');
    expect(s.dirty).toBe(false);
    expect(s.baseSha).toBe('sha:other');
  });

  it('retries after a network error and keeps the draft meanwhile', async () => {
    const srv = new FakeServer();
    const drafts = new Map<string, string>();
    const s = new NoteSaver('n', 'k', 'v0', 'sha:v0', srv.deps(drafts));
    srv.fail = { status: 0 };
    s.edit('offline text');
    await vi.advanceTimersByTimeAsync(1000);
    expect(s.status).toBe('error');
    expect(drafts.get('k')).toBe('offline text');
    srv.fail = null;
    await vi.advanceTimersByTimeAsync(2500);
    expect(s.status).toBe('saved');
    expect(srv.body).toBe('offline text');
  });

  it('drops the draft when the text goes back to what is saved', async () => {
    const srv = new FakeServer();
    const drafts = new Map<string, string>();
    const s = new NoteSaver('n', 'k', 'v0', 'sha:v0', srv.deps(drafts));
    s.edit('v0x');
    await vi.advanceTimersByTimeAsync(300); // draft written
    expect(drafts.get('k')).toBe('v0x');
    s.edit('v0'); // typo deleted before autosave
    await vi.advanceTimersByTimeAsync(1000);
    expect(drafts.has('k')).toBe(false);
    expect(srv.calls.length).toBe(0);
  });

  it('a retried save that already landed is accepted (idempotent server)', async () => {
    const srv = new FakeServer();
    const s = new NoteSaver('n', 'k', 'v0', 'sha:v0', srv.deps(new Map()));
    srv.body = 'x';
    srv.sha = 'sha:x'; // the previous attempt reached disk, response was lost
    s.edit('x');
    await vi.advanceTimersByTimeAsync(1000);
    expect(s.status).toBe('saved');
  });
});
