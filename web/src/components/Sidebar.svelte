<script lang="ts">
  import { app, type View } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { hue, PALETTE } from '../lib/colors';
  import type { Tag } from '../lib/types';
  import { menu } from '../lib/a11y';
  import Icon from './Icon.svelte';

  let menuFor = $state<string | null>(null);
  let renaming = $state<string | null>(null);
  let renameText = $state('');
  let userMenu = $state(false);

  const views: { id: View; label: string; icon: string }[] = [
    { id: 'all', label: 'All notes', icon: 'notes' },
    { id: 'pinned', label: 'Pinned', icon: 'pin' },
    { id: 'attachments', label: 'With files', icon: 'clip' },
    { id: 'untagged', label: 'Untagged', icon: 'untag' },
    { id: 'trash', label: 'Trash', icon: 'trash' },
  ];

  async function recolor(t: Tag, color: string) {
    menuFor = null;
    try {
      const u = await api.patchTag(t.id, { color });
      app.tags = app.tags.map((x) => (x.id === t.id ? { ...x, color: u.color } : x));
    } catch (e) {
      app.error(e);
    }
  }

  async function rename(t: Tag) {
    const name = renameText.trim();
    renaming = null;
    if (!name || name === t.name) return;
    try {
      const u = await api.patchTag(t.id, { name });
      app.tags = app.tags.map((x) => (x.id === t.id ? { ...x, name: u.name } : x)).sort((a, b) => a.name.localeCompare(b.name));
    } catch (e) {
      app.error(e);
    }
  }

  async function remove(t: Tag) {
    menuFor = null;
    if (!confirm(`Delete the tag “${t.name}”? Notes keep their text; only the tag is removed.`)) return;
    try {
      await api.deleteTag(t.id);
      app.tags = app.tags.filter((x) => x.id !== t.id);
      app.notes = app.notes.map((n) => ({ ...n, tag_ids: n.tag_ids.filter((id) => id !== t.id) }));
      if (app.tagFilter.includes(t.id)) app.toggleTag(t.id, true);
    } catch (e) {
      app.error(e);
    }
  }

  async function logout() {
    await api.logout().catch(() => {});
    location.href = '/';
  }

  const rail = $derived(app.sidebarCollapsed && app.desktop);
  const filterNames = $derived(app.tagFilter.map((id) => app.tagById.get(id)?.name ?? '…').join(' + '));

  const themeIcon = $derived(app.theme === 'dark' ? 'moon' : app.theme === 'light' ? 'sun' : 'monitor');
  const nextTheme = $derived(app.theme === 'system' ? 'light' : app.theme === 'light' ? 'dark' : 'system');
</script>

