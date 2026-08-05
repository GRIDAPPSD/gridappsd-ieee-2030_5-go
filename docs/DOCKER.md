# Docker

## Two different compose files, two different jobs

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

`docker-compose.bridge.yml`, also at the repo root, is a deployment
fragment for the bridge image itself, meant to be composed alongside a
GridAPPS-D platform stack such as `gridappsd-docker`, not alongside
this repository's own dev broker. It is not brought up by a bare
`docker compose up` in this checkout; read it directly, or reference it
with an explicit `-f` flag against the platform compose stack you are
deploying to.

## The bridge image

Published as `gridappsd/gridappsd-ieee-2030_5`, tagged with the release
version (for example `v0.2.0`). No `latest` tag, and no tag is pushed
for a branch or a `main` build: a compose stack pinned to a specific
tag does not change under an operator without a deliberate version
bump. See `.github/workflows/release.yml` for exactly when a tag is
pushed and what it authenticates with.

Build it yourself with:

```bash
make image
```

which compiles `cmd/bridge` for `linux/amd64` with `CGO_ENABLED=0` and
runs `docker build` against the result. The image build itself never
receives a credential of any kind: the `Dockerfile` `COPY`s in a
finished binary and this repository's `LICENSE`, nothing else, and
`.dockerignore` denies everything else in the repository by default.
This matters because the two private Go modules this repository
depends on need a credential to resolve, and that credential must
never become an input to the image build; only `go build`, run before
`docker build` starts, ever sees it.

Design:

- **Single-stage, distroless `static-debian12:nonroot`, digest-pinned.**
  Runs as uid:gid `65532:65532`, confirmed by inspecting the built
  image (`docker image inspect ... --format '{{.Config.User}}'`), not
  assumed from documentation.
- **No shell, no package manager.** Also means no `HEALTHCHECK`: a
  `CMD-SHELL` or `CMD` probe needs something to exec, and distroless
  has nothing. A healthcheck that cannot run is worse than none.
- **The 2030.5 listener is meant to be exposed**; it is mTLS with a
  per-device access check, so publishing it on the compose network is
  the intended use. **The admin UI is not.** It defaults to loopback
  and disabled, and that default should not be widened casually inside
  a container: see [CONFIGURATION.md](CONFIGURATION.md) before
  changing it. The image sets no `ENV` for either bind address, on
  purpose: that decision belongs in the compose fragment an operator
  reads and edits, not in the least visible layer of the system.
- **Certificate material is always a runtime mount, never baked in.**
  `docker-compose.bridge.yml` mounts `SEP2_SERVER_CERT_DIR`
  READ-WRITE from an operator-supplied path outside any repository or
  workspace, matching [CERTIFICATES.md](CERTIFICATES.md). Writable is
  deliberate: dev-mint mode needs to persist what it generates across
  restarts, or every restart mints a new CA and invalidates every
  device's identity. Read CERTIFICATES.md for what a writable mount
  means for a preprovisioned deployment before pointing one at real
  certificate material.

An illustrative excerpt (see `docker-compose.bridge.yml` for the full,
commented fragment):

```yaml
services:
  ieee-2030_5-bridge:
    image: gridappsd/gridappsd-ieee-2030_5:${SEP2_IMAGE_TAG:?set SEP2_IMAGE_TAG}
    restart: unless-stopped
    ports:
      - "8443:8443"   # 2030.5 mTLS listener, authenticated, safe to expose
    environment:
      SEP2_STOMP_ADDR: gridappsd:61613
      SEP2_SERVER_ADDR: "0.0.0.0:8443"
      SEP2_SERVER_CERT_DIR: /var/lib/sep2/certs
      SEP2_DEVICE_CERT_MODE: dev-mint
      # Admin UI stays off by default. Read CONFIGURATION.md before enabling it.
    volumes:
      - "${SEP2_CERT_DIR:?set SEP2_CERT_DIR}:/var/lib/sep2/certs"
```

`SEP2_CERT_DIR` above is a host path the operator sets, outside any
repository or workspace checkout; the fragment intentionally has no
default for it, so a missing value fails the `docker compose`
invocation rather than silently creating a directory somewhere
convenient and wrong.

## Publishing

Publishing is wired into `.github/workflows/release.yml` as the
`image` job: it runs only when the `build-and-verify` job's canary
smoke test has already passed, only on a real `v*` tag push or an
explicit non-dry-run dispatch, and only for the `GRIDAPPSD`-owned
repository. It authenticates to Docker Hub with the same mechanism
`GOSS-GridAPPS-D`'s own publish workflow uses (`DOCKER_USERNAME` /
`DOCKER_TOKEN`, piped into a plain `docker login`), and it never sees
the private-module App credential the build job uses: those are two
different secrets, kept in two different jobs, on purpose.

Before any image job runs against a real tag, `build-and-verify` also
runs an IEEE-material redistribution check: it fails the whole
pipeline if any `.xsd`, `.wadl`, or `.xml` file shows up anywhere in
this repository or in the two GRIDAPPSD dependency modules, because
IEEE 2030.5's schema and WADL are copyrighted with no redistribution
right granted. A public image is redistributed forever with no recall,
so this check runs on every release-pipeline invocation rather than
having been verified once and trusted from then on.
