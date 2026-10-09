#!/usr/bin/env bats

setup() {
  repo="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
  work="$BATS_TEST_TMPDIR"
  mkdir -p "$work/home"
  export HOME="$work/home"
  export BRIDGE_ENV_FILE="$work/configured.env"
  export BRIDGE_CERT_DIR="$work/certs"
  export BRIDGE_ADMIN_BIND_IP=172.20.10.5
  export BRIDGE_USER=1234:5678
  export GRIDAPPSD_PASSWORD=broker-secret
  export SEP2_STOMP_PASSWORD=
}

@test "configure creates a private env file with generated key and Hyper-V bind address" {
  run "$repo/scripts/configure-docker.sh"
  [ "$status" -eq 0 ]
  [ "$(stat -c '%a' "$BRIDGE_ENV_FILE")" = 600 ]
  [ -d "$BRIDGE_CERT_DIR" ]
  [ "$(stat -c '%a' "$BRIDGE_CERT_DIR")" = 700 ]
  /usr/bin/grep -q "^GRIDAPPSD_PASSWORD='broker-secret'$" "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q '^BRIDGE_ADMIN_BIND_IP=172.20.10.5$' "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q '^BRIDGE_USER=1234:5678$' "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q "^BRIDGE_CERT_DIR='$BRIDGE_CERT_DIR'$" "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q '^SEP2_REGISTRATION_PIN=123455$' "$BRIDGE_ENV_FILE"
  key="$(sed -n 's/^SEP2_ADMIN_UI_KEY=//p' "$BRIDGE_ENV_FILE")"
  [[ "$key" =~ ^[a-f0-9]{48}$ ]]
  [[ "$output" != *broker-secret* ]]
}

@test "configure detects the Hyper-V guest IP from the default route" {
  mkdir -p "$work/bin"
  printf '#!/bin/sh\nprintf "microsoft\\n"\n' >"$work/bin/systemd-detect-virt"
  printf '#!/bin/sh\nprintf "1.1.1.1 via 172.20.10.1 dev eth0 src 172.20.10.5 uid 1000\\n"\n' >"$work/bin/ip"
  chmod +x "$work/bin/systemd-detect-virt" "$work/bin/ip"
  unset BRIDGE_ADMIN_BIND_IP
  PATH="$work/bin:$PATH" run "$repo/scripts/configure-docker.sh"
  [ "$status" -eq 0 ]
  /usr/bin/grep -q '^BRIDGE_ADMIN_BIND_IP=172.20.10.5$' "$BRIDGE_ENV_FILE"
}

@test "configure detects the VirtualBox guest IP from the default route" {
  mkdir -p "$work/bin"
  printf '#!/bin/sh\nprintf "oracle\\n"\n' >"$work/bin/systemd-detect-virt"
  printf '#!/bin/sh\nprintf "1.1.1.1 via 10.0.2.2 dev enp0s3 src 10.0.2.15 uid 1000\\n"\n' >"$work/bin/ip"
  chmod +x "$work/bin/systemd-detect-virt" "$work/bin/ip"
  unset BRIDGE_ADMIN_BIND_IP
  PATH="$work/bin:$PATH" run "$repo/scripts/configure-docker.sh"
  [ "$status" -eq 0 ]
  /usr/bin/grep -q '^BRIDGE_ADMIN_BIND_IP=10.0.2.15$' "$BRIDGE_ENV_FILE"
}

@test "configure keeps the admin bind loopback on non-Hyper-V hosts" {
  mkdir -p "$work/bin"
  printf '#!/bin/sh\nprintf "none\\n"\n' >"$work/bin/systemd-detect-virt"
  chmod +x "$work/bin/systemd-detect-virt"
  unset BRIDGE_ADMIN_BIND_IP
  PATH="$work/bin:$PATH" run "$repo/scripts/configure-docker.sh"
  [ "$status" -eq 0 ]
  /usr/bin/grep -q '^BRIDGE_ADMIN_BIND_IP=127.0.0.1$' "$BRIDGE_ENV_FILE"
}

@test "configure refuses to overwrite an existing env file" {
  printf 'keep-this-file\n' >"$BRIDGE_ENV_FILE"
  run "$repo/scripts/configure-docker.sh"
  [ "$status" -ne 0 ]
  [[ "$output" == *"already exists"* ]]
  [ "$(cat "$BRIDGE_ENV_FILE")" = keep-this-file ]
}