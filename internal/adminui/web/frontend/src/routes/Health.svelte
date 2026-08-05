<script lang="ts">
  // Health is the admin UI's landing panel. GET /api/health (handlers.go's handleHealth,
  // healthResponse) now carries STOMP connection state, the mTLS
  // listener address, this server's own SFDI/LFDI identity, the feeder
  // mRID, the simulation ID, the registry/placeholder/certificate
  // tally, uptime, and the optional server-of-record link. This panel
  // renders those fields directly from /api/health; the registry tally
  // is no longer derived client side from a separate /api/registry
  // fetch, since the server now computes and serializes that tally
  // itself (registryCount, placeholderCount, certificateCount).
  import { onDestroy, onMount } from 'svelte'
  import { fetchJSON } from '../lib/api'

  interface HealthResponse {
    status: string
    stompConnected: boolean
    mtlsListener: string
    serverSfdi: string
    serverLfdi: string
    feederMrid: string
    simulationId: string
    registryCount: number
    placeholderCount: number
    certificateCount: number
    uptimeSeconds: number
    sorLink: string
  }

  const POLL_INTERVAL_MS = 5000

  // ALLOWED_SOR_SCHEMES is the fail-closed allowlist: only
  // http and https render as a link. Every other scheme (javascript:,
  // data:, or anything the URL parser rejects outright) renders no
  // link at all. Svelte's text interpolation auto-escapes rendered
  // text, but that escaping does not protect an href attribute
  // context: a javascript: URL is a script-execution sink regardless
  // of how the surrounding text is escaped, so the scheme itself must
  // be validated before sorLink ever reaches an href.
  const ALLOWED_SOR_SCHEMES = new Set(['http:', 'https:'])

  // safeSorLink parses rawLink with the URL API and returns it
  // unchanged only when its scheme is in the allowlist above.
  // Anything unparseable, or parseable but on a disallowed scheme,
  // returns null so the caller renders no href.
  function safeSorLink(rawLink: string): string | null {
    if (!rawLink) return null
    let parsed: URL
    try {
      parsed = new URL(rawLink)
    } catch {
      return null
    }
    return ALLOWED_SOR_SCHEMES.has(parsed.protocol) ? rawLink : null
  }

  let reachable: 'loading' | 'ok' | 'disconnected' = $state('loading')
  let health: HealthResponse | null = $state(null)
  let healthError = $state('')
  let sorHref: string | null = $state(null)

  async function poll(): Promise<void> {
    const result = await fetchJSON<HealthResponse>('/api/health')
    // A failed fetch or a non-"ok" status is treated identically as
    // "disconnected", never as an error state: a bridge with STOMP
    // down is an expected operating condition, not a fault in the
    // admin UI itself. The enriched fields (and the SOR link) are only
    // trusted when the health check itself succeeded with status ok;
    // a disconnected/failed poll clears them rather than showing stale
    // values from a prior successful poll.
    if (result.ok && result.data.status === 'ok') {
      reachable = 'ok'
      health = result.data
      healthError = ''
      sorHref = safeSorLink(result.data.sorLink)
    } else {
      reachable = 'disconnected'
      health = null
      sorHref = null
      healthError = result.ok ? '' : result.error
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

  <div class="health-fields" data-testid="health-fields">
    {#if health === null}
      <p>Bridge health detail unavailable{healthError ? `: ${healthError}` : ''}.</p>
    {:else}
      <dl>
        <dt>STOMP connection</dt>
        <dd data-testid="health-stomp">{health.stompConnected ? 'connected' : 'disconnected'}</dd>
        <dt>mTLS listener</dt>
        <dd data-testid="health-mtls">{health.mtlsListener}</dd>
        <dt>Server SFDI</dt>
        <dd data-testid="health-server-sfdi">{health.serverSfdi}</dd>
        <dt>Server LFDI</dt>
        <dd data-testid="health-server-lfdi">{health.serverLfdi}</dd>
        <dt>Feeder mRID</dt>
        <dd data-testid="health-feeder-mrid">{health.feederMrid}</dd>
        <dt>Simulation ID</dt>
        <dd data-testid="health-simulation-id">{health.simulationId}</dd>
        <dt>Registry entries</dt>
        <dd data-testid="health-registry-count">{health.registryCount}</dd>
        <dt>Placeholder identities</dt>
        <dd data-testid="health-placeholder-count">{health.placeholderCount}</dd>
        <dt>Certificate derived identities</dt>
        <dd data-testid="health-certificate-count">{health.certificateCount}</dd>
        <dt>Uptime (seconds)</dt>
        <dd data-testid="health-uptime">{health.uptimeSeconds}</dd>
      </dl>
    {/if}
  </div>

  <div class="sor-link" data-testid="sor-link-container">
    {#if sorHref !== null}
      <a data-testid="sor-link" href={sorHref} target="_blank" rel="noopener noreferrer">
        Server of record dashboard
      </a>
    {/if}
  </div>
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

  dl {
    display: grid;
    grid-template-columns: max-content auto;
    gap: 4px 16px;
    font-size: 0.9em;
  }

  dt {
    color: var(--text);
  }

  dd {
    margin: 0;
    color: var(--text-h);
  }

  .sor-link {
    margin-top: 20px;
  }

  .sor-link a {
    color: var(--text-h);
  }
</style>
