# gridappsd-ieee-2030_5-go

[![Build, vet, and test](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/actions/workflows/ci.yml)
[![Go 1.26.8](https://img.shields.io/badge/go-1.26.8-00ADD8?logo=go)](https://go.dev)

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

- Go 1.26.8 or newer (the `go` line of `go.mod`); targets that compile check it first and stop with a message on an older Go.

## Quickstart

```bash
git clone <this repo>
cd gridappsd-ieee-2030_5-go
make build
make test
```

`make build` compiles every package and writes `./bridge` (override the
path with `BRIDGE=`), with its version stamped in from `git describe`.
`make test` runs the unit test suite; no broker is required for it.

`make run` builds, then starts the bridge in the foreground. It needs an
admin key of at least 16 characters, from `SEP2_ADMIN_UI_KEY` or from a
file named by `ADMIN_UI_KEY_FILE`, and refuses to start without one:

```bash
export SEP2_ADMIN_UI_KEY=...   # or use ADMIN_UI_KEY_FILE below
make run
make run SEP2_SERVER_ADDR=127.0.0.1:9443 ADMIN_UI_ADDR=127.0.0.1:9444 \
  SEP2_SERVER_CERT_DIR=$HOME/bridge-certs FEEDER_MRID=_YOUR-FEEDER-MRID \
  REGISTRATION_PIN=123455 STOMP_ALLOW_PLAINTEXT=false \
  ADMIN_UI_KEY_FILE=$HOME/bridge-admin.key
```

The defaults are `127.0.0.1:18443` and `127.0.0.1:18444`, `./sep2-certs`,
plaintext STOMP on (local dev brokers), the binary's own feeder, and no
fleet-wide PIN. `SEP2_SIMULATION_ID` passes through when set. A bare
`make` prints `make help`, the list of targets.

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

## Run with Docker Compose

To run the bridge as a container on the GridAPPS-D platform network:

1. **Prerequisites**: GridAPPS-D platform running from
   [gridappsd-docker](https://github.com/GRIDAPPSD/gridappsd-docker), and Docker
   Engine 28 or newer.
2. **Environment**: Copy `.env.example` to `.env`, then set:
   - `SEP2_ADMIN_UI_KEY`: 16 or more characters (login password)
   - `GRIDAPPSD_PASSWORD`: the platform broker password (`SEP2_STOMP_PASSWORD` also works); `GRIDAPPSD_USER` is optional
   - `SEP2_REGISTRATION_PIN`: optional; the registration PIN your clients expect
3. **Start**: `make docker-up` pulls the published image and starts the container; `make docker-up BRIDGE_USE_PUBLISHED=0` builds the image locally instead.
4. **Monitor**: `make docker-logs` follows the container log.
5. **Stop**: `make docker-down` stops and removes the container.

The admin UI listens at `http://127.0.0.1:18444/ui` and shows the bridge's
status, DERs, and telemetry. Its Bus monitor tab watches any `/topic/` on the
broker live, each topic on its own connection under the bridge's credential.
Its Bus sender tab publishes DER controls to the bridge's application input
topic, behind the switch on the Bus publishing tab, which is off after
every start.

Only one bridge may run against a broker at a time. If the binary bridge is
running, stop it before bringing up the container.

For complete configuration details, see [docs/DOCKER.md](docs/DOCKER.md) and
[docs/CONFIGURATION.md](docs/CONFIGURATION.md).

## Configuration

The settings you will touch first. A flag beats its env var, which beats
the default. `make run` takes the make variables shown; it does not pass
the STOMP login or `SEP2_SIMULATION_ID`, which it inherits from your
environment.

| Setting | Flag | Env var | make run variable | Default | What it controls |
|---|---|---|---|---|---|
| Admin UI key (login) | `-admin-ui-key` | `SEP2_ADMIN_UI_KEY` | `ADMIN_UI_KEY_FILE` (path to a file holding the key) | none: no key, no admin UI (`make run` refuses to start) | The admin UI login password and the API Bearer token. At least 16 characters. Running with no key is `make docker-up` only: see [the admin UI section](docs/CONFIGURATION.md#admin-ui). |
| STOMP user | `-stomp-user` | `SEP2_STOMP_USER` | none | built-in default | Login to the GridAPPS-D broker. |
| STOMP password | `-stomp-password` | `SEP2_STOMP_PASSWORD` | none | built-in default | Login to the GridAPPS-D broker. |
| 2030.5 address | `-sep2-server-addr` | `SEP2_SERVER_ADDR` | `SEP2_SERVER_ADDR` | `127.0.0.1:8443` (`make run`: `127.0.0.1:18443`) | Where the embedded IEEE 2030.5 mTLS server listens. |
| Admin UI address | `-admin-ui-addr` | `SEP2_ADMIN_UI_ADDR` | `ADMIN_UI_ADDR` | `127.0.0.1:8444` (`make run`: `127.0.0.1:18444`) | Where the admin UI listens. |
| Cert dir | `-sep2-server-cert-dir` | `SEP2_SERVER_CERT_DIR` | `SEP2_SERVER_CERT_DIR` | `./sep2-certs` | Where the server's CA and leaf certs are read or minted. |
| Feeder mRID | `-feeder-mrid` | `SEP2_FEEDER_MRID` | `FEEDER_MRID` | the binary's built-in feeder (`make run`: empty, so the same) | The CIM feeder whose DERs are discovered. |
| Registration PIN | `-sep2-registration-pin` | none | `REGISTRATION_PIN` | unset: no fleet-wide PIN | Fleet-wide fallback IEEE 2030.5 registration PIN. |
| Plaintext STOMP | `-stomp-allow-plaintext` | `SEP2_STOMP_ALLOW_PLAINTEXT` | `STOMP_ALLOW_PLAINTEXT` | `false` (`make run`: `true`) | Dial the broker over plain TCP instead of TLS. Dev only. |
| Simulation ID | `-simulation-id` | `SEP2_SIMULATION_ID` | none | empty | The GridAPPS-D simulation to subscribe to. Empty skips only the measurement subscribe. |
| Application ID | `-application-id` | `SEP2_APPLICATION_ID` | none | `IEEE_2030_5` | The GridAPPS-D application id the status and control topics are built from. |
| Edition | none | `SEP2_EDITION` | none | `2018` | IEEE 2030.5 edition. Only 2018 is supported; 2023 stops the bridge at startup. |

**To change the login:** set the env var (or pass the flag), then restart
the bridge. For the admin key under `make run`, put it in a file and
point `ADMIN_UI_KEY_FILE` at it. The key never goes in the Makefile.

Everything else is in [docs/CONFIGURATION.md](docs/CONFIGURATION.md).

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
   `internal/cimstomp/`, `internal/busmonitor/` and `internal/adminui/`
   against a real GridAPPS-D platform stack
   (broker plugins, auth-token responder, request routing). The
   platform must already be running: this target probes the STOMP
   port and fails fast with a hint if it is not reachable. Bring the
   platform up first from wherever you have the `gridappsd-docker`
   stack cloned, then run this target from this repo.

Other useful targets: `make vet`, `make fmt-check`, `make coverage`,
and `make test-race`. The admin UI is the server's; this repository ships
no frontend of its own.

## License

See LICENSE and NOTICE.
