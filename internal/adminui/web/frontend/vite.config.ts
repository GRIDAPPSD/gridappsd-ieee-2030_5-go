import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// The build output lands in ../dist (internal/adminui/web/dist), the
// directory embed.go embeds into the bridge binary via
// "//go:embed all:dist". This keeps the frontend source tree
// (this directory) and the committed build artifact (../dist) as two
// clearly separate things: `dist` here would collide with this
// project's own default and make the embed target ambiguous.
//
// https://vite.dev/config/
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: '../dist',
    emptyOutDir: true,
  },
})
