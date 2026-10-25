import type { Attachment, Note, NoteList, NoteMeta, SaveResult, Storage, Tag, User, DoctorJob } from './types';

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details?: any,
  ) {
    super(message);
  }
}

let onUnauthorized: () => void = () => {};
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

export async function request<T>(method: string, path: string, body?: unknown, init: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = { 'X-Requested-With': 'notes' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
      ...init,
    });
  } catch {
    throw new ApiError(0, 'network', 'Cannot reach the server.');
  }
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    const e = data?.error ?? {};
    if (res.status === 401 && path !== '/api/login') onUnauthorized();
    throw new ApiError(res.status, e.code ?? 'error', e.message ?? res.statusText, e.details);
  }
  return data as T;
}

export interface ListQuery {
  q?: string;
  tags?: string[];
  untagged?: boolean;
  pinned?: boolean;
  trash?: boolean;
  has_attachments?: boolean;
  sort?: string;
  cursor?: string;
  limit?: number;
}

export const api = {
  setup: () => request<{ has_users: boolean; trash_days: number }>('GET', '/api/setup'),
  login: (username: string, password: string) => request<User>('POST', '/api/login', { username, password }),
  logout: () => request<void>('POST', '/api/logout'),
  logoutOthers: () => request<{ revoked: number }>('POST', '/api/logout-others'),
  me: () => request<User>('GET', '/api/me'),
  changePassword: (current: string, next: string) => request<void>('PUT', '/api/me/password', { current, new: next }),
  storage: () => request<Storage>('GET', '/api/me/storage'),
  doctor: () => request<DoctorJob>('GET', '/api/me/doctor'),
  startDoctor: (fix: boolean) => request<DoctorJob>('POST', '/api/me/doctor', { fix }),

  listNotes(q: ListQuery) {
    const p = new URLSearchParams();
    if (q.q) p.set('q', q.q);
    for (const t of q.tags ?? []) p.append('tag', t);
    if (q.untagged) p.set('untagged', '1');
    if (q.pinned) p.set('pinned', '1');
    if (q.trash) p.set('trash', '1');
    if (q.has_attachments) p.set('has_attachments', '1');
    if (q.sort) p.set('sort', q.sort);
    if (q.cursor) p.set('cursor', q.cursor);
    if (q.limit) p.set('limit', String(q.limit));
    return request<NoteList>('GET', '/api/notes?' + p.toString());
  },
  createNote: (body = '', tag_ids: string[] = []) => request<NoteMeta>('POST', '/api/notes', { body, tag_ids }),
  getNote: (id: string) => request<Note>('GET', `/api/notes/${id}`),
  saveBody: (id: string, body: string, base_sha256: string, keepalive = false) =>
    request<SaveResult>('PUT', `/api/notes/${id}/body`, { body, base_sha256 }, { keepalive }),
  patchNote: (id: string, patch: { pinned?: boolean; tag_ids?: string[]; trashed?: boolean }) =>
    request<NoteMeta>('PATCH', `/api/notes/${id}`, patch),
  duplicate: (id: string) => request<NoteMeta>('POST', `/api/notes/${id}/duplicate`),
  purge: (id: string) => request<void>('DELETE', `/api/notes/${id}`),
  emptyTrash: () => request<{ deleted: number; kept?: number }>('POST', '/api/trash/empty'),

  tags: () => request<{ items: Tag[]; palette: string[] }>('GET', '/api/tags'),
  createTag: (name: string, color = '') => request<Tag>('POST', '/api/tags', { name, color }),
  patchTag: (id: string, patch: { name?: string; color?: string }) => request<Tag>('PATCH', `/api/tags/${id}`, patch),
  deleteTag: (id: string) => request<void>('DELETE', `/api/tags/${id}`),

  archiveCheck: (id: string) => request<{ missing: number }>('GET', `/api/notes/${id}/archive?check=1`),
  pageSize: (id: string, theme: string) => request<{ bytes: number }>('GET', `/api/notes/${id}/html?estimate=1&theme=${encodeURIComponent(theme)}`),
  attachmentMeta: (id: string) => request<Attachment>('GET', `/api/attachments/${id}/meta`),
  byRef: (ref: string) => request<Attachment>('GET', `/api/refs/${ref}`),
  renameAttachment: (id: string, filename: string) => request<Attachment>('PATCH', `/api/attachments/${id}`, { filename }),
  deleteAttachment: (id: string) => request<void>('DELETE', `/api/attachments/${id}`),
};

/** Starts a download of one note as a self-contained web page (SPEC §23). A quick HEAD
 * first, so an error (note deleted, upload still running) is shown, never saved as the page. */
export async function downloadPage(id: string, theme: string) {
  const url = `/api/notes/${id}/html?theme=${encodeURIComponent(theme)}`;
  const res = await fetch(url, { method: 'HEAD', headers: { 'X-Requested-With': 'notes' } });
  if (!res.ok) {
    // HEAD has no body: ask again for the message
    await request('GET', `/api/notes/${id}/html?estimate=1&theme=${encodeURIComponent(theme)}`);
    throw new ApiError(res.status, 'error', 'The page could not be made. Try again.');
  }
  startDownload(url);
}

/** Starts a download through the browser (streamed to disk, never held in memory). */
export function startDownload(url: string) {
  const a = document.createElement('a');
  a.href = url;
  a.download = '';
  document.body.append(a);
  a.click();
  a.remove();
}

/** Starts a download of the signed-in user's export zip (one folder per note). */
export const downloadExport = () => startDownload('/api/export');

/** One note as a .tar.gz with all its files (SPEC §24). */
export const archiveURL = (id: string) => `/api/notes/${id}/archive`;

export const attachmentURL = (id: string, download = false) => `/api/attachments/${id}${download ? '?download=1' : ''}`;
