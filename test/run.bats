#!/usr/bin/env bats
# Tests for `make run` and scripts/run-bridge.sh. The real bridge is never
# started: BRIDGE points at a stub that records its argv and environment,
# and `make -o build` skips the build so the stub is not overwritten.

setup() {
  repo="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
  work="$BATS_TEST_TMPDIR"
  stub="$work/stub-bridge"
  cat >"$stub" <<'STUB'
#!/usr/bin/env bash
{
  printf 'ARGS:'
  for a in "$@"; do printf ' [%s]' "$a"; done
  printf '\n'
  printf 'ARGV:%s\n' "$*"
  printf 'KEY=%s\n' "${SEP2_ADMIN_UI_KEY-unset}"
  printf 'KEYLEN=%s\n' "${#SEP2_ADMIN_UI_KEY}"
  printf 'SIM=%s\n' "${SEP2_SIMULATION_ID-unset}"
} >"$STUB_OUT"
STUB
  chmod +x "$stub"
  export STUB_OUT="$work/out"
  unset SEP2_ADMIN_UI_KEY SEP2_SIMULATION_ID ADMIN_UI_KEY_FILE
  goodkey="0123456789abcdef-key"
}

mk() {
  make -C "$repo" -o build run BRIDGE="$stub" "$@"
}

@test "defaults: addresses, cert dir, plaintext, key in env only" {
  SEP2_ADMIN_UI_KEY="$goodkey" run mk
  [ "$status" -eq 0 ]
  /usr/bin/grep -qF 'ARGS: [-sep2-server-addr] [127.0.0.1:18443] [-admin-ui-addr] [127.0.0.1:18444] [-sep2-server-cert-dir] [./sep2-certs] [-stomp-allow-plaintext]' "$STUB_OUT"
  /usr/bin/grep -qx "KEY=$goodkey" "$STUB_OUT"
  [ "$(/usr/bin/grep -c 'feeder-mrid\|registration-pin' "$STUB_OUT")" -eq 0 ]
}

@test "overrides reach the binary" {
  SEP2_ADMIN_UI_KEY="$goodkey" run mk SEP2_SERVER_ADDR=127.0.0.1:1 ADMIN_UI_ADDR=127.0.0.1:2 \
    SEP2_SERVER_CERT_DIR=/some/dir FEEDER_MRID=_ABC REGISTRATION_PIN=123455 STOMP_ALLOW_PLAINTEXT=false
  [ "$status" -eq 0 ]
  /usr/bin/grep -qF 'ARGS: [-sep2-server-addr] [127.0.0.1:1] [-admin-ui-addr] [127.0.0.1:2] [-sep2-server-cert-dir] [/some/dir] [-feeder-mrid] [_ABC] [-sep2-registration-pin] [123455]' "$STUB_OUT"
  [ "$(/usr/bin/grep -c 'stomp-allow-plaintext' "$STUB_OUT")" -eq 0 ]
}

@test "key file is read and its trailing newline dropped" {
  printf '%s\n' "$goodkey" >"$work/keyfile"
  run mk ADMIN_UI_KEY_FILE="$work/keyfile"
  [ "$status" -eq 0 ]
  /usr/bin/grep -qx "KEY=$goodkey" "$STUB_OUT"
  /usr/bin/grep -qx "KEYLEN=${#goodkey}" "$STUB_OUT"
}

@test "SEP2_SIMULATION_ID passes through only when set" {
  SEP2_ADMIN_UI_KEY="$goodkey" run mk
  /usr/bin/grep -qx 'SIM=unset' "$STUB_OUT"
  SEP2_SIMULATION_ID=12345 SEP2_ADMIN_UI_KEY="$goodkey" run mk
  /usr/bin/grep -qx 'SIM=12345' "$STUB_OUT"
}

@test "missing key is refused and the binary does not start" {
  run mk
  [ "$status" -ne 0 ]
  [[ "$output" == *"no admin key"* ]]
  [ ! -e "$STUB_OUT" ]
}

@test "short key is refused, value not echoed" {
  SEP2_ADMIN_UI_KEY=shortkey123 run mk
  [ "$status" -ne 0 ]
  [[ "$output" == *"shorter than 16"* ]]
  [[ "$output" != *shortkey123* ]]
  [ ! -e "$STUB_OUT" ]
}

@test "unreadable key file is refused" {
  run mk ADMIN_UI_KEY_FILE="$work/nope"
  [ "$status" -ne 0 ]
  [[ "$output" == *"not a readable file"* ]]
  [ ! -e "$STUB_OUT" ]
}

@test "bad STOMP_ALLOW_PLAINTEXT is refused" {
  SEP2_ADMIN_UI_KEY="$goodkey" run mk STOMP_ALLOW_PLAINTEXT=maybe
  [ "$status" -ne 0 ]
  [[ "$output" == *"must be true or false"* ]]
  [ ! -e "$STUB_OUT" ]
}

@test "key value never appears in make output" {
  SEP2_ADMIN_UI_KEY="$goodkey" run mk
  [[ "$output" != *"$goodkey"* ]]
  printf '%s\n' "$goodkey" >"$work/keyfile"
  run mk ADMIN_UI_KEY_FILE="$work/keyfile"
  [[ "$output" != *"$goodkey"* ]]
}

@test "run depends on build and build writes BRIDGE" {
  run make -C "$repo" -n run BRIDGE="$stub"
  [[ "$output" == *"-o $stub ./cmd/bridge"* ]]
}

@test "admin key never appears in the binary's argv" {
  SEP2_ADMIN_UI_KEY="$goodkey" run mk
  [ "$status" -eq 0 ]
  /usr/bin/grep -q '^ARGV:' "$STUB_OUT"
  [ "$(/usr/bin/grep '^ARGV:' "$STUB_OUT" | /usr/bin/grep -cF "$goodkey")" -eq 0 ]
  printf '%s\n' "$goodkey" >"$work/keyfile"
  run mk ADMIN_UI_KEY_FILE="$work/keyfile"
  [ "$(/usr/bin/grep '^ARGV:' "$STUB_OUT" | /usr/bin/grep -cF "$goodkey")" -eq 0 ]
}

@test "key length boundary: 15 refused, 16 accepted" {
  k15=123456789012345
  k16=1234567890123456
  SEP2_ADMIN_UI_KEY="$k15" run mk
  [ "$status" -ne 0 ]
  [ ! -e "$STUB_OUT" ]
  SEP2_ADMIN_UI_KEY="$k16" run mk
  [ "$status" -eq 0 ]
  /usr/bin/grep -qx "KEYLEN=16" "$STUB_OUT"
}

@test "bare make prints help and does not build" {
  mkdir "$work/bin"
  printf '#!/bin/sh\ntouch "%s/go-called"\n' "$work" >"$work/bin/go"
  chmod +x "$work/bin/go"
  PATH="$work/bin:$PATH" run make -C "$repo"
  [ "$status" -eq 0 ]
  [[ "$output" == *"Show this help"* ]]
  [ ! -e "$work/go-called" ]
}

@test "every target with a ## description appears in help" {
  targets=$(/usr/bin/grep -E '^[a-zA-Z0-9_-]+:.*## ' "$repo/Makefile" | cut -d: -f1)
  [ -n "$targets" ]
  run make -C "$repo" help
  [ "$status" -eq 0 ]
  n=0
  for t in $targets; do
    n=$((n + 1))
    [[ "$output" == *"  $t "* ]] || { echo "missing from help: $t"; return 1; }
  done
  [ "$n" -ge 11 ]
}
