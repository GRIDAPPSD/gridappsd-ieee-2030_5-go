# Docker

## Today: a dev broker, not a bridge image

There is no Dockerfile for the bridge in this repository yet, and no
published image. What exists today is `docker-compose.yml` at the
repo root, which brings up a bare ActiveMQ Classic broker for local
testing, nothing more:

```bash
docker compose up -d
```

This binds the STOMP port (`61613`) and the web console (`8161`) to
loopback only, using the upstream `system` / `manager` default
credentials. It is what `make test-integration` and `make bridge-e2e`
run against; see the root README's Testing section and
[cmd/bridge/README.md](../cmd/bridge/README.md). Do not expose this
broker beyond loopback: the credentials are dev-only and public.

## Planned: a bridge image

A container image for the bridge is planned but not yet built. This
section describes the intended design, so anything below is a target,
not something you can `docker pull` today.

The intended shape:

- **Single-stage, distroless, non-root.** The Go binary is compiled
  outside the image build (in CI, where the module-access
  credentials already live) and `COPY`'d in. The image build itself
  never receives a credential, and no GRIDAPPSD module source ever
  enters a layer.
- **No `latest` tag**, and no branch or `main` tags. Only release-tag
  images are pushed, so a compose stack never changes under an
  operator without a deliberate version bump.
- **Certificate material is always a runtime mount, never baked in.**
  Mount `SEP2_SERVER_CERT_DIR` read-only from an operator-supplied
  path outside any repository or workspace, matching
  [CERTIFICATES.md](CERTIFICATES.md). `preprovisioned` mode never
  writes to that directory, so `:ro` is the supported shape rather
  than a workaround, and an incomplete directory fails loudly at
  startup whether or not the mount is read-only.
- **The 2030.5 listener is meant to be exposed**; it is mTLS with a
  per-device access check, so publishing it on the compose network is
  the intended use. **The admin UI is not.** It defaults to loopback
  and disabled, and that default should not be widened casually
  inside a container: see [CONFIGURATION.md](CONFIGURATION.md) before
  changing it.

An illustrative compose fragment, once an image exists:

```yaml
services:
  ieee-2030_5-bridge:
    image: gridappsd/ieee-2030_5-bridge:vX.Y.Z   # not yet published
    restart: unless-stopped
    ports:
      - "8443:8443"   # 2030.5 mTLS listener, authenticated, safe to expose
    environment:
      SEP2_STOMP_ADDR: gridappsd:61613
      SEP2_SERVER_ADDR: "0.0.0.0:8443"
      SEP2_SERVER_CERT_DIR: /etc/sep2/certs
      SEP2_DEVICE_CERT_MODE: preprovisioned
      # Admin UI stays off by default. Read CONFIGURATION.md before enabling it.
    volumes:
      - "${SEP2_CERT_DIR:?SEP2_CERT_DIR must be set}:/etc/sep2/certs:ro"
```

`SEP2_CERT_DIR` above is a host path the operator sets, outside any
repository or workspace checkout; the compose file intentionally has
no default for it, so a missing value fails the `docker compose`
invocation rather than silently creating a directory somewhere
convenient and wrong.

Do not treat any tag, image name, or example above as available until
a release announcement says otherwise.
