// ControlFlow.svelte.test.ts exercises GAGO-065: exact field-value
// rendering for a populated last-delta snapshot, the explicit idle
// state when last is null (a real, distinct third state per
// handleControlFlow's doc comment, not a present-but-empty delta), and
// the error state. Mocked at the fetchJSON boundary.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import ControlFlow from './ControlFlow.svelte'
import * as api from '../lib/api'

describe('ControlFlow', () => {
  it('renders topics, counters, and the last delta field values', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: {
        applied: 4,
        skipped: 1,
        last: {
          object: 'mrid-inv-1',
          attribute: 'DERControl.DERControlBase.opModTargetW',
          value: { multiplier: 0, value: 4200 },
          appliedAt: '2026-07-18T12:30:00.000Z',
        },
        outputTopic: '/topic/goss.gridappsd.simulation.output.12345',
        inputTopic: '/topic/goss.gridappsd.simulation.input.12345',
      },
    })

    render(ControlFlow)

    const topics = await screen.findByTestId('controlflow-topics')
    expect(topics).toHaveTextContent('/topic/goss.gridappsd.simulation.output.12345')
    expect(topics).toHaveTextContent('/topic/goss.gridappsd.simulation.input.12345')

    const counters = screen.getByTestId('controlflow-counters')
    expect(counters).toHaveTextContent('4')
    expect(counters).toHaveTextContent('1')

    const lastDelta = screen.getByTestId('controlflow-last-delta')
    expect(lastDelta).toHaveTextContent('mrid-inv-1')
    expect(lastDelta).toHaveTextContent('DERControl.DERControlBase.opModTargetW')
    expect(lastDelta).toHaveTextContent('2026-07-18T12:30:00.000Z')
  })

  it('renders a clear idle state, not an empty table, when no delta has been applied', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: {
        applied: 0,
        skipped: 0,
        last: null,
        outputTopic: '/topic/goss.gridappsd.simulation.output.1',
        inputTopic: '/topic/goss.gridappsd.simulation.input.1',
      },
    })

    render(ControlFlow)

    const idle = await screen.findByTestId('controlflow-idle')
    expect(idle).toHaveTextContent('No control delta has been applied yet.')
    expect(screen.queryByTestId('controlflow-last-delta')).toBeNull()
  })

  it('shows an error state when the fetch fails', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: false,
      error: 'request failed with status 500',
      status: 500,
    })

    render(ControlFlow)

    const error = await screen.findByTestId('controlflow-error')
    expect(error).toHaveTextContent('request failed with status 500')
  })
})
