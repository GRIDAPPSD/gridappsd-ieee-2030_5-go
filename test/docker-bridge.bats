#!/usr/bin/env bats
# Tests for scripts/docker-bridge.sh. docker and ss are stubs on PATH: docker
# records its argv and the BRIDGE_* environment, ss reports the ports named in
# SS_LISTEN. No image is built and no container is started.

setup() {
  repo="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
  work="$BATS_TEST_TMPDIR"
  mkdir -p "$work/bin" "$work/certs"
  export DOCKER_LOG="$work/docker.log"
  cat >"$work/bin/docker" <<'STUB'
#!/usr/bin/env bash
{
  printf 'ARGS:'
  for a in "$@"; do printf ' [%s]' "$a"; done
  printf '\n'
  printf 'ENV: sep2=%s admin=%s user=%s certs=%s mode=%s image=%s ip=%s subnet=%s\n' "${BRIDGE_SEP2_PORT-}" \
    "${BRIDGE_ADMIN_PORT-}" "${BRIDGE_USER-}" "${BRIDGE_CERT_DIR-}" "${BRIDGE_CERT_MODE-}" "${BRIDGE_IMAGE-}" "${BRIDGE_ADMIN_IP-}" "${BRIDGE_ADMIN_SUBNET-}"
} >>"$DOCKER_LOG"
if [ "${1-}" = network ] && [ "${NO_NETWORK:-}" = 1 ]; then exit 1; fi
exit 0
STUB
  cat >"$work/bin/ss" <<'STUB'
#!/usr/bin/env bash
# Last argument is the filter, "sport = :PORT".
port="${*: -1}"
port="${port##*:}"
for p in ${SS_LISTEN:-}; do
  if [ "$p" = "$port" ]; then printf 'LISTEN 0 4096 127.0.0.1:%s 0.0.0.0:*\n' "$p"; fi
done
exit 0
STUB
  chmod +x "$work/bin/docker" "$work/bin/ss"
  export PATH="$work/bin:$PATH"
  goodkey="0123456789abcdef-key"
  printf 'SEP2_ADMIN_UI_KEY=%s\n' "$goodkey" >"$work/env"
  export BRIDGE_ENV_FILE="$work/env"
  export SEP2_STOMP_PASSWORD="broker-pass"
  export BRIDGE_CERT_DIR="$work/certs"
  unset SEP2_ADMIN_UI_KEY BRIDGE_ADMIN_IP BRIDGE_ADMIN_SUBNET COMPOSE_FILE BRIDGE_COMPOSE_FILE BRIDGE_SEP2_PORT BRIDGE_ADMIN_PORT BRIDGE_USER BRIDGE_CERT_MODE BRIDGE_IMAGE SS_LISTEN NO_NETWORK
}

run_script() {
  run "$repo/scripts/docker-bridge.sh" "$@"
}

@test "up: builds, then starts compose with the resolved env and defaults" {
  run_script up
  [ "$status" -eq 0 ]
  /usr/bin/grep -qxF "ARGS: [build] [-f] [$repo/Dockerfile.bridge] [--build-arg] [VERSION=dev] [-t] [gridappsd-ieee-2030_5-go:dev] [$repo]" "$DOCKER_LOG"
  /usr/bin/grep -qxF "ARGS: [compose] [--env-file] [$work/env] [-f] [$repo/docker-compose.bridge.yml] [up] [-d] [--no-build]" "$DOCKER_LOG"
  /usr/bin/grep -qF "ENV: sep2=18443 admin=18444 user=1000:1000 certs=$work/certs mode=rw image=gridappsd-ieee-2030_5-go:dev ip=10.213.168.2 subnet=10.213.168.0/24" "$DOCKER_LOG"
}

@test "up: env file values override the defaults, shell values override the file" {
  printf 'BRIDGE_SEP2_PORT=28443\nBRIDGE_ADMIN_PORT="28444"\nBRIDGE_USER=2000:2000\nBRIDGE_CERT_MODE=ro\n' >>"$work/env"
  BRIDGE_ADMIN_PORT=29999 run_script up
  [ "$status" -eq 0 ]
  /usr/bin/grep -qF "ENV: sep2=28443 admin=29999 user=2000:2000 certs=$work/certs mode=ro" "$DOCKER_LOG"
}

@test "up: missing env file is refused before docker is called" {
  BRIDGE_ENV_FILE="$work/absent" run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"env file not found"* ]]
  [ ! -e "$DOCKER_LOG" ]
}

@test "up: placeholder admin key from the example file is refused, value not echoed" {
  printf 'SEP2_ADMIN_UI_KEY=\n' >"$work/env"
  run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"SEP2_ADMIN_UI_KEY"* ]]
  [ ! -e "$DOCKER_LOG" ]
  printf 'SEP2_ADMIN_UI_KEY=shortkey123\n' >"$work/env"
  run_script up
  [ "$status" -ne 0 ]
  [[ "$output" != *shortkey123* ]]
  [ ! -e "$DOCKER_LOG" ]
}

