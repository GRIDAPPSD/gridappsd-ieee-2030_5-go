<script lang="ts">
  // App is the SPA's shell: a fixed header plus nav plus the current
  // route's component, resolved from currentPath via the routes
  // table. It is deliberately thin: adding a panel means adding an
  // entry to src/routes/index.ts, not touching this file.
  import { currentPath, navigate } from './lib/router'
  import { routeList, routes, defaultRoute } from './routes'

  let Route = $derived(routes[$currentPath] ?? defaultRoute)

  function onNavClick(event: MouseEvent, path: string): void {
    event.preventDefault()
    navigate(path)
  }
</script>

<div id="shell">
  <header>
    <span class="title">gridappsd-ieee-2030_5-go</span>
    <span class="subtitle">admin</span>
  </header>
  <nav aria-label="Admin panels">
    {#each routeList as route (route.path)}
      <a
        href={route.path}
        class:active={$currentPath === route.path}
        onclick={(event) => onNavClick(event, route.path)}
      >
        {route.label}
      </a>
    {/each}
  </nav>
  <main>
    <Route />
  </main>
</div>

<style>
  #shell {
    min-height: 100vh;
    display: flex;
    flex-direction: column;
  }

  header {
    display: flex;
    align-items: baseline;
    gap: 8px;
    padding: 12px 20px;
    border-bottom: 1px solid var(--border);
  }

  .title {
    font-weight: 600;
    color: var(--text-h);
  }

  .subtitle {
    color: var(--text);
    font-size: 0.85em;
  }

  nav {
    display: flex;
    gap: 16px;
    padding: 8px 20px;
    border-bottom: 1px solid var(--border);
  }

  nav a {
    color: var(--text);
    text-decoration: none;
    font-size: 0.9em;
    padding-bottom: 2px;
  }

  nav a.active {
    color: var(--text-h);
    border-bottom: 2px solid var(--text-h);
  }

  main {
    flex: 1;
  }
</style>
