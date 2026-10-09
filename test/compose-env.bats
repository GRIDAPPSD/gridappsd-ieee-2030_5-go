#!/usr/bin/env bats
# Tests that docker-compose.bridge.yml and .env.example carry every environment
# variable cmd/bridge reads. The set is derived from the Go source, so a new
# setting added there without a compose line and an example line fails here.
# No docker, no network.

setup() {
  repo="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
  # COMPOSE_FILE_UNDER_TEST and ENV_EXAMPLE_UNDER_TEST point the checks at a mutated copy.
  compose="${COMPOSE_FILE_UNDER_TEST:-$repo/docker-compose.bridge.yml}"
  example="${ENV_EXAMPLE_UNDER_TEST:-$repo/.env.example}"
}

# Names the compose file pins to a literal: not user settings, so .env.example
# carries them only as commented lines saying so.
PINNED="SEP2_ADMIN_UI_ADDR SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK SEP2_SERVER_ADDR SEP2_SERVER_CERT_DIR"
SECRETS="SEP2_ADMIN_UI_KEY"
# Read by the bridge but deliberately not passed to the container: SEP2_STOMP_ADDR
# always has a value there and wins. .env.example carries them as commented lines.
NOTPASSED="GRIDAPPSD_ADDRESS GRIDAPPSD_PORT"

# in_list NAME LIST: true when NAME is one of the words in LIST.
in_list() {
  case " $2 " in *" $1 "*) return 0 ;; *) return 1 ;; esac
}

# bridge_env_names prints every SEP2_ and GRIDAPPSD_ variable the bridge reads,
# one per line: the getenv-style calls in config.go (the name is the first
# string literal in the call), every name literal on a resolveCred line, the knob
# table in tuning.go, and the four admin-plane settings read through the
# server module's SettingsFromEnv.
bridge_env_names() {
  {
    /usr/bin/grep -oE '(getenvDefault|getenvBool|getenvList|os\.Getenv|os\.LookupEnv)\([^"]*"(SEP2|GRIDAPPSD)_[A-Z0-9_]+"' "$repo/cmd/bridge/config.go" |
      /usr/bin/grep -oE '(SEP2|GRIDAPPSD)_[A-Z0-9_]+'
    /usr/bin/grep -E 'resolveCred\(' "$repo/cmd/bridge/config.go" |
      /usr/bin/grep -oE '"(SEP2|GRIDAPPSD)_[A-Z0-9_]+"' | tr -d '"'
    /usr/bin/grep -oE '^\s+[dn]\("[a-z0-9-]+", "SEP2_[A-Z0-9_]+"' "$repo/cmd/bridge/tuning.go" |
      /usr/bin/grep -oE 'SEP2_[A-Z0-9_]+'
    adminplane_names
  } | sort -u
}

# adminplane_names reads the names from the pinned server module when go and
# the module are available, and otherwise falls back to the four it reads at
# the pinned version, so the suite still runs without a toolchain.
adminplane_names() {
  local dir=""
  if command -v go >/dev/null 2>&1; then
    dir=$(cd "$repo" && go list -m -f '{{.Dir}}' github.com/GRIDAPPSD/ieee-2030_5-server-go 2>/dev/null || true)
  fi
  if [ -n "$dir" ] && [ -r "$dir/pkg/sep2adminplane/settings.go" ]; then
    /usr/bin/grep -oE 'getenv\("SEP2_[A-Z0-9_]+"' "$dir/pkg/sep2adminplane/settings.go" | /usr/bin/grep -oE 'SEP2_[A-Z0-9_]+'
  else
    printf '%s\n' SEP2_EDITION SEP2_PEN SEP2_FLOW_RESERVATION_DEADLINE_SECONDS SEP2_FLOW_RESERVATION_RETENTION_GRACE_SECONDS
  fi
}

compose_names() {
  /usr/bin/grep -oE '^      (SEP2|GRIDAPPSD)_[A-Z0-9_]+:' "$compose" | tr -d ' :' | sort -u
}

# example_names: the active settings, plus the pinned names that appear as a
# commented "is fixed in docker-compose.bridge.yml" line.
example_names() {
  {
    /usr/bin/grep -oE '^(SEP2|GRIDAPPSD)_[A-Z0-9_]+=' "$example" | tr -d '='
    /usr/bin/grep -oE '^# GRIDAPPSD_[A-Z0-9_]+ is not passed to the container' "$example" | /usr/bin/grep -oE 'GRIDAPPSD_[A-Z0-9_]+'
    /usr/bin/grep -oE '^# SEP2_[A-Z0-9_]+ is fixed in docker-compose.bridge.yml' "$example" | /usr/bin/grep -oE 'SEP2_[A-Z0-9_]+'
  } | sort -u
}

