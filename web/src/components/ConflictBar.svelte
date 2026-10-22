<script lang="ts">
  import { diffLines } from '../lib/diff';
  import Icon from './Icon.svelte';

  interface Props {
    mine: string;
    theirs: string;
    reason: 'conflict' | 'draft';
    onkeepmine: () => void;
    onusetheirs: () => void;
    onsaveasnew: () => void;
  }
  let { mine, theirs, reason, onkeepmine, onusetheirs, onsaveasnew }: Props = $props();
  let showDiff = $state(false);
  const diff = $derived(showDiff ? diffLines(mine, theirs) : []);
  const changed = $derived(diff.filter((d) => d.kind !== 'same').length);
</script>

<div class="conflict" role="alert">
  <div class="conflict-head">
    <Icon name="alert" size={18} />
    <div class="conflict-text">
      {#if reason === 'conflict'}
        <strong>This note changed somewhere else</strong> (another tab or device). Autosave is paused — nothing has been lost.
      {:else}
        <strong>You have unsaved changes from earlier</strong>, but the note has changed since. Choose which version to keep.
      {/if}
    </div>
  </div>
  <div class="conflict-actions">
    <button class="btn small" onclick={() => (showDiff = !showDiff)}>{showDiff ? 'Hide' : 'Show'} differences</button>
    <button class="btn small primary" onclick={onkeepmine}>Keep mine</button>
    <button class="btn small" onclick={onusetheirs}>Use the other version</button>
    <button class="btn small" onclick={onsaveasnew}>Save mine as a new note</button>
  </div>
  {#if showDiff}
    <div class="diff" aria-label="Differences">
      <div class="diff-legend"><span class="mine">+ yours</span><span class="theirs">− other version</span><span class="muted">{changed} changed lines</span></div>
      <pre>{#each diff as d, i (i)}<div class="diff-{d.kind}">{d.kind === 'mine' ? '+ ' : d.kind === 'theirs' ? '− ' : '  '}{d.text}</div>{/each}</pre>
    </div>
  {/if}
</div>
