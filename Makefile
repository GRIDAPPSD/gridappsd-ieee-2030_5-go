.PHONY: help check-go build run configure docker-build docker-pull docker-up docker-down docker-logs test test-shell test-race test-integration test-gridappsd bridge-e2e vet fmt-check coverage

.DEFAULT_GOAL := help

# help lists every target that carries a ## description, the same way the
# server's Makefile does. It is the default goal, so a bare `make` prints it.
help:                     ## Show this help
	@/usr/bin/grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  %-20s %s\n", $$1, $$2}'

# VERSION is stamped into internal/buildinfo.Version at link time via
# LDFLAGS below. `git describe` gives the nearest tag plus a
# commit-count/sha suffix when HEAD is past the last tag, and a
# `-dirty` suffix when the worktree has uncommitted changes; falling
# back to "dev" covers a checkout with no tags at all (a shallow clone,
# or a tarball export with no .git directory).
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/buildinfo.Version=$(VERSION)

# check-go stops a compiling target early when the installed Go is older
# than the go line of go.mod, which the script reads so the version is
# written once.
check-go:
	@scripts/check-go-version.sh go.mod

# BRIDGE is the binary `build` writes and `run` starts.
BRIDGE ?= ./bridge

# build first compiles every package (the check this target always did),
# then writes the bridge binary.
build: check-go                    ## Build every package and write the bridge binary
	go build -ldflags "$(LDFLAGS)" ./...
	go build -ldflags "$(LDFLAGS)" -o $(BRIDGE) ./cmd/bridge

# run starts the bridge in the foreground against a local dev stack.
# Override any of these on the make line, e.g.
#   make run SEP2_SERVER_ADDR=127.0.0.1:9443 ADMIN_UI_KEY_FILE=./admin.key
# FEEDER_MRID and REGISTRATION_PIN default to empty, which leaves the
# binary's own feeder default and no fleet-wide PIN. STOMP_ALLOW_PLAINTEXT
# defaults true, like bridge-e2e, because the local dev brokers are plain
# TCP; the binary's own default stays TLS. SEP2_SIMULATION_ID passes
# through when set. The admin key is never a Makefile variable: it comes
# from SEP2_ADMIN_UI_KEY in the environment or from the file named by
# ADMIN_UI_KEY_FILE, and a missing or short key is refused before start
# (the admin UI is not started disabled from here).
SEP2_SERVER_ADDR ?= 127.0.0.1:18443
ADMIN_UI_ADDR ?= 127.0.0.1:18444
SEP2_SERVER_CERT_DIR ?= ./sep2-certs
FEEDER_MRID ?=
REGISTRATION_PIN ?=
STOMP_ALLOW_PLAINTEXT ?= true
ADMIN_UI_KEY_FILE ?=

run: build                ## Build and start the bridge (needs SEP2_ADMIN_UI_KEY or ADMIN_UI_KEY_FILE)
	@BRIDGE='$(BRIDGE)' \
	SEP2_SERVER_ADDR='$(SEP2_SERVER_ADDR)' \
	ADMIN_UI_ADDR='$(ADMIN_UI_ADDR)' \
	SEP2_SERVER_CERT_DIR='$(SEP2_SERVER_CERT_DIR)' \
	FEEDER_MRID='$(FEEDER_MRID)' \
	REGISTRATION_PIN='$(REGISTRATION_PIN)' \
	STOMP_ALLOW_PLAINTEXT='$(STOMP_ALLOW_PLAINTEXT)' \
	ADMIN_UI_KEY_FILE='$(ADMIN_UI_KEY_FILE)' \
	scripts/run-bridge.sh

# The docker-* targets drive docker-compose.bridge.yml through
# scripts/docker-bridge.sh, which refuses a start whose env file, cert dir,
# platform network or host ports are not ready. Settings: .env.
configure:                ## Create .env with a generated admin key and local bridge defaults
	@scripts/configure-docker.sh

docker-build:             ## Build the bridge image
	@VERSION='$(VERSION)' scripts/docker-bridge.sh build

docker-pull:              ## Pull the published bridge image only (BRIDGE_IMAGE_TAG picks the version, default latest)
	@scripts/docker-bridge.sh pull

