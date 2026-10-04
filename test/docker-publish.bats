#!/usr/bin/env bats
# Tests the Docker Hub publish path: the workflow triggers and pins, the tag
# script it runs, and the compose image line. No network and no image is
# built or pushed. WORKFLOW_UNDER_TEST, COMPOSE_FILE_UNDER_TEST and
# ENV_EXAMPLE_UNDER_TEST point a check at a mutated copy.

setup() {
  repo="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
  wf="${WORKFLOW_UNDER_TEST:-$repo/.github/workflows/docker-publish.yml}"
  compose="${COMPOSE_FILE_UNDER_TEST:-$repo/docker-compose.bridge.yml}"
  example="${ENV_EXAMPLE_UNDER_TEST:-$repo/.env.example}"
}

# code_lines FILE: the file without comment-only and blank lines.
code_lines() {
  /usr/bin/grep -vE '^[[:space:]]*(#|$)' "$1"
}

# step_block NAME: the lines of the workflow step whose name is NAME, up to the next step.
step_block() {
  awk -v want="      - name: $1" '
    $0 == want { on = 1; next }
    on && /^      - name: / { exit }
    on { print }
  ' "$wf"
}

@test "workflow triggers only on pushes to main and v* tags: no PR, other branch or manual trigger" {
  on_block=$(code_lines "$wf" | awk '/^on:/ { on = 1; print; next } on && /^[^ ]/ { exit } on { print }')
  [ "$on_block" = "$(printf "on:\n  push:\n    branches: [main]\n    tags: ['v*']")" ]
}

@test "every uses: is pinned by a full 40-hex commit SHA with a version comment" {
  total=$(/usr/bin/grep -cE '^\s+(- )?uses:' "$wf")
  [ "$total" -ge 4 ]
  pinned=$(/usr/bin/grep -cE '^\s+(- )?uses: [A-Za-z0-9._/-]+@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+$' "$wf")
  [ "$pinned" -eq "$total" ]
}

@test "all publishes share one concurrency group that queues rather than cancels" {
  [ "$(/usr/bin/grep -cxF '      group: docker-publish' "$wf")" -eq 1 ]
  [ "$(/usr/bin/grep -cxF '      cancel-in-progress: false' "$wf")" -eq 1 ]
}

@test "permissions are contents: read at the top and nothing grants write" {
  [ "$(code_lines "$wf" | awk '/^permissions:/ { print; getline; print; exit }')" = "$(printf 'permissions:\n  contents: read')" ]
  [ "$(/usr/bin/grep -cE '^\s+[a-z-]+: (write|write-all)' "$wf")" -eq 0 ]
  [ "$(/usr/bin/grep -cE '^\s+permissions:' "$wf")" -eq 0 ]
}

@test "the two Docker secrets appear only inside the login step" {
  [ "$(/usr/bin/grep -c 'secrets\.' "$wf")" -eq 2 ]
  login=$(step_block "Login to Docker Hub")
  # shellcheck disable=SC2016 # the ${...} is workflow or compose syntax matched literally, not a shell expansion
  printf '%s\n' "$login" | /usr/bin/grep -qF 'username: ${{ secrets.DOCKER_USERNAME }}'
  # shellcheck disable=SC2016 # the ${...} is workflow or compose syntax matched literally, not a shell expansion
  printf '%s\n' "$login" | /usr/bin/grep -qF 'password: ${{ secrets.DOCKER_TOKEN }}'
}

@test "the job is guarded to the organisation and runs the ref check before the login step" {
  /usr/bin/grep -qxF "    if: github.repository_owner == 'GRIDAPPSD'" "$wf"
  # shellcheck disable=SC2016 # literal text under test, not a shell expansion
  check=$(/usr/bin/grep -n 'scripts/release-tag-check.sh "${REF_TYPE}" "${REF_NAME}"' "$wf" | cut -d: -f1)
  login=$(/usr/bin/grep -n 'docker/login-action@' "$wf" | cut -d: -f1)
  [ -n "$check" ] && [ -n "$login" ] && [ "$check" -lt "$login" ]
}

