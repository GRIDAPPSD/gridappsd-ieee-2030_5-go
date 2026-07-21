<script lang="ts">
  // Health is the admin UI's landing panel (GAGO-061). It polls two
  // endpoints every POLL_INTERVAL_MS: /api/health for basic
  // reachability, and /api/registry so the panel can derive a
  // registry entry count and a placeholder-vs-certificate identity
  // tally client side, since GET /api/health itself (handlers.go's
  // handleHealth) returns only a fixed { status: "ok" } today and does
  // not carry STOMP connection state, the mTLS listener address, the
  // server's own SFDI/LFDI, the feeder mRID, the simulation ID, or an
  // uptime value. Those fields are a known gap surfaced in this
  // panel's own "known gaps" note rather than fabricated here: see
  // this card's report for the backend follow-up.
  import { onDestroy, onMount } from 'svelte'
  import { fetchJSON } from '../lib/api'

  interface HealthResponse {
    status: string
  }

  interface RegistryEntryResponse {
    mrid: string
    name: string
    lfdi: string
    sfdi: string
    placeholder: boolean
  }

  const POLL_INTERVAL_MS = 5000

  let reachable: 'loading' | 'ok' | 'disconnected' = $state('loading')
  let registryCount: number | null = $state(null)
  let placeholderCount: number | null = $state(null)
  let registryError = $state('')

  async function poll(): Promise<void> {
    const health = await fetchJSON<HealthResponse>('/api/health')
    // A failed fetch or a non-"ok" status is treated identically as
    // "disconnected", never as an error state: a bridge with STOMP
    // down is an expected operating condition, not a fault in the
    // admin UI itself.
    reachable = health.ok && health.data.status === 'ok' ? 'ok' : 'disconnected'

    const registry = await fetchJSON<RegistryEntryResponse[]>('/api/registry')
    if (registry.ok) {
      registryCount = registry.data.length
      placeholderCount = registry.data.filter((entry) => entry.placeholder).length
      registryError = ''
    } else {
      registryCount = null
      placeholderCount = null
      registryError = registry.error
    }
  }

  let intervalId: ReturnType<typeof setInterval> | undefined

  onMount(() => {
    void poll()
    intervalId = setInterval(() => void poll(), POLL_INTERVAL_MS)
  })

  onDestroy(() => {
    if (intervalId !== undefined) clearInterval(intervalId)
  })
</script>

<section class="panel health">
  <h1>Bridge Admin</h1>
  <p>Read only operator view over the gridappsd-ieee-2030_5-go bridge.</p>

  <div class="status" data-testid="health-status">
    {#if reachable === 'loading'}
      <p>Checking bridge connectivity...</p>
    {:else if reachable === 'ok'}
      <p class="ok">Bridge admin API reachable.</p>
    {:else}
      <p class="disconnected">Bridge disconnected (STOMP link down); will retry automatically.</p>
    {/if}
  </div>

  <div class="registry-summary" data-testid="registry-summary">
    {#if registryCount === null}
      <p>Registry summary unavailable: {registryError || 'loading'}</p>
    {:else}
      <p>
        Registry entries: <strong>{registryCount}</strong>, placeholder identities:
        <strong>{placeholderCount}</strong>, certificate derived:
        <strong>{registryCount - (placeholderCount ?? 0)}</strong>
      </p>
    {/if}
  </div>

  <p class="note" data-testid="health-known-gaps">
    STOMP connection state, the mTLS listener address, the server's own
    SFDI/LFDI identity, the feeder mRID, the simulation ID, and uptime are
    not yet exposed by GET /api/health; this panel reports connectivity and
    the registry identity tally only until that endpoint is extended.
  </p>
</section>

<style>
  .panel {
    max-width: 720px;
    margin: 0 auto;
    padding: 32px 20px;
  }

  .ok {
    color: #1a7f37;
  }

  .disconnected {
    color: #9a6700;
  }

  .note {
    color: var(--text);
    font-size: 0.9em;
  }
</style>
