<script lang="ts">
  // ControlFlow renders the control-flow observation panel from
  // GET /api/controlflow (handlers.go's handleControlFlow,
  // controlFlowResponse). Read only: this panel has no form, no
  // button, no mutating fetch call. `last: null` is a real, distinct
  // third state (no delta applied yet), not a present-but-empty delta,
  // per handleControlFlow's own doc comment; this panel renders that
  // as an explicit idle state rather than an empty table.
  import { onMount } from 'svelte'
  import { fetchJSON } from '../lib/api'

  interface LastDeltaResponse {
    object: string
    attribute: string
    value: unknown
    appliedAt: string
  }

  interface ControlFlowResponse {
    applied: number
    skipped: number
    last: LastDeltaResponse | null
    outputTopic: string
    inputTopic: string
  }

  let flow: ControlFlowResponse | null = $state(null)
  let error = $state('')
  let loaded = $state(false)

  onMount(async () => {
    const result = await fetchJSON<ControlFlowResponse>('/api/controlflow')
    if (result.ok) {
      flow = result.data
    } else {
      error = result.error
    }
    loaded = true
  })
</script>

<section class="panel">
  <h1>Control flow</h1>
  <p>STOMP subscription topics and the last applied control delta. Read only.</p>

  {#if !loaded}
    <p>Loading control flow state...</p>
  {:else if error}
    <p class="error" data-testid="controlflow-error">Control flow unavailable: {error}</p>
  {:else if flow}
    <dl class="topics" data-testid="controlflow-topics">
      <dt>Simulation output topic</dt>
      <dd>{flow.outputTopic}</dd>
      <dt>Control delta input topic</dt>
      <dd>{flow.inputTopic}</dd>
    </dl>

    <div class="counters" data-testid="controlflow-counters">
      <span>Applied: <strong>{flow.applied}</strong></span>
      <span>Skipped: <strong>{flow.skipped}</strong></span>
    </div>

    {#if flow.last === null}
      <p data-testid="controlflow-idle">No control delta has been applied yet.</p>
    {:else}
      <dl class="last-delta" data-testid="controlflow-last-delta">
        <dt>Object</dt>
        <dd>{flow.last.object}</dd>
        <dt>Attribute</dt>
        <dd>{flow.last.attribute}</dd>
        <dt>Value</dt>
        <dd>{JSON.stringify(flow.last.value)}</dd>
        <dt>Applied at</dt>
        <dd>{flow.last.appliedAt}</dd>
      </dl>
    {/if}
  {/if}
</section>

<style>
  .panel {
    max-width: 720px;
    margin: 0 auto;
    padding: 32px 20px;
  }

  dl {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 4px 12px;
    margin: 12px 0;
  }

  dt {
    color: var(--text);
    font-size: 0.85em;
  }

  dd {
    margin: 0;
  }

  .counters {
    display: flex;
    gap: 20px;
    margin: 12px 0;
  }

  .error {
    color: #b91c1c;
  }
</style>
