// ConnectedClients.svelte.test.ts exercises exact field-value
// rendering for connected clients and mTLS handshake attempts from
// GET /api/clients, plus the served-vs-connected cross-reference
// against GET /api/served/edev (a served EndDevice that has never
// issued a request renders "never connected", distinct from one that
// is actively polling). Mocked at the fetchJSON boundary.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import ConnectedClients from './ConnectedClients.svelte'
import * as api from '../lib/api'

describe('ConnectedClients', () => {
  it('renders connected clients and distinguishes accepted vs rejected handshakes', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/clients') {
        return {
          ok: true,
          data: {
            clients: [
              {
                lfdi: 'LFDI-CONNECTED',
                lastSeen: '2026-07-27T09:15:30.000Z',
                requestCount: 7,
                paths: ['/dcap', '/edev'],
              },
            ],
            handshakes: [
              {
                lfdi: 'LFDI-CONNECTED',
                remoteAddr: '10.0.0.4:54000',
                accepted: true,
                reason: '',
                known: true,
                at: '2026-07-27T09:14:30.000Z',
              },
              {
                lfdi: 'LFDI-UNKNOWN',
                remoteAddr: '10.0.0.5:54321',
                accepted: false,
                reason: 'x509: certificate signed by unknown authority',
                known: false,
                at: '2026-07-27T09:14:00.000Z',
              },
              {
                lfdi: 'LFDI-NO-REMOTE-ADDR',
                remoteAddr: '',
                accepted: false,
                reason: 'connection closed before handshake',
                known: false,
                at: '2026-07-27T09:13:00.000Z',
              },
            ],
            observationDisabled: false,
          },
        }
      }
      // /api/served/edev
      return {
        ok: true,
        data: [
          {
            id: 'edev-1',
            lfdi: 'LFDI-CONNECTED',
            sfdi: 'SFDI1',
            href: '/edev/LFDI-CONNECTED',
            enabled: true,
            ders: [],
          },
          {
            id: 'edev-2',
            lfdi: 'LFDI-NEVER-CONNECTED',
            sfdi: 'SFDI2',
            href: '/edev/LFDI-NEVER-CONNECTED',
            enabled: true,
            ders: [],
          },
        ],
      }
    })

    render(ConnectedClients)

    const clientsTable = await screen.findByTestId('clients-table')
    expect(clientsTable).toHaveTextContent('LFDI-CONNECTED')
    expect(clientsTable).toHaveTextContent('7')
    expect(clientsTable).toHaveTextContent('/dcap, /edev')

    const handshakeResults = screen.getAllByTestId('handshake-result')
    expect(handshakeResults).toHaveLength(3)
    expect(handshakeResults[0]).toHaveTextContent('accepted')
    expect(handshakeResults[1]).toHaveTextContent('rejected')
    expect(handshakeResults[2]).toHaveTextContent('rejected')

    const handshakesTable = screen.getByTestId('handshakes-table')
    expect(handshakesTable).toHaveTextContent('x509: certificate signed by unknown authority')
    expect(handshakesTable).toHaveTextContent('LFDI-UNKNOWN')

    // known/unknown badge: rendered from the contract's known field for
    // each row, in handshake order (known, unknown, unknown).
    const knownBadges = screen.getAllByTestId('handshake-known')
    expect(knownBadges).toHaveLength(3)
    expect(knownBadges[0]).toHaveTextContent('known')
    expect(knownBadges[1]).toHaveTextContent('unknown')
    expect(knownBadges[2]).toHaveTextContent('unknown')

    // remoteAddr fallback: populated value renders as-is; empty
    // remoteAddr (LFDI-NO-REMOTE-ADDR) renders '-', consistent with the
    // reason column's existing '-' fallback, rather than a blank cell
    // that is indistinguishable from a rendering bug.
    const handshakeRows = Array.from(handshakesTable.querySelectorAll('tbody tr'))
    const noRemoteAddrRow = handshakeRows.find((row) =>
      row.textContent?.includes('LFDI-NO-REMOTE-ADDR'),
    )
    expect(noRemoteAddrRow).toBeDefined()
    expect(noRemoteAddrRow).toHaveTextContent('LFDI-NO-REMOTE-ADDR')
    const remoteAddrCell = noRemoteAddrRow?.querySelectorAll('td')[1]
    expect(remoteAddrCell).toHaveTextContent('-')
    expect(remoteAddrCell?.textContent).toBe('-')

    const connectedRow = handshakeRows.find((row) => row.textContent?.includes('LFDI-CONNECTED'))
    const connectedRemoteAddrCell = connectedRow?.querySelectorAll('td')[1]
    expect(connectedRemoteAddrCell?.textContent).toBe('10.0.0.4:54000')

    // served-vs-connected cross-reference: LFDI-CONNECTED shows
    // "connected", LFDI-NEVER-CONNECTED (served but never in
    // clients.Clients) shows "never connected".
    const statusTable = screen.getByTestId('served-status-table')
    const badges = screen.getAllByTestId('status-badge')
    expect(badges).toHaveLength(2)
    expect(statusTable).toHaveTextContent('LFDI-NEVER-CONNECTED')
    const neverConnectedBadge = badges.find((b) => b.textContent === 'never connected')
    const connectedBadge = badges.find((b) => b.textContent === 'connected')
    expect(neverConnectedBadge).toBeDefined()
    expect(connectedBadge).toBeDefined()
  })

  it('shows explicit empty states, not a crash, when clients and handshakes are both []', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/clients') {
        return { ok: true, data: { clients: [], handshakes: [], observationDisabled: false } }
      }
      return { ok: true, data: [] }
    })

    render(ConnectedClients)

    await waitFor(() => {
      expect(screen.getByTestId('clients-empty')).toHaveTextContent('No clients connected yet.')
      expect(screen.getByTestId('handshakes-empty')).toBeInTheDocument()
      expect(screen.getByTestId('served-status-empty')).toBeInTheDocument()
    })
    expect(screen.queryByTestId('clients-table')).toBeNull()
    expect(screen.queryByTestId('handshakes-table')).toBeNull()
  })

  it('shows an error state when the /api/clients fetch fails', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/clients') {
        return { ok: false, error: 'request failed with status 500', status: 500 }
      }
      return { ok: true, data: [] }
    })

    render(ConnectedClients)

    const error = await screen.findByTestId('clients-error')
    expect(error).toHaveTextContent('request failed with status 500')
    const handshakesError = screen.getByTestId('handshakes-error')
    expect(handshakesError).toHaveTextContent('request failed with status 500')
  })

  it('badges served devices "unknown", not "never connected", when the /api/clients fetch fails', async () => {
    // #101 round 3 review HIGH: the served-status section ignored
    // clientsError entirely and rendered servedRows regardless, so a
    // device the panel has no data about was badged "never connected",
    // the exact false claim this PR exists to remove, just reached by a
    // fetch failure instead of a config flag. Reproduced first with a
    // scratch test (removed) before this permanent one was written.
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/clients') {
        return { ok: false, error: 'request failed with status 500', status: 500 }
      }
      return {
        ok: true,
        data: [
          {
            id: 'edev-1',
            lfdi: 'LFDI-MAYBE-POLLING',
            sfdi: 'SFDI1',
            href: '/edev/LFDI-MAYBE-POLLING',
            enabled: true,
            ders: [],
          },
        ],
      }
    })

    render(ConnectedClients)

    await screen.findByTestId('clients-error')
    const note = screen.getByTestId('served-status-clients-error-note')
    expect(note).toHaveTextContent('request failed with status 500')

    const badges = screen.getAllByTestId('status-badge')
    expect(badges).toHaveLength(1)
    expect(badges[0]).toHaveTextContent('unknown')
    expect(badges[0]).not.toHaveTextContent('never connected')
  })

  it('badges every served device "unknown", not "never connected", when observation is disabled', async () => {
    // #101: with the connection observer off (SEP2_ENABLE_CCM), clients
    // is always [] regardless of real traffic, so the panel must not
    // assert a false "connected" or "never connected" verdict for an
    // actively polling device, in any of its three sections.
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/clients') {
        return { ok: true, data: { clients: [], handshakes: [], observationDisabled: true } }
      }
      return {
        ok: true,
        data: [
          {
            id: 'edev-1',
            lfdi: 'LFDI-POLLING',
            sfdi: 'SFDI1',
            href: '/edev/LFDI-POLLING',
            enabled: true,
            ders: [],
          },
        ],
      }
    })

    render(ConnectedClients)

    const note = await screen.findByTestId('observation-disabled-note')
    expect(note).toHaveTextContent('unknown')

    const badges = screen.getAllByTestId('status-badge')
    expect(badges).toHaveLength(1)
    expect(badges[0]).toHaveTextContent('unknown')
    expect(badges[0]).not.toHaveTextContent('never connected')

    // Round 3 review MEDIUM: the top "Connected clients" table and the
    // "Handshake attempts" table both asserted "No clients connected
    // yet." / "No handshake attempts recorded yet." while observation
    // was off, the same false-positive claim the served-status badge
    // above was already fixed for.
    expect(screen.queryByTestId('clients-empty')).toBeNull()
    const clientsNote = screen.getByTestId('clients-observation-disabled')
    expect(clientsNote).toHaveTextContent('disabled')
    expect(clientsNote).not.toHaveTextContent('No clients connected')

    expect(screen.queryByTestId('handshakes-empty')).toBeNull()
    const handshakesNote = screen.getByTestId('handshakes-observation-disabled')
    expect(handshakesNote).toHaveTextContent('disabled')
    expect(handshakesNote).not.toHaveTextContent('No handshake attempts')
  })

  it('badges a never-observed served device "never connected" when observation is enabled (default)', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/clients') {
        return { ok: true, data: { clients: [], handshakes: [], observationDisabled: false } }
      }
      return {
        ok: true,
        data: [
          {
            id: 'edev-1',
            lfdi: 'LFDI-NEVER-CONNECTED',
            sfdi: 'SFDI1',
            href: '/edev/LFDI-NEVER-CONNECTED',
            enabled: true,
            ders: [],
          },
        ],
      }
    })

    render(ConnectedClients)

    const badges = await screen.findAllByTestId('status-badge')
    expect(badges).toHaveLength(1)
    expect(badges[0]).toHaveTextContent('never connected')
    expect(screen.queryByTestId('observation-disabled-note')).toBeNull()
  })

  it('shows an error state when the /api/served/edev fetch fails, independent of clients', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/clients') {
        return { ok: true, data: { clients: [], handshakes: [], observationDisabled: false } }
      }
      return { ok: false, error: 'request failed with status 500', status: 500 }
    })

    render(ConnectedClients)

    const error = await screen.findByTestId('served-status-error')
    expect(error).toHaveTextContent('request failed with status 500')
    // The clients panel itself is unaffected by the served-roster error.
    expect(screen.getByTestId('clients-empty')).toBeInTheDocument()
  })
})
