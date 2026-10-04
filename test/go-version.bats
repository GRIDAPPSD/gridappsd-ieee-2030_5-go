#!/usr/bin/env bats
# Tests for scripts/check-go-version.sh and the check-go prerequisite of the
# compiling make targets. `go` is always a stub that prints a chosen
# `go version` line; the real toolchain is never reached.

setup() {
  repo="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
  work="$BATS_TEST_TMPDIR"
  stubs="$work/stubs"
  mkdir -p "$stubs"
  gomod="$work/go.mod"
  printf 'module example.test/m\n\ngo 1.26.3\n' >"$gomod"
  script="$repo/scripts/check-go-version.sh"
}

# stub_go installs a go that prints $1 for `go version` and exits $2.
stub_go() {
  printf '#!/bin/sh\necho "%s"\nexit %s\n' "$1" "${2:-0}" >"$stubs/go"
  chmod +x "$stubs/go"
}

check() {
  PATH="$stubs:$PATH" run "$script" "$gomod"
}

@test "older Go: names both versions and exits non-zero" {
  stub_go "go version go1.20.4 linux/amd64"
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"installed Go 1.20.4 is older than the 1.26.3"* ]]
}

@test "same minor, older patch is refused" {
  stub_go "go version go1.26.2 linux/amd64"
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"installed Go 1.26.2 is older than the 1.26.3"* ]]
}

@test "numeric compare: 1.9 is older than 1.26" {
  stub_go "go version go1.9 linux/amd64"
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"installed Go 1.9.0 is older"* ]]
}

@test "equal Go passes silently" {
  stub_go "go version go1.26.3 linux/amd64"
  check
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "newer patch, minor and major pass silently" {
  for v in go1.26.4 go1.27 go1.30.1 go2.0; do
    stub_go "go version $v linux/amd64"
    check
    [ "$status" -eq 0 ]
    [ -z "$output" ]
  done
}

@test "devel build is read by its go<major>.<minor> token" {
  stub_go "go version devel go1.27-abc123 Mon Jan 1 00:00:00 2029 +0000 linux/amd64"
  check
  [ "$status" -eq 0 ]
  stub_go "go version devel go1.20-abc123 Mon Jan 1 00:00:00 2023 +0000 linux/amd64"
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"installed Go 1.20.0 is older"* ]]
}

@test "version required comes from go.mod, not the script" {
  printf 'module example.test/m\n\ngo 1.18\n' >"$gomod"
  stub_go "go version go1.20.4 linux/amd64"
  check
  [ "$status" -eq 0 ]
  printf 'module example.test/m\n\ngo 1.40.2 // newer than the stub\n' >"$gomod"
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"the 1.40.2 that"* ]]
}

@test "go.mod line without a patch means patch 0" {
  printf 'module example.test/m\n\ngo 1.21\n' >"$gomod"
  stub_go "go version go1.21.0 linux/amd64"
  check
  [ "$status" -eq 0 ]
  stub_go "go version go1.20.9 linux/amd64"
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"the 1.21.0 that"* ]]
}

@test "go version output of an unrecognised form is refused, quoting it" {
  stub_go "go version weekly.2012-03-13 linux/amd64"
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"cannot read a Go version from 'go version weekly.2012-03-13 linux/amd64'"* ]]
  [[ "$output" == *"1.26.3"* ]]
}

@test "go version that fails is refused" {
  stub_go "go: unknown command" 2
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"'go version' failed"* ]]
}

@test "go missing from PATH is refused, naming the required version" {
  bin="$work/bin"
  mkdir -p "$bin"
  ln -s "$(command -v bash)" "$bin/bash"
  ln -s "$(command -v env)" "$bin/env"
  PATH="$bin" run "$script" "$gomod"
  [ "$status" -ne 0 ]
  [[ "$output" == *"go is not on PATH"* ]]
  [[ "$output" == *"1.26.3"* ]]
}

@test "missing go.mod and go.mod without a go line are refused" {
  stub_go "go version go1.26.3 linux/amd64"
  rm "$gomod"
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"cannot read"* ]]
  printf 'module example.test/m\n' >"$gomod"
  check
  [ "$status" -ne 0 ]
  [[ "$output" == *"no go line found"* ]]
}

# stub_go_recording installs a go that reports version $1 and appends the
# arguments of every other invocation (a compile) to $work/compiled.
stub_go_recording() {
  cat >"$stubs/go" <<STUB
#!/bin/sh
if [ "\$1" = version ]; then echo "go version $1 linux/amd64"; exit 0; fi
echo "\$*" >>"$work/compiled"
STUB
  chmod +x "$stubs/go"
}

@test "make build on an older Go stops before compiling" {
  stub_go_recording go1.20.4
  PATH="$stubs:$PATH" run make -C "$repo" build BRIDGE="$work/bridge"
  [ "$status" -ne 0 ]
  [[ "$output" == *"installed Go 1.20.4 is older than the 1.26.8"* ]]
  [ ! -e "$work/compiled" ]
}

@test "make build on a current Go compiles with no check output" {
  stub_go_recording go1.27.0
  PATH="$stubs:$PATH" run make -C "$repo" build BRIDGE="$work/bridge"
  [ "$status" -eq 0 ]
  [[ "$output" != *"check-go-version"* ]]
  [ "$(wc -l <"$work/compiled")" -eq 2 ]
}
