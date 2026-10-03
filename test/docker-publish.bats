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

@test "workflow triggers only on v* tag pushes: no branch, PR or manual trigger" {
  on_block=$(code_lines "$wf" | awk '/^on:/ { on = 1; print; next } on && /^[^ ]/ { exit } on { print }')
  [ "$on_block" = "$(printf "on:\n  push:\n    tags: ['v*']")" ]
}

@test "every uses: is pinned by a full 40-hex commit SHA with a version comment" {
  total=$(/usr/bin/grep -cE '^\s+(- )?uses:' "$wf")
  [ "$total" -ge 4 ]
  pinned=$(/usr/bin/grep -cE '^\s+(- )?uses: [A-Za-z0-9._/-]+@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+$' "$wf")
  [ "$pinned" -eq "$total" ]
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

@test "the push is guarded to the organisation and to a tag ref" {
  /usr/bin/grep -qF "if: github.repository_owner == 'GRIDAPPSD' && github.ref_type == 'tag'" "$wf"
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

# run_tags REF: runs the Resolve tags script body with REF_NAME=REF and prints
# the GITHUB_OUTPUT it wrote; the script's exit status is the function's.
run_tags() {
  step_block "Resolve tags" | awk '/^        run: \|$/ { on = 1; next } on { sub(/^          /, ""); print }' >"$BATS_TEST_TMPDIR/tags.sh"
  [ -s "$BATS_TEST_TMPDIR/tags.sh" ]
  : >"$BATS_TEST_TMPDIR/out"
  REF_NAME="$1" IMAGE=gridappsd/gridappsd-ieee-2030_5-go GITHUB_OUTPUT="$BATS_TEST_TMPDIR/out" \
    bash "$BATS_TEST_TMPDIR/tags.sh"
}

@test "a stable tag publishes the version tag and latest, and stamps the version" {
  run_tags v1.2.3
  [ "$(cat "$BATS_TEST_TMPDIR/out")" = "$(printf 'version=v1.2.3\ntags<<EOT\ngridappsd/gridappsd-ieee-2030_5-go:v1.2.3\ngridappsd/gridappsd-ieee-2030_5-go:latest\nEOT')" ]
}

@test "a pre-release tag publishes only its own tag, never latest" {
  run_tags v1.2.3-rc.1
  [ "$(cat "$BATS_TEST_TMPDIR/out")" = "$(printf 'version=v1.2.3-rc.1\ntags<<EOT\ngridappsd/gridappsd-ieee-2030_5-go:v1.2.3-rc.1\nEOT')" ]
}

@test "a tag that is not vMAJOR.MINOR.PATCH is refused and writes no output" {
  # shellcheck disable=SC2016 # literal text under test, not a shell expansion
  for bad in v1 v1.2 main 'v1.2.3;id' 'v1.2.3 x' vX.Y.Z 'v1.2.3-$(id)'; do
    run run_tags "$bad"
    [ "$status" -ne 0 ]
    [[ "$output" == *"refusing to publish"* ]]
    [ ! -s "$BATS_TEST_TMPDIR/out" ]
  done
}

@test "compose image defaults to the published image at latest, pinned by BRIDGE_IMAGE_TAG, and keeps build" {
  # shellcheck disable=SC2016 # literal text under test, not a shell expansion
  /usr/bin/grep -qxF '    image: ${BRIDGE_IMAGE:-gridappsd/gridappsd-ieee-2030_5-go:${BRIDGE_IMAGE_TAG:-latest}}' "$compose"
  awk '/^    build:$/ { on = 1; next } on && /^      / { print; next } on { exit }' "$compose" >"$BATS_TEST_TMPDIR/build"
  [ "$(cat "$BATS_TEST_TMPDIR/build")" = "$(printf '      context: .\n      dockerfile: Dockerfile.bridge')" ]
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
