.PHONY: build test test-race test-integration test-gridappsd bridge-e2e vet fmt-check coverage

build:
	go build ./...

test:
	go test ./...

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
#     SEP2_FEEDER_MRID=_C1C3E687-6FFD-C753-582B-632A27E28507
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
