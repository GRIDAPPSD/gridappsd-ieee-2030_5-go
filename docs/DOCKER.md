# Docker

## Three compose files, three different jobs

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

The other two, `docker-compose-gridappsd-ieee-2030_5.yml` and
`docker-compose.bridge.yml`, both deploy the published bridge image.
Neither is brought up by a bare `docker compose up` in this checkout;
each is for a different deployment shape, described below. Getting
this choice wrong produces a bridge that cannot see the broker and no
obvious reason why, so read this section before picking one.

## Which one: platform-integrated, or standalone

**`docker-compose-gridappsd-ieee-2030_5.yml` (recommended for a
GridAPPS-D deployment).** Drop it, unmodified, into a `gridappsd-docker`
clone's `docker-compose.d/` directory and it runs inside that platform's
own compose project: same Docker network as `gridappsd`, `blazegraph`,
`mysql`, and every other platform service, reached and reaching them by
service name.

**`docker-compose.bridge.yml` (standalone).** Use this only when the
bridge is not being composed together with a `gridappsd-docker`
checkout at all: a dedicated host, a different orchestrator, anything
where the bridge is its own compose project. It talks to the platform
broker at whatever host-published address you tell it, because it has
no shared network to use instead.

### How the platform-integrated path actually works, verified against gridappsd-docker

Read directly from `GRIDAPPSD/gridappsd-docker` (`main` at `4b526bb`),
not inferred from file naming:

> "The `docker-compose.d/` directory allows you to extend the base
> docker-compose configuration with additional services. Any `.yml`
> file in this directory is automatically included when running
> `./run.sh`... 1. The `run.sh` script scans `docker-compose.d/` for
> files ending in `.yml` 2. Each `.yml` file is added to the
> docker-compose command with `-f`"
> (`README.md`, "docker-compose.d Directory" section)

Confirmed against `utils.sh`'s `get_compose_files()`, which the
README's prose describes: `ls -1 docker-compose.d/*yml`, so a file
ending in `.yml` is picked up automatically and a file ending in
`.yml.dist` (every shipped optional-service template in that directory)
is not, until an operator copies it to a name ending in `.yml`. Our
file already ends in `.yml`, not `.yml.dist`, so it needs no
copy-and-rename step the way `docker-compose_pyvvo.yml.dist` and its
siblings do: once it is in `docker-compose.d/`, the next `./run.sh`
includes it.

Because it is merged into the same `docker compose` invocation as
`gridappsd-docker`'s own `docker-compose.yml`, our service joins the
same network every platform service is on. Neither file in
`gridappsd-docker` declares an explicit `networks:` key anywhere
(checked directly: none exists in the repository), so this is Compose's
own implicit default per-project network, the same mechanism the
shipped `docker-compose_der-dispatch.yml.dist` fragment already relies
on for its own `GRIDAPPSD_URI: tcp://gridappsd:61613`.

**Service name and port, inside that network: `gridappsd:61613`.**
Not a host-published address: `docker-compose.yml`'s `61613:61613`
mapping is for reaching the broker from outside the compose project,
which does not apply to a service inside it. `61613` is the platform's
plain STOMP port (not `61614`, STOMP+SSL); `docker-compose_der-dispatch.yml.dist`
and the README's own "Creating Custom Services" example both connect
the same way, to the same address.

### The exposure decision

The platform-integrated fragment still publishes `8443:8443` to the
host by default. Reasoned explicitly, not left as a leftover from the
standalone fragment: the bridge's DER clients (real devices, the EPRI
reference client, or any other IEEE 2030.5 client) are not, today,
containers on the platform's compose network, so a same-network client
is the case this default does NOT need to serve, and a host-reachable
client is the case it does. `docker-compose_der-dispatch.yml.dist`
follows the same pattern for its own consumer, publishing `9001:9001`.
If a DER client is itself a container on the same compose project, it
already reaches the bridge at `ieee-2030_5-bridge:8443` with no
published port at all; the fragment's comments say so and show how to
drop the `ports:` line for that case, as the explicit opt-out rather
than a default an operator has to notice and remove.

The admin UI stays off in both fragments; enabling it is a separate,
commented-out block in each, per [CONFIGURATION.md](CONFIGURATION.md).

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
  per-device access check. See "The exposure decision" above for
  exactly when and why each fragment publishes it.
- **Certificate material is always a runtime mount, never baked in.**
  Both fragments mount `SEP2_SERVER_CERT_DIR` READ-WRITE from an
  operator-supplied path outside any repository or workspace, matching
  [CERTIFICATES.md](CERTIFICATES.md). Writable is what both ship with:
  `dev-mint` mode needs to persist what it generates across restarts,
  or every restart mints a new CA and invalidates every device's
  identity. For a `preprovisioned` deployment, a **read-only** mount is
  now also genuinely supported, not merely tolerated: `preprovisioned`
  mode writes nothing to that directory in any circumstance, so `:ro`
  costs nothing and turns any future accidental write attempt into a
  hard failure instead of a silent one.

**`SEP2_CERT_DIR` must be outside every git checkout, including a
`gridappsd-docker` clone.** A `gridappsd-docker` clone is a checkout
like any other; a path such as `./sep2-certs` inside it resolves
relative to that clone and would land private key material in exactly
the kind of working tree this repository's own `.gitignore` fix exists
to keep it out of. Both fragments fail `docker compose` closed with no
default if `SEP2_CERT_DIR` is unset, and both fragments' comments say
this explicitly. Use a path with nothing to do with any repository,
for example `/srv/gridappsd/sep2-certs`.

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
