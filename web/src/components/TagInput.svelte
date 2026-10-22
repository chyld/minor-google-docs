<script lang="ts">
  import { app } from '../lib/state.svelte';
  import { api } from '../lib/api';
  import { hue } from '../lib/colors';
  import { composing } from '../lib/a11y';
  import Icon from './Icon.svelte';

  interface Props {
    tagIds: string[];
    disabled?: boolean;
    onchange: (ids: string[]) => void;
  }
  let { tagIds, disabled = false, onchange }: Props = $props();

  let text = $state('');
  let open = $state(false);
  let active = $state(0);
  let input: HTMLInputElement | undefined = $state();

  const selected = $derived(tagIds.map((id) => app.tagById.get(id)).filter((t) => !!t));
  const suggestions = $derived.by(() => {
    const q = text.trim().toLowerCase();
    const pool = app.tags.filter((t) => !tagIds.includes(t.id));
    const list = q ? pool.filter((t) => t.name.toLowerCase().includes(q)) : pool;
    return list.slice(0, 8);
  });
  const canCreate = $derived(text.trim() !== '' && !app.tags.some((t) => t.name.toLowerCase() === text.trim().toLowerCase()));

  async function add(name: string) {
    name = name.trim().replace(/,/g, ' ');
    if (!name) return;
    try {
      let t = app.tags.find((x) => x.name.toLowerCase() === name.toLowerCase());
      if (!t) {
        t = await api.createTag(name);
        app.tags = [...app.tags, { ...t, count: 0 }].sort((a, b) => a.name.localeCompare(b.name));
      }
      if (!tagIds.includes(t.id)) onchange([...tagIds, t.id]);
      text = '';
      active = 0;
    } catch (e) {
      app.error(e);
    }
  }

  function remove(id: string) {
    onchange(tagIds.filter((t) => t !== id));
  }

  function onkeydown(e: KeyboardEvent) {
    if (composing(e)) return;
    const total = suggestions.length + (canCreate ? 1 : 0);
    if (e.key === 'Enter' || e.key === ',' || e.key === 'Tab') {
      if (!text.trim() && e.key === 'Tab') return;
      e.preventDefault();
      if (active < suggestions.length && open && suggestions.length) add(suggestions[active].name);
      else add(text);
    } else if (e.key === 'Backspace' && text === '' && tagIds.length) {
      remove(tagIds[tagIds.length - 1]);
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      active = (active + 1) % Math.max(total, 1);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      active = (active - 1 + total) % Math.max(total, 1);
    } else if (e.key === 'Escape') {
      open = false;
      input?.blur();
    }
  }
</script>

<div class="taginput" class:disabled>
  {#each selected as t (t.id)}
    <span class="chip" style="--h:{hue(t.color)}">
      <span class="dot"></span>{t.name}
      {#if !disabled}
        <button class="chip-x" title="Remove tag {t.name}" aria-label="Remove tag {t.name}" onclick={() => remove(t.id)}><Icon name="x" size={12} stroke={2.4} /></button>
      {/if}
    </span>
  {/each}
  {#if !disabled}
    <div class="taginput-field">
      <Icon name="tag" size={14} />
      <input
        bind:this={input}
        bind:value={text}
        placeholder={selected.length ? 'Add tag' : 'Add tags…'}
        aria-label="Add tag"
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={open && (suggestions.length > 0 || canCreate)}
        aria-controls="taginput-menu"
        aria-activedescendant={open && (suggestions.length || canCreate) ? `tagopt-${active}` : undefined}
        onfocus={() => (open = true)}
        onblur={() => setTimeout(() => (open = false), 150)}
        oninput={() => {
          open = true;
          active = 0;
        }}
        {onkeydown}
      />
      {#if open && (suggestions.length || canCreate)}
        <ul class="menu taginput-menu" role="listbox" id="taginput-menu" aria-label="Tag suggestions">
          {#each suggestions as t, i (t.id)}
            <li role="presentation">
              <button id="tagopt-{i}" tabindex="-1" class:active={i === active} role="option" aria-selected={i === active} onmousedown={(e) => (e.preventDefault(), add(t.name))}>
                <span class="dot" style="--h:{hue(t.color)}"></span>{t.name}<span class="muted count">{t.count}</span>
              </button>
            </li>
          {/each}
          {#if canCreate}
            <li role="presentation">
              <button id="tagopt-{suggestions.length}" tabindex="-1" role="option" aria-selected={active === suggestions.length} class:active={active === suggestions.length} onmousedown={(e) => (e.preventDefault(), add(text))}>
                <Icon name="plus" size={14} /> Create “{text.trim()}”
              </button>
            </li>
          {/if}
        </ul>
      {/if}
    </div>
  {/if}
</div>
