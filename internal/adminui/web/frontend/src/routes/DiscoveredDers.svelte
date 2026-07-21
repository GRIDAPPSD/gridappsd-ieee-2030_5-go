<script lang="ts">
  // DiscoveredDers renders GAGO-063's discovered-DER list from
  // GET /api/ders (internal/adminui/handlers.go's handleDERs,
  // derWithOwnerResponse). GAGO-074/077: the response now also carries
  // feederMrid, the bridge's own configured feeder mRID stamped onto
  // every entry, and this panel renders it as a column. DER type
  // (inverter/solar/battery) is still NOT carried on the wire (GAGO-076
  // pending, needs a CIM query change); the gap note below is kept for
  // that field only, not fabricated here.
  import { onMount } from 'svelte'
  import { fetchJSON } from '../lib/api'

  interface DerWithOwnerResponse {
    edevId: string
    id: string
    href: string
    feederMrid: string
  }

  let ders: DerWithOwnerResponse[] = $state([])
  let error = $state('')
  let loaded = $state(false)

  onMount(async () => {
    const result = await fetchJSON<DerWithOwnerResponse[]>('/api/ders')
    if (result.ok) {
      ders = result.data
    } else {
      error = result.error
    }
    loaded = true
  })
</script>

<section class="panel">
  <h1>Discovered DERs</h1>
  <p>DER resources discovered from the CIM model, joined to the owning EndDevice. Read only.</p>

  {#if !loaded}
    <p>Loading DERs...</p>
  {:else if error}
    <p class="error" data-testid="ders-error">Discovered DERs unavailable: {error}</p>
  {:else if ders.length === 0}
    <p data-testid="ders-empty">
      No DERs discovered (the feeder model had no PowerElectronicsConnection).
    </p>
  {:else}
    <table data-testid="ders-table">
      <thead>
        <tr>
          <th>EndDevice</th>
          <th>DER ID</th>
          <th>Href</th>
          <th>Feeder mRID</th>
        </tr>
      </thead>
      <tbody>
        {#each ders as der (der.edevId + ':' + der.id)}
          <tr>
            <td>{der.edevId}</td>
            <td>{der.id}</td>
            <td>{der.href}</td>
            <td data-testid="der-feeder-mrid-cell">{der.feederMrid}</td>
          </tr>
        {/each}
      </tbody>
    </table>
    <p class="note" data-testid="ders-known-gaps">
      DER type (inverter/solar/battery) is not yet exposed by GET /api/ders; this table
      shows the owning EndDevice, DER ID, Href, and feeder mRID only.
    </p>
  {/if}
</section>

<style>
  .panel {
    max-width: 960px;
    margin: 0 auto;
    padding: 32px 20px;
  }

  table {
    width: 100%;
    border-collapse: collapse;
  }

  th,
  td {
    text-align: left;
    padding: 6px 10px;
    border-bottom: 1px solid var(--border);
    font-size: 0.9em;
  }

  .error {
    color: #b91c1c;
  }

  .note {
    color: var(--text);
    font-size: 0.85em;
  }
</style>
