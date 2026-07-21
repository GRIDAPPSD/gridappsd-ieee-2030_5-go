<script lang="ts">
  // App is the SPA's shell: a fixed header plus the current route's
  // component, resolved from currentPath via the routes table. It is
  // deliberately thin: adding a panel means adding an entry to
  // src/routes/index.ts, not touching this file.
  import { currentPath } from './lib/router'
  import { routes, defaultRoute } from './routes'

  let Route = $derived(routes[$currentPath] ?? defaultRoute)
</script>

<div id="shell">
  <header>
    <span class="title">gridappsd-ieee-2030_5-go</span>
    <span class="subtitle">admin</span>
  </header>
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

  main {
    flex: 1;
  }
</style>