@test "the build pushes Dockerfile.bridge for amd64 with the version build-arg and OCI labels" {
  b=$(step_block "Build and push")
  printf '%s\n' "$b" | /usr/bin/grep -qxF '          file: Dockerfile.bridge'
  printf '%s\n' "$b" | /usr/bin/grep -qxF '          platforms: linux/amd64'
  printf '%s\n' "$b" | /usr/bin/grep -qxF '          push: true'
  # shellcheck disable=SC2016 # the ${...} is workflow or compose syntax matched literally, not a shell expansion
  printf '%s\n' "$b" | /usr/bin/grep -qxF '            VERSION=${{ steps.tags.outputs.version }}'
  # shellcheck disable=SC2016 # the ${...} is workflow or compose syntax matched literally, not a shell expansion
  printf '%s\n' "$b" | /usr/bin/grep -qF 'org.opencontainers.image.version=${{ steps.tags.outputs.version }}'
  # shellcheck disable=SC2016 # the ${...} is workflow or compose syntax matched literally, not a shell expansion
  printf '%s\n' "$b" | /usr/bin/grep -qF 'org.opencontainers.image.revision=${{ github.sha }}'
  # shellcheck disable=SC2016 # the ${...} is workflow or compose syntax matched literally, not a shell expansion
  printf '%s\n' "$b" | /usr/bin/grep -qF 'org.opencontainers.image.source=${{ github.server_url }}/${{ github.repository }}'
}

# mkrepo: builds $BATS_TEST_TMPDIR/r with main at C2, tags v1.0.0 (C1), v1.1.0 (C2), v1.2.0-rc1 (C2),
# v1.0.1 (C1) and an unmerged branch commit tagged v9.9.9.
mkrepo() {
  r="$BATS_TEST_TMPDIR/r"
  git init -q -b main "$r"
  git -C "$r" config user.email t@example.test
  git -C "$r" config user.name t
  git -C "$r" commit -q --allow-empty -m c1
  git -C "$r" tag v1.0.0
  git -C "$r" tag v1.0.1
  git -C "$r" commit -q --allow-empty -m c2
  git -C "$r" tag v1.1.0
  git -C "$r" tag v1.2.0-rc1
  git -C "$r" checkout -q -b side
  git -C "$r" commit -q --allow-empty -m unmerged
  git -C "$r" tag v9.9.9
  git -C "$r" checkout -q main
  # MAIN_REF points at the local branch; the workflow's fetched ref is origin/main.
  export MAIN_REF=refs/heads/main
}

# check KIND NAME: runs release-tag-check.sh in the fixture repo and leaves its
# GITHUB_OUTPUT in $out_file. The script's exit status is the function's.
check() {
  out_file="$BATS_TEST_TMPDIR/out"
  : >"$out_file"
  (cd "$r" && IMAGE=gridappsd/gridappsd-ieee-2030_5-go GITHUB_OUTPUT="$out_file" "$repo/scripts/release-tag-check.sh" "$@")
}

I=gridappsd/gridappsd-ieee-2030_5-go

@test "the highest stable tag on main publishes its version and latest, and stamps the version" {
  mkrepo
  check tag v1.1.0
  [ "$(cat "$out_file")" = "$(printf 'version=v1.1.0\ntags<<EOT\n%s:v1.1.0\n%s:latest\nEOT' "$I" "$I")" ]
}

@test "a patch on an older line publishes its own tag and does not move latest backwards" {
  mkrepo
  check tag v1.0.1
  [ "$(cat "$out_file")" = "$(printf 'version=v1.0.1\ntags<<EOT\n%s:v1.0.1\nEOT' "$I")" ]
}

@test "a pre-release tag publishes only its own tag, never latest" {
  mkrepo
  check tag v1.2.0-rc1
  [ "$(cat "$out_file")" = "$(printf 'version=v1.2.0-rc1\ntags<<EOT\n%s:v1.2.0-rc1\nEOT' "$I")" ]
}

