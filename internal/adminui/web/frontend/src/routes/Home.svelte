<script lang="ts">
  // Home is the SPA's placeholder landing view. It exists to prove the
  // client wiring end to end (fetch against a real /api endpoint,
  // through the server's Bearer-auth and host-allowlist middleware,
  // rendered into the page) before any real operator panel
  // (GAGO-061..066) lands. Those panels replace this component's body;
  // they do not need to touch the router or the shell.
  import { onMount } from 'svelte'
  import { fetchJSON } from '../lib/api'

  interface HealthResponse {
    status: string
  }

  let status: 'loading' | 'ok' | 'error' = $state('loading')
  let errorMessage = $state('')

  onMount(async () => {
    const result = await fetchJSON<HealthResponse>('/api/health')
    if (result.ok) {
      status = result.data.status === 'ok' ? 'ok' : 'error'
      if (status === 'error') errorMessage = `unexpected status value: ${result.data.status}`
    } else {
      status = 'error'
      errorMessage = result.error
    }
  })
</script>

<section class="home">
  <h1>Bridge Admin</h1>
  <p>Read only operator view over the gridappsd-ieee-2030_5-go bridge.</p>

  <div class="health" data-testid="health-status">
    {#if status === 'loading'}
      <p>Checking bridge connectivity...</p>
    {:else if status === 'ok'}
      <p class="ok">Bridge admin API reachable.</p>
    {:else}
      <p class="error">Bridge admin API unreachable: {errorMessage}</p>
    {/if}
  </div>

  <p class="note">
    Panels for the registry, served devices, DER programs, and control flow
    land on this shell in follow-up cards.
  </p>
</section>

<style>
  .home {
    max-width: 640px;
    margin: 0 auto;
    padding: 32px 20px;
  }

  .ok {
    color: #1a7f37;
  }

  .error {
    color: #b91c1c;
  }

  .note {
    color: var(--text);
    font-size: 0.9em;
  }
</style>
