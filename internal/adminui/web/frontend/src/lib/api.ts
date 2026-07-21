// api.ts is the SPA's single client for the bridge's read only
// /api/* endpoints (internal/adminui/handlers.go). Every panel added
// under src/routes/ should go through fetchJSON rather than calling
// fetch directly, so the error shape and the base-path handling stay
// consistent as more endpoints are wired up.

export interface ApiError {
  error: string
}

export type ApiResult<T> = { ok: true; data: T } | { ok: false; error: string; status: number }

// fetchJSON issues a GET against path (e.g. "/api/health") relative to
// the current origin and decodes the JSON body. The admin UI's Bearer
// token and host allowlist middleware (internal/adminui/middleware.go)
// run server side on every request; this client never holds or sends
// a token itself; the browser session's own cookies/headers are not
// used by the admin UI at all, so no credential of any kind lives in
// this bundle.
export async function fetchJSON<T>(path: string): Promise<ApiResult<T>> {
  let res: Response
  try {
    res = await fetch(path, { method: 'GET', headers: { Accept: 'application/json' } })
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : 'network error', status: 0 }
  }

  if (!res.ok) {
    let message = `request failed with status ${res.status}`
    try {
      const body = (await res.json()) as ApiError
      if (body.error) message = body.error
    } catch {
      // Body was not JSON (or empty); fall back to the generic message
      // above rather than throwing.
    }
    return { ok: false, error: message, status: res.status }
  }

  const data = (await res.json()) as T
  return { ok: true, data }
}
