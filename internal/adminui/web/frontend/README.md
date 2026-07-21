# Admin UI frontend

Svelte plus TypeScript, built with Vite. This is the source for the bridge's
read only admin UI SPA, served by `internal/adminui` at `GET /` behind the
existing Bearer-auth and host-allowlist middleware.

## Build

From the repo root:

```
make ui-build
```

This runs `npm ci && npm run build` here, writing the static output to
`../dist` (`internal/adminui/web/dist/`), then rebuilds the Go binary so the
freshly built assets are embedded via `internal/adminui/web/embed.go`.

Node and npm are build-time only. The shipped `bridge` binary embeds the
built assets in `../dist`; nothing under this directory or its
`node_modules` is present in the binary or required at runtime.

## Develop

```
npm install
npm run dev
```

`npm run dev` starts Vite's dev server with hot module reload against the
static markup only: it does not proxy to a running bridge, since the admin
UI's `/api/*` endpoints require a Bearer token and the loopback bind that
`internal/adminui` enforces server side. Point `fetch` calls in
`src/lib/api.ts` at a running bridge's admin UI port for end to end manual
testing, or use `npm run build` plus `make ui-build` to exercise the real
embedded path.

## Type check

```
npm run check
```
