# Docker

## The bridge in a container

`make docker-up` pulls the published image and starts the bridge next to a
running GridAPPS-D platform (`BRIDGE_USE_PUBLISHED=0` builds it locally instead); `make docker-down` stops it, `make docker-logs`
follows its log, and `make docker-build` builds the image alone.

One-time setup:

```bash
cp .env.example .env && chmod 600 .env
# set SEP2_ADMIN_UI_KEY (16+ characters, e.g. `openssl rand -hex 24`)
# and GRIDAPPSD_PASSWORD (the platform broker password; SEP2_STOMP_PASSWORD also works)
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
- **Ports.** The 2030.5 listener (18443) is published on `0.0.0.0` by
  default and the admin UI (18444) on `127.0.0.1` only. Change the host ports with
  `BRIDGE_SEP2_PORT` and `BRIDGE_ADMIN_PORT`, and the address each is
  published on with `BRIDGE_SEP2_BIND_IP` and `BRIDGE_ADMIN_BIND_IP` (IPv4;
  see "Reaching the bridge from another machine").
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
  describes each. The admin key `SEP2_ADMIN_UI_KEY` has no default and is required. The broker
  login is read as `GRIDAPPSD_USER` and `GRIDAPPSD_PASSWORD`, the names other
  GridAPPS-D apps use; `SEP2_STOMP_USER` and `SEP2_STOMP_PASSWORD` override
  them. The password is required under one of the two names, and `make
  docker-up` refuses to start without it. Put comments on
  their own lines: the script refuses a value followed by `# comment` for the
  names it reads itself (`SEP2_ADMIN_UI_KEY`, the two password names and the
  `BRIDGE_*` launcher settings); compose strips an inline comment from any
  other value, so it is dropped silently there. The
  registration PIN is optional: `SEP2_REGISTRATION_PIN` is a secret with no
  default, and `SEP2_REGISTRATION_PIN_FILE` must name a path inside a mount
  (such as the certificate directory); leave both empty for none. `.env`
  is also read by the dev broker's `docker-compose.yml`, which uses different
  variable names. `BRIDGE_ENV_FILE` points the script at another file. Upgrading: the file was
  `.env.bridge`; rename it to `.env`. Four settings are pinned in the compose
  file and cannot be overridden: `SEP2_SERVER_ADDR`, `SEP2_SERVER_CERT_DIR`,
  `SEP2_ADMIN_UI_ADDR` and `SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK`.
- **Docker Engine.** Docker Engine 28 or newer is required: the admin network
  uses `gw_priority`, which older engines do not support.
- **Certs.** The cert directory is bind-mounted at `/etc/sep2/certs` from
  `BRIDGE_CERT_DIR` (default
  `~/.config/gridappsd/2030.5server/sep2-certs`). With the default `dev-mint`
  mode (`SEP2_DEVICE_CERT_MODE`) the bridge may write new device certs, so the
  mount is `rw` (`BRIDGE_CERT_MODE`, default `rw`) and a compromised bridge
  could write that directory. With `SEP2_DEVICE_CERT_MODE=preprovisioned` it
  writes nothing, and `BRIDGE_CERT_MODE=ro` is the better mount; the launcher
  refuses `ro` with `dev-mint` and warns on `preprovisioned` with `rw`. The files are mode 0600 and owned by the host user,
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

## Reaching the bridge from another machine

The 2030.5 port is published on `0.0.0.0` by default, so clients on the
network can reach it. Docker-published ports bypass the host firewall's
INPUT rules (ufw, firewalld), so a firewall rule on the host does not close
this port; mTLS is what protects it. To restrict it, set
`BRIDGE_SEP2_BIND_IP=127.0.0.1` (host only) or a LAN address, or add rules
to the `DOCKER-USER` iptables chain. To serve clients on the LAN:

1. Leave `BRIDGE_SEP2_BIND_IP` at its default, or set it to the host address
   to publish the 2030.5 port on (for example `192.168.1.50`).
