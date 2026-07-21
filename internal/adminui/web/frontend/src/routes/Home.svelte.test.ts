// Home.svelte.test.ts exercises the two branches of Home's onMount
// health check (src/routes/Home.svelte:18-27): the fetch-success path
// that renders the resolved status, and the fetch-error path that
// renders the returned error message. Both tests mock at the
// fetchJSON boundary (src/lib/api.ts): fetchJSON is this component's
// only client of the server, and per the workspace integrations
// testing guidance mocking the real fetch/XHR layer would only prove
// the mock is consistent with itself, not that the component reacts
// correctly to what fetchJSON resolves.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import Home from './Home.svelte'
import * as api from '../lib/api'

describe('Home', () => {
  it('renders the ok status once fetchJSON resolves with status: ok', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: true,
      data: { status: 'ok' },
    })

    render(Home)

    const health = screen.getByTestId('health-status')
    await waitFor(() => expect(health).toHaveTextContent('Bridge admin API reachable.'))
    expect(health.querySelector('.error')).toBeNull()
  })

  it('renders the returned error message once fetchJSON resolves with ok: false', async () => {
    vi.spyOn(api, 'fetchJSON').mockResolvedValue({
      ok: false,
      error: 'request failed with status 503',
      status: 503,
    })

    render(Home)

    const health = screen.getByTestId('health-status')
    await waitFor(() =>
      expect(health).toHaveTextContent(
        'Bridge admin API unreachable: request failed with status 503',
      ),
    )
    expect(health.querySelector('.ok')).toBeNull()
  })
})
