#!/usr/bin/env bash
# Bump the release version, commit it, tag it, and build the release binary.
#
#   scripts/release.sh                 # show the current version and the next candidates
#   scripts/release.sh patch           # 0.1.0 -> 0.1.1
#   scripts/release.sh minor --no-tag  # 0.1.1 -> 0.2.0, skip the git tag
#
# The version is the truth in ./VERSION (see Makefile): editing it is the release
# action, and the git tag is an index over it. The script refuses to guess a version
# for you — a release number that nobody chose is worse than no release at all.
#
# What it does NOT do: deploy. Building and publishing are separate steps so a failed
# deploy can be retried (or rolled back) without minting a second version number.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION_FILE="$ROOT/VERSION"
BUMP="${1:-}"
NO_TAG=0
for arg in "$@"; do
  [ "$arg" = "--no-tag" ] && NO_TAG=1
done
[ "${1:-}" = "--no-tag" ] && BUMP=""

current="$(cat "$VERSION_FILE" 2>/dev/null | tr -d '[:space:]')"
current="${current:-0.0.0}"

# next_version computes the bump without touching the file, so the "what would happen"
# output below and the real write cannot disagree.
next_version() {
  local bump="$1" major minor patch
  IFS='.' read -r major minor patch <<<"$current"
  case "$bump" in
    major) echo "$((major + 1)).0.0" ;;
    minor) echo "${major}.$((minor + 1)).0" ;;
    patch) echo "${major}.${minor}.$((patch + 1))" ;;
    *) return 1 ;;
  esac
}

if [ -z "$BUMP" ]; then
  echo "current version: $current"
  for bump in patch minor major; do
    echo "  $bump -> $(next_version "$bump")"
  done
  echo
  echo "usage: scripts/release.sh patch|minor|major [--no-tag]"
  exit 0
fi

case "$BUMP" in
  patch|minor|major) ;;
  *) echo "release: unknown bump '$BUMP' (want patch|minor|major)" >&2; exit 2 ;;
esac

if [ -n "$(git status --porcelain)" ]; then
  echo "release: the working tree is dirty; commit or stash first." >&2
  echo "  A release tag must point at a commit that contains the version it names." >&2
  git status --short >&2
  exit 1
fi

target="$(next_version "$BUMP")"

# The version alone does not have to be unique across history (you may release 0.2.0
# twice while iterating), but the *tag* does — git would silently move an existing tag
# and the old release would stop being reproducible.
if [ "$NO_TAG" -eq 0 ] && git rev-parse -q --verify "refs/tags/v${target}" >/dev/null; then
  echo "release: tag v${target} already exists; pick another version or drop the tag." >&2
  exit 1
fi

printf '%s\n' "$target" >"$VERSION_FILE"
echo "release: $current -> $target"

if [ "$NO_TAG" -eq 0 ]; then
  git add VERSION
  git commit -m "release: v${target}"
  git tag -a "v${target}" -m "v${target}"
  echo "release: committed and tagged v${target}"
else
  echo "release: VERSION updated (not committed; --no-tag given)"
fi

make build
echo "release: built $(./bin/aigw -version)"