2. Make the server certificate name the address or host name clients dial.
   The leaf dev-mint creates names only `localhost` and `127.0.0.1`, so a
   client using anything else fails verification. Either set
   `SEP2_SERVER_CERT_HOSTS=192.168.1.50,bridge.lan` before the first start, or
   mount a certificate set that names them (see
   [CERTIFICATES.md](CERTIFICATES.md), "A certificate set made on another
   computer"). Give the clients `serving-ca.pem`.
3. An existing certificate directory keeps its old `server.pem`:
   `SEP2_SERVER_CERT_HOSTS` is read only when the directory is empty, and the
   bridge logs a warning at start when the leaf lacks a requested name. To
   re-mint, stop the bridge and move the directory aside (that also replaces
   both CAs, so every client must be given the new `serving-ca.pem` and
   every device certificate is minted again).
4. No host firewall change is needed to publish the port; see the note
   above on restricting it.

What the default bind exposes: the 2030.5 port is mTLS, so a peer needs a
certificate signed by the device CA. The admin UI port is plain HTTP behind
one key, and its `Host` allowlist is not a defence (a client chooses its own
`Host` header), so leave `BRIDGE_ADMIN_BIND_IP` on its `127.0.0.1` default. If you
publish it anyway, add the name or address you browse to in
`SEP2_ADMIN_UI_ALLOWED_HOSTS`, or the UI answers 403 for it. The in-container
admin bind (`BRIDGE_ADMIN_IP` on its own network) is unchanged by any of
these settings.

## The published image

The image is public on Docker Hub, under the `gridappsd` organisation as
`gridappsd/gridappsd-ieee-2030_5`. Its visibility is set on Docker Hub, not in
this repository. It is linux/amd64 and every image carries the
`org.opencontainers.image.version`, `.revision` (the commit it was built from)
and `.source` labels.

* Pushing a release tag `v*` publishes `:<tag>`, and `:latest` as well when the
  tag has no suffix and is the highest such version among the repository's
  tags. A pre-release such as `v0.3.0-rc1`, or a patch on an older line, does
  not move `latest`. A tag whose commit is not on `main` is refused before
  login.
* A push to `main` publishes `:main`. A branch push never moves `latest` or a
  version tag.

`make docker-up` runs the published image by default: it pulls
`gridappsd/gridappsd-ieee-2030_5:${BRIDGE_IMAGE_TAG:-latest}` first, so the
run uses the current image, then starts it with no build. If the pull fails it
stops and says to set `BRIDGE_USE_PUBLISHED=0`. A push to `main` publishes only
`:main` and `latest` moves only on a stable `v*` release, so until the first
stable release the default path fails, so build locally with
`make docker-up BRIDGE_USE_PUBLISHED=0`: that builds and tags
`gridappsd-ieee-2030_5-go:dev` and runs it. A local build is always tagged
`gridappsd-ieee-2030_5-go:dev`, never the Hub name, whatever `BRIDGE_IMAGE` or
`BRIDGE_IMAGE_TAG` say, so it cannot shadow the published image. With the
default switch an explicit `BRIDGE_IMAGE` is the image pulled and run; with
`BRIDGE_USE_PUBLISHED=0` it is ignored. `make docker-pull` pulls the published
image alone, and `make docker-build` always builds the local `:dev` image. The compose file has
no `build:`, so compose run by hand only pulls the published image and fails if
it is missing; it never builds one under the published name. To run the published image, call compose directly
with the launcher's exports in place and `BRIDGE_IMAGE` unset: compose then uses
`gridappsd/gridappsd-ieee-2030_5:${BRIDGE_IMAGE_TAG:-latest}`. Pin a version
with `BRIDGE_IMAGE_TAG=v0.1.0`, in the shell or in `.env`:

```
docker pull gridappsd/gridappsd-ieee-2030_5:v0.1.0
docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' \
  gridappsd/gridappsd-ieee-2030_5:v0.1.0
```

## Behind a TLS-inspecting proxy

If `make docker-up` with `BRIDGE_USE_PUBLISHED=0` fails in the image build with `x509: certificate signed by unknown authority`, your network re-signs HTTPS with its own CA. Two things need that CA:

* Docker pulls (the base images): install the CA on the host and restart the Docker daemon.
* The build's Go downloads: set `BRIDGE_EXTRA_CA_FILE` in `.env` to the host path of a PEM bundle holding the CA.

The launcher refuses a path that is missing, unreadable or has no PEM certificate. The file reaches the build as a BuildKit secret for the download step only, so it is in no image layer. Unset, the build is unchanged. The published image needs none of this.

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
