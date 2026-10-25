import { api, ApiError, setUnauthorizedHandler } from './api';
import type { NoteMeta, Sort, Tag, User } from './types';
import { effectiveWidth, loadLayout, saveLayout, type PageWidth } from './layout';

export type View = 'all' | 'pinned' | 'untagged' | 'attachments' | 'trash';
export type Theme = 'system' | 'light' | 'dark';

export interface Toast {
  id: number;
  message: string;
  kind: 'info' | 'error' | 'success';
  action?: { label: string; run: () => void };
}

class AppState {
  me = $state<User | null>(null);
  booting = $state(true);
  trashDays = $state(30);

  tags = $state<Tag[]>([]);
  tagById = $derived(new Map(this.tags.map((t) => [t.id, t])));

  view = $state<View>('all');
  tagFilter = $state<string[]>([]);
  query = $state('');
  sort = $state<Sort>((localStorage.getItem('notes.sort') as Sort) || 'updated');

  notes = $state<NoteMeta[]>([]);
  nextCursor = $state('');
  listLoading = $state(false);
  listError = $state('');

  selectedId = $state<string | null>(null);

  theme = $state<Theme>((localStorage.getItem('notes.theme') as Theme) || 'system');
  mono = $state(localStorage.getItem('notes.mono') === '1');
  raw = $state(localStorage.getItem('notes.raw') === '1'); // editor shows the stored text as-is
  noteSections = $state<{ name: string; color: string }[]>([]); // the open note's sections (palette)
  noteCanSection = $state(false); // the open note can take a new section (editable, has a title)
  focusMode = $state(false);
  sidebarOpen = $state(false); // mobile drawer
  // desktop layout (ignored on phones, where one pane shows at a time)
  sidebarCollapsed = $state(false); // icon rail
  listCollapsed = $state(false);
  pageWidth = $state<PageWidth>('auto');
  desktop = $state(true);
  /** The writing column's width right now (focus mode counts as "list hidden"). */
  width = $derived(effectiveWidth(this.pageWidth, (this.listCollapsed && this.desktop) || this.focusMode));
  mobilePane = $state<'list' | 'note'>('list');

  paletteOpen = $state(false);
  accountOpen = $state(false);
  /** The open note is read-only (trashed): the palette hides editing commands. */
  noteReadonly = $state(false);
  /** Start "Check my notes" as soon as Account & data opens (from the palette). */
  checkOnOpen = $state(false);
  shortcutsOpen = $state(false);
  lightbox = $state<{ noteId: string; ref: string } | null>(null);

  toasts = $state<Toast[]>([]);
  private toastSeq = 0;
  private listSeq = 0;

  constructor() {
    Object.assign(this, loadLayout(localStorage));
    const mq = matchMedia('(min-width: 861px)');
    this.desktop = mq.matches;
    mq.addEventListener('change', () => (this.desktop = mq.matches));
    setUnauthorizedHandler(() => {
      if (this.me) {
        this.me = null;
        this.toast('Your session ended. Please log in again.', 'info');
      }
    });
  }

  toast(message: string, kind: Toast['kind'] = 'info', action?: Toast['action'], ms = 4500) {
    const id = ++this.toastSeq;
    this.toasts.push({ id, message, kind, action });
    setTimeout(() => this.dismiss(id), action ? ms + 2500 : ms);
    return id;
  }

  dismiss(id: number) {
    this.toasts = this.toasts.filter((t) => t.id !== id);
  }

  error(e: unknown) {
    const msg = e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e);
    this.toast(msg, 'error');
  }

  setTheme(t: Theme) {
    this.theme = t;
    localStorage.setItem('notes.theme', t);
  }

  setMono(v: boolean) {
    this.mono = v;
    localStorage.setItem('notes.mono', v ? '1' : '0');
  }

  setRaw(v: boolean) {
    this.raw = v;
    localStorage.setItem('notes.raw', v ? '1' : '0');
  }

  private saveLayout() {
    saveLayout(localStorage, { sidebarCollapsed: this.sidebarCollapsed, listCollapsed: this.listCollapsed, pageWidth: this.pageWidth });
  }

  setSidebarCollapsed(v: boolean) {
    this.sidebarCollapsed = v;
    this.saveLayout();
  }

  setListCollapsed(v: boolean) {
    this.listCollapsed = v;
    this.saveLayout();
  }

  setPageWidth(w: PageWidth) {
    this.pageWidth = w;
    this.saveLayout();
  }

  setSort(s: Sort) {
    this.sort = s;
    localStorage.setItem('notes.sort', s);
    this.loadList();
  }

  setView(v: View) {
    this.view = v;
    this.tagFilter = [];
    this.sidebarOpen = false;
    if (this.listCollapsed && this.desktop) this.setListCollapsed(false); // phones keep the desktop setting
    this.loadList();
  }

  toggleTag(id: string, additive: boolean) {
    if (this.view === 'trash' || this.view === 'untagged') this.view = 'all';
    if (additive) {
      this.tagFilter = this.tagFilter.includes(id) ? this.tagFilter.filter((t) => t !== id) : [...this.tagFilter, id];
    } else {
      this.tagFilter = this.tagFilter.length === 1 && this.tagFilter[0] === id ? [] : [id];
    }
    this.sidebarOpen = false;
    if (this.listCollapsed && this.desktop) this.setListCollapsed(false); // phones keep the desktop setting
    this.loadList();
  }

  async loadTags() {
    try {
      this.tags = (await api.tags()).items;
    } catch (e) {
      this.error(e);
    }
  }

  async loadList(more = false) {
    const seq = ++this.listSeq;
    this.listLoading = true;
    this.listError = '';
    try {
      const res = await api.listNotes({
        q: this.query.trim() || undefined,
        tags: this.tagFilter,
        untagged: this.view === 'untagged',
        pinned: this.view === 'pinned',
        trash: this.view === 'trash',
        has_attachments: this.view === 'attachments',
        sort: this.sort,
        cursor: more ? this.nextCursor : undefined,
      });
      if (seq !== this.listSeq) return;
      this.notes = more ? [...this.notes, ...res.items] : res.items;
      this.nextCursor = res.next_cursor;
    } catch (e) {
      if (seq === this.listSeq) this.listError = e instanceof Error ? e.message : String(e);
    } finally {
      if (seq === this.listSeq) this.listLoading = false;
    }
  }

  /** Update (or insert at top) a note in the visible list. */
  upsert(m: Partial<NoteMeta> & { id: string }) {
    const i = this.notes.findIndex((n) => n.id === m.id);
    if (i >= 0) this.notes[i] = { ...this.notes[i], ...m };
    // only a complete, live note may join the list (a partial one would break the cards)
    else if ('tag_ids' in m && !m.trashed_at && this.view !== 'trash') this.notes = [m as NoteMeta, ...this.notes];
  }

  remove(id: string) {
    this.notes = this.notes.filter((n) => n.id !== id);
  }

  select(id: string | null, push = true) {
    this.selectedId = id;
    this.mobilePane = id ? 'note' : 'list';
    if (push) {
      const url = id ? `/n/${id}` : '/';
      if (location.pathname !== url) history.pushState({ id }, '', url);
    }
  }

  async newNote(body = '') {
    try {
      const tags = this.view === 'trash' ? [] : this.tagFilter;
      if (this.view === 'trash') this.view = 'all';
      const m = await api.createNote(body, tags);
      this.notes = [m, ...this.notes.filter((n) => n.id !== m.id)];
      this.select(m.id);
      if (tags.length) this.loadTags();
      return m;
    } catch (e) {
      this.error(e);
    }
  }
}

export const app = new AppState();
