<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import { api, ApiError, downloadPage } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import { drafts } from '../lib/drafts';
  import { uploads } from '../lib/uploads.svelte';
  import { registry } from '../lib/registry';
  import { refsIn } from '../lib/editor/markers';
  import { bytes, fullTime, relativeTime, wordCount } from '../lib/format';
  import { hue } from '../lib/colors';
  import { menu } from '../lib/a11y';
  import { PAGE_WIDTHS } from '../lib/layout';
  import type { NoteSaver, SaveStatus, Conflict } from '../lib/saver';
  import { anyUnsaved, claimSaver, createSaver, draftAll, existingSaver, fixRetiredRefs, forgetSaver, releaseSaver, setLiveEditor, unloadAll } from '../lib/savers';
  import type { Attachment, Note } from '../lib/types';
  import Editor from './Editor.svelte';
  import TagInput from './TagInput.svelte';
  import ConflictBar from './ConflictBar.svelte';
  import Attachments from './Attachments.svelte';
  import Lightbox from './Lightbox.svelte';
  import Icon from './Icon.svelte';
  import HighlightMenu from './HighlightMenu.svelte';
  import { hlHue, isHlName, stripHighlights } from '../lib/highlight';
  import { stripSections } from '../lib/heading';
  import { MARKER_RE } from '../lib/ids';
  import type { HighlightInfo, StyleOp } from '../lib/editor/highlights';
  import type { SectionInfo } from '../lib/editor/sections';

  interface Props {
    id: string;
  }
  let { id }: Props = $props();

  let editor: Editor;
  let saver: NoteSaver | null = null;
  let note = $state<Note | null>(null);
  let loadError = $state('');
  let status = $state<SaveStatus>('saved');
  let statusDetail = $state('');
  let conflict = $state<Conflict | null>(null);
  let pending = $state(''); // mirrors the editor; counts/refs derive from a debounced copy
  let counted = $state('');
  let attOpen = $state(false);
  let lightboxRef = $state<string | null>(null);
  let menuOpen = $state(false);
  let picker: HTMLInputElement | undefined = $state();
  let loadSeq = 0;
  let countTimer: ReturnType<typeof setTimeout> | undefined;

  let hl = $state<HighlightInfo>({ can: false, empty: true, current: null, focused: false, dragging: false, rect: null, bold: 'none', underline: 'none', hasColor: false, styled: false });
  let hlMenu = $state<'bar' | 'float' | null>(null);
  let hlLast = $state<string | null>(isHlName(localStorage.getItem('notes.hlLast') ?? '') ? localStorage.getItem('notes.hlLast') : null);
  let scroller: HTMLDivElement | undefined = $state();
  let box = $state<DOMRect | null>(null); // the scroll area, when the selection was last reported
  let barW = $state(90);
  let barH = $state(36);
  let popW = $state(430);
  let popH = $state(110);

  const readonly = $derived(!!note?.trashed_at);
  const hasText = $derived(/\S/.test(pending)); // stops at the first visible character: no copy of the note
  // sections go after the title line: a note of only images has none yet (checked on the debounced copy)
  const hasTitle = $derived(counted.split('\n').some((l) => l.trim() !== '' && !MARKER_RE.test(l)));
  const plain = $derived(app.raw ? counted : stripSections(stripHighlights(counted))); // counts what is on screen: markup only in raw text
  const words = $derived(wordCount(plain));
  const shownRefs = $derived(refsIn(counted));
  const draftKey = (nid: string) => `${app.me?.id}:${nid}`;
  const accent = $derived(note?.tag_ids.length ? hue(app.tagById.get(note.tag_ids[0])?.color ?? '') : null);

  // ---- loading ----

  $effect(() => {
    const nid = id;
    untrack(() => load(nid));
  });

  function attachSaver(s: NoteSaver) {
    saver = s;
    s.onChange = (st) => {
      status = st.status;
      statusDetail = st.detail;
      conflict = st.conflict;
    };
    s.onSaved = (r) => {
      if (note && note.id === s.noteId) {
        note.title = r.title;
        note.updated_at = r.updated_at;
        if (r.attachments) note.attachments = r.attachments;
      }
      if (r.attachments) registry.seed(r.attachments); // pasted images now have their own copy here
      app.upsert({
        id: s.noteId, title: r.title, preview: r.preview, updated_at: r.updated_at, sha256: r.sha256, size_bytes: r.size_bytes,
        ...(r.attachments ? { attachment_count: r.attachments.length } : {}),
      });
    };
    status = s.status;
    statusDetail = s.detail;
    conflict = s.conflict;
  }

  /** Let canceled uploads / re-minted refs edit what's on screen for this note. */
  function goLive(nid: string) {
    setLiveEditor({
      noteId: nid,
      removeMarker: (ref) => editor?.removeMarker(ref),
      replaceRef: (a, b) => editor?.replaceRef(a, b),
    });
  }

  function setText(text: string) {
    pending = text;
    clearTimeout(countTimer);
    counted = text;
  }

  async function load(nid: string) {
    if (saver) releaseSaver(saver, () => app.toast('Some changes are kept as a draft; reopen the note to finish saving them.', 'info'));
    saver = null;
    const seq = ++loadSeq;
    note = null;
    loadError = '';
    conflict = null;
    status = 'saved';
    lightboxRef = null;
    menuOpen = false;
    hlMenu = null;
    secPop = null;
    setLiveEditor(null);
    editor?.setDoc('', true); // read-only until we know what to show
    try {
      // a save still running from a moment ago must land first, or we'd fetch the old text
      await existingSaver(nid)?.flush().catch(() => {});
      if (seq !== loadSeq) return;
      const [n, d] = await Promise.all([api.getNote(nid), drafts.get(draftKey(nid))]);
      if (seq !== loadSeq) return;
      registry.seed(n.attachments);
      note = n;
      attOpen = attOpen && n.attachments.length > 0;
      const { body: _body, attachments: _atts, ...meta } = n;
      app.upsert({ ...meta, attachment_count: n.attachments.length });

      const live = existingSaver(nid);
      if (live && (live.dirty || live.conflict) && !n.trashed_at) {
        // unsaved work from a moment ago (switched away and back): keep its state
        claimSaver(live);
        attachSaver(live);
        setText(live.pending);
        show(live.pending);
        goLive(nid);
        void live.flush();
        return;
      }
      const s = createSaver(nid, draftKey(nid), n.body, n.sha256);
      attachSaver(s);
      if (d && d.body !== n.body && !n.trashed_at) {
        s.pending = d.body;
        setText(d.body);
        show(d.body);
        if (d.base_sha256 === n.sha256) {
          app.toast('Restored unsaved changes from earlier.', 'success');
          s.edit(d.body);
        } else {
          s.setConflict({ body: n.body, sha: n.sha256, reason: 'draft' });
        }
        goLive(nid);
        return;
      }
      // a trashed note keeps its draft: it is offered again if the note is restored
      if (d && !n.trashed_at) drafts.delete(draftKey(nid));
      setText(n.body);
      show(n.body, !!n.trashed_at);
      if (!n.trashed_at) goLive(nid);
      if (!n.body.trim() && !n.trashed_at) {
        await tick(); // the editor is still hidden until the DOM catches up with `note`
        if (seq === loadSeq) editor?.focus();
      }
    } catch (e) {
      if (seq !== loadSeq) return;
      loadError = e instanceof ApiError && e.status === 404 ? 'This note no longer exists.' : e instanceof Error ? e.message : String(e);
    }
  }

  // ---- sections ----

  let sectionList = $state<SectionInfo[]>([]);
  type Rect = { left: number; top: number; bottom: number; right: number };
  // one popover at a time: a new section's colour, the list, a heading's menu, or its colour
  let secPop = $state<{ kind: 'add' | 'list' | 'menu' | 'color'; index: number; rect: Rect } | null>(null);
  let secPopW = $state(240);
  let secPopH = $state(120);
  let secLast = $state<string | null>(isHlName(localStorage.getItem('notes.secLast') ?? '') ? localStorage.getItem('notes.secLast') : null);
  let moreBtn: HTMLButtonElement | undefined = $state();
  const secPos = $derived.by(() => {
    if (!secPop) return { left: 0, top: 0 };
    const r = secPop.rect;
    const left = Math.min(Math.max(8, r.right - secPopW), window.innerWidth - secPopW - 8);
    const below = r.bottom + 6;
    const top = below + secPopH <= window.innerHeight - 8 ? below : Math.max(8, r.top - secPopH - 6);
    return { left, top };
  });

  $effect(() => {
    app.noteCanSection = !!note && !readonly && hasTitle;
  });
  let secReady = $state(false); // the popover is placed once its size is known (no jump on opening)
  $effect(() => {
    if (!secPop) return void (secReady = false);
    const f = requestAnimationFrame(() => (secReady = true));
    return () => cancelAnimationFrame(f);
  });

  function onSections(list: SectionInfo[]) {
    sectionList = list;
    app.noteSections = list.map(({ name, color }) => ({ name, color }));
  }

  /** Show a note's text (the editor brings back the sections collapsed last time, kept per note in this browser). */
  function show(text: string, ro = false) {
    editor?.setDoc(text, ro, note ? `notes.folds.${app.me?.id}:${note.id}` : null);
  }

  const rectOf = (el: Element | null | undefined): Rect => {
    const r = el?.getBoundingClientRect();
    return r ? { left: r.left, top: r.top, bottom: r.bottom, right: r.right } : { left: window.innerWidth - 8, right: window.innerWidth - 8, top: 56, bottom: 56 };
  };

  function openSecPop(kind: 'add' | 'list' | 'menu', index: number, at: Element | Rect | null | undefined) {
    if (!note || readonly || app.raw) return;
    secPop = { kind, index, rect: at && 'left' in at && !(at instanceof Element) ? at : rectOf(at as Element) };
  }

  function closeSecPop() {
    secPop = null;
    editor?.focus();
  }

  function pickSectionColor(name: string | null) {
    const p = secPop;
    secPop = null;
    if (!p) return;
    if (name) {
      secLast = name;
      localStorage.setItem('notes.secLast', name);
    }
    if (p.kind === 'add') {
      if (name) editor.addSection(name);
    } else if (name) editor.sectionColor(p.index, name);
    else editor.sectionRemove(p.index);
  }

  /** Run a menu action on its section (read before the menu closes: closing clears it). */
  function sectionAction(run: (index: number, s: SectionInfo) => void) {
    const k = secPop?.index;
    secPop = null;
    if (k !== undefined && sectionList[k]) run(k, sectionList[k]);
  }

  // ---- editing ----

  function onchange(text: string) {
    if (!note || !saver || saver.noteId !== note.id) return;
    pending = text;
    clearTimeout(countTimer);
    countTimer = setTimeout(() => (counted = pending), 300);
    saver.edit(text);
  }

  function saveNow(): Promise<void> {
    return saver?.flush() ?? Promise.resolve();
  }

  // ---- highlighting ----

  // the small bar above a selection (desktop only: on touch it would fight the system's own)
  // (viewport coordinates, kept inside the scroll area: above the selection, or below it near the top)
  const inView = $derived(!!hl.rect && !!box && hl.rect.bottom > box.top && hl.rect.top < box.bottom);
  const floatShown = $derived(!readonly && app.desktop && inView && !hl.empty && ((hl.focused && !hl.dragging) || hlMenu === 'float'));
  const barPos = $derived.by(() => {
    if (!hl.rect || !box) return { left: 0, top: 0 };
    const above = hl.rect.top - barH - 8;
    const top = above >= box.top + 4 ? above : hl.rect.bottom + 8;
    const left = Math.min(Math.max(hl.rect.left - 8, box.left + 4), box.right - barW - 4);
    return { left, top };
  });
  const popPos = $derived.by(() => {
    const below = barPos.top + barH + 6;
    const top = below + popH <= window.innerHeight - 8 ? below : Math.max(8, barPos.top - popH - 6);
    const left = Math.min(Math.max(barPos.left, 8), window.innerWidth - popW - 8);
    return { left, top };
  });

  // the raw text has no colours or sections to pick: their buttons go away, so focus on them (or in a
  // popover) goes back to the note instead of the page. Back in the rendered view, collapsed sections
  // come back.
  let wasRaw = untrack(() => app.raw);
  $effect(() => {
    const r = app.raw;
    untrack(() => {
      if (r && !wasRaw && (hlMenu || secPop || document.activeElement === document.body)) {
        hlMenu = null;
        secPop = null;
        editor?.focus();
      }
      wasRaw = r;
    });
  });

  // A plain click on the end mark, or on the empty space under the note (in line with the text, not
  // the side margins), puts the cursor at the end. Only a click that also started there: a drag from the
  // text that ends below must keep its selection.
  let pressBelow: { x: number; y: number } | null = null;

  function isBelow(e: MouseEvent): boolean {
    if (!note || readonly || e.button !== 0 || e.ctrlKey || e.metaKey || e.altKey) return false;
    const t = e.target as HTMLElement;
    if (t.closest('.note-end')) return true;
    if (t.closest('.cm-editor, .note-tags, button, input, a, [role="menu"]')) return false;
    const host = scroller?.querySelector('.editor-host')?.getBoundingClientRect();
    return !!host && e.clientY > host.bottom && e.clientX >= host.left && e.clientX <= host.right;
  }

  function pressScroll(e: MouseEvent) {
    pressBelow = isBelow(e) ? { x: e.clientX, y: e.clientY } : null;
  }

  function clickScroll(e: MouseEvent) {
    const p = pressBelow;
    pressBelow = null;
    if (!p || !isBelow(e) || Math.hypot(e.clientX - p.x, e.clientY - p.y) >= 6) return;
    if (e.shiftKey) editor.extendToEnd(); // as Shift+click under the text does in other editors
    else editor.focus(true);
  }

  function toggleRaw() {
    app.setRaw(!app.raw);
    if (app.desktop) editor?.focus(); // keep typing; on a phone that would pop the keyboard up
  }

  function onSelectionInfo(i: HighlightInfo) {
    hl = i;
    box = scroller?.getBoundingClientRect() ?? null;
  }

  function pickHighlight(name: string | null) {
    hlMenu = null;
    editor.highlight(name);
    if (name && isHlName(name)) {
      hlLast = name;
      localStorage.setItem('notes.hlLast', name);
    }
  }

  /** Bold / underline / clear. The grid stays open after a toggle, so B then U works in one go. */
  function format(op: StyleOp, focus = app.desktop): boolean {
    return editor.format(op, focus);
  }
  $effect(() => {
    app.noteReadonly = readonly;
  });
  // Esc closes the colour menu even when the editor has focus (a click on Bold keeps it there)
  $effect(() => {
    if (!hlMenu) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      e.preventDefault();
      closeHighlightMenu();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  });
  // from the grid, focus stays on its buttons (a keyboard user can press Bold, then Underline)
  const formatting = $derived({ bold: hl.bold, underline: hl.underline, hasColor: hl.hasColor, styled: hl.styled, onformat: (op: StyleOp) => format(op, false) });

  function closeHighlightMenu() {
    hlMenu = null;
    editor.focus();
  }

  function openHighlightMenu() {
    if (!hl.can || readonly) return;
    hlMenu = floatShown ? 'float' : 'bar';
  }

  // ---- conflict resolution ----

  function keepMine() {
    void saver?.keepMine();
  }

  function useTheirs() {
    if (!saver?.conflict) return;
    const body = saver.useTheirs();
    setText(body);
    show(body);
  }

  async function saveMineAsNew() {
    if (!note || !saver) return;
    try {
      const m = await api.createNote(saver.pending, note.tag_ids);
      app.notes = [m, ...app.notes];
      useTheirs();
      app.toast('Your version was saved as a new note.', 'success', { label: 'Open', run: () => app.select(m.id) });
    } catch (e) {
      app.error(e);
    }
  }

  // ---- note actions ----

  async function patch(p: { pinned?: boolean; tag_ids?: string[]; trashed?: boolean }) {
    if (!note) return;
    try {
      const m = await api.patchNote(note.id, p);
      Object.assign(note, { pinned: m.pinned, tag_ids: m.tag_ids, trashed_at: m.trashed_at });
      app.upsert(m);
      if (p.tag_ids) app.loadTags();
      return m;
    } catch (e) {
      app.error(e);
    }
  }

  async function trash() {
    if (!note) return;
    await saveNow(); // the server refuses body saves to trashed notes
    if (saver?.dirty || saver?.conflict) {
      app.toast('Resolve the unsaved changes before moving this note to the trash.', 'error');
      return;
    }
    const nid = note.id;
    const m = await patch({ trashed: true });
    if (!m) return;
    app.remove(nid);
    app.loadTags();
    app.select(app.notes[0]?.id ?? null);
    app.toast('Moved to trash.', 'info', {
      label: 'Undo',
      run: async () => {
        const r = await api.patchNote(nid, { trashed: false });
        app.loadList();
        app.loadTags();
        app.select(r.id);
      },
    });
  }

  async function restore() {
    if (!note) return;
    const nid = note.id;
    if (await patch({ trashed: false })) {
      app.remove(nid);
      app.loadTags();
      // uploads canceled or re-minted while it was in the trash couldn't be saved then
      const fixed = fixRetiredRefs(pending);
      show(fixed);
      goLive(nid); // upload cancels / ref changes must reach the editor again
      if (fixed !== pending) {
        setText(fixed);
        saver?.edit(fixed);
      }
      app.toast('Restored.', 'success', { label: 'Open', run: () => app.setView('all') });
    }
  }

  async function purge() {
    if (!note || !confirm('Delete this note and its attachments forever? This cannot be undone.')) return;
    try {
      await api.purge(note.id);
      drafts.delete(draftKey(note.id));
      forgetSaver(note.id);
      app.remove(note.id);
      app.select(null);
      app.toast('Deleted forever.', 'info');
    } catch (e) {
      app.error(e);
    }
  }

  // ---- save as web page (SPEC §23) ----
  let pageBusy = $state(false);
  async function saveAsPage() {
    if (!note || pageBusy) return;
    menuOpen = false;
    const id = note.id; // the note the action was for, even if another opens meanwhile
    const ro = readonly;
    pageBusy = true;
    // checked again just before downloading: typing or an upload may start while waiting
    const ready = (): boolean => {
      const s = existingSaver(id);
      if (!ro && s && (s.dirty || s.conflict || s.status === 'error')) {
        app.toast('Save your changes first: the page is made from the saved note.', 'error');
        return false;
      }
      if (uploads.tasks.some((t) => t.noteId === id && (t.status === 'uploading' || t.status === 'retrying'))) {
        app.toast('Wait for the upload to finish: the page includes every file.', 'info');
        return false;
      }
      return true;
    };
    try {
      if (!ro) await saveNow();
      if (!ready()) return;
      const { bytes: size } = await api.pageSize(id, app.theme);
      if (size > 100 * 1024 * 1024 && !confirm(`This page will be about ${bytes(size)} because it includes every file. Very large pages can be slow to open, and some tunnels or proxies refuse downloads this big.\n\nDownload it?`)) return;
      if (!ready()) return;
      await downloadPage(id, app.theme);
    } catch (e) {
      app.error(e);
    } finally {
      pageBusy = false;
    }
  }

  async function duplicate() {
    if (!note) return;
    menuOpen = false;
    await saveNow(); // so the copy has what you just typed
    if (saver?.dirty || saver?.conflict) {
      app.toast('Resolve the unsaved changes before duplicating this note.', 'error');
      return;
    }
    try {
      const m = await api.duplicate(note.id);
      app.notes = [m, ...app.notes];
      app.select(m.id);
      app.toast('Duplicated.', 'success');
    } catch (e) {
      app.error(e);
    }
  }

  // ---- files ----

  const INLINE = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);

  function screenshotName(f: File) {
    if (f.name && !/^image\.(png|jpe?g|gif|webp)$/i.test(f.name)) return f.name;
    const ext = f.type.split('/')[1]?.replace('jpeg', 'jpg') ?? 'png';
    const d = new Date();
    const p = (n: number) => String(n).padStart(2, '0');
    return `screenshot-${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}.${ext}`;
  }

  function handleFiles(files: File[], at: 'cursor' | number) {
    if (!note || readonly) return;
    const images = files.filter((f) => INLINE.has(f.type));
    const others = files.filter((f) => !INLINE.has(f.type));
    const items = images.map((f) => {
      const name = screenshotName(f);
      const t = uploads.start(f, name, note!.id, true);
      return { ref: t.ref, filename: name };
    });
    if (items.length) editor.insertMarkers(items, at);
    for (const f of others) uploads.start(f, f.name || 'file', note.id, false);
    if (others.length) attOpen = true;
  }

  function addFilesFromPanel(files: File[]) {
    const images = files.filter((f) => INLINE.has(f.type));
    const others = files.filter((f) => !INLINE.has(f.type));
    if (images.length) {
      editor.focusForInsert();
      handleFiles(images, 'cursor');
    }
    for (const f of others) uploads.start(f, f.name || 'file', note!.id, false);
  }

  onMount(() => {
    const offDone = uploads.onDone((t, att) => {
      if (!note || t.noteId !== note.id) return;
      if (!note.attachments.some((a) => a.id === att.id)) note.attachments = [...note.attachments, att];
      app.upsert({ id: note.id, attachment_count: note.attachments.length });
    });
    const onLightbox = (e: Event) => (lightboxRef = (e as CustomEvent).detail.ref);
    const onSectionMenu = (e: Event) => {
      const d = (e as CustomEvent).detail;
      openSecPop('menu', d.index, d.rect);
    };
    const onAddSection = () => openSecPop('add', -1, document.querySelector('.note-bar button[aria-label="Add section"]')?.getClientRects().length ? document.querySelector('.note-bar button[aria-label="Add section"]') : moreBtn);
    const onGotoSection = (e: Event) => editor?.sectionGo((e as CustomEvent).detail);
    const onSectionKeys = () => {
      if (!editor?.openSectionMenu()) app.toast('Put the cursor in a section first.', 'info');
    };
    const onSave = () => void saveNow();
    const onSavePage = () => void saveAsPage();
    const onFormat = (e: Event) => {
      if (readonly || app.raw) return;
      if (!format((e as CustomEvent).detail as StyleOp)) app.toast('Select some text first.', 'info');
    };
    const onHide = () => {
      if (document.visibilityState === 'hidden') {
        saver?.writeDraft();
        void saveNow();
      }
    };
    // beforeunload may be canceled ("Stay"), so only drafts are written there;
    // the keepalive save goes out on pagehide, when the page is really leaving.
    const onUnload = (e: BeforeUnloadEvent) => {
      draftAll();
      if (anyUnsaved() || uploads.active) e.preventDefault();
    };
    const onPageHide = () => unloadAll();
    window.addEventListener('notes:lightbox', onLightbox);
    window.addEventListener('notes:section-menu', onSectionMenu);
    window.addEventListener('notes:add-section', onAddSection);
    window.addEventListener('notes:goto-section', onGotoSection);
    window.addEventListener('notes:section-options', onSectionKeys);
    window.addEventListener('notes:save', onSave);
    window.addEventListener('notes:save-page', onSavePage);
    window.addEventListener('notes:format', onFormat);
    document.addEventListener('visibilitychange', onHide);
    window.addEventListener('beforeunload', onUnload);
    window.addEventListener('pagehide', onPageHide);
    return () => {
      setLiveEditor(null);
      if (saver) releaseSaver(saver, () => {});
      offDone();
      window.removeEventListener('notes:lightbox', onLightbox);
      window.removeEventListener('notes:section-menu', onSectionMenu);
      window.removeEventListener('notes:add-section', onAddSection);
      window.removeEventListener('notes:goto-section', onGotoSection);
      window.removeEventListener('notes:section-options', onSectionKeys);
      app.noteSections = [];
      window.removeEventListener('notes:save', onSave);
      window.removeEventListener('notes:save-page', onSavePage);
      window.removeEventListener('notes:format', onFormat);
      document.removeEventListener('visibilitychange', onHide);
      window.removeEventListener('beforeunload', onUnload);
      window.removeEventListener('pagehide', onPageHide);
    };
  });

  const lightboxImages = $derived.by(() => {
    if (!note || !lightboxRef) return [];
    const refs = refsIn(pending);
    const byRef = new Map(note.attachments.map((a) => [a.ref, a]));
    const ordered: Attachment[] = [];
    for (const r of refs) {
      const a = byRef.get(r);
      if (a?.inline) ordered.push(a);
    }
    for (const a of note.attachments) if (a.inline && !refs.has(a.ref)) ordered.push(a);
    if (!ordered.some((a) => a.ref === lightboxRef)) {
      const s = registry.get(lightboxRef);
      if (s.kind === 'ready') ordered.unshift(s.att);
    }
    return ordered;
  });

  const statusLabel: Record<SaveStatus, string> = {
    saved: 'Saved',
    dirty: 'Editing…',
    saving: 'Saving…',
    error: 'Not saved',
    conflict: 'Needs attention',
  };
