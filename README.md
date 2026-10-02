# gridappsd-ieee-2030_5-go

[![Build, vet, and test](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/ci.yml)
[![CodeQL](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/codeql.yml)
[![Go 1.26.3](https://img.shields.io/badge/go-1.26.3-00ADD8?logo=go)](https://go.dev)

Go bridge between the IEEE 2030.5 protocol and the GridAPPS-D platform.
It connects to the GridAPPS-D message bus over STOMP, discovers a
feeder's inverter, solar, and battery DERs from the platform's CIM
model, and runs an in-process IEEE 2030.5 mTLS server those devices
register and report against. Device identity (LFDI and SFDI) comes
from real client certificates per IEEE 2030.5 sections 6.3.3 and
6.3.4, access is enforced per device, DERControl flows to devices, and
telemetry flows back onto the GridAPPS-D bus. A read-only admin UI is
available for observing what the bridge is doing.

## Requirements

- Go 1.26.3.

## Quickstart

```bash
git clone <this repo>
cd gridappsd-ieee-2030_5-go
make build
make test
```

`make build` compiles every package, with the version stamped in from
`git describe`. `make test` runs the unit test suite; no broker is
required for it.

Running the bridge for real needs two more things: a GridAPPS-D
broker to talk to, and certificate material for its embedded IEEE
2030.5 server. Start here:

- **Certificates**: [docs/CERTIFICATES.md](docs/CERTIFICATES.md).
  Short version: point `SEP2_SERVER_CERT_DIR` at an empty directory
  outside any repository checkout, and the bridge mints everything it
  needs the first time it starts.
- **Configuration**: [docs/CONFIGURATION.md](docs/CONFIGURATION.md)
  documents every environment variable and flag the bridge reads.
- **Running it**: [cmd/bridge/README.md](cmd/bridge/README.md) covers
  a local run against a dev broker. [docs/DOCKER.md](docs/DOCKER.md)
  covers the container path.

## Layout

- `cmd/bridge/`: the bridge binary's entry point and configuration
  loader.
- `internal/cimstomp/`: STOMP publisher, configuration, and
  connection management.
- `internal/cim/`: the CIM model client and SPARQL queries used to
  discover DERs (`internal/cim/sim/` handles the simulation
  output subscription).
- `internal/gridappsdclient/`: the GridAPPS-D platform client
  (broker connect, auth-token bootstrap, simulation subscribe).
- `internal/registry/`: the in-memory mRID-to-LFDI device registry.
- `internal/sep2embed/`: the embedded IEEE 2030.5 mTLS server,
  including certificate load-or-mint (`certs.go`, `devicecert.go`)
  and DERControl handling (`control.go`).
- `internal/sep2acl/`: per-device access control enforcing device
  ownership.
- `internal/sep2config/`: the operator-facing IEEE 2030.5 policy
  layer (default DERControl, poll and post rates, registration PINs).
- `internal/controlobs/`, `internal/connobs/`: observation hooks the
  admin UI reads from.
- `internal/adminui/`: the admin listener: the server's admin plane with
  the bridge's views as extra tabs, plus two JSON routes.
- `internal/buildinfo/`: the link-time version stamp.

## Testing

Three layers, slowest last:

1. `make test` runs the unit tests. No broker required.
2. `make test-integration` brings up a bare ActiveMQ Classic broker
   via `docker-compose.yml` at the repo root, runs the
   `integration`-tagged tests in `internal/cimstomp/`, and tears the
   broker down. Fast; good for iterating on cimstomp internals.
3. `make test-gridappsd` runs the `gridappsd`-tagged tests in
   `internal/cimstomp/` against a real GridAPPS-D platform stack
   (broker plugins, auth-token responder, request routing). The
   platform must already be running: this target probes the STOMP
   port and fails fast with a hint if it is not reachable. Bring the
   platform up first from wherever you have the `gridappsd-docker`
   stack cloned, then run this target from this repo.

Other useful targets: `make vet`, `make fmt-check`, `make coverage`,
and `make test-race`. The admin UI is the server's; this repository ships
no frontend of its own.

## License

See LICENSE.