@test "up: missing cert dir is refused before docker is called" {
  BRIDGE_CERT_DIR="$work/nope" run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"cert dir not found"* ]]
  [[ "$output" == *"$work/nope"* ]]
  [ ! -e "$DOCKER_LOG" ]
}

@test "up: missing platform network is refused and nothing is built or started" {
  NO_NETWORK=1 run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"platform network gridappsd-docker_default not found"* ]]
  [ "$(/usr/bin/grep -c 'ARGS: \[network\] \[inspect\] \[gridappsd-docker_default\]' "$DOCKER_LOG")" -eq 1 ]
  [ "$(/usr/bin/grep -c 'build\|compose' "$DOCKER_LOG")" -eq 0 ]
}

@test "up: a listener on the protocol port is refused and the message names the binary bridge" {
  SS_LISTEN="18443" run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"host port 18443 is already in use"* ]]
  [[ "$output" == *"binary bridge"* ]]
  [ "$(/usr/bin/grep -c 'build\|compose' "$DOCKER_LOG")" -eq 0 ]
}

@test "up: a listener on the admin port is refused" {
  SS_LISTEN="18444" run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"host port 18444 is already in use"* ]]
}

@test "up: the port check follows an overridden port" {
  SS_LISTEN="18443" BRIDGE_SEP2_PORT=28443 BRIDGE_ADMIN_PORT=28444 run_script up
  [ "$status" -eq 0 ]
  SS_LISTEN="28444" BRIDGE_SEP2_PORT=28443 BRIDGE_ADMIN_PORT=28444 run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"host port 28444 is already in use"* ]]
}

@test "up: bad port, user and cert mode values are refused" {
  BRIDGE_SEP2_PORT=http run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"BRIDGE_SEP2_PORT must be a port number"* ]]
  BRIDGE_ADMIN_PORT=70000 run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"BRIDGE_ADMIN_PORT must be a port number"* ]]
  BRIDGE_ADMIN_PORT=18443 run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"must differ"* ]]
  BRIDGE_USER=root run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"BRIDGE_USER must be uid:gid"* ]]
  BRIDGE_CERT_MODE=rwx run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"BRIDGE_CERT_MODE must be ro or rw"* ]]
  [ ! -e "$DOCKER_LOG" ]
}

@test "up: ss missing is refused rather than skipping the port check" {
  # A PATH with the docker stub and the two coreutils the script needs, no ss.
  mkdir -p "$work/only-docker"
  cp "$work/bin/docker" "$work/only-docker/docker"
  ln -s /usr/bin/dirname "$work/only-docker/dirname"
  ln -s /usr/bin/tail "$work/only-docker/tail"
  PATH="$work/only-docker" run "$BASH" "$repo/scripts/docker-bridge.sh" up
  [ "$status" -ne 0 ]
  [[ "$output" == *"ss is required"* ]]
  [ ! -e "$DOCKER_LOG" ]
}

@test "build: passes the version and tag without any preflight" {
  rm -f "$work/env"
  VERSION=v1.2.3 BRIDGE_IMAGE=example:tag run_script build
  [ "$status" -eq 0 ]
  /usr/bin/grep -qxF "ARGS: [build] [-f] [$repo/Dockerfile.bridge] [--build-arg] [VERSION=v1.2.3] [-t] [example:tag] [$repo]" "$DOCKER_LOG"
}

@test "down: stops the compose stack and nothing else" {
  run_script down
  [ "$status" -eq 0 ]
  /usr/bin/grep -qxF "ARGS: [compose] [--env-file] [$work/env] [-f] [$repo/docker-compose.bridge.yml] [down]" "$DOCKER_LOG"
  [ "$(/usr/bin/grep -c '^ARGS' "$DOCKER_LOG")" -eq 1 ]
}

@test "logs: follows the compose log" {
  run_script logs
  [ "$status" -eq 0 ]
  /usr/bin/grep -qxF "ARGS: [compose] [--env-file] [$work/env] [-f] [$repo/docker-compose.bridge.yml] [logs] [-f] [--tail] [200]" "$DOCKER_LOG"
}

@test "unknown action and no action are refused" {
  run_script frobnicate
  [ "$status" -ne 0 ]
  [[ "$output" == *"unknown action"* ]]
  run_script
  [ "$status" -ne 0 ]
  [[ "$output" == *"usage"* ]]
}