@test "a tag whose commit is not on main is refused before any output is written" {
  mkrepo
  run check tag v9.9.9
  [ "$status" -ne 0 ]
  [[ "$output" == *"not an ancestor"* ]]
  [ ! -s "$out_file" ]
}

@test "a tag missing from the clone is refused" {
  mkrepo
  run check tag v3.0.0
  [ "$status" -ne 0 ]
  [[ "$output" == *"not found in this clone"* ]]
  [ ! -s "$out_file" ]
}

@test "a tag that is not vMAJOR.MINOR.PATCH is refused and writes no output" {
  mkrepo
  # shellcheck disable=SC2016 # literal text under test, not a shell expansion
  for bad in v1 v1.2 main 'v1.2.3;id' 'v1.2.3 x' vX.Y.Z 'v1.2.3-$(id)'; do
    run check tag "$bad"
    [ "$status" -ne 0 ]
    [[ "$output" == *"refusing to publish"* ]]
    [ ! -s "$out_file" ]
  done
}

@test "a push to main publishes only that branch name, never latest or a version tag" {
  mkrepo
  check branch main
  sha=$(git -C "$r" rev-parse --short=7 HEAD)
  [ "$(cat "$out_file")" = "$(printf 'version=main-%s\ntags<<EOT\n%s:main\nEOT' "$sha" "$I")" ]
}

@test "any other branch, and an unknown kind, is refused and writes no output" {
  mkrepo
  for args in "branch side" "branch feature/x" "branch develop" "branch latest" "branch v1.1.0" "ref main"; do
    # word splitting of $args is the point here
    # shellcheck disable=SC2086
    run check $args
    [ "$status" -ne 0 ]
    [ ! -s "$out_file" ]
  done
}

@test "compose image defaults to the published image at latest, pinned by BRIDGE_IMAGE_TAG, and has no build section" {
  # shellcheck disable=SC2016 # literal text under test, not a shell expansion
  /usr/bin/grep -qxF '    image: ${BRIDGE_IMAGE:-gridappsd/gridappsd-ieee-2030_5-go:${BRIDGE_IMAGE_TAG:-latest}}' "$compose"
  [ "$(/usr/bin/grep -cE '^\s+build:' "$compose")" -eq 0 ]
}

@test "compose config resolves the image from BRIDGE_IMAGE_TAG and BRIDGE_IMAGE" {
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 || skip "docker compose is not available"
  printf 'SEP2_ADMIN_UI_KEY=0123456789abcdef-key\n' >"$BATS_TEST_TMPDIR/env"
  cfg() {
    env -i PATH="$PATH" HOME="$BATS_TEST_TMPDIR" "$@" BRIDGE_USER=1:1 BRIDGE_ADMIN_IP=10.0.0.2 \
      BRIDGE_SEP2_PORT=1 BRIDGE_ADMIN_PORT=2 BRIDGE_CERT_DIR=/c BRIDGE_CERT_MODE=rw BRIDGE_ADMIN_SUBNET=10.0.0.0/24 \
      docker compose --env-file "$BATS_TEST_TMPDIR/env" -f "$compose" config | /usr/bin/grep -E '^    image:'
  }
  [ "$(cfg)" = '    image: gridappsd/gridappsd-ieee-2030_5-go:latest' ]
  [ "$(cfg BRIDGE_IMAGE_TAG=v1.2.3)" = '    image: gridappsd/gridappsd-ieee-2030_5-go:v1.2.3' ]
  [ "$(cfg BRIDGE_IMAGE=gridappsd-ieee-2030_5-go:dev BRIDGE_IMAGE_TAG=v9)" = '    image: gridappsd-ieee-2030_5-go:dev' ]
}

@test ".env.example documents BRIDGE_IMAGE_TAG with the compose default, commented out" {
  /usr/bin/grep -qxF '# BRIDGE_IMAGE_TAG=latest' "$example"
  [ "$(/usr/bin/grep -cE '^BRIDGE_IMAGE_TAG=' "$example")" -eq 0 ]
}
