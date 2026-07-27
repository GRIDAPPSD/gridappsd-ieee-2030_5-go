<script lang="ts">
  // ConnectedClients renders GAGO-092's operator view over GAGO-090's
  // per-LFDI connection observer and GAGO-091's mTLS handshake log,
  // both served from GET /api/clients (handlers.go's handleClients,
  // clientsResponse). Read only: this panel has no form, no button, no
  // mutating fetch call.
  //
  // The served-vs-connected distinction (this card's third
  // requirement) is built by cross-referencing the already-existing
  // GET /api/served/edev roster (GAGO-064, also used by
  // ServedResources.svelte) against the /api/clients snapshot by LFDI:
  // a served EndDevice whose LFDI never appears in clients.Clients has
  // never issued an authenticated request, and reads as "never
  // connected" rather than being visually indistinguishable from one
  // that is actively polling. No new endpoint was introduced for this:
  // both endpoints the cross-reference needs already exist.
  import { onMount } from 'svelte'
  import { fetchJSON } from '../lib/api'

  interface ClientSnapshotResponse {
    lfdi: string
    lastSeen: string
    requestCount: number
    paths: string[]
  }

  interface HandshakeAttemptResponse {
    lfdi: string
    remoteAddr: string
    accepted: boolean
    reason: string
    known: boolean
    at: string
  }

  interface ClientsResponse {
    clients: ClientSnapshotResponse[]
    handshakes: HandshakeAttemptResponse[]
  }

  interface ServedEndDeviceResponse {
    id: string
    lfdi: string
    sfdi: string
    href: string
    enabled: boolean
    ders: { id: string; href: string }[]
  }

  interface ServedStatusRow {
    edevId: string
    lfdi: string
    connected: boolean
    lastSeen: string | null
    requestCount: number | null
  }

  let clients: ClientSnapshotResponse[] = $state([])
  let handshakes: HandshakeAttemptResponse[] = $state([])
  let served: ServedEndDeviceResponse[] = $state([])
  let clientsError = $state('')
  let servedError = $state('')
  let loaded = $state(false)

  // servedRows is the served-vs-connected cross-reference described
  // above: derived, not stored, so it always reflects the latest
  // fetched clients/served state per the workspace's JS/TS state
  // discipline (derived values are computed, not duplicated into a
  // second piece of state that could drift).
  let servedRows: ServedStatusRow[] = $derived(
    served.map((edev) => {
      const client = clients.find((c) => c.lfdi === edev.lfdi)
      return {
        edevId: edev.id,
        lfdi: edev.lfdi,
        connected: client !== undefined,
        lastSeen: client?.lastSeen ?? null,
        requestCount: client?.requestCount ?? null,
      }
    }),
  )

  // humanizeAge renders an RFC 3339 timestamp as a short relative age
  // ("42s ago", "3m ago", "2h ago", "1d ago") for operator scanning,
  // while the underlying ISO value is still carried on the <time>
  // element's datetime attribute so nothing precise is lost. An
  // unparseable timestamp renders unchanged rather than "NaN ago".
  function humanizeAge(iso: string): string {
    const then = new Date(iso).getTime()
    if (Number.isNaN(then)) return iso
    const diffSec = Math.max(0, Math.floor((Date.now() - then) / 1000))
    if (diffSec < 5) return 'just now'
    if (diffSec < 60) return `${diffSec}s ago`
    const diffMin = Math.floor(diffSec / 60)
    if (diffMin < 60) return `${diffMin}m ago`
    const diffHr = Math.floor(diffMin / 60)
    if (diffHr < 24) return `${diffHr}h ago`
    const diffDay = Math.floor(diffHr / 24)
    return `${diffDay}d ago`
  }

  onMount(async () => {
    const [clientsResult, servedResult] = await Promise.all([
      fetchJSON<ClientsResponse>('/api/clients'),
      fetchJSON<ServedEndDeviceResponse[]>('/api/served/edev'),
    ])

    if (clientsResult.ok) {
      clients = clientsResult.data.clients
      handshakes = clientsResult.data.handshakes
    } else {
      clientsError = clientsResult.error
    }

    if (servedResult.ok) {
      served = servedResult.data
    } else {
      servedError = servedResult.error
    }

    loaded = true
  })
</script>

