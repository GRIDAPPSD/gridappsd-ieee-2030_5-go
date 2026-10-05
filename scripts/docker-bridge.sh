#!/usr/bin/env bash
# Drives the bridge compose stack for `make docker-build|pull|up|down|logs`.
# Usage: docker-bridge.sh build|pull|up|down|logs
# Settings come from the shell environment first, then the env file, then the
# defaults below; the resolved values are exported for docker-compose.bridge.yml.
set -euo pipefail

die() {
  echo "docker-bridge: $*" >&2
  exit 1
}

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# BRIDGE_-prefixed so a COMPOSE_FILE set for another stack cannot redirect this script.
env_file="${BRIDGE_ENV_FILE:-$repo/.env}"
compose_file="${BRIDGE_COMPOSE_FILE:-$repo/docker-compose.bridge.yml}"
platform_network="gridappsd-docker_default"
# The only tag a local build ever carries, so it can never shadow the Hub name.
local_image="gridappsd-ieee-2030_5-go:dev"

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

# published_image names the Docker Hub image at BRIDGE_IMAGE_TAG (default latest).
published_image() {
  printf 'gridappsd/gridappsd-ieee-2030_5:%s' "$(setting BRIDGE_IMAGE_TAG latest)"
}

# env_missing says why the env file is unusable, and when the old name is still
# there, that it only needs renaming.
env_missing() {
  local hint="${1:-}"
  if [ "${env_file##*/}" = ".env" ] && [ -e "$env_file.bridge" ]; then
    hint="rename $env_file.bridge to $env_file"
  fi
  die "env file not found or unreadable: $env_file${hint:+ ($hint)}"
}

