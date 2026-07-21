// DiscoveredDers.svelte.test.ts exercises GAGO-063: exact field-value
// rendering for a populated DER list, and the empty state for a feeder
// with no PowerElectronicsConnection. Mocked at the fetchJSON boundary.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import DiscoveredDers from './DiscoveredDers.svelte'
import * as api from '../lib/api'

describe('DiscoveredDers', () => {
  it('renders each DER field value alongside its owning EndDevice', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: [
        { edevId: 'edev-1', id: 'der-1', href: '/edev/edev-1/der/1' },
        { edevId: 'edev-2', id: 'der-2', href: '/edev/edev-2/der/2' },
      ],
    })

    render(DiscoveredDers)

    const table = await screen.findByTestId('ders-table')
    expect(table).toHaveTextContent('edev-1')
    expect(table).toHaveTextContent('der-1')
    expect(table).toHaveTextContent('/edev/edev-1/der/1')
    expect(table).toHaveTextContent('edev-2')
    expect(table).toHaveTextContent('der-2')
    expect(table).toHaveTextContent('/edev/edev-2/der/2')
  })

  it('shows a clear empty state, not an error, for an empty fleet', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({ ok: true, data: [] })

    render(DiscoveredDers)

    const empty = await screen.findByTestId('ders-empty')
    expect(empty).toHaveTextContent('No DERs discovered')
    expect(screen.queryByTestId('ders-table')).toBeNull()
  })

  it('shows an error state when the fetch fails', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: false,
      error: 'request failed with status 500',
      status: 500,
    })

    render(DiscoveredDers)

    const error = await screen.findByTestId('ders-error')
    expect(error).toHaveTextContent('request failed with status 500')
  })
})
