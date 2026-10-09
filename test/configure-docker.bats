#!/usr/bin/env bats

feeder_fixture='{"results":{"bindings":[{"mrid":{"value":"00000000-0000-0000-0000-000000000001"},"name":{"value":"test-feeder-a"}},{"mrid":{"value":"00000000-0000-0000-0000-000000000002"},"name":{"value":"test-feeder-b"}}]}}'

setup() {
  repo="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
  work="$BATS_TEST_TMPDIR"
  mkdir -p "$work/home"
  export HOME="$work/home"
  export BRIDGE_ENV_FILE="$work/configured.env"
  export BRIDGE_CERT_DIR="$work/certs"
  export BRIDGE_ADMIN_BIND_IP=172.20.10.5
  export BRIDGE_CONFIGURE_NONINTERACTIVE=1
  export BRIDGE_USER=1234:5678
  export GRIDAPPSD_PASSWORD=broker-secret
  export SEP2_STOMP_PASSWORD=
}

@test "configure lists Blazegraph feeders and writes the selected mRID" {
  mkdir -p "$work/bin"
  cat >"$work/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$FEEDER_FIXTURE"
STUB
  chmod +x "$work/bin/curl"
  export PATH="$work/bin:$PATH"
  export FEEDER_FIXTURE="$feeder_fixture"
  unset BRIDGE_CONFIGURE_NONINTERACTIVE
  export BRIDGE_SEP2_BIND_IP=0.0.0.0
  export BLAZEGRAPH_SPARQL_URL=http://blazegraph.test/sparql
  run bash -c 'printf "2\\n" | script -q -e -c "$1" /dev/null' _ "$repo/scripts/configure-docker.sh"
  [ "$status" -eq 0 ]
  [[ "$output" == *"test-feeder-a"* && "$output" == *"test-feeder-b"* ]]
  selected_mrid="$(sed -n 's/^SEP2_FEEDER_MRID=//p' "$BRIDGE_ENV_FILE")"
  [ -n "$selected_mrid" ]
  jq -e --arg selected "$selected_mrid" '[.results.bindings[].mrid.value] | index($selected) != null' <<<"$feeder_fixture" >/dev/null
}

@test "configure creates a private env file with generated key and Hyper-V bind address" {
  run "$repo/scripts/configure-docker.sh"
  [ "$status" -eq 0 ]
  [ "$(stat -c '%a' "$BRIDGE_ENV_FILE")" = 600 ]
  [ -d "$BRIDGE_CERT_DIR" ]
  [ "$(stat -c '%a' "$BRIDGE_CERT_DIR")" = 700 ]
  /usr/bin/grep -q "^GRIDAPPSD_PASSWORD='broker-secret'$" "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q '^GRIDAPPSD_USER=system$' "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q '^BRIDGE_SEP2_BIND_IP=0.0.0.0$' "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q '^BRIDGE_ADMIN_BIND_IP=172.20.10.5$' "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q '^BRIDGE_USER=1234:5678$' "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q "^BRIDGE_CERT_DIR='$BRIDGE_CERT_DIR'$" "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q '^SEP2_REGISTRATION_PIN=123455$' "$BRIDGE_ENV_FILE"
  key="$(sed -n 's/^SEP2_ADMIN_UI_KEY=//p' "$BRIDGE_ENV_FILE")"
  [[ "$key" =~ ^[a-f0-9]{48}$ ]]
  [[ "$output" != *broker-secret* ]]
}

@test "configure lets SEP2 and admin host binds be overridden independently" {
  export BRIDGE_SEP2_BIND_IP=192.168.177.10
  export BRIDGE_ADMIN_BIND_IP=10.0.2.15
  run "$repo/scripts/configure-docker.sh"
  [ "$status" -eq 0 ]
  /usr/bin/grep -q '^BRIDGE_SEP2_BIND_IP=192.168.177.10$' "$BRIDGE_ENV_FILE"
  /usr/bin/grep -q '^BRIDGE_ADMIN_BIND_IP=10.0.2.15$' "$BRIDGE_ENV_FILE"
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

@test "configure expands a tilde in the certificate directory" {
  export BRIDGE_CERT_DIR='~/tls'
  run "$repo/scripts/configure-docker.sh"
  [ "$status" -eq 0 ]
  /usr/bin/grep -q "^BRIDGE_CERT_DIR='$HOME/tls'$" "$BRIDGE_ENV_FILE"
  [ -d "$HOME/tls" ]
  [ "$(stat -c '%a' "$HOME/tls")" = 700 ]
}

@test "configure refuses to overwrite an existing env file" {
  printf 'keep-this-file\n' >"$BRIDGE_ENV_FILE"
  run "$repo/scripts/configure-docker.sh"
  [ "$status" -ne 0 ]
  [[ "$output" == *"already exists"* ]]
  [ "$(cat "$BRIDGE_ENV_FILE")" = keep-this-file ]
}