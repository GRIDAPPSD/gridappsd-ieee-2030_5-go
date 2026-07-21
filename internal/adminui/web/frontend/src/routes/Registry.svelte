<script lang="ts">
  // Registry renders GAGO-062's mRID <-> LFDI/SFDI/Name table from
  // GET /api/registry (internal/adminui/handlers.go's handleRegistry,
  // registryEntryResponse). Placeholder is a first class column with
  // its own visual treatment (a badge plus a row class), never a
  // silently-dropped detail: a placeholder LFDI is a Stage 1 stand-in,
  // not a real device identity, and the operator needs to be able to
  // tell entries apart at a glance.
  import { onMount } from 'svelte'
  import { fetchJSON } from '../lib/api'

  interface RegistryEntryResponse {
    mrid: string
    name: string
    lfdi: string
    sfdi: string
    placeholder: boolean
  }

  let entries: RegistryEntryResponse[] = $state([])
  let error = $state('')
  let loaded = $state(false)

  onMount(async () => {
    const result = await fetchJSON<RegistryEntryResponse[]>('/api/registry')
    if (result.ok) {
      entries = result.data
    } else {
      error = result.error
    }
    loaded = true
  })
</script>

<section class="panel">
  <h1>Registry map</h1>
  <p>mRID to LFDI/SFDI identity mapping. Read only.</p>

  {#if !loaded}
    <p>Loading registry...</p>
  {:else if error}
    <p class="error" data-testid="registry-error">Registry unavailable: {error}</p>
  {:else if entries.length === 0}
    <p data-testid="registry-empty">No registry entries yet.</p>
  {:else}
    <table data-testid="registry-table">
      <thead>
        <tr>
          <th>mRID</th>
          <th>Name</th>
          <th>LFDI</th>
          <th>SFDI</th>
          <th>Identity</th>
        </tr>
      </thead>
      <tbody>
        {#each entries as entry (entry.mrid)}
          <tr class={entry.placeholder ? 'placeholder-row' : ''}>
            <td>{entry.mrid}</td>
            <td>{entry.name}</td>
            <td>{entry.lfdi}</td>
            <td>{entry.sfdi}</td>
            <td>
              {#if entry.placeholder}
                <span class="badge placeholder" data-testid="placeholder-badge">placeholder</span
                >
              {:else}
                <span class="badge certificate">certificate</span>
              {/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
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

  .placeholder-row {
    background: color-mix(in srgb, #9a6700 12%, transparent);
  }

  .badge {
    display: inline-block;
    padding: 2px 8px;
    border-radius: 10px;
    font-size: 0.8em;
  }

  .badge.placeholder {
    background: #9a6700;
    color: white;
  }

  .badge.certificate {
    background: #1a7f37;
    color: white;
  }

  .error {
    color: #b91c1c;
  }
</style>
