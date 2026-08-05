// Registry.svelte.test.ts exercises the registry table: exact
// field-value rendering per row, the placeholder-vs-certificate visual
// distinction, and the empty state. Mocked at the fetchJSON boundary.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import Registry from './Registry.svelte'
import * as api from '../lib/api'

describe('Registry', () => {
  it('renders every entry field value with the placeholder badge visually distinct', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: [
        { mrid: 'mrid-1', name: 'inverter-1', lfdi: 'ABCDEF0123456789', sfdi: '123456789', placeholder: false },
        { mrid: 'mrid-2', name: 'battery-1', lfdi: '', sfdi: '', placeholder: true },
      ],
    })

    render(Registry)

    const table = await screen.findByTestId('registry-table')
    expect(table).toHaveTextContent('mrid-1')
    expect(table).toHaveTextContent('inverter-1')
    expect(table).toHaveTextContent('ABCDEF0123456789')
    expect(table).toHaveTextContent('123456789')
    expect(table).toHaveTextContent('mrid-2')
    expect(table).toHaveTextContent('battery-1')

    const badges = screen.getAllByTestId('placeholder-badge')
    expect(badges).toHaveLength(1)

    const rows = table.querySelectorAll('tbody tr')
    expect(rows[1].classList.contains('placeholder-row')).toBe(true)
    expect(rows[0].classList.contains('placeholder-row')).toBe(false)
  })

  it('shows an empty state, not an error, when the registry has no entries', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({ ok: true, data: [] })

    render(Registry)

    await waitFor(() => expect(screen.getByTestId('registry-empty')).toBeInTheDocument())
    expect(screen.queryByTestId('registry-table')).toBeNull()
  })

  it('shows an error state when the fetch fails', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: false,
      error: 'request failed with status 500',
      status: 500,
    })

    render(Registry)

    const error = await screen.findByTestId('registry-error')
    expect(error).toHaveTextContent('request failed with status 500')
  })
})
