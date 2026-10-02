#!/usr/bin/env bash
# Fails when the installed Go is older than the `go` line of a go.mod, so a
# build stops with a message instead of an obscure compile error such as
# `malformed module path "cmp"`. Usage: check-go-version.sh [go.mod]
# The Makefile runs it before every target that compiles. Prints nothing
# when the installed Go is new enough.
set -euo pipefail

die() {
  echo "check-go-version: $*" >&2
  exit 1
}

# Prints "major minor patch" for the first go.mod line of the form `go X.Y[.Z]`.
required_from_gomod() {
  local line
  [ -r "$1" ] || die "cannot read $1"
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ "$line" =~ ^go[[:space:]]+([0-9]+)\.([0-9]+)(\.([0-9]+))? ]]; then
      echo "${BASH_REMATCH[1]} ${BASH_REMATCH[2]} ${BASH_REMATCH[4]:-0}"
      return 0
    fi
  done <"$1"
  die "no go line found in $1"
}

main() {
  local gomod="${1:-go.mod}"
  local req req_text out have_text

  req=$(required_from_gomod "$gomod")
  read -r rmaj rmin rpat <<<"$req"
  req_text="$rmaj.$rmin.$rpat"

  command -v go >/dev/null 2>&1 || die "go is not on PATH; $gomod needs Go $req_text or newer"
  out=$(go version 2>&1) || die "'go version' failed (output: $out); $gomod needs Go $req_text or newer"

  # Release, rc, beta and devel builds all carry a go<major>.<minor>[.<patch>]
  # token ("go1.26.3", "go1.27rc1", "devel go1.27-abc123"). Anything without
  # one is a Go too old to print that form, so refuse rather than guess.
  if [[ ! "$out" =~ go([0-9]+)\.([0-9]+)(\.([0-9]+))? ]]; then
    die "cannot read a Go version from '$out'; $gomod needs Go $req_text or newer"
  fi
  local hmaj="${BASH_REMATCH[1]}" hmin="${BASH_REMATCH[2]}" hpat="${BASH_REMATCH[4]:-0}"
  have_text="$hmaj.$hmin.$hpat"

  # Numeric compare, not string compare: 1.9 is older than 1.26.
  if (( hmaj > rmaj )) ||
    (( hmaj == rmaj && hmin > rmin )) ||
    (( hmaj == rmaj && hmin == rmin && hpat >= rpat )); then
    return 0
  fi
  die "installed Go $have_text is older than the $req_text that $gomod requires; install Go $req_text or newer"
}

main "$@"
