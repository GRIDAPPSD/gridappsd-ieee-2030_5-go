// index.ts is the SPA's route table. Each entry maps an exact pathname
// to the Svelte component that renders it. Panels (GAGO-061..066) add
// their own entry here as they land; App.svelte only needs to import
// this table, never the individual route components directly.
//
// notFoundPath is served for any pathname with no exact match: since
// the server side SPA fallback (internal/adminui) already serves
// index.html for any unknown non-/api path, every client side path the
// SPA itself does not recognize should land here rather than a bare
// blank page.

import type { Component } from 'svelte'
import Home from './Home.svelte'

export const routes: Record<string, Component> = {
  '/': Home,
}

export const defaultRoute: Component = Home