@test "compose file: loopback-only ports, platform network, no key baked in" {
  f="$repo/docker-compose.bridge.yml"
  # shellcheck disable=SC2016 # the ${...} is compose syntax matched literally, not a shell expansion
  /usr/bin/grep -qF '"127.0.0.1:${BRIDGE_SEP2_PORT:?use make docker-up}:18443"' "$f"
  # shellcheck disable=SC2016 # same: literal compose syntax
  /usr/bin/grep -qF '"127.0.0.1:${BRIDGE_ADMIN_PORT:?use make docker-up}:18444"' "$f"
  [ "$(/usr/bin/grep -cE '^\s+- "?(0\.0\.0\.0:)?[0-9]+:' "$f")" -eq 0 ]
  /usr/bin/grep -qF 'name: gridappsd-docker_default' "$f"
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qF 'SEP2_STOMP_ADDR: ${SEP2_STOMP_ADDR:-gridappsd:61613}' "$f"
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qF 'SEP2_ADMIN_UI_KEY: ${SEP2_ADMIN_UI_KEY:?' "$f"
}

@test "dockerignore: allowlist, and every deny pattern matches at any depth" {
  f="$repo/.dockerignore"
  first=$(/usr/bin/grep -vE '^(#|$)' "$f" | head -n 1)
  [ "$first" = "*" ]
  [ "$(/usr/bin/grep -vE '^(#|$|\*$|!)' "$f" | /usr/bin/grep -vc '^\*\*/')" -eq 0 ]
  /usr/bin/grep -qxF '**/*.pem' "$f"
  /usr/bin/grep -qxF '**/.env.*' "$f"
  [ "$(/usr/bin/grep -c '^COPY \. ' "$repo/Dockerfile.bridge")" -eq 0 ]
}

@test "compose: admin UI binds its own network's address, never 0.0.0.0, and stays off the platform network" {
  f="$repo/docker-compose.bridge.yml"
  [ "$(/usr/bin/grep -c '0\.0\.0\.0:18444' "$f")" -eq 0 ]
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qF 'SEP2_ADMIN_UI_ADDR: ${BRIDGE_ADMIN_IP:?use make docker-up}:18444' "$f"
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qF 'ipv4_address: ${BRIDGE_ADMIN_IP:?use make docker-up}' "$f"
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qF 'subnet: ${BRIDGE_ADMIN_SUBNET:?use make docker-up}' "$f"
  /usr/bin/grep -qE '^\s+gw_priority: [1-9]' "$f"
  [ "$(/usr/bin/grep -c 'name: gridappsd-docker_default' "$f")" -eq 1 ]
}

@test "compose: read-only root, tmpfs /tmp, no capabilities, no-new-privileges, cert mount stays writable by variable" {
  f="$repo/docker-compose.bridge.yml"
  /usr/bin/grep -qx '    read_only: true' "$f"
  /usr/bin/grep -qF 'tmpfs: ["/tmp:mode=1777,size=16m"]' "$f"
  /usr/bin/grep -qx '    cap_drop: \[ALL\]' "$f"
  /usr/bin/grep -qF 'no-new-privileges:true' "$f"
  # shellcheck disable=SC2016 # literal compose syntax
  /usr/bin/grep -qF ':/etc/sep2/certs:${BRIDGE_CERT_MODE:?' "$f"
}

@test "admin ip and subnet: bad values are refused, overrides reach compose" {
  BRIDGE_ADMIN_IP=nope run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"BRIDGE_ADMIN_IP must be an IPv4 address"* ]]
  BRIDGE_ADMIN_SUBNET=10.1.1.0 run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"BRIDGE_ADMIN_SUBNET must be an IPv4 CIDR"* ]]
  [ ! -e "$DOCKER_LOG" ]
  BRIDGE_ADMIN_IP=10.9.9.2 BRIDGE_ADMIN_SUBNET=10.9.9.0/24 run_script up
  [ "$status" -eq 0 ]
  /usr/bin/grep -qF 'ip=10.9.9.2 subnet=10.9.9.0/24' "$DOCKER_LOG"
}

@test "a caller's COMPOSE_FILE does not redirect the script" {
  COMPOSE_FILE=/some/other/stack.yml run_script down
  [ "$status" -eq 0 ]
  /usr/bin/grep -qxF "ARGS: [compose] [--env-file] [$work/env] [-f] [$repo/docker-compose.bridge.yml] [down]" "$DOCKER_LOG"
  BRIDGE_COMPOSE_FILE=/mine.yml run_script down
  /usr/bin/grep -qF "[-f] [/mine.yml] [down]" "$DOCKER_LOG"
}