docker-up:                ## Build the image and start the bridge (pulls and runs the published image; BRIDGE_USE_PUBLISHED=0 builds locally)
	@VERSION='$(VERSION)' scripts/docker-bridge.sh up

docker-down:              ## Stop the bridge container
	@scripts/docker-bridge.sh down

docker-logs:              ## Follow the bridge container log
	@scripts/docker-bridge.sh logs

test: check-go                     ## Run all Go tests
	go test ./...

# test-shell runs the bats suites for `make run`, the docker targets, the image publish path and the Go version check; it needs bats on PATH and
# is separate from `test` so `test` stays plain `go test ./...`.
test-shell:               ## Run the bats suites (needs bats)
	bats test/run.bats test/go-version.bats test/docker-bridge.bats test/configure-docker.bats test/compose-env.bats test/docker-publish.bats test/workflow.bats

test-race: check-go            ## Run all Go tests with the race detector
	go test -race ./...

# test-integration brings up an ActiveMQ classic broker via docker compose,
# runs the cimstomp.Client integration tests with the `integration` build
# tag, and tears the broker down. Requires docker compose on PATH.
test-integration: check-go     ## Run cimstomp integration tests against ActiveMQ (needs docker compose)
	docker compose up -d
	# Give ActiveMQ a moment to bind 61613.
	sleep 5
	go test -tags=integration -race ./internal/cimstomp/; \
	  rc=$$?; \
	  docker compose down; \
	  exit $$rc

# test-gridappsd runs the cimstomp integration tests against the real
# GridAPPS-D platform stack (the gridappsd-docker compose from
# ~/repos/sentient_gridappsd_integration/gridappsd-docker/, brought up via
# `pixi run gridappsd-start`).
#
# Approach: idempotent probe. The target probes the STOMP port; if the
# platform is already running, tests run immediately. If not, the target
# fails fast with a clear message pointing at the pixi command. We do NOT
# bring up the platform from inside this repo, for three reasons:
#
#   1. The platform stack lives in a sibling repo with its own toolchain
#      (pixi). Coupling this Makefile to that toolchain would tangle the
#      bridge repo's build with a non-Go dependency.
#   2. The platform takes 30 to 60 seconds to come up and is heavy. A
#      developer iterating on cimstomp internals wants the bare-ActiveMQ
#      `test-integration` target, not this one.
#   3. Tear-down is destructive (stops simulations, drops state). Leaving
#      the platform up across runs is the common case; let the developer
#      manage its lifecycle.
#
# The bare-ActiveMQ `test-integration` target stays for fast cimstomp
# iteration. This target is for verifying that the bridge's STOMP wire
# format is accepted by the actual production-equivalent broker.
#
# Probe uses `nc -z` (with a 2-second connect timeout) rather than bash's
# `/dev/tcp` redirection. `/dev/tcp` is a bash builtin that is absent from
# dash and busybox, so the prior probe broke on systems where /bin/sh is
# not bash. `nc` is present on essentially every Linux dev box; if it is
# missing on yours, install netcat (`apt install netcat-openbsd` or
# equivalent) before running this target.
test-gridappsd: check-go       ## Run cimstomp tests against a running GridAPPS-D stack
	@if ! command -v nc >/dev/null 2>&1; then \
	  echo "test-gridappsd requires nc (netcat) for the port probe."; \
	  echo "Install with one of:"; \
	  echo "  apt install netcat-openbsd     # Debian/Ubuntu"; \
	  echo "  dnf install nmap-ncat          # RHEL/Fedora"; \
	  exit 1; \
	fi
	@if ! nc -z -w 2 127.0.0.1 61613 2>/dev/null; then \
	  echo "GridAPPS-D STOMP port 61613 is not reachable."; \
	  echo "Bring the platform up first:"; \
	  echo "  cd ~/repos/sentient_gridappsd_integration && pixi run gridappsd-start"; \
	  echo "Then re-run: make test-gridappsd"; \
	  exit 1; \
	fi
	go test -tags=gridappsd -race -timeout 5m -v ./internal/cimstomp/ ./internal/busmonitor/ ./internal/adminui/

