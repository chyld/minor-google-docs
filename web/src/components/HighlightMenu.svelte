<script lang="ts">
  import { untrack } from 'svelte';
  import { HL_HUES, hlHue, hlLabel } from '../lib/highlight';
  import Icon from './Icon.svelte';
  import type { Coverage, StyleOp } from '../lib/editor/highlights';

  /** Bold / underline / clear for note text (never for section colours). */
  interface Formatting {
    bold: Coverage;
    underline: Coverage;
    hasColor: boolean;
    styled: boolean;
    onformat: (op: StyleOp) => void;
  }

  interface Props {
    current: string | null; // the highlight the selection is in
    last: string | null; // most recently used colour
    onpick: (name: string | null) => void;
    onclose: () => void;
    class?: string;
    label?: string; // what the colours are for
    removeLabel?: string;
    formatting?: Formatting;
  }
  let { current, last, onpick, onclose, class: cls = '', label = 'Highlight colours', removeLabel = 'Remove highlight', formatting }: Props = $props();
  const pressed = (c: Coverage) => (c === 'all' ? 'true' : c === 'some' ? 'mixed' : 'false');

  // light colours, then strong ones; 12 per row (6 on phones, where each row wraps in two)
  const names = [...HL_HUES, ...HL_HUES.map((h) => `${h}-deep`)];
  const narrow = matchMedia('(max-width: 560px)');
  const start = untrack(() => current ?? last ?? names[0]); // where focus starts when it opens
  let at = $state(Math.max(0, names.indexOf(start)));
  let grid: HTMLDivElement;

  $effect(() => {
    grid?.querySelector<HTMLElement>(`[data-i="${at}"]`)?.focus();
  });

  function onkeydown(e: KeyboardEvent) {
    // move by what is on screen: the swatch list wraps every `cols`
    const cols = narrow.matches ? 6 : 12;
    const n = names.length;
    const row = Math.floor(at / cols);
    const col = at % cols;
    if (e.key === 'ArrowRight') at = row * cols + ((col + 1) % cols);
    else if (e.key === 'ArrowLeft') at = row * cols + ((col - 1 + cols) % cols);
    else if (e.key === 'ArrowDown') at = (at + cols) % n;
    else if (e.key === 'ArrowUp') at = (at - cols + n) % n;
    else if (e.key === 'Home') at = row * cols;
    else if (e.key === 'End') at = row * cols + cols - 1;
    else return;
    e.preventDefault();
  }

  function onRootKey(e: KeyboardEvent) {
    if (e.key !== 'Escape') return;
    e.preventDefault();
    e.stopPropagation(); // not also the app's Escape (focus mode, drawer)
    onclose();
  }
</script>

<!-- mousedown is prevented so the editor keeps its selection while the menu is used -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="hl-menu {cls}" onmousedown={(e) => e.preventDefault()} onkeydown={onRootKey}>
  {#if formatting}
    <!-- bold, underline and clear sit beside the remove button; the swatch grid below is unchanged -->
    <div class="hl-format" role="toolbar" aria-label="Text formatting">
      <button class="hl-fmt fmt-btn-b" title="Bold (Ctrl+B)" aria-label="Bold" aria-pressed={pressed(formatting.bold)} onclick={() => formatting.onformat({ kind: 'toggle', style: 'b' })}><b>B</b></button>
      <button class="hl-fmt fmt-btn-u" title="Underline (Ctrl+U)" aria-label="Underline" aria-pressed={pressed(formatting.underline)} onclick={() => formatting.onformat({ kind: 'toggle', style: 'u' })}><u>U</u></button>
      {#if formatting.styled}
        <button class="hl-remove" onclick={() => formatting.onformat({ kind: 'clear' })}><Icon name="x" size={14} /> Clear formatting</button>
      {/if}
    </div>
    {#if formatting.hasColor}
      <button class="hl-remove" onclick={() => onpick(null)}><Icon name="x" size={14} /> Remove colour</button>
    {/if}
  {:else if current}
    <button class="hl-remove" onclick={() => onpick(null)}><Icon name="x" size={14} /> {removeLabel}</button>
  {/if}
  <div class="hl-grid" role="grid" aria-label={label} tabindex="-1" bind:this={grid} {onkeydown}>
    {#each [0, 1] as r (r)}
      <div role="row">
        {#each names.slice(r * 12, r * 12 + 12) as name, c (name)}
          {@const i = r * 12 + c}
          <div role="gridcell">
            <button
              class="hl-swatch"
              class:deep={r === 1}
              class:current={name === current}
              style="--h:{hlHue(name)}"
              data-i={i}
              tabindex={i === at ? 0 : -1}
              title={hlLabel(name)}
              aria-label={hlLabel(name)}
              aria-pressed={name === current}
              onfocus={() => (at = i)}
              onclick={() => onpick(name)}><span aria-hidden="true">a</span></button>
          </div>
        {/each}
      </div>
    {/each}
  </div>
  {#if !current}
    <p class="hl-hint muted small">Enter applies · Esc closes</p>
  {/if}
</div>