check_port() {
  local label="$1" port="$2"
  [[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] ||
    die "$label must be a port number 1-65535, got: $port"
}

# check_ipv4 requires a dotted quad with every octet 0-255 and no leading zeros,
# so 999.1.1.1 and 010.0.0.1 are refused rather than handed to compose.
check_ipv4() {
  local label="$1" ip="$2" octet
  local -a octets
  [[ "$ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "$label must be an IPv4 address, got: $ip"
  IFS=. read -r -a octets <<<"$ip"
  for octet in "${octets[@]}"; do
    { [ "$octet" -le 255 ] && { [ "${#octet}" -eq 1 ] || [ "${octet:0:1}" != 0 ]; }; } ||
      die "$label must be an IPv4 address, got: $ip"
  done
}

# cross_check_cert_mode compares the device cert mode the container will run
# with against the mount mode: dev-mint writes device certificates, so a
# read-only mount fails at the first mint, late and with a file error.
cross_check_cert_mode() {
  local device_mode
  device_mode=$(setting SEP2_DEVICE_CERT_MODE dev-mint)
  if [ "$device_mode" = dev-mint ] && [ "$BRIDGE_CERT_MODE" = ro ]; then
    die "BRIDGE_CERT_MODE=ro cannot be used with SEP2_DEVICE_CERT_MODE=dev-mint: dev-mint writes certificates into the mount; set SEP2_DEVICE_CERT_MODE=preprovisioned, or BRIDGE_CERT_MODE=rw"
  fi
  if [ "$device_mode" = preprovisioned ] && [ "$BRIDGE_CERT_MODE" = rw ]; then
    echo "docker-bridge: warning: SEP2_DEVICE_CERT_MODE=preprovisioned never writes to the cert dir; BRIDGE_CERT_MODE=ro is the safer mount" >&2
  fi
}

# resolve reads and validates every launcher setting, then exports them.
resolve() {
  BRIDGE_SEP2_PORT=$(setting BRIDGE_SEP2_PORT 18443)
  BRIDGE_ADMIN_PORT=$(setting BRIDGE_ADMIN_PORT 18444)
  BRIDGE_SEP2_BIND_IP=$(setting BRIDGE_SEP2_BIND_IP 127.0.0.1)
  BRIDGE_ADMIN_BIND_IP=$(setting BRIDGE_ADMIN_BIND_IP 127.0.0.1)
  BRIDGE_USER=$(setting BRIDGE_USER 1000:1000)
  BRIDGE_CERT_DIR=$(setting BRIDGE_CERT_DIR "${HOME:?HOME is not set}/.config/gridappsd/2030.5server/sep2-certs")
  BRIDGE_CERT_MODE=$(setting BRIDGE_CERT_MODE rw)
  BRIDGE_ADMIN_IP=$(setting BRIDGE_ADMIN_IP 10.213.168.2)
  BRIDGE_ADMIN_SUBNET=$(setting BRIDGE_ADMIN_SUBNET 10.213.168.0/24)
  # BRIDGE_USE_PUBLISHED=1 (the default) runs the Hub image, pulled at start;
  # 0 builds the local image instead. An explicit BRIDGE_IMAGE picks the image
  # pulled and run when the switch is 1; a local build is always $local_image.
  use_published=$(setting BRIDGE_USE_PUBLISHED 1)
  case "$use_published" in 0 | 1) ;; *) die "BRIDGE_USE_PUBLISHED must be 0 or 1, got: $use_published" ;; esac
  if [ "$use_published" = 1 ]; then
    BRIDGE_IMAGE="${BRIDGE_IMAGE:-$(published_image)}"
  else
    BRIDGE_IMAGE="$local_image"
  fi
  check_port BRIDGE_SEP2_PORT "$BRIDGE_SEP2_PORT"
  check_port BRIDGE_ADMIN_PORT "$BRIDGE_ADMIN_PORT"
  [ "$BRIDGE_SEP2_PORT" != "$BRIDGE_ADMIN_PORT" ] || die "BRIDGE_SEP2_PORT and BRIDGE_ADMIN_PORT must differ"
  check_ipv4 BRIDGE_SEP2_BIND_IP "$BRIDGE_SEP2_BIND_IP"
  check_ipv4 BRIDGE_ADMIN_BIND_IP "$BRIDGE_ADMIN_BIND_IP"
  [[ "$BRIDGE_USER" =~ ^[0-9]+:[0-9]+$ ]] || die "BRIDGE_USER must be uid:gid, got: $BRIDGE_USER"
  [[ "$BRIDGE_ADMIN_IP" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "BRIDGE_ADMIN_IP must be an IPv4 address, got: $BRIDGE_ADMIN_IP"
  [[ "$BRIDGE_ADMIN_SUBNET" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$ ]] || die "BRIDGE_ADMIN_SUBNET must be an IPv4 CIDR, got: $BRIDGE_ADMIN_SUBNET"
  case "$BRIDGE_CERT_MODE" in ro | rw) ;; *) die "BRIDGE_CERT_MODE must be ro or rw, got: $BRIDGE_CERT_MODE" ;; esac
  cross_check_cert_mode
  export BRIDGE_SEP2_BIND_IP BRIDGE_ADMIN_BIND_IP BRIDGE_SEP2_PORT BRIDGE_ADMIN_PORT BRIDGE_USER BRIDGE_CERT_DIR BRIDGE_CERT_MODE BRIDGE_IMAGE BRIDGE_ADMIN_IP BRIDGE_ADMIN_SUBNET
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
  [ -r "$env_file" ] || env_missing "copy .env.example to .env and fill it in"
  local key
  key=$(setting SEP2_ADMIN_UI_KEY "")
  [ "${#key}" -ge 16 ] || die "SEP2_ADMIN_UI_KEY in $env_file is missing or shorter than 16 characters"
  # Compose passes an empty password through and the bridge would then fall back
  # to its built-in default, so refuse here, before the image is built.
  [ -n "$(setting SEP2_STOMP_PASSWORD "")" ] || [ -n "$(setting GRIDAPPSD_PASSWORD "")" ] ||
    die "broker password missing: set GRIDAPPSD_PASSWORD (or SEP2_STOMP_PASSWORD) in $env_file"
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

# extra_ca_file prints the validated BRIDGE_EXTRA_CA_FILE path, or nothing when
# unset. It never prints file contents: the bundle may sit beside private material.
extra_ca_file() {
  local path
  path=$(setting BRIDGE_EXTRA_CA_FILE "")
  [ -n "$path" ] || return 0
  [ -f "$path" ] || die "BRIDGE_EXTRA_CA_FILE is not a file: $path"
  [ -r "$path" ] || die "BRIDGE_EXTRA_CA_FILE is not readable: $path"
  /usr/bin/grep -q -e '-----BEGIN CERTIFICATE-----' "$path" ||
    die "BRIDGE_EXTRA_CA_FILE has no PEM certificate (no BEGIN CERTIFICATE line): $path"
  printf '%s' "$path"
}

build() {
  local version="${VERSION:-dev}" ca
  local -a secret=()
  ca=$(extra_ca_file)
  if [ -n "$ca" ]; then
    secret=(--secret "id=extra_ca,src=$ca")
  fi
  docker build -f "$repo/Dockerfile.bridge" --build-arg "VERSION=$version" "${secret[@]+"${secret[@]}"}" -t "$local_image" "$repo"
}

# pull fetches the published image only; it needs docker but no env file.
pull() {
  command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
  docker pull "$(published_image)"
}

main() {
  local action="${1:-}"
  [ -n "$action" ] || die "usage: docker-bridge.sh build|pull|up|down|logs"
  case "$action" in
    build | pull | up | down | logs) ;;
    *) die "unknown action: $action (use build, pull, up, down or logs)" ;;
  esac
  if [ "$action" = pull ]; then
    pull
    return
  fi
  case "$action" in
    down | logs) [ -r "$env_file" ] || env_missing ;;
  esac
  resolve
  case "$action" in
    build) build ;;
    up)
      preflight
      if [ "$use_published" = 1 ]; then
        # Pull first so the run uses the current image, not a stale local copy.
        docker pull "$BRIDGE_IMAGE" ||
          die "could not pull $BRIDGE_IMAGE; set BRIDGE_USE_PUBLISHED=0 to build the image locally"
      else
        build
      fi
      compose up -d --no-build
      ;;
    down) compose down ;;
    logs) compose logs -f --tail 200 ;;
  esac
}

main "$@"