<section class="panel">
  <h1>Connected clients</h1>
  <p>
    LFDIs that have issued authenticated requests, recent mTLS handshake attempts (accepted vs
    rejected, and why), and how the served EndDevice roster maps onto connection state. Read only.
  </p>

  {#if !loaded}
    <p>Loading connected clients...</p>
  {:else}
    <h2>Connected clients</h2>
    {#if clientsError}
      <p class="error" data-testid="clients-error">Connected clients unavailable: {clientsError}</p>
    {:else if clients.length === 0}
      <p data-testid="clients-empty">No clients connected yet.</p>
    {:else}
      <table data-testid="clients-table">
        <thead>
          <tr>
            <th scope="col">LFDI</th>
            <th scope="col">Last seen</th>
            <th scope="col">Requests</th>
            <th scope="col">Paths touched</th>
          </tr>
        </thead>
        <tbody>
          {#each clients as client (client.lfdi)}
            <tr>
              <td>{client.lfdi}</td>
              <td><time datetime={client.lastSeen}>{humanizeAge(client.lastSeen)}</time></td>
              <td>{client.requestCount}</td>
              <td>{client.paths.join(', ')}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}

    <h2>Served EndDevices: connection status</h2>
    <p class="note">
      Cross references GET /api/served/edev (the served roster) against the connected-client
      snapshot above, by LFDI: a served EndDevice that has never issued a request shows "never
      connected" instead of being indistinguishable from one that is actively polling.
    </p>
    {#if servedError}
      <p class="error" data-testid="served-status-error">
        Served EndDevice roster unavailable: {servedError}
      </p>
    {:else if servedRows.length === 0}
      <p data-testid="served-status-empty">No EndDevices served.</p>
    {:else}
      <table data-testid="served-status-table">
        <thead>
          <tr>
            <th scope="col">EndDevice</th>
            <th scope="col">LFDI</th>
            <th scope="col">Status</th>
            <th scope="col">Last seen</th>
            <th scope="col">Requests</th>
          </tr>
        </thead>
        <tbody>
          {#each servedRows as row (row.edevId)}
            <tr>
              <td>{row.edevId}</td>
              <td>{row.lfdi}</td>
              <td>
                {#if row.connected}
                  <span class="badge connected" data-testid="status-badge">connected</span>
                {:else}
                  <span class="badge never-connected" data-testid="status-badge"
                    >never connected</span
                  >
                {/if}
              </td>
              <td>{row.lastSeen ? humanizeAge(row.lastSeen) : '-'}</td>
              <td>{row.requestCount ?? '-'}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}

    <h2>Handshake attempts (cert validity)</h2>
    {#if clientsError}
      <p class="error" data-testid="handshakes-error">
        Handshake attempts unavailable: {clientsError}
      </p>
    {:else if handshakes.length === 0}
      <p data-testid="handshakes-empty">No handshake attempts recorded yet.</p>
    {:else}
      <table data-testid="handshakes-table">
        <thead>
          <tr>
            <th scope="col">LFDI</th>
            <th scope="col">Remote address</th>
            <th scope="col">Result</th>
            <th scope="col">Reason</th>
            <th scope="col">Known</th>
            <th scope="col">At</th>
          </tr>
        </thead>
        <tbody>
          {#each handshakes as handshake (handshake.lfdi + ':' + handshake.at)}
            <tr>
              <td>{handshake.lfdi}</td>
              <td>{handshake.remoteAddr || '-'}</td>
              <td>
                {#if handshake.accepted}
                  <span class="badge accepted" data-testid="handshake-result">accepted</span>
                {:else}
                  <span class="badge rejected" data-testid="handshake-result">rejected</span>
                {/if}
              </td>
              <td>{handshake.reason || '-'}</td>
              <td>
                {#if handshake.known}
                  <span class="badge known" data-testid="handshake-known">known</span>
                {:else}
                  <span class="badge unknown" data-testid="handshake-known">unknown</span>
                {/if}
              </td>
              <td><time datetime={handshake.at}>{humanizeAge(handshake.at)}</time></td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  {/if}
</section>

<style>
  .panel {
    max-width: 1080px;
    margin: 0 auto;
    padding: 32px 20px;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    margin-bottom: 20px;
  }

  th,
  td {
    text-align: left;
    padding: 6px 10px;
    border-bottom: 1px solid var(--border);
    font-size: 0.9em;
  }

  .note {
    color: var(--text);
    font-size: 0.85em;
  }

  .error {
    color: #b91c1c;
  }

  .badge {
    display: inline-block;
    padding: 2px 8px;
    border-radius: 10px;
    font-size: 0.8em;
    color: white;
  }

  .badge.connected,
  .badge.accepted,
  .badge.known {
    background: #1a7f37;
  }

  .badge.never-connected,
  .badge.unknown {
    background: #9a6700;
  }

  .badge.rejected {
    background: #b91c1c;
  }
</style>