# bridge-e2e probes the GridAPPS-D STOMP listener and, if reachable,
# runs the cmd/bridge binary against it. The same probe pattern as
# test-gridappsd: we do NOT bring up the platform from this Makefile.
# The developer runs gridappsd-docker (or the bare ActiveMQ via
# `docker compose up -d`) themselves; this target validates the bridge
# wiring against whatever broker is up.
#
# Override the env on the make line for non-default targets, e.g.:
#
#   make bridge-e2e \
#     SEP2_STOMP_ADDR=172.20.0.2:61613 \
#     SEP2_STOMP_USER=system \
#     SEP2_STOMP_PASSWORD=manager \
#     SEP2_SIMULATION_ID=1234567890 \
#     SEP2_FEEDER_MRID=_C1C3E687-6FFD-C753-582B-632A27E28507 \
#     SEP2_STOMP_ALLOW_PLAINTEXT=true
#
# SEP2_STOMP_ALLOW_PLAINTEXT defaults true here because this target's
# two documented dev brokers (bare ActiveMQ via `docker compose up -d`
# and gridappsd-docker) are both plain TCP. The bridge binary's own
# compiled-in default stays fail-closed (TLS); this dev-only default
# lives in the Makefile, not the binary, so a production invocation of
# `bridge` still has to opt in explicitly.
#
# The probe hard-fails when `nc` is missing, matching test-gridappsd's
# behavior above (Dutch M2): the two targets previously
# diverged (test-gridappsd failed on missing nc, bridge-e2e warned and
# proceeded to `go run` anyway), which let a developer run this target
# without a working port probe and get a confusing bridge-side connect
# failure instead of the sharp, actionable message below. Pick the
# stricter behavior for both.
SEP2_STOMP_ADDR ?= 127.0.0.1:61613
SEP2_STOMP_USER ?= system
SEP2_STOMP_PASSWORD ?= manager
SEP2_SIMULATION_ID ?=
SEP2_FEEDER_MRID ?= _F49D1288-9EC6-47DB-8769-57E2B6EDB124
SEP2_STOMP_ALLOW_PLAINTEXT ?= true

bridge-e2e: check-go           ## Run the bridge against a running STOMP broker
	@set -e; \
	if ! command -v nc >/dev/null 2>&1; then \
	  echo "bridge-e2e requires nc (netcat) for the port probe."; \
	  echo "Install with one of:"; \
	  echo "  apt install netcat-openbsd     # Debian/Ubuntu"; \
	  echo "  dnf install nmap-ncat          # RHEL/Fedora"; \
	  exit 1; \
	fi; \
	host=$$(echo $(SEP2_STOMP_ADDR) | cut -d: -f1); \
	port=$$(echo $(SEP2_STOMP_ADDR) | cut -d: -f2); \
	if ! nc -z -w 2 $$host $$port 2>/dev/null; then \
	  echo "bridge-e2e: STOMP $$host:$$port not reachable."; \
	  echo "Bring the broker up first, e.g.:"; \
	  echo "  docker compose up -d                                       # bare ActiveMQ in this repo"; \
	  echo "  cd ~/repos/sentient_gridappsd_integration/gridappsd-docker && docker compose up -d"; \
	  exit 1; \
	fi; \
	SEP2_STOMP_ADDR=$(SEP2_STOMP_ADDR) \
	SEP2_STOMP_USER=$(SEP2_STOMP_USER) \
	SEP2_STOMP_PASSWORD=$(SEP2_STOMP_PASSWORD) \
	SEP2_SIMULATION_ID=$(SEP2_SIMULATION_ID) \
	SEP2_FEEDER_MRID=$(SEP2_FEEDER_MRID) \
	SEP2_STOMP_ALLOW_PLAINTEXT=$(SEP2_STOMP_ALLOW_PLAINTEXT) \
	go run ./cmd/bridge

vet: check-go                  ## Run go vet
	go vet ./...

fmt-check:            ## Fail if gofmt would change anything
	@diff=$$(gofmt -s -d . internal cmd); \
	  if [ -n "$$diff" ]; then \
	    echo "gofmt diff:"; \
	    echo "$$diff"; \
	    exit 1; \
	  fi

coverage: check-go             ## Print cimstomp test coverage
	go test -cover ./internal/cimstomp/
