// ServedResources.svelte.test.ts exercises GAGO-064: exact field-value
// rendering for served EndDevices+DERs and DERPrograms, plus the
// DefaultDERControl column's honest "not exposed by API" state (see
// this card's report: derProgramResponse omits DefaultDERControlLink
// on the wire today, so this panel cannot render a true present/absent
// distinction). Mocked at the fetchJSON boundary.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import ServedResources from './ServedResources.svelte'
import * as api from '../lib/api'

describe('ServedResources', () => {
  it('renders EndDevice and DERProgram field values from both endpoints', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/served/edev') {
        return {
          ok: true,
          data: [
            {
              id: 'edev-1',
              lfdi: 'LFDI1',
              sfdi: 'SFDI1',
              href: '/edev/LFDI1',
              enabled: true,
              ders: [{ id: 'der-1', href: '/edev/LFDI1/der/1' }],
            },
          ],
        }
      }
      return {
        ok: true,
        data: [
          {
            edevId: 'edev-1',
            id: '1',
            href: '/edev/edev-1/fsa/1/derp/1',
            mrid: 'derp-mrid-1',
            description: 'default',
            primacy: 0,
          },
        ],
      }
    })

    render(ServedResources)

    const edevTable = await screen.findByTestId('edevs-table')
    expect(edevTable).toHaveTextContent('edev-1')
    expect(edevTable).toHaveTextContent('LFDI1')
    expect(edevTable).toHaveTextContent('SFDI1')
    expect(edevTable).toHaveTextContent('yes')

    const programsTable = screen.getByTestId('programs-table')
    expect(programsTable).toHaveTextContent('derp-mrid-1')
    expect(programsTable).toHaveTextContent('default')
    expect(programsTable).toHaveTextContent('0')
  })

  it('shows the DefaultDERControl column as not exposed by API for a present program (known gap)', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/served/edev') return { ok: true, data: [] }
      return {
        ok: true,
        data: [
          {
            edevId: 'edev-1',
            id: '1',
            href: '/h',
            mrid: 'derp-1',
            description: 'default',
            primacy: 0,
          },
        ],
      }
    })

    render(ServedResources)

    const cell = await screen.findByTestId('default-der-control-cell')
    expect(cell).toHaveTextContent('not exposed by API')
  })

  it('shows an empty state, not an error, when no EndDevices or DERPrograms are served', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({ ok: true, data: [] })

    render(ServedResources)

    await waitFor(() => {
      expect(screen.getByTestId('edevs-empty')).toBeInTheDocument()
      expect(screen.getByTestId('programs-empty')).toBeInTheDocument()
    })
  })

  it('shows an error state when a fetch fails', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/served/edev') {
        return { ok: false, error: 'request failed with status 500', status: 500 }
      }
      return { ok: true, data: [] }
    })

    render(ServedResources)

    const error = await screen.findByTestId('served-error')
    expect(error).toHaveTextContent('request failed with status 500')
  })
})
