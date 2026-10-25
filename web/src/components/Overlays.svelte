<script lang="ts">
  import { app } from '../lib/state.svelte';
  import { uploads } from '../lib/uploads.svelte';
  import { bytes } from '../lib/format';
  import { dialog } from '../lib/a11y';
  import Icon from './Icon.svelte';

  const shortcuts: [string, string][] = [
    ['Ctrl K', 'Command palette — jump to any note'],
    ['Alt N', 'New note'],
    ['/', 'Search notes'],
    ['Ctrl S', 'Save now (autosave is always on)'],
    ['Ctrl F', 'Find in note'],
    ['Alt T', 'Insert current date & time'],
    ['Ctrl ⇧ H', 'Highlight the selection (Enter repeats the last colour)'],
    ['Ctrl B / Ctrl U', 'Bold / underline the selection (⌘ on a Mac)'],
    ['Ctrl V', 'Paste a screenshot into the note'],
    ['Ctrl /', 'Raw text / rendered note'],
    ['Ctrl ⇧ F', 'Focus mode'],
    ['Ctrl \\', 'Collapse / expand the sidebar'],
    ['Ctrl ⇧ \\', 'Hide / show the notes list'],
    ['c', 'New note (when the note list has focus)'],
    ['j / k', 'Next / previous note (in the list)'],
    ['?', 'This cheat sheet'],
  ];

  const visible = $derived(uploads.tasks.filter((t) => t.status !== 'canceled'));
  const totalSize = $derived(visible.reduce((n, t) => n + t.size, 0));
  const totalSent = $derived(visible.reduce((n, t) => n + t.sent, 0));
</script>

<!-- Toasts -->
<div class="toasts" role="status" aria-live="polite">
  {#each app.toasts as t (t.id)}
    <div class="toast {t.kind}">
      <span class="toast-icon"><Icon name={t.kind === 'error' ? 'alert' : t.kind === 'success' ? 'check' : 'sparkle'} size={16} /></span>
      <span class="grow">{t.message}</span>
      {#if t.action}
        <button
          class="toast-action"
          onclick={() => {
            t.action!.run();
            app.dismiss(t.id);
          }}>{t.action.label}</button>
      {/if}
      <button class="icon-btn tiny" aria-label="Dismiss" onclick={() => app.dismiss(t.id)}><Icon name="x" size={14} /></button>
    </div>
  {/each}
</div>

<!-- Upload tray -->
{#if visible.length}
  <div class="tray" role="region" aria-label="Uploads" aria-live="polite">
    <div class="tray-head">
      <Icon name="upload" size={16} />
      <span class="grow">{uploads.active ? `Uploading ${uploads.active} ${uploads.active === 1 ? 'file' : 'files'}` : 'Uploads'}</span>
      <span class="muted small">{bytes(totalSent)} / {bytes(totalSize)}</span>
    </div>
    <ul>
      {#each visible as t (t.id)}
        <li class="tray-item" data-status={t.status}>
          <div class="grow">
            <div class="ellipsis small">{t.filename}</div>
            <div
              class="progress"
              class:error={t.status === 'error'}
              class:done={t.status === 'done'}
              role="progressbar"
              aria-label="{t.filename} upload progress"
              aria-valuemin="0"
              aria-valuemax="100"
              aria-valuenow={Math.round(t.size ? (t.sent / t.size) * 100 : 100)}
            ><span style="width:{t.size ? (t.sent / t.size) * 100 : 100}%"></span></div>
            {#if t.status === 'error'}<div class="small danger">{t.error}</div>{:else if t.status === 'retrying'}<div class="small muted">Reconnecting…</div>{/if}
          </div>
          {#if t.status === 'done'}
            <span class="ok"><Icon name="check" size={16} /></span>
          {:else if t.status === 'error'}
            <button class="icon-btn tiny" aria-label="Retry" onclick={() => uploads.retry(t.id)}><Icon name="refresh" size={14} /></button>
            <button class="icon-btn tiny" aria-label="Dismiss" onclick={() => uploads.dismiss(t.id)}><Icon name="x" size={14} /></button>
          {:else}
            <button class="icon-btn tiny" aria-label="Cancel upload" onclick={() => uploads.cancel(t.id)}><Icon name="x" size={14} /></button>
          {/if}
        </li>
      {/each}
    </ul>
  </div>
{/if}

<!-- Shortcuts -->
{#if app.shortcutsOpen}
  <div class="modal-wrap" role="presentation" use:dialog={{ onclose: () => (app.shortcutsOpen = false) }}>
    <button class="modal-scrim" aria-label="Close" tabindex="-1" onclick={() => (app.shortcutsOpen = false)}></button>
    <div class="modal small" role="dialog" aria-modal="true" aria-labelledby="keys-title">
      <header class="modal-head">
        <h2 id="keys-title">Keyboard shortcuts</h2>
        <button class="icon-btn" aria-label="Close" onclick={() => (app.shortcutsOpen = false)}><Icon name="x" /></button>
      </header>
      <dl class="keys">
        {#each shortcuts as [k, d] (k)}
          <dt>{#each k.split(' ') as part, i (i)}<kbd>{part}</kbd>{/each}</dt>
          <dd>{d}</dd>
        {/each}
      </dl>
    </div>
  </div>
{/if}
