# gridappsd-ieee-2030_5-go

[![Build, vet, and test](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/ci.yml)
[![CodeQL](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/codeql.yml)
[![Go 1.26.3](https://img.shields.io/badge/go-1.26.3-00ADD8?logo=go)](https://go.dev)

This repo is private: the workflow badges above render for viewers with
repository access and show nothing for anonymous visitors. No release
badge yet; this repo has not cut a tagged release.

Go bridge that wires the IEEE 2030.5 protocol surface to the GridAPPS-D
platform via STOMP/ActiveMQ messaging.

## Status

v0.0.0 scaffold. The bridge is not yet wired. internal/cimstomp/ carries
a STOMP publisher ported from gotocim. internal/cim/ and internal/registry/
are placeholders.

## Repository access and build requirements

This repository is private under the GRIDAPPSD GitHub org. A reader needs
`GRIDAPPSD` org access (or an explicit collaborator grant) to clone it at
all.

Today's dependency graph is mixed: `github.com/go-stomp/stomp/v3` is
public, but the bridge also `require`s the private
`github.com/GRIDAPPSD/gridappsd-go` module (the standard GridAPPS-D Go
client). `go build ./...` therefore needs `GOPRIVATE=github.com/GRIDAPPSD/*`
set and `GRIDAPPSD` org read access to `gridappsd-go` in addition to this
repo; a reader who can clone this repo but lacks access to `gridappsd-go`
will fail at `go mod download`, not at clone time.

That access requirement will widen with the planned IEEE 2030.5 server
embedding (see `cmd/bridge/README.md`, "Out of scope"). Once that work
lands, the bridge will additionally `require` the private
`github.com/GRIDAPPSD/ieee-2030_5-core-go` module, and a builder will need
read access to that module too (a fine-grained GitHub PAT scoped to it,
per the family migration plan). This section will be updated with the
exact access steps when that dependency lands.

## Layout

- cmd/bridge/: bridge binary entry point.
- internal/cimstomp/: STOMP publisher, configuration, and connection management.
- internal/cim/: CIM model client (placeholder).
- internal/registry/: mRID-to-LFDI registry (placeholder).

## Build

    go build ./...
    go vet ./...
    go test ./...

## Tests

Three layers, slowest last:

1. `make test` runs the unit tests. No broker required.
2. `make test-integration` brings up a bare ActiveMQ Classic 6.1.6 via
   `docker-compose.yml` at the repo root, runs the `integration`-tagged
   tests in `internal/cimstomp/`, and tears the broker down. Credentials
   are `system / manager`, matching the GridAPPS-D platform stack so the
   bare-broker and platform test layers no longer diverge on creds. Fast;
   intended for iterating on cimstomp internals.
3. `make test-gridappsd` runs the `gridappsd`-tagged tests in
   `internal/cimstomp/` against the real GridAPPS-D platform stack
   (broker plugins, auth-token responder, request routing). Credentials
   are `system / manager`. The platform must be running before the target
   is invoked; the target probes port 61613 and fails fast with a hint if
   it is not reachable.

   To bring the platform up (one-time per session):

       cd ~/repos/sentient_gridappsd_integration
       pixi run gridappsd-start

   Then from this repo:

       make test-gridappsd

   The platform stack is heavy (30 to 60 seconds to boot, multiple
   containers). Use the bare-ActiveMQ `test-integration` target for fast
   iteration; reach for `test-gridappsd` when verifying that wire-format
   changes still ride on the production-equivalent broker.

   The gridappsd-docker stack should only be brought up bound to loopback
   (`127.0.0.1`). Do not run it on a publicly reachable host without
   tightening broker authentication first; the dev-default `system / manager`
   credentials baked into the compose are not production-safe.

## License

See LICENSE.
