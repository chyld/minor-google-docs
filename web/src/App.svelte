<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from './lib/api';
  import { app } from './lib/state.svelte';
  import Login from './components/Login.svelte';
  import Sidebar from './components/Sidebar.svelte';
  import NoteList from './components/NoteList.svelte';
  import NoteView from './components/NoteView.svelte';
  import Palette from './components/Palette.svelte';
  import Account from './components/Account.svelte';
  import Overlays from './components/Overlays.svelte';
  import Icon from './components/Icon.svelte';

  let list: NoteList | undefined = $state();
  let listWidth = $state(Number(localStorage.getItem('notes.listWidth')) || 340);

  const idFromPath = () => location.pathname.match(/^\/n\/([0-9a-f-]{36})$/)?.[1] ?? null;

  // theme
  $effect(() => {
    const mq = matchMedia('(prefers-color-scheme: dark)');
    const apply = () => (document.documentElement.dataset.theme = app.theme === 'system' ? (mq.matches ? 'dark' : 'light') : app.theme);
    apply();
    mq.addEventListener('change', apply);
    return () => mq.removeEventListener('change', apply);
  });

  // after login: load data
  $effect(() => {
    if (!app.me) return;
    api.setup().then((s) => (app.trashDays = s.trash_days)).catch(() => {});
    app.loadTags();
    app.loadList().then(() => {
      const id = idFromPath();
      if (id) app.select(id, false);
      else if (app.notes.length && matchMedia('(min-width: 861px)').matches) app.select(app.notes[0].id, false);
    });
  });

  onMount(() => {
    api
      .me()
      .then((u) => (app.me = u))
      .catch(() => {})
      .finally(() => (app.booting = false));
    const onPop = () => app.select(idFromPath(), false);
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  });

  function inEditable(t: EventTarget | null) {
    const el = t as HTMLElement | null;
    return !!el && (el.isContentEditable || el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || !!el.closest?.('.cm-editor'));
  }

  const modalOpen = () => app.paletteOpen || app.accountOpen || app.shortcutsOpen || !!document.querySelector('[aria-modal="true"]');

  function onkeydown(e: KeyboardEvent) {
    if (!app.me) return;
    const mod = e.ctrlKey || e.metaKey;
    const k = e.key.toLowerCase();
    if (mod && k === 'k') {
      e.preventDefault();
      app.paletteOpen = !app.paletteOpen;
    } else if (e.altKey && k === 'n' && !mod) {
      e.preventDefault();
      app.newNote();
    } else if (mod && e.shiftKey && k === 'f') {
      e.preventDefault();
      app.focusMode = !app.focusMode;
    } else if (mod && e.key === '\\') {
      e.preventDefault();
      if (matchMedia('(max-width: 860px)').matches) app.sidebarOpen = !app.sidebarOpen;
      else {
        app.sidebarHidden = !app.sidebarHidden;
        localStorage.setItem('notes.sidebarHidden', app.sidebarHidden ? '1' : '0');
      }
    } else if (mod && k === 's' && !e.defaultPrevented) {
      e.preventDefault(); // never the browser's "save page"; the open note saves now
      window.dispatchEvent(new CustomEvent('notes:save'));
    } else if (e.key === 'Escape' && app.focusMode && !modalOpen()) {
      app.focusMode = false;
    } else if (!inEditable(e.target) && !mod && !e.altKey && !modalOpen()) {
      if (e.key === '/') {
        e.preventDefault();
        app.focusMode = false;
        list?.focusSearch();
      } else if (e.key === '?') {
        e.preventDefault();
        app.shortcutsOpen = true;
      }
    }
  }

  function startResize(e: PointerEvent) {
    const startX = e.clientX;
    const start = listWidth;
    const move = (ev: PointerEvent) => (listWidth = Math.min(560, Math.max(260, start + ev.clientX - startX)));
    const up = () => {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', up);
      localStorage.setItem('notes.listWidth', String(listWidth));
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
  }
</script>

<svelte:window {onkeydown} />

{#if app.booting}
  <div class="boot"><span class="logo big pulse"></span></div>
{:else if !app.me}
  <Login />
{:else}
  <div class="shell" class:focus={app.focusMode} class:no-sidebar={app.sidebarHidden} data-pane={app.mobilePane} style="--list-w:{listWidth}px">
    <Sidebar />
    <NoteList bind:this={list} />
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="resizer" onpointerdown={startResize} title="Drag to resize"></div>
    <main class="main">
      {#if app.selectedId}
        <NoteView id={app.selectedId} />
      {:else}
        <div class="empty">
          <div class="orb"></div>
          <h2>Pick a note, or start a fresh one</h2>
          <p class="muted">Everything is plain text, saved as you type.</p>
          <button class="btn primary" onclick={() => app.newNote()}><Icon name="plus" size={16} /> New note</button>
          <p class="muted small hints"><kbd>Ctrl</kbd> <kbd>K</kbd> jump anywhere · <kbd>?</kbd> shortcuts</p>
        </div>
      {/if}
    </main>
    {#if app.focusMode}
      <button class="focus-exit" onclick={() => (app.focusMode = false)} title="Exit focus mode (Esc)"><Icon name="focus" size={16} /></button>
    {/if}
  </div>
  {#if app.paletteOpen}<Palette />{/if}
  {#if app.accountOpen}<Account />{/if}
{/if}
<Overlays />
