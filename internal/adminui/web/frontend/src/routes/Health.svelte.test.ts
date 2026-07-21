// Health.svelte.test.ts exercises GAGO-061's landing panel: the
// reachable/disconnected branches driven by /api/health, and the
// registry-derived count/placeholder-tally branch driven by
// /api/registry. Both are mocked at the fetchJSON boundary
// (src/lib/api.ts), per the same rationale as Home.svelte.test.ts:
// fetchJSON is this component's only client of the server.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import Health from './Health.svelte'
import * as api from '../lib/api'

describe('Health', () => {
  it('renders ok and the registry tally when both endpoints resolve', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/health') {
        return { ok: true, data: { status: 'ok' } }
      }
      return {
        ok: true,
        data: [
          { mrid: 'mrid-1', name: 'inv-1', lfdi: 'LFDI1', sfdi: 'SFDI1', placeholder: false },
          { mrid: 'mrid-2', name: 'bat-1', lfdi: '', sfdi: '', placeholder: true },
        ],
      }
    })

    render(Health)

    const status = screen.getByTestId('health-status')
    await waitFor(() => expect(status).toHaveTextContent('Bridge admin API reachable.'))

    const summary = screen.getByTestId('registry-summary')
    await waitFor(() => {
      expect(summary).toHaveTextContent('Registry entries:')
      expect(summary).toHaveTextContent('2')
      expect(summary).toHaveTextContent('1')
    })
  })

  it('degrades to a disconnected state, not an error, when /api/health reports a non ok status', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/health') {
        return { ok: true, data: { status: 'degraded' } }
      }
      return { ok: true, data: [] }
    })

    render(Health)

    const status = screen.getByTestId('health-status')
    await waitFor(() =>
      expect(status).toHaveTextContent(
        'Bridge disconnected (STOMP link down); will retry automatically.',
      ),
    )
    expect(status.querySelector('.ok')).toBeNull()
  })

  it('degrades to a disconnected state, not an error, when the health fetch itself fails', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/health') {
        return { ok: false, error: 'network error', status: 0 }
      }
      return { ok: true, data: [] }
    })

    render(Health)

    const status = screen.getByTestId('health-status')
    await waitFor(() =>
      expect(status).toHaveTextContent(
        'Bridge disconnected (STOMP link down); will retry automatically.',
      ),
    )
  })
})
