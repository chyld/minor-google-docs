<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import { api, ApiError } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import { drafts } from '../lib/drafts';
  import { uploads } from '../lib/uploads.svelte';
  import { registry } from '../lib/registry';
  import { refsIn } from '../lib/editor/markers';
  import { fullTime, relativeTime, wordCount } from '../lib/format';
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
  import type { HighlightInfo } from '../lib/editor/highlights';

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

  let hl = $state<HighlightInfo>({ can: false, empty: true, current: null, focused: false, dragging: false, rect: null });
  let hlMenu = $state<'bar' | 'float' | null>(null);
  let hlLast = $state<string | null>(isHlName(localStorage.getItem('notes.hlLast') ?? '') ? localStorage.getItem('notes.hlLast') : null);
  let scroller: HTMLDivElement | undefined = $state();
  let box = $state<DOMRect | null>(null); // the scroll area, when the selection was last reported
  let barW = $state(90);
  let barH = $state(36);
  let popW = $state(430);
  let popH = $state(110);

  const readonly = $derived(!!note?.trashed_at);
  const plain = $derived(app.raw ? counted : stripHighlights(counted)); // counts what is on screen: delimiters only in raw text
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
      }
      app.upsert({ id: s.noteId, title: r.title, preview: r.preview, updated_at: r.updated_at, sha256: r.sha256, size_bytes: r.size_bytes });
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
        editor?.setDoc(live.pending);
        goLive(nid);
        void live.flush();
        return;
      }
      const s = createSaver(nid, draftKey(nid), n.body, n.sha256);
      attachSaver(s);
      if (d && d.body !== n.body && !n.trashed_at) {
        s.pending = d.body;
        setText(d.body);
        editor?.setDoc(d.body);
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
      editor?.setDoc(n.body, !!n.trashed_at);
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

  // the raw text has no colours to pick: their button goes away, so focus on it (or in the colours)
  // goes back to the note instead of the page
  let wasRaw = untrack(() => app.raw);
  $effect(() => {
    const r = app.raw;
    untrack(() => {
      if (r && !wasRaw && (hlMenu || document.activeElement === document.body)) {
        hlMenu = null;
        editor?.focus();
      }
      wasRaw = r;
    });
  });

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
    editor.setDoc(body);
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
      editor.setDoc(fixed);
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
    const onSave = () => void saveNow();
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
    window.addEventListener('notes:save', onSave);
    document.addEventListener('visibilitychange', onHide);
    window.addEventListener('beforeunload', onUnload);
    window.addEventListener('pagehide', onPageHide);
    return () => {
      setLiveEditor(null);
      if (saver) releaseSaver(saver, () => {});
      offDone();
      window.removeEventListener('notes:lightbox', onLightbox);
      window.removeEventListener('notes:save', onSave);
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
            <HighlightMenu class="hl-pop-bar" current={hl.current} last={hlLast} onpick={pickHighlight} onclose={closeHighlightMenu} />
          {/if}
        </div>
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
          <button class="btn small" onclick={restore}><Icon name="restore" size={15} /> Restore</button>
          <button class="btn small danger" onclick={purge}><Icon name="trash" size={15} /> Delete forever</button>
        {:else}
          <div class="menu-wrap">
            <button class="icon-btn" title="More" aria-label="More actions" aria-haspopup="menu" aria-expanded={menuOpen} onclick={() => (menuOpen = !menuOpen)}><Icon name="more" /></button>
            {#if menuOpen}
              <button class="menu-scrim" tabindex="-1" aria-hidden="true" onclick={() => (menuOpen = false)}></button>
              <div class="menu menu-right" role="menu" use:menu={{ onclose: () => (menuOpen = false) }}>
                <button role="menuitem" data-first onclick={duplicate}><Icon name="copy" size={16} /> Duplicate</button>
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

    <div class="note-scroll" bind:this={scroller} onscroll={() => editor?.report()}>
      <div class="page" class:mono={app.mono}>
        {#if note}
          <div class="note-tags">
            <TagInput tagIds={note.tag_ids} disabled={readonly} onchange={(ids) => patch({ tag_ids: ids })} />
          </div>
        {:else if !loadError}
          <div class="note-skeleton" aria-hidden="true"><span></span><span></span><span></span></div>
        {/if}
        <div class:invisible={!note}>
          <Editor bind:this={editor} raw={app.raw} {onchange} onfiles={handleFiles} onsave={saveNow} onblur={saveNow} onselection={onSelectionInfo} onhighlightkey={openHighlightMenu} />
        </div>
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
      {#if hlLast}
        <button class="hl-float-last" title="Highlight ({hlLast})" aria-label="Highlight in the last colour" style="--h:{hlHue(hlLast)}" class:deep={hlLast.endsWith('-deep')} onclick={() => pickHighlight(hlLast)}><span>a</span></button>
      {/if}
      <button class="icon-btn tiny" title="Highlight colours (Ctrl+Shift+H)" aria-label="Highlight colours" aria-haspopup="grid" aria-expanded={hlMenu === 'float'} onclick={() => (hlMenu = hlMenu === 'float' ? null : 'float')}><Icon name="highlight" size={16} /></button>
      {#if hl.current}
        <button class="icon-btn tiny" title="Remove highlight" aria-label="Remove highlight" onclick={() => pickHighlight(null)}><Icon name="x" size={14} /></button>
      {/if}
    </div>
    {#if hlMenu === 'float'}
      <button class="menu-scrim" tabindex="-1" aria-hidden="true" onmousedown={(e) => e.preventDefault()} onclick={closeHighlightMenu}></button>
      <div class="hl-pop-float" style="left:{popPos.left}px; top:{popPos.top}px" bind:offsetWidth={popW} bind:offsetHeight={popH}>
        <HighlightMenu current={hl.current} last={hlLast} onpick={pickHighlight} onclose={closeHighlightMenu} />
      </div>
    {/if}
  {/if}

  {#if lightboxRef && lightboxImages.length}
    <Lightbox images={lightboxImages} start={lightboxRef} onclose={() => (lightboxRef = null)} />
  {/if}
</section>
