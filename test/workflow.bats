#!/usr/bin/env bats
# Tests for CI workflow configuration. Checks that workflows are pinned to
# specific runner images rather than relying on ubuntu-latest.

setup() {
  repo="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
}

@test "no workflow uses ubuntu-latest" {
  # Check that all runs-on lines in workflows are pinned to a specific version.
  cd "$repo"
  result=$(git grep "runs-on: ubuntu-latest" -- .github/workflows 2>&1 || true)
  [ -z "$result" ]
}

@test "all workflows pin to ubuntu-24.04" {
  # Count the number of runs-on lines that are pinned to ubuntu-24.04.
  # This ensures all workflows have been updated.
  cd "$repo"
  count=$(git grep "runs-on: ubuntu-24.04" -- .github/workflows | wc -l)
  [ "$count" -gt 0 ]
}
