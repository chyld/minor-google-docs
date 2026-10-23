<script lang="ts">
  import { app } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { hue } from '../lib/colors';
  import type { NoteMeta } from '../lib/types';
  import { composing, dialog } from '../lib/a11y';
  import Icon from './Icon.svelte';
  import { PAGE_WIDTHS } from '../lib/layout';

  type Item =
    | { kind: 'note'; id: string; label: string; note: NoteMeta }
    | { kind: 'tag'; id: string; label: string; color: string }
    | { kind: 'action'; id: string; label: string; icon: string; hint?: string; run: () => void };

  let q = $state('');
  let active = $state(0);
  let results = $state<NoteMeta[]>([]);
  let list: HTMLUListElement | undefined = $state();
  let seq = 0;

  const actions: Item[] = [
    { kind: 'action', id: 'new', label: 'New note', icon: 'plus', hint: 'Alt N', run: () => app.newNote() },
    { kind: 'action', id: 'theme', label: 'Toggle dark mode', icon: 'moon', run: () => app.setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark') },
    { kind: 'action', id: 'trash', label: 'Go to trash', icon: 'trash', run: () => app.setView('trash') },
    { kind: 'action', id: 'raw', label: `Raw text${app.raw ? ' (on)' : ''}`, icon: 'code', hint: 'Ctrl /', run: () => app.setRaw(!app.raw) },
    { kind: 'action', id: 'focus', label: 'Toggle focus mode', icon: 'focus', hint: 'Ctrl ⇧ F', run: () => (app.focusMode = !app.focusMode) },
    ...(app.desktop
      ? ([
          { kind: 'action', id: 'sidebar', label: app.sidebarCollapsed ? 'Expand sidebar' : 'Collapse sidebar', icon: 'sidebar', hint: 'Ctrl \\', run: () => app.setSidebarCollapsed(!app.sidebarCollapsed) },
          { kind: 'action', id: 'list', label: app.listCollapsed ? 'Show notes list' : 'Hide notes list', icon: 'list', hint: 'Ctrl ⇧ \\', run: () => app.setListCollapsed(!app.listCollapsed) },
          ...PAGE_WIDTHS.map((w) => ({ kind: 'action', id: `width-${w.id}`, label: `Page width: ${w.label}${app.pageWidth === w.id ? ' (current)' : ''}`, icon: 'width', run: () => app.setPageWidth(w.id) })),
        ] as Item[])
      : []),
    { kind: 'action', id: 'account', label: 'Account & data', icon: 'user', run: () => (app.accountOpen = true) },
    { kind: 'action', id: 'keys', label: 'Keyboard shortcuts', icon: 'keyboard', hint: '?', run: () => (app.shortcutsOpen = true) },
    {
      kind: 'action',
      id: 'logout',
      label: 'Log out',
      icon: 'logout',
      run: async () => {
        await api.logout().catch(() => {});
        location.href = '/';
      },
    },
  ];

  function fuzzy(text: string, query: string): boolean {
    const t = text.toLowerCase();
    let i = 0;
    for (const c of query.toLowerCase()) {
      i = t.indexOf(c, i);
      if (i < 0) return false;
      i++;
    }
    return true;
  }

  $effect(() => {
    const query = q.trim();
    const s = ++seq;
    const run = async () => {
      try {
        const r = query ? await api.listNotes({ q: query, limit: 8 }) : await api.listNotes({ limit: 8 });
        if (s === seq) results = r.items.slice(0, 8);
      } catch {
        /* palette is best effort */
      }
    };
    const t = setTimeout(run, query ? 120 : 0);
    return () => clearTimeout(t);
  });

  const items = $derived.by<Item[]>(() => {
    const query = q.trim();
    const notes: Item[] = results.map((n) => ({ kind: 'note', id: n.id, label: n.title, note: n }));
    const tags: Item[] = app.tags
      .filter((t) => !query || fuzzy(t.name, query))
      .slice(0, query ? 5 : 3)
      .map((t) => ({ kind: 'tag', id: t.id, label: t.name, color: t.color }));
    const acts = actions.filter((a) => !query || fuzzy(a.label, query));
    return [...notes, ...tags, ...acts];
  });

  $effect(() => {
    void items;
    active = 0;
  });

  function move(to: number) {
    active = to;
    queueMicrotask(() => list?.querySelector(`#pal-${active}`)?.scrollIntoView({ block: 'nearest' }));
  }

  function close() {
    app.paletteOpen = false;
  }

  function choose(it: Item) {
    close();
    if (it.kind === 'note') {
      if (app.view === 'trash' && !it.note.trashed_at) app.setView('all');
      app.select(it.id);
    } else if (it.kind === 'tag') app.toggleTag(it.id, false);
    else it.run();
  }

  function onkeydown(e: KeyboardEvent) {
    if (composing(e)) return;
    if (e.key === 'ArrowDown') move((active + 1) % Math.max(items.length, 1));
    else if (e.key === 'ArrowUp') move((active - 1 + items.length) % Math.max(items.length, 1));
    else if (e.key === 'Enter' && items[active]) choose(items[active]);
    else return;
    e.preventDefault();
  }
</script>

<div class="modal-wrap palette-wrap" role="presentation" use:dialog={{ onclose: close }}>
  <button class="modal-scrim" aria-label="Close" tabindex="-1" onclick={close}></button>
  <div class="palette" role="dialog" aria-modal="true" aria-label="Command palette">
    <label class="palette-input">
      <Icon name="search" size={18} />
      <input
        data-autofocus
        bind:value={q}
        placeholder="Jump to a note, tag or action…"
        {onkeydown}
        aria-label="Search"
        role="combobox"
        aria-expanded="true"
        aria-controls="palette-list"
        aria-autocomplete="list"
        aria-activedescendant={items[active] ? `pal-${active}` : undefined}
      />
      <kbd>Esc</kbd>
    </label>
    <ul class="palette-list" role="listbox" id="palette-list" aria-label="Results" bind:this={list}>
      {#each items as it, i (it.kind + it.id)}
        {#if i === 0 || items[i - 1].kind !== it.kind}
          <li class="palette-group" role="presentation">{it.kind === 'note' ? 'Notes' : it.kind === 'tag' ? 'Tags' : 'Actions'}</li>
        {/if}
        <li role="presentation">
          <button id="pal-{i}" tabindex="-1" class="palette-item" class:active={i === active} role="option" aria-selected={i === active} onmouseenter={() => (active = i)} onclick={() => choose(it)}>
            {#if it.kind === 'note'}
              <Icon name="notes" size={16} /><span class="grow ellipsis">{it.label}</span>
              {#if it.note.trashed_at}<span class="muted small">in trash</span>{/if}
            {:else if it.kind === 'tag'}
              <span class="dot" style="--h:{hue(it.color)}"></span><span class="grow">{it.label}</span><span class="muted small">filter</span>
            {:else}
              <Icon name={it.icon} size={16} /><span class="grow">{it.label}</span>{#if it.hint}<kbd>{it.hint}</kbd>{/if}
            {/if}
          </button>
        </li>
      {:else}
        <li class="palette-empty muted" role="presentation">No matches</li>
      {/each}
    </ul>
  </div>
</div>
