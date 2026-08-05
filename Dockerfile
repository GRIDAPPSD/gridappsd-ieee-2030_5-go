# The bridge binary is an INPUT to this build, not built here. The build
# context must already contain a "bridge" binary compiled for linux/amd64
# with CGO_ENABLED=0. Produce it with: make bridge-artifact
#
# This is deliberate, not a shortcut: the private modules this repository
# depends on (github.com/GRIDAPPSD/ieee-2030_5-core-go and
# github.com/GRIDAPPSD/gridappsd-go) need a credential to resolve. Building
# the binary here would mean this Dockerfile has to receive that
# credential. Instead, the binary is compiled once, in the environment that
# already holds the credential (the release workflow, or a developer's own
# checkout), and this build never sees anything but the finished artifact.
# There is no ARG, no --secret, and no --mount=type=secret in this file:
# a credential that is never an input cannot leak from a layer, from image
# history, or from the build cache.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35

# OCI labels for provenance. Populated by the caller (release workflow or
# `make image`); default to empty rather than a guessed value.
ARG IMAGE_VERSION=""
ARG IMAGE_REVISION=""
LABEL org.opencontainers.image.source="https://github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go" \
      org.opencontainers.image.version="${IMAGE_VERSION}" \
      org.opencontainers.image.revision="${IMAGE_REVISION}" \
      org.opencontainers.image.title="gridappsd-ieee-2030_5" \
      org.opencontainers.image.description="GridAPPS-D bridge to an embedded IEEE 2030.5 server"

COPY bridge /bridge

# This repository carries no NOTICE file: the bridge embeds no IEEE
# 2030.5 schema or WADL material (verified by source-tree count, gated on
# every change by the release workflow's IEEE-material check) and no
# other third-party redistribution obligation beyond this LICENSE. Ship
# it in the image so a public pull carries its own attribution.
COPY LICENSE /LICENSE

# The base image's nonroot user, uid and gid 65532 (confirmed by inspecting
# the built image; see the deployment notes). Set explicitly rather than
# relying on the base image's own USER directive, so this Dockerfile keeps
# working if a future base image changes its default.
USER 65532:65532

# IEEE 2030.5 mTLS listener. The binary defaults this bind to loopback,
# which is unreachable from outside the container; a deployment that wants
# to serve devices sets SEP2_SERVER_ADDR explicitly. See the compose
# fragment and docs/CONFIGURATION.md.
EXPOSE 8443
# Read-only admin UI. Disabled unless SEP2_ADMIN_UI_KEY(_FILE) is set, and
# loopback-only unless SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK is also set. See
# docs/CONFIGURATION.md before changing either.
EXPOSE 8444

# No HEALTHCHECK: distroless/static has no shell, so neither CMD-SHELL nor
# a CMD healthcheck invoking a second binary can run. A HEALTHCHECK that
# cannot execute is worse than none.

ENTRYPOINT ["/bridge"]
