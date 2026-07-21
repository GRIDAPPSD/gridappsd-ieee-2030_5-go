/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// The build output lands in ../dist (internal/adminui/web/dist), the
// directory embed.go embeds into the bridge binary via
// "//go:embed all:dist". This keeps the frontend source tree
// (this directory) and the committed build artifact (../dist) as two
// clearly separate things: `dist` here would collide with this
// project's own default and make the embed target ambiguous.
//
// The test block below runs vitest in jsdom (component tests exercise
// rendered DOM output, not just non-throw); it shares this config file
// rather than a separate vitest.config.ts so plugin setup (the svelte
// compiler) is not duplicated between build and test.
//
// https://vite.dev/config/
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: '../dist',
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test-setup.ts'],
    globals: false,
  },
  resolve: process.env.VITEST
    ? {
        // Vitest runs in Node, where Vite's default export-condition
        // resolution picks Svelte's server (SSR) build, which has no
        // mount()/onMount lifecycle. The browser condition forces the
        // client build so component tests exercise the same runtime
        // the bridge actually serves.
        conditions: ['browser'],
      }
    : undefined,
})
