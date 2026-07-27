// index.ts is the SPA's route table. Each entry maps an exact pathname
// to the Svelte component that renders it, plus a human label for the
// nav (src/lib/router.ts's Route interface). App.svelte only needs to
// import routeList/routes/defaultRoute from this table, never the
// individual route components directly.
//
// notFoundPath is served for any pathname with no exact match: since
// the server side SPA fallback (internal/adminui) already serves
// index.html for any unknown non-/api path, every client side path the
// SPA itself does not recognize should land here rather than a bare
// blank page. Health ("/") is the default/landing route per GAGO-061.

import type { Component } from 'svelte'
import type { Route } from '../lib/router'
import Health from './Health.svelte'
import Registry from './Registry.svelte'
import DiscoveredDers from './DiscoveredDers.svelte'
import ServedResources from './ServedResources.svelte'
import ControlFlow from './ControlFlow.svelte'
import ConnectedClients from './ConnectedClients.svelte'

// routeList is the ordered nav: App.svelte renders one link per entry,
// in this order, with Health first as the landing panel.
export const routeList: Route[] = [
  { path: '/', label: 'Health' },
  { path: '/registry', label: 'Registry' },
  { path: '/ders', label: 'Discovered DERs' },
  { path: '/served', label: 'Served resources' },
  { path: '/clients', label: 'Connected clients' },
  { path: '/controlflow', label: 'Control flow' },
]

export const routes: Record<string, Component> = {
  '/': Health,
  '/registry': Registry,
  '/ders': DiscoveredDers,
  '/served': ServedResources,
  '/clients': ConnectedClients,
  '/controlflow': ControlFlow,
}

export const defaultRoute: Component = Health
