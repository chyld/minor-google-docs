// Unsaved note text is mirrored to IndexedDB so a crashed tab or a lost
// connection never loses typing. Cleared after a successful save.

export interface Draft {
  key: string; // `${userId}:${noteId}`
  body: string;
  base_sha256: string;
  at: number;
}

let dbp: Promise<IDBDatabase> | null = null;

function db(): Promise<IDBDatabase> {
  dbp ??= new Promise((resolve, reject) => {
    const req = indexedDB.open('notes-drafts', 1);
    req.onupgradeneeded = () => req.result.createObjectStore('drafts', { keyPath: 'key' });
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
  return dbp;
}

async function tx<T>(mode: IDBTransactionMode, fn: (s: IDBObjectStore) => IDBRequest<T>): Promise<T | undefined> {
  try {
    const d = await db();
    return await new Promise((resolve, reject) => {
      const t = d.transaction('drafts', mode);
      const r = fn(t.objectStore('drafts'));
      t.oncomplete = () => resolve(r.result);
      t.onerror = () => reject(t.error);
    });
  } catch {
    return undefined; // private mode / quota: drafts are best effort
  }
}

export const drafts = {
  put: (d: Draft) => tx('readwrite', (s) => s.put(d)),
  get: (key: string) => tx<Draft>('readonly', (s) => s.get(key)),
  delete: (key: string) => tx('readwrite', (s) => s.delete(key)),
};
