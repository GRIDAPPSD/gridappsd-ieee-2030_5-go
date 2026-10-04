#!/usr/bin/env bash
# Decides what docker-publish.yml may push for a ref, and refuses a ref that
# must not publish. Usage: release-tag-check.sh tag|branch NAME
# A branch publishes only as its own name (main) and never as latest
# or a version tag. A tag publishes as itself, and as latest when it is the
# highest stable version. Run inside a clone that has the tags and origin/main.
# Reads IMAGE (the image name) and GITHUB_OUTPUT (the file to append version=
# and tags= to); MAIN_REF overrides the default refs/remotes/origin/main, for
# tests.
set -euo pipefail

die() {
  echo "release-tag-check: $*" >&2
  exit 1
}

kind="${1:-}"
tag="${2:-}"
image="${IMAGE:?IMAGE is not set}"
out="${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set}"
main_ref="${MAIN_REF:-refs/remotes/origin/main}"
stable='^v[0-9]+\.[0-9]+\.[0-9]+$'

[ -n "$tag" ] || die "usage: release-tag-check.sh tag|branch NAME"

if [ "$kind" = branch ]; then
  case "$tag" in
    main) ;;
    *) die "branch $tag does not publish; only main does" ;;
  esac
  {
    echo "version=$tag-$(git rev-parse --short=7 HEAD)"
    echo "tags<<EOT"
    echo "$image:$tag"
    echo "EOT"
  } >>"$out"
  exit 0
fi
[ "$kind" = tag ] || die "kind must be tag or branch, got: $kind"
# The tag name reaches tags= below, so only a strict release shape passes.
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] ||
  die "tag $tag is not vMAJOR.MINOR.PATCH[-suffix]; refusing to publish"

# Anyone who can push a tag could otherwise publish an unmerged commit.
commit=$(git rev-parse --verify --quiet "refs/tags/$tag^{commit}") || die "tag $tag not found in this clone"
git rev-parse --verify --quiet "$main_ref^{commit}" >/dev/null || die "$main_ref not found; fetch main first"
git merge-base --is-ancestor "$commit" "$main_ref" ||
  die "tag $tag ($commit) is not an ancestor of $main_ref; refusing to publish"

tags=("$image:$tag")
# latest moves only to the highest stable version on main, so a patch on an
# older line cannot move it backwards and a stray tag on an unmerged commit
# cannot block it. The tag being published is on main (checked above).
highest=""
while IFS= read -r t; do
  [[ "$t" =~ $stable ]] || continue
  git merge-base --is-ancestor "refs/tags/$t^{commit}" "$main_ref" || continue
  highest="$t"
done < <(git tag --list 'v*' | sort -V)
if [[ "$tag" =~ $stable ]] && [ "$tag" = "$highest" ]; then
  tags+=("$image:latest")
fi

{
  echo "version=$tag"
  echo "tags<<EOT"
  printf '%s\n' "${tags[@]}"
  echo "EOT"
} >>"$out"