@test "derived set: pattern fires on known names and finds the whole table" {
  names=$(bridge_env_names)
  # Control: one name from each source, so a pattern that stopped matching shows here.
  for n in SEP2_ADMIN_UI_KEY SEP2_STOMP_PASSWORD SEP2_TELEMETRY_INTERVAL SEP2_STOMP_PROBE_INTERVAL SEP2_NOTIFY_QUEUE_SIZE SEP2_PEN GRIDAPPSD_USER GRIDAPPSD_PASSWORD GRIDAPPSD_ADDRESS GRIDAPPSD_PORT; do
    printf '%s\n' "$names" | /usr/bin/grep -qx "$n"
  done
  [ "$(printf '%s\n' "$names" | wc -l)" -ge 50 ]
}

@test "every variable the bridge reads is a key of the compose environment block" {
  missing=$(comm -23 <(bridge_env_names) <(compose_names) | /usr/bin/grep -vxE "$(tr ' ' '|' <<<"$NOTPASSED")" || true)
  [ -z "$missing" ] || { echo "missing from compose: $missing" >&2; false; }
}

@test "every variable the bridge reads has a line in .env.example" {
  missing=$(comm -23 <(bridge_env_names) <(example_names))
  [ -z "$missing" ] || { echo "missing from .env.example: $missing" >&2; false; }
}

@test "the compose file and .env.example name no SEP2_ or GRIDAPPSD_ variable the bridge does not read" {
  extra=$(comm -13 <(bridge_env_names) <(compose_names))
  [ -z "$extra" ] || { echo "unknown in compose: $extra" >&2; false; }
  extra=$(comm -13 <(bridge_env_names) <(example_names))
  [ -z "$extra" ] || { echo "unknown in .env.example: $extra" >&2; false; }
}

@test "compose: every non-secret setting is NAME: \${NAME:-default}" {
  for n in $(compose_names); do
    if in_list "$n" "$SECRETS $PINNED SEP2_REGISTRATION_PIN"; then continue; fi
    # shellcheck disable=SC2016 # literal compose syntax
    /usr/bin/grep -qE "^      $n: \\\$\\{$n:-.*\\}\$" "$compose" || { echo "no default form: $n" >&2; false; }
  done
}

