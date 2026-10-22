<script lang="ts">
  import { app } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { hue } from '../lib/colors';
  import { relativeTime, fullTime } from '../lib/format';
  import type { Sort } from '../lib/types';
  import { menu } from '../lib/a11y';
  import { existingSaver } from '../lib/savers';
  import Icon from './Icon.svelte';

  let searchInput: HTMLInputElement;
  let sentinel: HTMLDivElement | undefined = $state();
  let listEl: HTMLDivElement;
  let debounce: ReturnType<typeof setTimeout> | undefined;
  let sortOpen = $state(false);

  const titles: Record<string, string> = { all: 'All notes', pinned: 'Pinned', untagged: 'Untagged', attachments: 'With files', trash: 'Trash' };
  const heading = $derived(
    app.tagFilter.length ? app.tagFilter.map((id) => app.tagById.get(id)?.name ?? '…').join(' + ') : titles[app.view],
  );
  const headHue = $derived(app.tagFilter.length === 1 ? hue(app.tagById.get(app.tagFilter[0])?.color ?? '') : null);

  export function focusSearch() {
    searchInput?.focus();
    searchInput?.select();
  }

  function onSearch() {
    clearTimeout(debounce);
    debounce = setTimeout(() => app.loadList(), 180);
  }

  $effect(() => {
    if (!sentinel) return;
    const io = new IntersectionObserver((entries) => {
      if (entries[0].isIntersecting && app.nextCursor && !app.listLoading) app.loadList(true);
    });
    io.observe(sentinel);
    return () => io.disconnect();
  });

  function purgeDays(trashedAt: number) {
    const days = app.trashDays - Math.floor((Date.now() - trashedAt) / 86_400_000);
    return Math.max(0, days);
  }

  async function emptyTrash() {
    if (!confirm('Permanently delete every note in the trash?')) return;
    try {
      const r = await api.emptyTrash();
      app.notes = [];
      app.select(null);
      app.toast(`Deleted ${r.deleted} ${r.deleted === 1 ? 'note' : 'notes'} forever.`, 'info');
    } catch (e) {
      app.error(e);
    }
  }

  function onkeydown(e: KeyboardEvent) {
    if (e.target instanceof HTMLInputElement) return;
    if (e.key === 'c' && !e.ctrlKey && !e.metaKey && !e.altKey && app.view !== 'trash') {
      e.preventDefault();
      app.newNote();
      return;
    }
    const i = app.notes.findIndex((n) => n.id === app.selectedId);
    if (e.key === 'j' || e.key === 'ArrowDown') {
      const n = app.notes[Math.min(app.notes.length - 1, i + 1)];
      if (n) app.select(n.id);
    } else if (e.key === 'k' || e.key === 'ArrowUp') {
      const n = app.notes[Math.max(0, i - 1)];
      if (n) app.select(n.id);
    } else return;
    e.preventDefault();
    queueMicrotask(() => listEl.querySelector<HTMLElement>('.card.selected')?.focus());
  }

  async function quickPin(id: string, pinned: boolean) {
    try {
      app.upsert(await api.patchNote(id, { pinned }));
      if (app.view === 'pinned' && !pinned) app.remove(id);
    } catch (e) {
      app.error(e);
    }
  }

  async function quickTrash(id: string) {
    try {
      await existingSaver(id)?.flush(); // the server refuses saves to trashed notes
      await api.patchNote(id, { trashed: true });
      if (app.selectedId === id) app.select(null);
      app.remove(id);
      app.toast('Moved to trash.', 'info', {
        label: 'Undo',
        run: () => api.patchNote(id, { trashed: false }).then(() => app.loadList(), (e) => app.error(e)),
      });
    } catch (e) {
      app.error(e);
    }
  }

  const sorts: { id: Sort; label: string }[] = [
    { id: 'updated', label: 'Last edited' },
    { id: 'created', label: 'Date created' },
    { id: 'title', label: 'Title' },
  ];
</script>

