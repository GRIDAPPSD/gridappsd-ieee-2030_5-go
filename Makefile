.PHONY: build test test-race test-integration test-gridappsd bridge-e2e vet fmt-check coverage ui-build ui-check

build:
	go build ./...

test:
	go test ./...

# ui-build builds the admin UI's Svelte frontend
# (internal/adminui/web/frontend/) and writes the static assets into
# internal/adminui/web/dist/, then rebuilds the Go binary so the
# freshly built assets are embedded via internal/adminui/web/embed.go's
# "//go:embed all:dist" directive.
#
# Node and npm are needed to RUN this target, but not to run the
# resulting binary: the embedded bundle in dist/ is committed to the
# repo, so `go build ./...` alone (with no Node toolchain at all)
# already succeeds on a fresh checkout using whatever dist/ content is
# currently committed. Run this target only when the frontend source
# under frontend/ has changed and dist/ needs regenerating.
ui-build:
	cd internal/adminui/web/frontend && npm ci && npm run build
	go build ./...

# ui-check rebuilds the frontend into internal/adminui/web/dist/ and
# then diffs that directory against what is committed, so a frontend
# source change landed WITHOUT a matching `make ui-build` (a stale
# embedded bundle) fails loudly instead of shipping silently. `go
# build ./...` alone cannot catch this: it succeeds against whatever
# dist/ content is on disk, committed or not.
#
# The diff is scoped to internal/adminui/web/dist/ only, so an
# unrelated dirty file elsewhere in the working tree does not produce
# a false positive here. This target is meant to run against a clean
# checkout (CI's default); running it locally on a dirty tree may
# report drift caused by unrelated uncommitted changes under dist/.
ui-check:
	cd internal/adminui/web/frontend && npm ci && npm run build
	@if ! git diff --exit-code -- internal/adminui/web/dist/; then \
	  echo "ui-check: internal/adminui/web/dist/ is stale."; \
	  echo "The committed build output does not match what the frontend source in"; \
	  echo "internal/adminui/web/frontend/ currently builds. Run 'make ui-build'"; \
	  echo "and commit the updated dist/ directory."; \
	  exit 1; \
	fi

test-race:
	go test -race ./...

# test-integration brings up an ActiveMQ classic broker via docker compose,
# runs the cimstomp.Client integration tests with the `integration` build
# tag, and tears the broker down. Requires docker compose on PATH.
test-integration:
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
test-gridappsd:
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
	go test -tags=gridappsd -race -timeout 5m -v ./internal/cimstomp/

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
# The probe tolerates `nc` being absent: if no nc on PATH the target
# warns and proceeds to `go run`, letting the bridge produce its own
# connect-failure diagnostic. nc is the cleaner gate when present (it
# prints a sharp message and exits non-zero before spinning up Go),
# but on stripped containers /dev/tcp is also unavailable. Letting the
# bridge fail naturally is acceptable; it is what `go run` does anyway.
SEP2_STOMP_ADDR ?= 127.0.0.1:61613
SEP2_STOMP_USER ?= system
SEP2_STOMP_PASSWORD ?= manager
SEP2_SIMULATION_ID ?=
SEP2_FEEDER_MRID ?= _C1C3E687-6FFD-C753-582B-632A27E28507
SEP2_STOMP_ALLOW_PLAINTEXT ?= true

bridge-e2e:
	@set -e; \
	host=$$(echo $(SEP2_STOMP_ADDR) | cut -d: -f1); \
	port=$$(echo $(SEP2_STOMP_ADDR) | cut -d: -f2); \
	if command -v nc >/dev/null 2>&1; then \
	  if ! nc -z -w 2 $$host $$port 2>/dev/null; then \
	    echo "bridge-e2e: STOMP $$host:$$port not reachable."; \
	    echo "Bring the broker up first, e.g.:"; \
	    echo "  docker compose up -d                                       # bare ActiveMQ in this repo"; \
	    echo "  cd ~/repos/sentient_gridappsd_integration/gridappsd-docker && docker compose up -d"; \
	    exit 1; \
	  fi; \
	else \
	  echo "bridge-e2e: nc not on PATH; skipping probe and letting the bridge connect attempt fail naturally on its own."; \
	fi; \
	SEP2_STOMP_ADDR=$(SEP2_STOMP_ADDR) \
	SEP2_STOMP_USER=$(SEP2_STOMP_USER) \
	SEP2_STOMP_PASSWORD=$(SEP2_STOMP_PASSWORD) \
	SEP2_SIMULATION_ID=$(SEP2_SIMULATION_ID) \
	SEP2_FEEDER_MRID=$(SEP2_FEEDER_MRID) \
	SEP2_STOMP_ALLOW_PLAINTEXT=$(SEP2_STOMP_ALLOW_PLAINTEXT) \
	go run ./cmd/bridge

vet:
	go vet ./...

fmt-check:
	@diff=$$(gofmt -s -d . internal cmd); \
	  if [ -n "$$diff" ]; then \
	    echo "gofmt diff:"; \
	    echo "$$diff"; \
	    exit 1; \
	  fi

coverage:
	go test -cover ./internal/cimstomp/