<nav class="sidebar" class:open={app.sidebarOpen} class:rail aria-label="Sidebar">
  <div class="brand">
    <span class="logo" aria-hidden="true"></span>
    <span class="brand-name">Notes</span>
    <button class="icon-btn mobile-only" aria-label="Close sidebar" onclick={() => (app.sidebarOpen = false)}><Icon name="x" /></button>
    <button
      class="icon-btn desktop-only rail-toggle"
      title={rail ? 'Expand sidebar (Ctrl+\\)' : 'Collapse sidebar (Ctrl+\\)'}
      aria-label={rail ? 'Expand sidebar' : 'Collapse sidebar'}
      aria-expanded={!rail}
      onclick={() => app.setSidebarCollapsed(!app.sidebarCollapsed)}><Icon name={rail ? 'expand' : 'collapse'} /></button>
  </div>

  {#if rail}
    <button class="nav-item rail-new" title="New note (Alt+N)" aria-label="New note" onclick={() => app.newNote()}><Icon name="plus" size={18} /></button>
  {/if}

  <button class="palette-btn" title={rail ? 'Jump to… (Ctrl+K)' : undefined} aria-label="Jump to…" onclick={() => (app.paletteOpen = true)}>
    <Icon name="search" size={16} /><span>Jump to…</span><kbd>Ctrl K</kbd>
  </button>

  <ul class="nav">
    {#each views as v (v.id)}
      <li>
        <button class="nav-item" class:active={app.view === v.id && !app.tagFilter.length} title={rail ? v.label : undefined} aria-label={v.label} onclick={() => app.setView(v.id)}>
          <Icon name={v.icon} size={17} /><span>{v.label}</span>
        </button>
      </li>
    {/each}
    {#if rail && app.tagFilter.length}
      {@const first = app.tagById.get(app.tagFilter[0])}
      <li>
        <button class="nav-item active rail-filter" style="--h:{hue(first?.color ?? '')}" title="Filtered by {filterNames} — show tags" aria-label="Filtered by {filterNames}. Show tags" onclick={() => app.setSidebarCollapsed(false)}>
          <Icon name="tag" size={17} /><span class="dot"></span>
        </button>
      </li>
    {/if}
  </ul>

  <div class="tags-head">
    <span>Tags</span>
    {#if app.tagFilter.length > 1}<span class="muted small">match all</span>{/if}
  </div>
  <ul class="tags">
    {#each app.tags as t (t.id)}
      <li class="tag-row" style="--h:{hue(t.color)}">
        {#if renaming === t.id}
          <!-- svelte-ignore a11y_autofocus -->
          <input
            class="tag-rename"
            bind:value={renameText}
            autofocus
            onkeydown={(e) => {
              if (e.key === 'Enter') rename(t);
              if (e.key === 'Escape') renaming = null;
            }}
            onblur={() => rename(t)}
          />
        {:else}
          <button class="nav-item tag-item" class:active={app.tagFilter.includes(t.id)} onclick={(e) => app.toggleTag(t.id, e.shiftKey || e.metaKey || e.ctrlKey)} title="Click to filter · Shift+click to combine">
            <span class="dot"></span><span class="tag-name">{t.name}</span><span class="count">{t.count}</span>
          </button>
          <button class="icon-btn tiny tag-more" aria-label="Edit tag {t.name}" aria-haspopup="menu" aria-expanded={menuFor === t.id} onclick={() => (menuFor = menuFor === t.id ? null : t.id)}><Icon name="more" size={14} /></button>
        {/if}
        {#if menuFor === t.id}
          <button class="menu-scrim" aria-label="Close menu" tabindex="-1" onclick={() => (menuFor = null)}></button>
          <div class="menu tag-menu" role="menu" aria-label="Tag {t.name}" use:menu={{ onclose: () => (menuFor = null) }}>
            <div class="swatches" role="group" aria-label="Tag color">
              {#each PALETTE as c (c)}
                <button role="menuitemradio" aria-checked={c === t.color} class="swatch" class:current={c === t.color} style="--h:{hue(c)}" title={c} aria-label="Color {c}" onclick={() => recolor(t, c)}></button>
              {/each}
            </div>
            <button
              role="menuitem"
              onclick={() => {
                menuFor = null;
                renaming = t.id;
                renameText = t.name;
              }}><Icon name="edit" size={15} /> Rename</button>
            <button role="menuitem" class="danger" onclick={() => remove(t)}><Icon name="trash" size={15} /> Delete tag</button>
          </div>
        {/if}
      </li>
    {:else}
      <li class="muted small tags-empty">Tags you add to notes show up here, each in its own color.</li>
    {/each}
  </ul>

  <div class="sidebar-foot">
    <div class="menu-wrap grow">
      <button class="user-btn" title={rail ? app.me?.username : undefined} aria-label="Account menu for {app.me?.username}" onclick={() => (userMenu = !userMenu)} aria-haspopup="menu" aria-expanded={userMenu}>
        <span class="avatar">{app.me?.username.slice(0, 1).toUpperCase()}</span>
        <span class="user-name">{app.me?.username}</span>
      </button>
      {#if userMenu}
        <button class="menu-scrim" aria-label="Close menu" tabindex="-1" onclick={() => (userMenu = false)}></button>
        <div class="menu menu-up" role="menu" aria-label="Account" use:menu={{ onclose: () => (userMenu = false) }}>
          <button role="menuitem" onclick={() => ((userMenu = false), (app.accountOpen = true))}><Icon name="user" size={16} /> Account & data</button>
          <button role="menuitem" onclick={() => ((userMenu = false), (app.shortcutsOpen = true))}><Icon name="keyboard" size={16} /> Keyboard shortcuts</button>
          <hr />
          <button role="menuitem" onclick={logout}><Icon name="logout" size={16} /> Log out</button>
        </div>
      {/if}
    </div>
    <button class="icon-btn" title="Theme: {app.theme} (click for {nextTheme})" aria-label="Switch theme" onclick={() => app.setTheme(nextTheme)}><Icon name={themeIcon} /></button>
  </div>
</nav>
{#if app.sidebarOpen}<button class="drawer-scrim" aria-label="Close sidebar" tabindex="-1" onclick={() => (app.sidebarOpen = false)}></button>{/if}