<section class="list" class:trash={app.view === 'trash'} aria-label="Notes">
  <header class="list-head">
    <div class="list-title-row">
      <button class="icon-btn mobile-only" aria-label="Open sidebar" onclick={() => (app.sidebarOpen = true)}><Icon name="menu" /></button>
      <h1 class="list-title" style={headHue !== null ? `--h:${headHue}` : ''}>
        {#if headHue !== null}<span class="dot big"></span>{/if}{heading}
      </h1>
      <div class="menu-wrap">
        <button class="icon-btn" title="Sort" aria-label="Sort notes" aria-haspopup="menu" aria-expanded={sortOpen} onclick={() => (sortOpen = !sortOpen)}><Icon name="sort" /></button>
        {#if sortOpen}
          <button class="menu-scrim" aria-label="Close menu" tabindex="-1" onclick={() => (sortOpen = false)}></button>
          <div class="menu menu-right" role="menu" aria-label="Sort by" use:menu={{ onclose: () => (sortOpen = false) }}>
            {#each sorts as s (s.id)}
              <button role="menuitemradio" aria-checked={app.sort === s.id} onclick={() => ((sortOpen = false), app.setSort(s.id))}>
                <span class="check">{#if app.sort === s.id}<Icon name="check" size={14} />{/if}</span>{s.label}
              </button>
            {/each}
          </div>
        {/if}
      </div>
      {#if app.view !== 'trash'}
        <button class="btn primary small new-btn" onclick={() => app.newNote()} title="New note (Alt+N)"><Icon name="plus" size={16} /> New</button>
      {/if}
    </div>
    <label class="search">
      <Icon name="search" size={16} />
      <input
        bind:this={searchInput}
        bind:value={app.query}
        oninput={onSearch}
        onkeydown={(e) => {
          if (e.key === 'Escape') {
            app.query = '';
            app.loadList();
            searchInput.blur();
          } else if (e.key === 'ArrowDown' || e.key === 'Enter') {
            e.preventDefault();
            const first = app.notes[0];
            if (first) {
              app.select(first.id);
              listEl.querySelector<HTMLElement>('.card')?.focus();
            }
          }
        }}
        type="search"
        placeholder="Search notes, IPs, paths, files…"
        aria-label="Search notes"
      />
      {#if app.query}<button class="icon-btn tiny" aria-label="Clear search" onclick={() => ((app.query = ''), app.loadList())}><Icon name="x" size={14} /></button>{/if}
    </label>
    {#if app.view === 'trash' && app.notes.length}
      <div class="trash-head">
        <span class="muted small">{app.trashDays > 0 ? `Notes are deleted forever after ${app.trashDays} days.` : 'Notes stay here until you delete them.'}</span>
        <button class="btn small danger" onclick={emptyTrash}>Empty trash</button>
      </div>
    {/if}
  </header>

  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div class="cards" bind:this={listEl} role="listbox" aria-label="Note list" tabindex="-1" {onkeydown}>
    {#if app.listError}
      <div class="list-empty"><p class="danger">{app.listError}</p><button class="btn small" onclick={() => app.loadList()}>Retry</button></div>
    {:else if app.listLoading && !app.notes.length}
      {#each Array(6) as _, i (i)}<div class="card skeleton"><span></span><span></span></div>{/each}
    {:else if !app.notes.length}
      <div class="list-empty">
        <div class="orb"></div>
        {#if app.query}
          <h3>Nothing matches “{app.query}”</h3>
          <p class="muted">Search looks inside note text, titles and file names.</p>
        {:else if app.view === 'trash'}
          <h3>Trash is empty</h3>
          <p class="muted">Deleted notes rest here{app.trashDays > 0 ? ` for ${app.trashDays} days` : ''}.</p>
        {:else}
          <h3>No notes here yet</h3>
          <p class="muted">Press <kbd>Alt</kbd> <kbd>N</kbd> or the button above to write one.</p>
          <button class="btn primary" onclick={() => app.newNote()}><Icon name="plus" size={16} /> New note</button>
        {/if}
      </div>
    {/if}

    {#each app.notes as n (n.id)}
      {@const first = n.tag_ids.length ? app.tagById.get(n.tag_ids[0]) : undefined}
      <div class="card-wrap" role="presentation">
      <button
        class="card"
        class:selected={n.id === app.selectedId}
        class:accented={!!first}
        style={first ? `--h:${hue(first.color)}` : ''}
        role="option"
        aria-selected={n.id === app.selectedId}
        onclick={() => app.select(n.id)}
      >
        <div class="card-top">
          <span class="card-title">{n.title}</span>
          {#if n.pinned}<span class="pin" title="Pinned"><Icon name="pin" size={13} /></span>{/if}
        </div>
        {#if n.snippet?.length}
          <div class="card-preview snippet">{#each n.snippet as s, i (i)}{#if s.hit}<mark>{s.text}</mark>{:else}{s.text}{/if}{/each}</div>
        {:else if n.preview}
          <div class="card-preview">{n.preview}</div>
        {/if}
        <div class="card-meta">
          {#if n.trashed_at && app.trashDays > 0}
            <span class="purge-chip">purges in {purgeDays(n.trashed_at)}d</span>
          {:else}
            <span title={fullTime(n.updated_at)}>{relativeTime(n.updated_at)}</span>
          {/if}
          {#if n.attachment_count}<span class="meta-files"><Icon name="clip" size={12} />{n.attachment_count}</span>{/if}
          <span class="card-tags">
            {#each n.tag_ids.slice(0, 4) as tid (tid)}
              {@const t = app.tagById.get(tid)}
              {#if t}<span class="mini-chip" style="--h:{hue(t.color)}">{t.name}</span>{/if}
            {/each}
            {#if n.tag_ids.length > 4}<span class="muted">+{n.tag_ids.length - 4}</span>{/if}
          </span>
        </div>
      </button>
      {#if !n.trashed_at}
        <div class="card-actions">
          <button class="icon-btn tiny" tabindex="-1" title={n.pinned ? 'Unpin' : 'Pin'} aria-label={n.pinned ? `Unpin ${n.title}` : `Pin ${n.title}`} onclick={() => quickPin(n.id, !n.pinned)}><Icon name="pin" size={14} /></button>
          <button class="icon-btn tiny" tabindex="-1" title="Move to trash" aria-label="Move {n.title} to trash" onclick={() => quickTrash(n.id)}><Icon name="trash" size={14} /></button>
        </div>
      {/if}
      </div>
    {/each}
    <div class="sentinel" bind:this={sentinel}>{#if app.listLoading && app.notes.length}<span class="muted small">Loading…</span>{/if}</div>
  </div>
  {#if app.view !== 'trash'}
    <button class="fab" aria-label="New note" onclick={() => app.newNote()}><Icon name="plus" size={24} /></button>
  {/if}
</section>
