#!/usr/bin/env bash
# Starts the bridge binary for `make run`. The Makefile passes every
# setting as an environment variable; this script validates them and execs
# the binary. The admin key is handed over in the environment, never on the
# command line, so it does not show in ps output or in make's echoed recipe.
set -euo pipefail

die() {
  echo "run-bridge: $*" >&2
  exit 1
}

bridge="${BRIDGE:?BRIDGE is not set}"
[ -x "$bridge" ] || die "bridge binary not found or not executable: $bridge (run make build)"

key="${SEP2_ADMIN_UI_KEY:-}"
if [ -z "$key" ] && [ -n "${ADMIN_UI_KEY_FILE:-}" ]; then
  [ -r "$ADMIN_UI_KEY_FILE" ] || die "ADMIN_UI_KEY_FILE is not a readable file: $ADMIN_UI_KEY_FILE"
  # Command substitution drops the trailing newline an editor leaves behind.
  key=$(cat -- "$ADMIN_UI_KEY_FILE")
fi
if [ -z "$key" ]; then
  case "${SEP2_ADMIN_UI_INSECURE_NO_KEY:-}" in
    "" | 0 | f | F | FALSE | false | False) ;;
    *) die "no admin key: SEP2_ADMIN_UI_INSECURE_NO_KEY is set, but no-key mode is supported only through make docker-up; set SEP2_ADMIN_UI_KEY or ADMIN_UI_KEY_FILE (at least 16 characters)" ;;
  esac
  die "no admin key: set SEP2_ADMIN_UI_KEY or ADMIN_UI_KEY_FILE (at least 16 characters)"
fi
[ "${#key}" -ge 16 ] || die "admin key is shorter than 16 characters"

args=(
  -sep2-server-addr "${SEP2_SERVER_ADDR:?SEP2_SERVER_ADDR is not set}"
  -admin-ui-addr "${ADMIN_UI_ADDR:?ADMIN_UI_ADDR is not set}"
  -sep2-server-cert-dir "${SEP2_SERVER_CERT_DIR:?SEP2_SERVER_CERT_DIR is not set}"
)
# Empty FEEDER_MRID and REGISTRATION_PIN mean the binary's own behavior.
[ -z "${FEEDER_MRID:-}" ] || args+=(-feeder-mrid "$FEEDER_MRID")
[ -z "${REGISTRATION_PIN:-}" ] || args+=(-sep2-registration-pin "$REGISTRATION_PIN")
case "${STOMP_ALLOW_PLAINTEXT:-false}" in
  true) args+=(-stomp-allow-plaintext) ;;
  false) ;;
  *) die "STOMP_ALLOW_PLAINTEXT must be true or false" ;;
esac

SEP2_ADMIN_UI_KEY="$key" exec "$bridge" "${args[@]}"
