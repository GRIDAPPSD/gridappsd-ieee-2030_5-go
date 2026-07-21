// router.ts is a minimal client side router for the admin UI SPA. It
// exists so future panels (GAGO-061..066) can register a route without
// pulling in a routing library: the admin UI is a small, single
// operator surface, not a general purpose web app, so the History API
// plus a Svelte store is enough surface.
//
// This router only ever needs to distinguish between the SPA's own
// client side routes; it never issues a request to /api itself. The
// server side SPA fallback (internal/adminui: the handler that serves
// index.html for any unknown non-/api path) is what makes a hard
// reload on a client side route like "/registry" still work.

import { readable } from 'svelte/store'

// currentPath is the current window.location.pathname, updated on
// popstate (back/forward) and on every navigate() call below.
export const currentPath = readable(window.location.pathname, (set) => {
  const onPopState = () => set(window.location.pathname)
  window.addEventListener('popstate', onPopState)
  return () => window.removeEventListener('popstate', onPopState)
})

// navigate pushes a new history entry for path and notifies
// currentPath's subscribers by dispatching a synthetic popstate. This
// keeps the store's single source of truth as window.location rather
// than a second, potentially-divergent piece of state.
export function navigate(path: string): void {
  if (path === window.location.pathname) return
  window.history.pushState({}, '', path)
  window.dispatchEvent(new PopStateEvent('popstate'))
}

// Route describes one client side route: the exact pathname it
// matches and a human label for the nav link. Panels register their
// own Route entries in src/routes/index.ts as they land; this file
// stays generic.
export interface Route {
  path: string
  label: string
}
