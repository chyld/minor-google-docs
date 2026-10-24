<script lang="ts">
  import { api, ApiError } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import { bytes } from '../lib/format';
  import type { DoctorJob, Storage } from '../lib/types';
  import { dialog } from '../lib/a11y';
  import Icon from './Icon.svelte';

  let storage = $state<Storage | null>(null);
  let current = $state('');
  let next = $state('');
  let repeat = $state('');
  let pwError = $state('');
  let busy = $state(false);

  $effect(() => {
    api.storage().then((s) => (storage = s)).catch(() => {});
  });

  // ---- Check my notes: a per-user doctor that runs on the server; we poll for its result ----
  let job = $state<DoctorJob | null>(null);
  let checkError = $state('');
  let poll: ReturnType<typeof setTimeout> | undefined;
  const checking = $derived(!!job?.running);

  async function follow() {
    try {
      job = await api.doctor();
      if (job.running) poll = setTimeout(follow, 800);
    } catch (e) {
      checkError = e instanceof Error ? e.message : String(e);
    }
  }

  async function runCheck(fix: boolean) {
    checkError = '';
    try {
      job = await api.startDoctor(fix);
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 409)) {
        checkError = e instanceof Error ? e.message : String(e);
        return;
      }
    }
    clearTimeout(poll);
    void follow();
  }

  $effect(() => {
    if (app.checkOnOpen) {
      app.checkOnOpen = false;
      void runCheck(false);
    } else {
      void follow(); // show a check still running (or its last result)
    }
    return () => clearTimeout(poll);
  });

  async function changePassword(e: SubmitEvent) {
    e.preventDefault();
    pwError = '';
    if (next !== repeat) {
      pwError = 'The new passwords do not match.';
      return;
    }
    busy = true;
    try {
      await api.changePassword(current, next);
      current = next = repeat = '';
      app.toast('Password changed. Other devices were signed out.', 'success');
    } catch (err) {
      pwError = err instanceof ApiError ? err.message : 'Could not change password.';
    } finally {
      busy = false;
    }
  }

  async function logoutOthers() {
    try {
      const r = await api.logoutOthers();
      app.toast(`Signed out ${r.revoked} other ${r.revoked === 1 ? 'session' : 'sessions'}.`, 'success');
    } catch (e) {
      app.error(e);
    }
  }

  const close = () => (app.accountOpen = false);
  const low = $derived(storage && storage.min_free_bytes > 0 && storage.free_bytes < storage.min_free_bytes * 2);
</script>

<div class="modal-wrap" role="presentation" use:dialog={{ onclose: close }}>
  <button class="modal-scrim" aria-label="Close" tabindex="-1" onclick={close}></button>
  <div class="modal" role="dialog" aria-modal="true" aria-labelledby="acct-title">
    <header class="modal-head">
      <h2 id="acct-title">Account & data</h2>
      <button class="icon-btn" aria-label="Close" onclick={close}><Icon name="x" /></button>
    </header>

    <section class="modal-section">
      <h3>Storage</h3>
      {#if storage}
        <div class="stats">
          <div class="stat"><span class="stat-value">{bytes(storage.used_bytes)}</span><span class="muted small">used by your notes & files</span></div>
          <div class="stat" class:warn={low}><span class="stat-value">{bytes(storage.free_bytes)}</span><span class="muted small">free on the server</span></div>
        </div>
        {#if low}<p class="small warn-text">The server is getting low on space. Uploads pause below {bytes(storage.min_free_bytes)} free.</p>{/if}
      {:else}
        <p class="muted">Loading…</p>
      {/if}
    </section>

    <section class="modal-section">
      <h3>Export</h3>
      <p class="muted small">Download all your notes in one zip: a folder per note with its text, its details and its images.</p>
      <a class="btn" href="/api/export" download><Icon name="download" size={16} /> Download export</a>
    </section>

    <section class="modal-section" aria-labelledby="check-title">
      <h3 id="check-title">Check my notes</h3>
      <p class="muted small">Makes sure every note, image and details file on the server matches. Nothing is deleted.</p>
      <div class="check-actions">
        <button class="btn" onclick={() => runCheck(false)} disabled={checking}><Icon name="check" size={16} /> {checking && !job?.fix ? 'Checking…' : 'Check my notes'}</button>
        {#if job?.report?.problems.length && !job.running && !job.fix}
          <button class="btn primary" onclick={() => runCheck(true)}>Repair</button>
        {/if}
        {#if checking && job?.fix}<span class="muted small">Repairing…</span>{/if}
      </div>
      <div class="check-result" role="status" aria-live="polite">
        {#if checkError}
          <p class="form-error">{checkError}</p>
        {:else if job?.error}
          <p class="form-error">{job.error}</p>
        {:else if job?.report && !job.running}
          {#if job.fix && job.report.fixed.length}
            <p class="small">Repaired:</p>
            <ul class="check-list">{#each job.report.fixed as f}<li>{f}</li>{/each}</ul>
          {/if}
          {#if job.report.problems.length === 0}
            <p class="small ok-text"><Icon name="check" size={14} /> No problems found.</p>
          {:else if job.fix}
            {#if job.report.fixed.length < job.report.problems.length}
              <p class="small">Some problems can't be repaired from here. Check again to see what's left; whoever runs the server can look with <code>notes doctor</code>.</p>
            {/if}
          {:else}
            <p class="small">{job.report.problems.length === 1 ? 'Found one problem:' : `Found ${job.report.problems.length} problems:`}</p>
            <ul class="check-list">{#each job.report.problems as p}<li>{p}</li>{/each}</ul>
          {/if}
        {/if}
      </div>
    </section>

    <section class="modal-section">
      <h3>Change password</h3>
      <form class="stack" onsubmit={changePassword}>
        <input type="text" autocomplete="username" value={app.me?.username} hidden readonly />
        <input type="password" placeholder="Current password" data-autofocus autocomplete="current-password" bind:value={current} required />
        <input type="password" placeholder="New password (10+ characters)" autocomplete="new-password" minlength="10" bind:value={next} required />
        <input type="password" placeholder="Repeat new password" autocomplete="new-password" bind:value={repeat} required />
        {#if pwError}<p class="form-error">{pwError}</p>{/if}
        <button class="btn primary" disabled={busy}>Change password</button>
      </form>
    </section>

    <section class="modal-section">
      <h3>Sessions</h3>
      <p class="muted small">Signed in somewhere you shouldn't be?</p>
      <button class="btn" onclick={logoutOthers}><Icon name="logout" size={16} /> Sign out all other devices</button>
    </section>
  </div>
</div>
