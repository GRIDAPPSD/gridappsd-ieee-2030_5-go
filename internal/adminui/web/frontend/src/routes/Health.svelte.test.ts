// Health.svelte.test.ts exercises the landing panel:
// the reachable/disconnected branches driven by /api/health, the
// enriched field rendering (stompConnected, mtlsListener,
// serverSfdi/Lfdi, feederMrid, simulationId, the registry tally,
// uptimeSeconds), and the SOR link fail-closed scheme guard.
// Mocked at the fetchJSON boundary (src/lib/api.ts).
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import Health from './Health.svelte'
import * as api from '../lib/api'

function healthPayload(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    status: 'ok',
    stompConnected: true,
    mtlsListener: '0.0.0.0:8443',
    serverSfdi: 'SFDI-SERVER-1',
    serverLfdi: 'LFDI-SERVER-1',
    feederMrid: 'feeder-mrid-1',
    simulationId: 'sim-1',
    registryCount: 3,
    placeholderCount: 1,
    certificateCount: 2,
    uptimeSeconds: 4242,
    sorLink: '',
    ...overrides,
  }
}

describe('Health', () => {
  it('renders ok and every enriched field from /api/health', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({ ok: true, data: healthPayload() })

    render(Health)

    const status = screen.getByTestId('health-status')
    await waitFor(() => expect(status).toHaveTextContent('Bridge admin API reachable.'))

    await waitFor(() => {
      expect(screen.getByTestId('health-stomp')).toHaveTextContent('connected')
      expect(screen.getByTestId('health-mtls')).toHaveTextContent('0.0.0.0:8443')
      expect(screen.getByTestId('health-server-sfdi')).toHaveTextContent('SFDI-SERVER-1')
      expect(screen.getByTestId('health-server-lfdi')).toHaveTextContent('LFDI-SERVER-1')
      expect(screen.getByTestId('health-feeder-mrid')).toHaveTextContent('feeder-mrid-1')
      expect(screen.getByTestId('health-simulation-id')).toHaveTextContent('sim-1')
      expect(screen.getByTestId('health-registry-count')).toHaveTextContent('3')
      expect(screen.getByTestId('health-placeholder-count')).toHaveTextContent('1')
      expect(screen.getByTestId('health-certificate-count')).toHaveTextContent('2')
      expect(screen.getByTestId('health-uptime')).toHaveTextContent('4242')
    })
  })

  it('renders stompConnected false as disconnected in the STOMP field, distinct from bridge reachability', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: healthPayload({ stompConnected: false }),
    })

    render(Health)

    await waitFor(() => expect(screen.getByTestId('health-stomp')).toHaveTextContent('disconnected'))
    // The overall bridge is still reachable (status: 'ok'); only the
    // STOMP link itself is down.
    expect(screen.getByTestId('health-status')).toHaveTextContent('Bridge admin API reachable.')
  })

  it('degrades to a disconnected state, not an error, when /api/health reports a non ok status', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: healthPayload({ status: 'degraded' }),
    })

    render(Health)

    const status = screen.getByTestId('health-status')
    await waitFor(() =>
      expect(status).toHaveTextContent(
        'Bridge disconnected (STOMP link down); will retry automatically.',
      ),
    )
    expect(status.querySelector('.ok')).toBeNull()
    expect(screen.queryByTestId('sor-link')).toBeNull()
  })

  it('degrades to a disconnected state, not an error, when the health fetch itself fails', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({ ok: false, error: 'network error', status: 0 })

    render(Health)

    const status = screen.getByTestId('health-status')
    await waitFor(() =>
      expect(status).toHaveTextContent(
        'Bridge disconnected (STOMP link down); will retry automatically.',
      ),
    )
  })

  it('renders the SOR link when sorLink is a valid https URL', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: healthPayload({ sorLink: 'https://sor.example.org/dashboard' }),
    })

    render(Health)

    const link = await screen.findByTestId('sor-link')
    expect(link).toHaveAttribute('href', 'https://sor.example.org/dashboard')
  })

  it('renders the SOR link when sorLink is a valid http URL', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: healthPayload({ sorLink: 'http://sor.internal/dashboard' }),
    })

    render(Health)

    const link = await screen.findByTestId('sor-link')
    expect(link).toHaveAttribute('href', 'http://sor.internal/dashboard')
  })

  it('hides the SOR link when sorLink is empty (unset)', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({ ok: true, data: healthPayload({ sorLink: '' }) })

    render(Health)

    await waitFor(() => expect(screen.getByTestId('health-status')).toHaveTextContent('reachable'))
    expect(screen.queryByTestId('sor-link')).toBeNull()
  })

  it('renders NO href when sorLink carries a javascript: scheme (fail closed)', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: healthPayload({ sorLink: 'javascript:alert(1)' }),
    })

    render(Health)

    await waitFor(() => expect(screen.getByTestId('health-status')).toHaveTextContent('reachable'))
    expect(screen.queryByTestId('sor-link')).toBeNull()
  })

  it('renders NO href when sorLink carries a data: scheme (fail closed)', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: healthPayload({ sorLink: 'data:text/html,<script>alert(1)</script>' }),
    })

    render(Health)

    await waitFor(() => expect(screen.getByTestId('health-status')).toHaveTextContent('reachable'))
    expect(screen.queryByTestId('sor-link')).toBeNull()
  })

  it('renders NO href when sorLink is not a parseable URL at all (fail closed)', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: healthPayload({ sorLink: 'not a url' }),
    })

    render(Health)

    await waitFor(() => expect(screen.getByTestId('health-status')).toHaveTextContent('reachable'))
    expect(screen.queryByTestId('sor-link')).toBeNull()
  })
})
