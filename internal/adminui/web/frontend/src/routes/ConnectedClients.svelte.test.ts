// ConnectedClients.svelte.test.ts exercises GAGO-092: exact field-value
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
        return { ok: true, data: { clients: [], handshakes: [] } }
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

  it('shows an error state when the /api/served/edev fetch fails, independent of clients', async () => {
    vi.spyOn(api, 'fetchJSON').mockImplementation(async (path: string) => {
      if (path === '/api/clients') {
        return { ok: true, data: { clients: [], handshakes: [] } }
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
