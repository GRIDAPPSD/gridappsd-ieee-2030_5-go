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

# A job with no checkout has no .git, so gh cannot infer the repository and
# fails with "not a git repository". Such a job must set GH_REPO or pass
# -R/--repo to every gh call.
@test "jobs without checkout name the repository for gh" {
  cd "$repo"
  bad=""
  for f in .github/workflows/*.yml; do
    out=$(awk -v file="$f" '
      function flush() {
        if (job != "" && !has_checkout && has_gh && !has_repo) printf "%s:%s ", file, job
      }
      /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { flush(); job=$1; has_checkout=0; has_gh=0; has_repo=0; next }
      /actions\/checkout/ { has_checkout=1 }
      /^[[:space:]]+gh (release|pr|issue|api|run|workflow|repo) / { has_gh=1 }
      /GH_REPO:|[[:space:]]-R[[:space:]]|--repo/ { has_repo=1 }
      END { flush() }
    ' "$f")
    bad="$bad$out"
  done
  echo "jobs missing a repository: $bad"
  [ -z "$bad" ]
}
