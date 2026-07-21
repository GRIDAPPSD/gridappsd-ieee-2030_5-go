<script lang="ts">
  // ServedResources renders GAGO-064's read only view of what the
  // embedded server actually serves: EndDevices+DERs from
  // GET /api/served/edev (handlers.go's handleServedEndDevices,
  // endDeviceResponse) and DERPrograms from
  // GET /api/served/derprogram (handleServedDERPrograms,
  // derProgramResponse). NO write controls: this panel has no form, no
  // button, no mutating fetch call.
  //
  // Known gap (see this card's report): derProgramResponse does not
  // serialize DefaultDERControlLink, even though the underlying
  // sep2embed.DERProgramSnapshot carries that field. This panel cannot
  // show a true present/absent DefaultDERControl state without that
  // field on the wire, and does not fabricate one; the column instead
  // reports "not exposed by API" until the backend adds the field.
  import { onMount } from 'svelte'
  import { fetchJSON } from '../lib/api'

  interface DerResponse {
    id: string
    href: string
  }

  interface EndDeviceResponse {
    id: string
    lfdi: string
    sfdi: string
    href: string
    enabled: boolean
    ders: DerResponse[]
  }

  interface DerProgramResponse {
    edevId: string
    id: string
    href: string
    mrid: string
    description: string
    primacy: number
  }

  let edevs: EndDeviceResponse[] = $state([])
  let programs: DerProgramResponse[] = $state([])
  let error = $state('')
  let loaded = $state(false)

  onMount(async () => {
    const [edevResult, programResult] = await Promise.all([
      fetchJSON<EndDeviceResponse[]>('/api/served/edev'),
      fetchJSON<DerProgramResponse[]>('/api/served/derprogram'),
    ])

    if (edevResult.ok) {
      edevs = edevResult.data
    } else {
      error = edevResult.error
    }
    if (programResult.ok) {
      programs = programResult.data
    } else if (!error) {
      error = programResult.error
    }
    loaded = true
  })
</script>

<section class="panel">
  <h1>Served resources</h1>
  <p>EndDevices, DERs, and DERPrograms as actually served by the embedded server. Read only.</p>

  {#if !loaded}
    <p>Loading served resources...</p>
  {:else if error}
    <p class="error" data-testid="served-error">Served resources unavailable: {error}</p>
  {:else}
    <h2>EndDevices</h2>
    {#if edevs.length === 0}
      <p data-testid="edevs-empty">No EndDevices served.</p>
    {:else}
      <table data-testid="edevs-table">
        <thead>
          <tr>
            <th>ID</th>
            <th>LFDI</th>
            <th>SFDI</th>
            <th>Enabled</th>
            <th>DERs</th>
          </tr>
        </thead>
        <tbody>
          {#each edevs as edev (edev.id)}
            <tr>
              <td>{edev.id}</td>
              <td>{edev.lfdi}</td>
              <td>{edev.sfdi}</td>
              <td>{edev.enabled ? 'yes' : 'no'}</td>
              <td>{edev.ders.length}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}

    <h2>DER programs</h2>
    {#if programs.length === 0}
      <p data-testid="programs-empty">No DERPrograms served.</p>
    {:else}
      <table data-testid="programs-table">
        <thead>
          <tr>
            <th>EndDevice</th>
            <th>MRID</th>
            <th>Description</th>
            <th>Primacy</th>
            <th>DefaultDERControl</th>
          </tr>
        </thead>
        <tbody>
          {#each programs as program (program.edevId + ':' + program.id)}
            <tr>
              <td>{program.edevId}</td>
              <td>{program.mrid}</td>
              <td>{program.description}</td>
              <td>{program.primacy}</td>
              <td data-testid="default-der-control-cell">not exposed by API</td>
            </tr>
          {/each}
        </tbody>
      </table>
      <p class="note" data-testid="served-known-gaps">
        DefaultDERControl presence/absence is not yet exposed by
        GET /api/served/derprogram; the underlying DERProgramSnapshot carries a
        DefaultDERControlLink field that the current JSON response omits.
      </p>
    {/if}
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
    margin-bottom: 20px;
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
