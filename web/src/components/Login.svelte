<script lang="ts">
  import { api, ApiError } from '../lib/api';
  import { app } from '../lib/state.svelte';

  let username = $state('');
  let password = $state('');
  let error = $state('');
  let busy = $state(false);
  let hasUsers = $state(true);

  $effect(() => {
    api.setup().then((s) => (hasUsers = s.has_users)).catch(() => {});
  });

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    if (busy) return;
    busy = true;
    error = '';
    try {
      app.me = await api.login(username.trim(), password);
      password = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Login failed.';
    } finally {
      busy = false;
    }
  }
</script>

<main class="login">
  <div class="mesh" aria-hidden="true"></div>
  <form class="login-card" onsubmit={submit}>
    <div class="login-brand"><span class="logo big" aria-hidden="true"></span><h1>Notes</h1></div>
    <p class="muted">Your private, plain-text notebook.</p>
    {#if !hasUsers}
      <div class="setup-hint">
        <strong>No accounts yet.</strong> Create one on the server:
        <code>docker compose exec -it notes notes user add yourname</code>
      </div>
    {/if}
    <label>
      <span>Username</span>
      <!-- svelte-ignore a11y_autofocus -->
      <input bind:value={username} autocomplete="username" autocapitalize="none" spellcheck="false" required autofocus />
    </label>
    <label>
      <span>Password</span>
      <input type="password" bind:value={password} autocomplete="current-password" required />
    </label>
    {#if error}<p class="form-error" role="alert">{error}</p>{/if}
    <button class="btn primary big" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
  </form>
</main>
