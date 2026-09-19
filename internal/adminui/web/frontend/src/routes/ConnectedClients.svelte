<script lang="ts">
  // ConnectedClients renders the operator view over the
  // per-LFDI connection observer and the mTLS handshake log,
  // both served from GET /api/clients (handlers.go's handleClients,
  // clientsResponse). Read only: this panel has no form, no button, no
  // mutating fetch call.
  //
  // The served-vs-connected distinction is built by cross-referencing
  // the already-existing
  // GET /api/served/edev roster (also used by
  // ServedResources.svelte) against the /api/clients snapshot by LFDI:
  // a served EndDevice whose LFDI never appears in clients.Clients has
  // never issued an authenticated request, and reads as "never
  // connected" rather than being visually indistinguishable from one
  // that is actively polling. No new endpoint was introduced for this:
  // both endpoints the cross-reference needs already exist.
  //
  // That distinction requires a live observer. When
  // clientsResponse.ObservationDisabled is true (SEP2_ENABLE_CCM),
  // clients.Clients is always empty regardless of real traffic, so every
  // served device reads "unknown" here instead of a false "connected"
  // or "never connected" claim (#101). The same reasoning applies when
  // the /api/clients fetch itself fails (clientsError set): the panel
  // has no clients data either way, and must not assert a status it
  // does not know on that path either.
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
    observationDisabled: boolean
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
  // observationDisabled mirrors clientsResponse.ObservationDisabled
  // (handlers.go): true when the bridge's connection observer is not
  // wired (SEP2_ENABLE_CCM), so clients/handshakes above are always
  // empty regardless of real traffic. Distinct from clientsError: this
  // is a valid, successful response saying "this snapshot cannot show a
  // connection", not a fetch failure. Only ever set inside the
  // clientsResult.ok branch below: a response the bridge and frontend
  // ship in the same binary always carries this field, so a payload
  // missing it (an older bridge served against a newer frontend, or
  // vice versa) is out of scope here: both halves ship in one binary.
  let observationDisabled = $state(false)

  // connectionStatusUnknown is true whenever the panel has no reliable
  // clients data to cross-reference against the served roster, for
  // either reason the "Served EndDevices: connection status" section
  // and the two panel sections above it must not assert a status they
  // do not know (#101): the observer is disabled (observationDisabled),
  // or the /api/clients fetch itself failed (clientsError). The two
  // causes get distinct messages below, but the SAME "unknown" badge
  // and the SAME refusal to claim "never connected": a fetch failure is
  // not evidence of anything about the device.
  let connectionStatusUnknown = $derived(observationDisabled || clientsError !== '')

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
      observationDisabled = clientsResult.data.observationDisabled
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
    {:else if observationDisabled}
      <p data-testid="clients-observation-disabled">
        Connection observation is disabled (SEP2_ENABLE_CCM): this list cannot show which clients,
        if any, are connected.
      </p>
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
    {#if observationDisabled}
      <p class="note" data-testid="observation-disabled-note">
        The connection observer is disabled on this bridge (SEP2_ENABLE_CCM). Connection status
        below is unknown for every served device, not "never connected": a device may be actively
        polling with nothing here to show it.
      </p>
    {:else if clientsError}
      <p class="error" data-testid="served-status-clients-error-note">
        Connection status is unavailable: the connected-client snapshot could not be fetched
        ({clientsError}). Status below is unknown for every served device, not "never connected".
      </p>
    {:else}
      <p class="note">
        Cross references GET /api/served/edev (the served roster) against the connected-client
        snapshot above, by LFDI: a served EndDevice that has never issued a request shows "never
        connected" instead of being indistinguishable from one that is actively polling.
      </p>
    {/if}
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
                {:else if connectionStatusUnknown}
                  <span class="badge unknown-status" data-testid="status-badge">unknown</span>
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
    {:else if observationDisabled}
      <p data-testid="handshakes-observation-disabled">
        Connection observation is disabled (SEP2_ENABLE_CCM): this list cannot show handshake
        attempts.
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
  .badge.unknown,
  .badge.unknown-status {
    background: #9a6700;
  }

  .badge.rejected {
    background: #b91c1c;
  }
</style>