@test "env file: an inline comment after a value is refused, naming the variable, not echoing the value" {
  printf 'SEP2_ADMIN_UI_KEY=abcd # 0123456789abcdef\n' >"$work/env"
  run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"SEP2_ADMIN_UI_KEY"* ]]
  [[ "$output" == *"inline comment"* ]]
  [[ "$output" != *abcd* ]]
  [ ! -e "$DOCKER_LOG" ]
  printf 'SEP2_ADMIN_UI_KEY="0123456789abcdef-key" # note\n' >"$work/env"
  run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"inline comment"* ]]
  printf 'SEP2_ADMIN_UI_KEY=0123456789abcdef-key\nBRIDGE_USER=2000:2000 # mine\n' >"$work/env"
  run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"BRIDGE_USER"* ]]
}

@test "env file: a # inside a value without a preceding space is kept, and a full-line comment is ignored" {
  printf '# a comment SEP2_ADMIN_UI_KEY=zzz\nSEP2_ADMIN_UI_KEY=0123456789abc#ef-key\n' >"$work/env"
  run_script up
  [ "$status" -eq 0 ]
}

@test "down and logs: a missing env file is a named error, docker not called" {
  for a in down logs; do
    BRIDGE_ENV_FILE="$work/absent" run_script "$a"
    [ "$status" -ne 0 ]
    [[ "$output" == *"env file not found"* ]]
    [ ! -e "$DOCKER_LOG" ]
  done
}

@test "up: the admin key length boundary is 16 characters" {
  printf 'SEP2_ADMIN_UI_KEY=%s\n' 012345678901234 >"$work/env"
  run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"shorter than 16"* ]]
  printf 'SEP2_ADMIN_UI_KEY=%s\n' 0123456789012345 >"$work/env"
  run_script up
  [ "$status" -eq 0 ]
}

@test "up: a missing broker password is refused before docker is called; the env file can supply it" {
  unset SEP2_STOMP_PASSWORD
  run_script up
  [ "$status" -ne 0 ]
  [[ "$output" == *"SEP2_STOMP_PASSWORD"* ]]
  [ ! -e "$DOCKER_LOG" ]
  printf 'SEP2_STOMP_PASSWORD=from-the-file\n' >>"$work/env"
  run_script up
  [ "$status" -eq 0 ]
}

# default_env_run copies the script and compose file to a scratch tree whose
# .env sits where the script looks when BRIDGE_ENV_FILE is unset.
default_env_run() {
  mkdir -p "$work/r/scripts"
  cp "$repo/scripts/docker-bridge.sh" "$work/r/scripts/"
  cp "$repo/docker-compose.bridge.yml" "$work/r/"
  printf 'SEP2_ADMIN_UI_KEY=%s\nSEP2_STOMP_PASSWORD=pw\n' "$goodkey" >"$work/r/.env"
  printf 'SEP2_ADMIN_UI_KEY=stale\n' >"$work/r/.env.bridge"
  unset BRIDGE_ENV_FILE
  run "$work/r/scripts/docker-bridge.sh" "$1"
}

@test "env file: up, down and logs default to .env beside the script and never read .env.bridge" {
  for a in up down logs; do
    rm -f "$DOCKER_LOG"
    default_env_run "$a"
    [ "$status" -eq 0 ]
    /usr/bin/grep -qF "ARGS: [compose] [--env-file] [$work/r/.env] [-f] [$work/r/docker-compose.bridge.yml] [$a]" "$DOCKER_LOG"
    [ "$(/usr/bin/grep -c 'env.bridge' "$DOCKER_LOG")" -eq 0 ]
  done
}

@test "env file: a missing .env is named in the error and points at .env.example" {
  mkdir -p "$work/r/scripts"
  cp "$repo/scripts/docker-bridge.sh" "$work/r/scripts/"
  unset BRIDGE_ENV_FILE
  run "$work/r/scripts/docker-bridge.sh" up
  [ "$status" -ne 0 ]
  [[ "$output" == *"$work/r/.env"* ]]
  [[ "$output" == *".env.example"* ]]
  [ ! -e "$DOCKER_LOG" ]
}

@test "env file: a missing .env names the old .env.bridge as a rename when it exists, for up, down and logs" {
  mkdir -p "$work/r/scripts"
  cp "$repo/scripts/docker-bridge.sh" "$work/r/scripts/"
  unset BRIDGE_ENV_FILE
  for a in up down logs; do
    run "$work/r/scripts/docker-bridge.sh" "$a"
    [ "$status" -ne 0 ]
    [[ "$output" != *"rename"* ]]
  done
  printf 'SEP2_ADMIN_UI_KEY=oldkeyoldkeyoldkey\n' >"$work/r/.env.bridge"
  for a in up down logs; do
    run "$work/r/scripts/docker-bridge.sh" "$a"
    [ "$status" -ne 0 ]
    [[ "$output" == *"rename $work/r/.env.bridge to $work/r/.env"* ]]
    [[ "$output" != *oldkeyoldkeyoldkey* ]]
  done
  [ ! -e "$DOCKER_LOG" ]
}
