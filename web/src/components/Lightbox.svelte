<script lang="ts">
  import { attachmentURL } from '../lib/api';
  import { bytes } from '../lib/format';
  import type { Attachment } from '../lib/types';
  import { dialog, portal } from '../lib/a11y';
  import Icon from './Icon.svelte';

  interface Props {
    images: Attachment[];
    start: string; // ref
    onclose: () => void;
  }
  let { images, start, onclose }: Props = $props();
  let i = $state(0);

  $effect(() => {
    i = Math.max(0, images.findIndex((a) => a.ref === start));
  });

  const cur = $derived(images[i]);

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'ArrowRight') i = (i + 1) % images.length;
    else if (e.key === 'ArrowLeft') i = (i - 1 + images.length) % images.length;
    else return;
    e.preventDefault();
    e.stopPropagation();
  }
</script>

{#if cur}
  <div class="lightbox" role="dialog" aria-modal="true" aria-label="Image viewer" tabindex="-1" use:portal use:dialog={{ onclose }} {onkeydown}>
    <button class="lightbox-scrim" aria-label="Close" tabindex="-1" onclick={onclose}></button>
    <figure>
      <img src={attachmentURL(cur.id)} alt={cur.filename} />
      <figcaption>
        <span>{cur.filename}</span>
        <span class="muted">{bytes(cur.size_bytes)}{images.length > 1 ? ` · ${i + 1} / ${images.length}` : ''}</span>
        <a class="icon-btn" href={attachmentURL(cur.id, true)} title="Download" aria-label="Download"><Icon name="download" /></a>
      </figcaption>
    </figure>
    {#if images.length > 1}
      <button class="lb-nav prev" aria-label="Previous image" onclick={() => (i = (i - 1 + images.length) % images.length)}><Icon name="back" size={22} /></button>
      <button class="lb-nav next" aria-label="Next image" onclick={() => (i = (i + 1) % images.length)}><Icon name="back" size={22} class="flip" /></button>
    {/if}
    <button class="lb-close icon-btn" aria-label="Close" onclick={onclose}><Icon name="x" size={22} /></button>
  </div>
{/if}
