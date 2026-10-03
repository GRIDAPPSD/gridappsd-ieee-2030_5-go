#!/usr/bin/env bash
# Drives the bridge compose stack for `make docker-build|up|down|logs`.
# Usage: docker-bridge.sh build|up|down|logs
# Settings come from the shell environment first, then the env file, then the
# defaults below; the resolved values are exported for docker-compose.bridge.yml.
set -euo pipefail

die() {
  echo "docker-bridge: $*" >&2
  exit 1
}

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# BRIDGE_-prefixed so a COMPOSE_FILE set for another stack cannot redirect this script.
env_file="${BRIDGE_ENV_FILE:-$repo/.env.bridge}"
compose_file="${BRIDGE_COMPOSE_FILE:-$repo/docker-compose.bridge.yml}"
platform_network="gridappsd-docker_default"

# setting NAME DEFAULT: shell value, else the last NAME= line of the env file,
# else DEFAULT. The file is parsed, never sourced, so it cannot run code.
# Compose strips an inline "# comment" from a value and this parser does not,
# so a value with one is refused rather than read two ways.
setting() {
  local name="$1" default="$2" line value=""
  if [ -n "${!name:-}" ]; then
    printf '%s' "${!name}"
    return
  fi
  if [ -r "$env_file" ]; then
    line=$(/usr/bin/grep -E "^${name}=" "$env_file" | tail -n 1 || true) # no match is the normal unset case
    value="${line#*=}"
    case "$value" in
      \"*\") value="${value#\"}"; value="${value%\"}" ;;
      \'*\') value="${value#\'}"; value="${value%\'}" ;;
      *[[:space:]]"#"*) die "$name in $env_file has an inline comment; put comments on their own line" ;;
    esac
  fi
  printf '%s' "${value:-$default}"
}

check_port() {
  local label="$1" port="$2"
  [[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] ||
    die "$label must be a port number 1-65535, got: $port"
}

# resolve reads and validates every launcher setting, then exports them.
resolve() {
  BRIDGE_SEP2_PORT=$(setting BRIDGE_SEP2_PORT 18443)
  BRIDGE_ADMIN_PORT=$(setting BRIDGE_ADMIN_PORT 18444)
  BRIDGE_USER=$(setting BRIDGE_USER 1000:1000)
  BRIDGE_CERT_DIR=$(setting BRIDGE_CERT_DIR "${HOME:?HOME is not set}/.config/gridappsd/2030.5server/sep2-certs")
  BRIDGE_CERT_MODE=$(setting BRIDGE_CERT_MODE rw)
  BRIDGE_ADMIN_IP=$(setting BRIDGE_ADMIN_IP 10.213.168.2)
  BRIDGE_ADMIN_SUBNET=$(setting BRIDGE_ADMIN_SUBNET 10.213.168.0/24)
  BRIDGE_IMAGE="${BRIDGE_IMAGE:-gridappsd-ieee-2030_5-go:dev}"
  check_port BRIDGE_SEP2_PORT "$BRIDGE_SEP2_PORT"
  check_port BRIDGE_ADMIN_PORT "$BRIDGE_ADMIN_PORT"
  [ "$BRIDGE_SEP2_PORT" != "$BRIDGE_ADMIN_PORT" ] || die "BRIDGE_SEP2_PORT and BRIDGE_ADMIN_PORT must differ"
  [[ "$BRIDGE_USER" =~ ^[0-9]+:[0-9]+$ ]] || die "BRIDGE_USER must be uid:gid, got: $BRIDGE_USER"
  [[ "$BRIDGE_ADMIN_IP" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "BRIDGE_ADMIN_IP must be an IPv4 address, got: $BRIDGE_ADMIN_IP"
  [[ "$BRIDGE_ADMIN_SUBNET" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$ ]] || die "BRIDGE_ADMIN_SUBNET must be an IPv4 CIDR, got: $BRIDGE_ADMIN_SUBNET"
  case "$BRIDGE_CERT_MODE" in ro | rw) ;; *) die "BRIDGE_CERT_MODE must be ro or rw, got: $BRIDGE_CERT_MODE" ;; esac
  export BRIDGE_SEP2_PORT BRIDGE_ADMIN_PORT BRIDGE_USER BRIDGE_CERT_DIR BRIDGE_CERT_MODE BRIDGE_IMAGE BRIDGE_ADMIN_IP BRIDGE_ADMIN_SUBNET
}

compose() {
  docker compose --env-file "$env_file" -f "$compose_file" "$@"
}

port_in_use() {
  [ -n "$(ss -H -ltn "sport = :$1")" ]
}

preflight() {
  command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
  command -v ss >/dev/null 2>&1 || die "ss is required for the port check and is not on PATH"
  [ -r "$env_file" ] || die "env file not found or unreadable: $env_file (copy .env.bridge.example and fill it in)"
  local key
  key=$(setting SEP2_ADMIN_UI_KEY "")
  [ "${#key}" -ge 16 ] || die "SEP2_ADMIN_UI_KEY in $env_file is missing or shorter than 16 characters"
  [ -d "$BRIDGE_CERT_DIR" ] || die "cert dir not found: $BRIDGE_CERT_DIR (set BRIDGE_CERT_DIR)"
  # inspect failing means the network is absent or the docker daemon is down; both stop the start.
  docker network inspect "$platform_network" >/dev/null 2>&1 ||
    die "platform network $platform_network not found: start the GridAPPS-D platform first"
  local port
  for port in "$BRIDGE_SEP2_PORT" "$BRIDGE_ADMIN_PORT"; do
    ! port_in_use "$port" ||
      die "host port $port is already in use (the binary bridge from start.sh does this: stop it first; only one bridge may run against a broker)"
  done
}

build() {
  local version="${VERSION:-dev}"
  docker build -f "$repo/Dockerfile.bridge" --build-arg "VERSION=$version" -t "$BRIDGE_IMAGE" "$repo"
}

main() {
  local action="${1:-}"
  [ -n "$action" ] || die "usage: docker-bridge.sh build|up|down|logs"
  case "$action" in
    build | up | down | logs) ;;
    *) die "unknown action: $action (use build, up, down or logs)" ;;
  esac
  case "$action" in
    down | logs) [ -r "$env_file" ] || die "env file not found or unreadable: $env_file" ;;
  esac
  resolve
  case "$action" in
    build) build ;;
    up)
      preflight
      build
      compose up -d --no-build
      ;;
    down) compose down ;;
    logs) compose logs -f --tail 200 ;;
  esac
}

main "$@"