@test "secrets: the admin key has no default; blank is allowed in compose and the bridge disables the UI" {
  n=SEP2_ADMIN_UI_KEY
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qxF "      $n: \${$n:-}" "$compose"
  # The exact line above is the only place the name takes a default form, and
  # the required form is gone.
  [ "$(/usr/bin/grep -cF "{$n:-" "$compose")" -eq 1 ]
  [ "$(/usr/bin/grep -cF "{$n:?" "$compose")" -eq 0 ]
  # The example carries the name with an empty value: a placeholder that
  # fails the 16 character check in the launcher.
  /usr/bin/grep -qx "$n=" "$example"
}

@test "no-key mode: off by default in compose and the example" {
  n=SEP2_ADMIN_UI_INSECURE_NO_KEY
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qxF "      $n: \${$n:-false}" "$compose"
  /usr/bin/grep -qx "$n=false" "$example"
}

@test "registration PIN defaults to the documented dev PIN and can be explicitly disabled" {
  # shellcheck disable=SC2016 # literal compose syntax; '-' preserves an explicit empty value
  /usr/bin/grep -qxF '      SEP2_REGISTRATION_PIN: ${SEP2_REGISTRATION_PIN-123455}' "$compose"
  /usr/bin/grep -qx 'SEP2_REGISTRATION_PIN=123455' "$example"
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qxF '      SEP2_REGISTRATION_PIN_FILE: ${SEP2_REGISTRATION_PIN_FILE:-}' "$compose"
  /usr/bin/grep -qx 'SEP2_REGISTRATION_PIN_FILE=' "$example"
}

@test "broker login defaults GRIDAPPSD_USER to system and keeps overrides optional; passwords stay empty" {
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qxF '      GRIDAPPSD_USER: ${GRIDAPPSD_USER:-system}' "$compose"
  /usr/bin/grep -qx 'GRIDAPPSD_USER=system' "$example"
  for n in GRIDAPPSD_PASSWORD SEP2_STOMP_USER SEP2_STOMP_PASSWORD; do
    # shellcheck disable=SC2016 # literal compose syntax
    /usr/bin/grep -qxF "      $n: \${$n:-}" "$compose"
    /usr/bin/grep -qx "$n=" "$example"
  done
  for n in $NOTPASSED; do
    [ "$(/usr/bin/grep -cE "^      $n:" "$compose")" -eq 0 ]
    [ "$(/usr/bin/grep -cE "^$n=" "$example")" -eq 0 ]
    /usr/bin/grep -qE "^# $n is not passed to the container" "$example"
  done
}

@test "compose config: GRIDAPPSD_USER and GRIDAPPSD_PASSWORD from the env file reach the container environment" {
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 || skip "docker compose is not available"
  printf 'SEP2_ADMIN_UI_KEY=0123456789abcdef-key\nGRIDAPPSD_USER=gu-value\nGRIDAPPSD_PASSWORD=gp-value\n' >"$BATS_TEST_TMPDIR/env"
  out=$(env -i PATH="$PATH" HOME="$BATS_TEST_TMPDIR" BRIDGE_IMAGE=i BRIDGE_USER=1:1 BRIDGE_ADMIN_IP=10.0.0.2 \
    BRIDGE_SEP2_BIND_IP=127.0.0.1 BRIDGE_ADMIN_BIND_IP=127.0.0.1 BRIDGE_SEP2_PORT=1 BRIDGE_ADMIN_PORT=2 BRIDGE_CERT_DIR=/c BRIDGE_CERT_MODE=rw BRIDGE_ADMIN_SUBNET=10.0.0.0/24 \
    docker compose --env-file "$BATS_TEST_TMPDIR/env" -f "$compose" config)
  printf '%s\n' "$out" | /usr/bin/grep -qx '      GRIDAPPSD_USER: gu-value'
  printf '%s\n' "$out" | /usr/bin/grep -qx '      GRIDAPPSD_PASSWORD: gp-value'
}

@test ".env.example default equals the compose default for every non-secret setting" {
  compared=0
  for n in $(compose_names); do
    if in_list "$n" "$SECRETS $PINNED SEP2_REGISTRATION_PIN"; then continue; fi
    compared=$((compared + 1))
    cdef=$(/usr/bin/grep -E "^      $n: " "$compose" | sed -E "s/^      $n: \\\$\\{$n:-(.*)\\}\$/\\1/")
    edef=$(/usr/bin/grep -E "^$n=" "$example" | sed -E "s/^$n=//")
    [ "$cdef" = "$edef" ] || { echo "$n: compose '$cdef' example '$edef'" >&2; false; }
  done
  # Exclude the admin key, the four pinned names, the two not passed and the PIN with its explicit-empty syntax.
  [ "$compared" -eq $(($(bridge_env_names | wc -l) - 8)) ]
}

@test "pinned: four names are literals in compose, never overridable, and not settings in .env.example" {
  # The names that are not the NAME: ${NAME:-...} or :? form are exactly PINNED.
  unwrapped=$(for n in $(compose_names); do
    # shellcheck disable=SC2016 # literal compose syntax
    if [ "$n" = SEP2_REGISTRATION_PIN ]; then continue; fi
    /usr/bin/grep -qE "^      $n: \\\$\\{$n:[-?]" "$compose" || echo "$n"
  done | sort | tr '\n' ' ')
  want=$(tr ' ' '\n' <<<"$PINNED" | sort | tr '\n' ' ')
  [ "$unwrapped" = "$want" ]
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qxF '      SEP2_ADMIN_UI_ADDR: ${BRIDGE_ADMIN_IP:?use make docker-up}:18444' "$compose"
  /usr/bin/grep -qxF '      SEP2_ADMIN_UI_ALLOW_NON_LOOPBACK: "true"' "$compose"
  /usr/bin/grep -qxF '      SEP2_SERVER_ADDR: 0.0.0.0:18443' "$compose"
  /usr/bin/grep -qxF '      SEP2_SERVER_CERT_DIR: /etc/sep2/certs' "$compose"
  for n in $PINNED; do
    [ "$(/usr/bin/grep -cE "^$n=" "$example")" -eq 0 ]
    /usr/bin/grep -qE "^# $n is fixed in docker-compose.bridge.yml" "$example"
  done
}

@test ".env, the old .env.bridge and .env.local are ignored; .env.example is not" {
  cd "$repo"
  for f in .env .env.bridge .env.local; do
    git check-ignore --no-index -q "$f" || { echo "not ignored: $f" >&2; false; }
  done
  if git check-ignore --no-index -q .env.example; then false; fi
  [ ! -e "$repo/.env.bridge.example" ]
  [ "$(/usr/bin/grep -c 'env\.bridge' "$repo/Makefile")" -eq 0 ]
}

@test "DOCKER.md says Docker Engine 28 or newer is required, because of gw_priority" {
  /usr/bin/grep -qE 'Docker Engine 28 or newer is required' "$repo/docs/DOCKER.md"
  /usr/bin/grep -q 'gw_priority' "$repo/docs/DOCKER.md"
}

@test "the dev broker compose file reads no variable the bridge section of .env.example sets" {
  dev=$(/usr/bin/grep -oE '[$][{][A-Z0-9_]+' "$repo/docker-compose.yml" | sed 's/^[$][{]//' | sort -u)
  [ -n "$dev" ]
  shared=$(comm -12 <(printf '%s\n' "$dev") <(bridge_env_names))
  [ -z "$shared" ]
}

# example_settings prints each assignment line of a file, active or commented
# default (`NAME=value` or `# NAME=value`), in file order.
example_settings() {
  /usr/bin/grep -E '^(# )?[A-Z][A-Z0-9_]+=' "$1"
}

@test ".env.example opens with the host ports and network, then the required credentials, then grid ids" {
  want="BRIDGE_SEP2_PORT BRIDGE_ADMIN_PORT BRIDGE_SEP2_BIND_IP BRIDGE_ADMIN_BIND_IP BRIDGE_ADMIN_SUBNET BRIDGE_ADMIN_IP SEP2_STOMP_ADDR SEP2_STOMP_ALLOW_PLAINTEXT SEP2_ADMIN_UI_KEY GRIDAPPSD_USER GRIDAPPSD_PASSWORD SEP2_STOMP_USER SEP2_STOMP_PASSWORD SEP2_REGISTRATION_PIN SEP2_REGISTRATION_PIN_FILE SEP2_SIMULATION_ID SEP2_APPLICATION_ID SEP2_FEEDER_MRID"
  count=$(wc -w <<<"$want")
  got=$(example_settings "$example" | sed -E 's/^(# )?([A-Z][A-Z0-9_]+)=.*/\2/' | head -n "$count" | tr '\n' ' ')
  [ "$got" = "$want " ] || { echo "got:  $got" >&2; echo "want: $want" >&2; false; }
}

@test ".env.example keeps every setting line of origin/main byte for byte" {
  git -C "$repo" rev-parse --verify -q origin/main >/dev/null || skip "origin/main is not available"
  git -C "$repo" show origin/main:.env.example >"$BATS_TEST_TMPDIR/before"
  example_settings "$BATS_TEST_TMPDIR/before" | sort >"$BATS_TEST_TMPDIR/before.sorted"
  example_settings "$example" | sort >"$BATS_TEST_TMPDIR/after.sorted"
  # Control: the file read is not empty, so an empty diff means a real comparison.
  [ "$(wc -l <"$BATS_TEST_TMPDIR/before.sorted")" -ge 60 ]
  # A new setting is allowed; a changed or dropped one is not.
  # The 2030.5 bind default moved from loopback to 0.0.0.0, and the admin
  # hostname is now the Caddy `server` name; these are the intentional changes.
  lost=$(comm -23 "$BATS_TEST_TMPDIR/before.sorted" "$BATS_TEST_TMPDIR/after.sorted" | /usr/bin/grep -vxF -e '# BRIDGE_SEP2_BIND_IP=127.0.0.1' -e 'SEP2_ADMIN_UI_ALLOWED_HOSTS=' -e 'SEP2_REGISTRATION_PIN=' -e 'GRIDAPPSD_USER=' -e '# BRIDGE_CERT_DIR=/absolute/path/to/sep2-certs   (default: ~/.config/gridappsd/2030.5server/sep2-certs)' || true)
  [ -z "$lost" ] || { echo "changed or dropped: $lost" >&2; false; }
}