</script>

<section class="note" class:has-panel={attOpen} class:trashed={readonly} style={accent !== null ? `--accent-h:${accent}` : ''}>
  {#if loadError}
    <div class="empty">
      <div class="orb"></div>
      <h2>{loadError}</h2>
      <button class="btn" onclick={() => app.select(null)}>Back to notes</button>
    </div>
  {/if}

  <div class="note-main" class:hidden={!!loadError}>
    <header class="note-bar">
      <button class="icon-btn mobile-only" aria-label="Back to list" onclick={() => app.select(null)}><Icon name="back" /></button>
      {#if app.listCollapsed && app.desktop && !app.focusMode}
        <button class="icon-btn desktop-only" title="Show notes list (Ctrl+Shift+\\)" aria-label="Show notes list" aria-expanded="false" onclick={() => app.setListCollapsed(false)}><Icon name="list" /></button>
      {/if}
      <div class="status" data-status={status} title={statusDetail || statusLabel[status]} role="status" aria-live="polite">
        <span class="status-dot" aria-hidden="true"></span><span class="status-text">{statusLabel[status]}{#if statusDetail}<span class="sr-only">: {statusDetail}</span>{/if}</span>
      </div>
      {#if note}
        <span class="muted small hide-narrow" title={fullTime(note.updated_at)}>Edited {relativeTime(note.updated_at)}</span>
      {/if}
      <div class="spacer"></div>
      {#if note}
        <button
          class="icon-btn hide-phone"
          class:on={app.raw}
          title={app.raw ? 'Show the rendered note (Ctrl+/)' : 'Edit the raw text (Ctrl+/)'}
          aria-label="Raw text"
          aria-pressed={app.raw}
          onclick={toggleRaw}><Icon name="code" /></button>
      {/if}
      {#if note && !readonly}
        {#if !app.raw}
        <div class="menu-wrap">
          <button
            class="icon-btn"
            title="Highlight (Ctrl+Shift+H)"
            aria-label="Highlight"
            aria-haspopup="grid"
            aria-expanded={hlMenu === 'bar'}
            disabled={!hl.can}
            onmousedown={(e) => e.preventDefault()}
            onclick={() => (hlMenu = hlMenu === 'bar' ? null : 'bar')}><Icon name="highlight" /></button>
          {#if hlMenu === 'bar'}
            <button class="menu-scrim" tabindex="-1" aria-hidden="true" onmousedown={(e) => e.preventDefault()} onclick={closeHighlightMenu}></button>
            <HighlightMenu class="hl-pop-bar" current={hl.current} last={hlLast} onpick={pickHighlight} onclose={closeHighlightMenu} {formatting} />
          {/if}
        </div>
        <button
          class="icon-btn hide-phone"
          title="Add section"
          aria-label="Add section"
          aria-haspopup="grid"
          aria-expanded={secPop?.kind === 'add'}
          disabled={!hasTitle}
          onmousedown={(e) => e.preventDefault()}
          onclick={(e) => openSecPop('add', -1, e.currentTarget)}><Icon name="section" /></button>
        {#if sectionList.length}
          <button class="icon-btn hide-phone" title="Sections" aria-label="Sections" aria-haspopup="menu" aria-expanded={secPop?.kind === 'list'} onclick={(e) => openSecPop('list', -1, e.currentTarget)}><Icon name="toc" /></button>
        {/if}
        {/if}
        <button class="icon-btn" class:on={note.pinned} title={note.pinned ? 'Unpin' : 'Pin to top'} aria-label={note.pinned ? 'Unpin' : 'Pin to top'} aria-pressed={note.pinned} onclick={() => patch({ pinned: !note!.pinned })}>
          <Icon name="pin" />
        </button>
        <button class="icon-btn" title="Attach files" aria-label="Attach files" onclick={() => picker?.click()}><Icon name="clip" /></button>
        <input
          type="file"
          multiple
          hidden
          bind:this={picker}
          onchange={(e) => {
            const f = [...((e.target as HTMLInputElement).files ?? [])];
            if (f.length) addFilesFromPanel(f);
            (e.target as HTMLInputElement).value = '';
          }}
        />
      {/if}
      {#if note}
        <button class="icon-btn att-toggle" class:on={attOpen} title="Attachments" aria-label="Attachments ({note.attachments.length})" aria-pressed={attOpen} onclick={() => (attOpen = !attOpen)}>
          <Icon name="files" />{#if note.attachments.length}<span class="count-badge" aria-hidden="true">{note.attachments.length}</span>{/if}
        </button>
        {#if readonly}
          <button class="icon-btn" title="Save as web page" aria-label="Save as web page" disabled={pageBusy} onclick={saveAsPage}><Icon name="page" /></button>
          <button class="btn small" onclick={restore}><Icon name="restore" size={15} /> Restore</button>
          <button class="btn small danger" onclick={purge}><Icon name="trash" size={15} /> Delete forever</button>
        {:else}
          <div class="menu-wrap">
            <button class="icon-btn" bind:this={moreBtn} title="More" aria-label="More actions" aria-haspopup="menu" aria-expanded={menuOpen} onclick={() => (menuOpen = !menuOpen)}><Icon name="more" /></button>
            {#if menuOpen}
              <button class="menu-scrim" tabindex="-1" aria-hidden="true" onclick={() => (menuOpen = false)}></button>
              <div class="menu menu-right" role="menu" use:menu={{ onclose: () => (menuOpen = false) }}>
                <button role="menuitem" data-first onclick={duplicate}><Icon name="copy" size={16} /> Duplicate</button>
                <button role="menuitem" disabled={pageBusy} onclick={saveAsPage}><Icon name="page" size={16} /> Save as web page</button>
                {#if !app.raw}
                  <button role="menuitem" disabled={!hasTitle} onclick={() => ((menuOpen = false), openSecPop('add', -1, moreBtn))}><Icon name="section" size={16} /> Add section</button>
                  {#if sectionList.length}
                    <button role="menuitem" onclick={() => ((menuOpen = false), openSecPop('list', -1, moreBtn))}><Icon name="toc" size={16} /> Sections</button>
                  {/if}
                {/if}
                <button role="menuitemcheckbox" aria-checked={app.raw} onclick={() => ((menuOpen = false), toggleRaw())}><Icon name="code" size={16} /> Raw text</button>
                <button role="menuitem" onclick={() => ((menuOpen = false), app.setMono(!app.mono))}><Icon name="type" size={16} /> {app.mono ? 'Proportional font' : 'Monospace font'}</button>
                <button role="menuitem" onclick={() => ((menuOpen = false), (app.focusMode = !app.focusMode))}><Icon name="focus" size={16} /> {app.focusMode ? 'Exit focus mode' : 'Focus mode'}</button>
                <hr />
                <div class="menu-label" id="width-label"><Icon name="width" size={14} /> Page width</div>
                <div role="group" aria-labelledby="width-label">
                  {#each PAGE_WIDTHS as w (w.id)}
                    <button role="menuitemradio" aria-checked={app.pageWidth === w.id} title={w.hint} onclick={() => ((menuOpen = false), app.setPageWidth(w.id))}>
                      <span class="check">{#if app.pageWidth === w.id}<Icon name="check" size={14} />{/if}</span>{w.label}
                    </button>
                  {/each}
                </div>
                <hr />
                <button role="menuitem" class="danger" onclick={() => ((menuOpen = false), trash())}><Icon name="trash" size={16} /> Move to trash</button>
              </div>
            {/if}
          </div>
        {/if}
      {/if}
    </header>

    {#if conflict}
      <ConflictBar mine={pending} theirs={conflict.body} reason={conflict.reason} onkeepmine={keepMine} onusetheirs={useTheirs} onsaveasnew={saveMineAsNew} />
    {/if}
    {#if readonly && note}
      <div class="trash-banner"><Icon name="trash" size={16} /> This note is in the trash{app.trashDays > 0 ? ` and will be deleted after ${app.trashDays} days` : ''}. Restore it to edit.</div>
    {/if}

    <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions (a mouse shortcut; the keyboard has Ctrl+End / Cmd+End) -->
    <div class="note-scroll" bind:this={scroller} onscroll={() => editor?.report()} onmousedown={pressScroll} onclick={clickScroll}>
      <div class="page" class:mono={app.mono}>
        {#if note}
          <div class="note-tags">
            <TagInput tagIds={note.tag_ids} disabled={readonly} onchange={(ids) => patch({ tag_ids: ids })} />
          </div>
        {:else if !loadError}
          <div class="note-skeleton" aria-hidden="true"><span></span><span></span><span></span></div>
        {/if}
        <div class:invisible={!note}>
          <Editor bind:this={editor} raw={app.raw} styles={floatShown || hlMenu !== null} onsections={onSections} {onchange} onfiles={handleFiles} onsave={saveNow} onblur={saveNow} onselection={onSelectionInfo} onhighlightkey={openHighlightMenu} />
        </div>
        {#if note && hasText}
          <!-- end of the note: drawn, never part of the text -->
          <div class="note-end" aria-hidden="true">
            <span class="end-rule"></span>
            <svg width="24" height="20" viewBox="0 0 24 20"><circle cx="12" cy="4.5" r="2.6" /><circle cx="5.5" cy="15" r="2.6" /><circle cx="18.5" cy="15" r="2.6" /></svg>
            <span class="end-rule"></span>
          </div>
        {/if}
      </div>
    </div>

    {#if note}
      <footer class="note-foot muted small">
        <span>{words.toLocaleString()} {words === 1 ? 'word' : 'words'}</span>
        <span>{plain.length.toLocaleString()} characters</span>
        {#if app.raw}
          <span class="raw-note">Raw text · Ctrl+/ shows the note</span>
        {:else}
          <span class="hide-narrow">Paste screenshots · drop files · Alt+T inserts time</span>
        {/if}
      </footer>
    {/if}
  </div>

  {#if note && attOpen}
    <Attachments
      noteId={note.id}
      attachments={note.attachments}
      {shownRefs}
      {readonly}
      onchange={(atts) => (note!.attachments = atts)}
      onfiles={addFilesFromPanel}
      onopen={(ref) => (lightboxRef = ref)}
      onclose={() => (attOpen = false)}
    />
  {/if}

  {#if floatShown}
    <!-- the little bar above a selection (desktop); mousedown is prevented so the editor keeps its selection -->
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="hl-float" style="left:{barPos.left}px; top:{barPos.top}px" bind:offsetWidth={barW} bind:offsetHeight={barH} onmousedown={(e) => e.preventDefault()}>
      <button class="icon-btn tiny hl-fmt" title="Bold (Ctrl+B)" aria-label="Bold" aria-pressed={hl.bold === 'all' ? 'true' : hl.bold === 'some' ? 'mixed' : 'false'} onclick={() => format({ kind: 'toggle', style: 'b' })}><b>B</b></button>
      <button class="icon-btn tiny hl-fmt" title="Underline (Ctrl+U)" aria-label="Underline" aria-pressed={hl.underline === 'all' ? 'true' : hl.underline === 'some' ? 'mixed' : 'false'} onclick={() => format({ kind: 'toggle', style: 'u' })}><u>U</u></button>
      <span class="hl-float-sep" aria-hidden="true"></span>
      {#if hlLast}
        <button class="hl-float-last" title="Highlight ({hlLast})" aria-label="Highlight in the last colour" style="--h:{hlHue(hlLast)}" class:deep={hlLast.endsWith('-deep')} onclick={() => pickHighlight(hlLast)}><span>a</span></button>
      {/if}
      <button class="icon-btn tiny" title="Highlight colours (Ctrl+Shift+H)" aria-label="Highlight colours" aria-haspopup="grid" aria-expanded={hlMenu === 'float'} onclick={() => (hlMenu = hlMenu === 'float' ? null : 'float')}><Icon name="highlight" size={16} /></button>
      {#if hl.hasColor}
        <button class="icon-btn tiny" title="Remove colour" aria-label="Remove colour" onclick={() => pickHighlight(null)}><Icon name="x" size={14} /></button>
      {/if}
      {#if hl.styled}
        <button class="icon-btn tiny" title="Clear formatting" aria-label="Clear formatting" onclick={() => format({ kind: 'clear' })}><Icon name="clear-format" size={15} /></button>
      {/if}
    </div>
    {#if hlMenu === 'float'}
      <button class="menu-scrim" tabindex="-1" aria-hidden="true" onmousedown={(e) => e.preventDefault()} onclick={closeHighlightMenu}></button>
      <div class="hl-pop-float" style="left:{popPos.left}px; top:{popPos.top}px" bind:offsetWidth={popW} bind:offsetHeight={popH}>
        <HighlightMenu current={hl.current} last={hlLast} onpick={pickHighlight} onclose={closeHighlightMenu} />
      </div>
    {/if}
  {/if}

  {#if secPop}
    <button class="menu-scrim" tabindex="-1" aria-hidden="true" onmousedown={(e) => e.preventDefault()} onclick={closeSecPop}></button>
    <div class="sec-pop" style="left:{secPos.left}px; top:{secPos.top}px; opacity:{secReady ? 1 : 0}" bind:offsetWidth={secPopW} bind:offsetHeight={secPopH}>
      {#if secPop.kind === 'add' || secPop.kind === 'color'}
        <HighlightMenu
          label={secPop.kind === 'add' ? 'New section colour' : 'Section colour'}
          removeLabel="Remove section (keeps its text)"
          current={secPop.kind === 'color' ? (sectionList[secPop.index]?.color ?? null) : null}
          last={secLast}
          onpick={pickSectionColor}
          onclose={closeSecPop} />
      {:else if secPop.kind === 'list'}
        <div class="menu sec-list" role="menu" aria-label="Sections" use:menu={{ onclose: () => (secPop = null) }}>
          {#each sectionList as s, i (i)}
            <button role="menuitem" onclick={() => ((secPop = null), editor.sectionGo(i))}>
              <span class="sec-dot" style="--h:{hlHue(s.color)}" aria-hidden="true"></span><span class="grow">{s.name}</span>{#if s.folded}<span class="muted small">collapsed</span>{/if}
            </button>
          {/each}
        </div>
      {:else}
        {@const i = secPop.index}
        {@const s = sectionList[i]}
        <div class="menu" role="menu" aria-label="Section {s?.name ?? ''}" use:menu={{ onclose: () => (secPop = null) }}>
          <button role="menuitem" data-first onclick={() => secPop && (secPop = { ...secPop, kind: 'color' })}><span class="sec-dot" style="--h:{hlHue(s?.color ?? 'teal')}" aria-hidden="true"></span> Colour…</button>
          <button role="menuitem" disabled={i === 0} onclick={() => sectionAction((k) => editor.sectionMove(k, -1))}><Icon name="up" size={16} /> Move up</button>
          <button role="menuitem" disabled={i >= sectionList.length - 1} onclick={() => sectionAction((k) => editor.sectionMove(k, 1))}><Icon name="down" size={16} /> Move down</button>
          <button role="menuitem" disabled={!s?.foldable} onclick={() => sectionAction((k, x) => (editor.sectionFold(k, !x.folded), editor.focus()))}><Icon name="fold" size={16} /> {s?.folded ? 'Show section' : 'Collapse section'}</button>
          <hr />
          <button role="menuitem" class="danger" onclick={() => sectionAction((k) => editor.sectionRemove(k))}><Icon name="x" size={16} /> Remove section <span class="muted small">keeps text</span></button>
        </div>
      {/if}
    </div>
  {/if}

  {#if lightboxRef && lightboxImages.length}
    <Lightbox images={lightboxImages} start={lightboxRef} onclose={() => (lightboxRef = null)} />
  {/if}
</section>
