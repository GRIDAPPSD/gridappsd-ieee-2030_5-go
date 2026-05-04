.PHONY: build test test-race test-integration test-gridappsd vet fmt-check coverage

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
test-gridappsd:
	@if ! timeout 2 bash -c 'cat </dev/null >/dev/tcp/127.0.0.1/61613' 2>/dev/null; then \
	  echo "GridAPPS-D STOMP port 61613 is not reachable."; \
	  echo "Bring the platform up first:"; \
	  echo "  cd ~/repos/sentient_gridappsd_integration && pixi run gridappsd-start"; \
	  echo "Then re-run: make test-gridappsd"; \
	  exit 1; \
	fi
	go test -tags=gridappsd -race -timeout 5m -v ./internal/cimstomp/

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
