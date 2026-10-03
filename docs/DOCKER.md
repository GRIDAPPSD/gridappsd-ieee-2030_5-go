# Docker

## The bridge in a container

`make docker-up` builds the image and starts the bridge next to a running
GridAPPS-D platform; `make docker-down` stops it, `make docker-logs`
follows its log, and `make docker-build` builds the image alone.

One-time setup:

```bash
cp .env.example .env && chmod 600 .env
# set SEP2_ADMIN_UI_KEY (16+ characters, e.g. `openssl rand -hex 24`)
# and SEP2_STOMP_PASSWORD (the platform broker password)
make docker-up
```

What it does:

- **Image.** `Dockerfile.bridge` builds the binary with `CGO_ENABLED=0` in a
  `golang` stage (the bridge and its tests pass without cgo) and copies only
  the binary into a static distroless image that runs as a non-root user. The
  modules are public and fetched through the Go proxy, so no credential
  enters a layer. `.dockerignore` is an allowlist (only `go.mod`, `go.sum`, `cmd` and
  `internal` enter the context) and drops key, cert and env files at any depth.
- **Network.** The container joins the external network
  `gridappsd-docker_default` and reaches the broker at `gridappsd:61613`, so
  the platform must be up first.
- **Ports.** The 2030.5 listener (18443) and the admin UI (18444) are
  published on `127.0.0.1` only. Change the host side with
  `BRIDGE_SEP2_PORT` and `BRIDGE_ADMIN_PORT`.
- **Admin UI isolation.** The admin UI is plain HTTP behind one key, so it
  is not put on the platform network. The bridge joins a second network of
  its own (`BRIDGE_ADMIN_SUBNET`, default `10.213.168.0/24`, address
  `BRIDGE_ADMIN_IP`, default `10.213.168.2`) and the admin UI binds only
  that address; other platform containers cannot reach it. Change both if
  the subnet collides with a route on your host.
- **Config.** Settings come from `.env` (copy `.env.example`), never from the
  image. `docker-compose.bridge.yml` lists every setting the bridge reads,
  each as `NAME: ${NAME:-default}`, so that file is the one place to see them
  and `.env` is where you change them; [CONFIGURATION.md](CONFIGURATION.md)
  describes each. The admin key `SEP2_ADMIN_UI_KEY` and the broker password
  `SEP2_STOMP_PASSWORD` have no default and are required. Put comments on
  their own lines: a value followed by `# comment` is refused. The
  registration PIN has no environment variable, so it is not passed. `.env`
  is also read by the dev broker's `docker-compose.yml`, which uses different
  variable names. `BRIDGE_ENV_FILE` points the script at another file.
- **Docker Engine.** Docker Engine 28 or newer is required: the admin network
  uses `gw_priority`, which older engines do not support.
- **Certs.** The cert directory is bind-mounted at `/etc/sep2/certs` from
  `BRIDGE_CERT_DIR` (default
  `~/.config/gridappsd/2030.5server/sep2-certs`). The container always runs
  the default `dev-mint` mode, which may write new device certs, so the mount
  is `rw` (`BRIDGE_CERT_MODE`, default `rw`) and a compromised bridge could
  write that directory. The files are mode 0600 and owned by the host user,
  so the container runs as `BRIDGE_USER` (default `1000:1000`); set it to
  your uid and gid if they differ.
- **Hardening.** The root filesystem is read-only with a small `/tmp`
  tmpfs, all capabilities are dropped, and `no-new-privileges` is set. The
  bridge writes only to the cert directory.

`make docker-up` refuses to start, with a message naming the cause, when the
env file, the admin key, the cert directory or the platform network is
missing, or when a host port is already listening. The binary bridge started
by a local `start.sh` holds the same ports: stop it first. Only one bridge
may run against a broker at a time, because two would register on the same
feeder.

The minted server certificate names only `localhost` and `127.0.0.1`
([CERTIFICATES.md](CERTIFICATES.md)). Clients must connect through the
published loopback port; a client using the host's LAN name or another
container's DNS name fails certificate verification.

There is no published image and no `latest` tag; the image is built locally
as `gridappsd-ieee-2030_5-go:dev` (override with `BRIDGE_IMAGE`).

## A dev broker for tests

`docker-compose.yml` at the repo root brings up a bare ActiveMQ Classic
broker for local testing, nothing more:

```bash
docker compose up -d
```

This binds the STOMP port (`61613`) and the web console (`8161`) to
loopback only, using the upstream `system` / `manager` default
credentials. It is what `make test-integration` and `make bridge-e2e`
run against; see the root README's Testing section and
[cmd/bridge/README.md](../cmd/bridge/README.md). Do not expose this
broker beyond loopback: the credentials are dev-only and public.
