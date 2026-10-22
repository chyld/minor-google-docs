<script lang="ts">
  import { api, attachmentURL } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import { uploads } from '../lib/uploads.svelte';
  import { bytes, fileKind, relativeTime } from '../lib/format';
  import type { Attachment } from '../lib/types';
  import Icon from './Icon.svelte';

  interface Props {
    noteId: string;
    attachments: Attachment[];
    shownRefs: Set<string>;
    readonly: boolean;
    onchange: (atts: Attachment[]) => void;
    onfiles: (files: File[]) => void;
    onopen: (ref: string) => void;
    onclose: () => void;
  }
  let { noteId, attachments, shownRefs, readonly, onchange, onfiles, onopen, onclose }: Props = $props();

  let picker: HTMLInputElement;
  let renaming = $state<string | null>(null);
  let renameText = $state('');
  let dragging = $state(false);

  const tasks = $derived(uploads.tasks.filter((t) => t.noteId === noteId && t.status !== 'done'));
  const total = $derived(attachments.reduce((n, a) => n + a.size_bytes, 0));

  async function rename(a: Attachment) {
    const name = renameText.trim();
    renaming = null;
    if (!name || name === a.filename) return;
    try {
      const upd = await api.renameAttachment(a.id, name);
      onchange(attachments.map((x) => (x.id === a.id ? upd : x)));
    } catch (e) {
      app.error(e);
    }
  }

  async function remove(a: Attachment) {
    if (!confirm(`Delete “${a.filename}” permanently?`)) return;
    try {
      await api.deleteAttachment(a.id);
      onchange(attachments.filter((x) => x.id !== a.id));
      app.upsert({ id: noteId, attachment_count: attachments.length - 1 });
      app.toast(`Deleted ${a.filename}`, 'success');
    } catch (e) {
      app.error(e);
    }
  }

  function pick(e: Event) {
    const files = [...((e.target as HTMLInputElement).files ?? [])];
    if (files.length) onfiles(files);
    (e.target as HTMLInputElement).value = '';
  }
</script>

<aside
  class="attachments"
  class:dragging
  aria-label="Attachments"
  ondragover={(e) => {
    if (readonly || !e.dataTransfer?.types.includes('Files')) return;
    e.preventDefault();
    dragging = true;
  }}
  ondragleave={() => (dragging = false)}
  ondrop={(e) => {
    dragging = false;
    if (readonly) return;
    e.preventDefault();
    const files = [...(e.dataTransfer?.files ?? [])];
    if (files.length) onfiles(files);
  }}
>
  <header class="att-head">
    <div>
      <h3>Attachments</h3>
      <span class="muted small">{attachments.length} {attachments.length === 1 ? 'file' : 'files'} · {bytes(total)}</span>
    </div>
    <div class="row">
      {#if !readonly}
        <button class="btn small primary" onclick={() => picker.click()}><Icon name="upload" size={15} /> Add files</button>
      {/if}
      <button class="icon-btn" title="Close" aria-label="Close attachments" onclick={onclose}><Icon name="x" /></button>
    </div>
    <input type="file" multiple hidden bind:this={picker} onchange={pick} />
  </header>

  <ul class="att-list">
    {#each tasks as t (t.id)}
      {@const k = fileKind('', t.filename)}
      <li class="att uploading">
        <span class="att-icon" style="--h:{k.hue}"><Icon name={t.status === 'error' ? 'alert' : 'upload'} size={16} /></span>
        <div class="att-body">
          <div class="att-name" title={t.filename}>{t.filename}</div>
          <div
            class="progress"
            class:error={t.status === 'error'}
            role="progressbar"
            aria-label="{t.filename} upload progress"
            aria-valuemin="0"
            aria-valuemax="100"
            aria-valuenow={Math.round(t.size ? (t.sent / t.size) * 100 : 100)}
          ><span style="width:{t.size ? (t.sent / t.size) * 100 : 100}%"></span></div>
          <div class="muted small">
            {#if t.status === 'error'}<span class="danger">{t.error}</span>
            {:else if t.status === 'retrying'}Connection lost — retrying…
            {:else if t.status === 'canceled'}Canceled
            {:else}{bytes(t.sent)} of {bytes(t.size)}{/if}
          </div>
        </div>
        <div class="att-actions">
          {#if t.status === 'error'}<button class="icon-btn" title="Retry" aria-label="Retry upload" onclick={() => uploads.retry(t.id)}><Icon name="refresh" /></button>{/if}
          {#if t.status !== 'canceled'}<button class="icon-btn" title="Cancel" aria-label="Cancel upload" onclick={() => uploads.cancel(t.id)}><Icon name="x" /></button>{/if}
        </div>
      </li>
    {/each}
    {#each attachments as a (a.id)}
      {@const k = fileKind(a.mime, a.filename)}
      <li class="att">
        {#if a.inline}
          <button class="att-thumb" title="Preview" onclick={() => onopen(a.ref)}><img src={attachmentURL(a.id)} alt="" loading="lazy" /></button>
        {:else}
          <span class="att-icon" style="--h:{k.hue}"><Icon name={k.kind === 'image' ? 'image' : k.kind === 'archive' ? 'archive' : 'notes'} size={16} /></span>
        {/if}
        <div class="att-body">
          {#if renaming === a.id}
            <!-- svelte-ignore a11y_autofocus -->
            <input
              class="att-rename"
              bind:value={renameText}
              autofocus
              onkeydown={(e) => {
                if (e.key === 'Enter') rename(a);
                if (e.key === 'Escape') renaming = null;
              }}
              onblur={() => rename(a)}
            />
          {:else}
            <a class="att-name" href={attachmentURL(a.id, true)} title="Download {a.filename}">{a.filename}</a>
          {/if}
          <div class="muted small">
            <span class="kind" style="--h:{k.hue}">{k.kind}</span> · {bytes(a.size_bytes)} · {relativeTime(a.created_at)}
            {#if a.inline && !shownRefs.has(a.ref)}<span class="badge" title="This image is attached but its marker isn't in the note text">not shown in note</span>{/if}
          </div>
        </div>
        <div class="att-actions">
          <a class="icon-btn" href={attachmentURL(a.id, true)} title="Download" aria-label="Download {a.filename}"><Icon name="download" /></a>
          {#if !readonly}
            <button
              class="icon-btn"
              title="Rename"
              aria-label="Rename {a.filename}"
              onclick={() => {
                renaming = a.id;
                renameText = a.filename;
              }}><Icon name="edit" /></button>
            <button class="icon-btn danger-hover" title="Delete" aria-label="Delete {a.filename}" onclick={() => remove(a)}><Icon name="trash" /></button>
          {/if}
        </div>
      </li>
    {/each}
  </ul>

  {#if !attachments.length && !tasks.length}
    <div class="att-empty">
      <div class="orb small"></div>
      <p>Drop any file here — any type, any size.</p>
      <p class="muted small">Paste screenshots straight into the note.</p>
    </div>
  {/if}
</aside>
