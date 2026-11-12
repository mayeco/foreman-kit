#!/bin/sh
# ==============================================================================
# scripts/version.sh - Print the version of foreman-kit, detected from the git tags
# ==============================================================================
# usage: scripts/version.sh [--next patch|minor|major] [--tag]
#
#   (no argument)   the version of the checkout:
#                     on a tag v4.1.0, clean         4.1.0
#                     3 commits past v4.1.0          4.1.1-dev.3+<commit>
#                     uncommitted changes            ...+<commit>.dirty
#                     no tag v<semver> yet           4.0.0-dev.<commits>+<commit>
#   --next part     the version the next release tag would carry: the latest tag v<semver> with
#                   that part raised, or 4.0.0 when there is no tag yet
#   --tag           print it as a tag (v4.1.0) rather than a version
#
# FOREMANKIT_KIT_VERSION, when set, is printed as it is. Only tags vX.Y.Z (and vX.Y.Z-<pre>) count.
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
FIRST="4.0.0"
NEXT=""
AS_TAG=0

while [ $# -gt 0 ]; do
  case "$1" in
    --next)
      [ $# -ge 2 ] || { echo "version: --next needs patch, minor or major" >&2; exit 2; }
      NEXT="$2"
      shift
      ;;
    --tag) AS_TAG=1 ;;
    -h|--help) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "version: unknown argument $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done

emit() {
  if [ "$AS_TAG" = 1 ]; then echo "v$1"; else echo "$1"; fi
}

if [ -n "${FOREMANKIT_KIT_VERSION:-}" ] && [ -z "$NEXT" ]; then
  emit "${FOREMANKIT_KIT_VERSION#v}"
  exit 0
fi

# latest_release: the highest tag vX.Y.Z without a prerelease part, as X.Y.Z (empty: none).
latest_release() {
  git -C "$ROOT" tag --list 'v[0-9]*' \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' \
    | sed 's/^v//' \
    | sort -t. -k1,1n -k2,2n -k3,3n \
    | tail -n 1
}

bump() { # bump <X.Y.Z> <part>
  major="${1%%.*}"; rest="${1#*.}"; minor="${rest%%.*}"; patch="${rest#*.}"
  case "$2" in
    major) echo "$((major + 1)).0.0" ;;
    minor) echo "$major.$((minor + 1)).0" ;;
    patch) echo "$major.$minor.$((patch + 1))" ;;
    *) echo "version: unknown part $2 (patch, minor or major)" >&2; exit 2 ;;
  esac
}

if [ -n "$NEXT" ]; then
  latest="$(latest_release)"
  if [ -z "$latest" ]; then
    case "$NEXT" in patch|minor|major) ;; *) bump 0.0.0 "$NEXT" >/dev/null ;; esac
    emit "$FIRST"
  else
    emit "$(bump "$latest" "$NEXT")"
  fi
  exit 0
fi

commit="$(git -C "$ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
dirty=""
if [ -n "$(git -C "$ROOT" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
  dirty=".dirty"
fi

# The nearest tag vX.Y.Z[-pre] reachable from HEAD, and how many commits lie past it.
described="$(git -C "$ROOT" describe --tags --long --match 'v[0-9]*.[0-9]*.[0-9]*' 2>/dev/null || true)"
if [ -z "$described" ]; then
  count="$(git -C "$ROOT" rev-list --count HEAD 2>/dev/null || echo 0)"
  emit "$FIRST-dev.$count+$commit$dirty"
  exit 0
fi

# v4.1.0-rc.1-3-gabc -> tag v4.1.0-rc.1, count 3
rest="${described%-g*}"
count="${rest##*-}"
tag="${rest%-*}"
version="${tag#v}"

if [ "$count" = 0 ] && [ -z "$dirty" ]; then
  emit "$version"
elif [ "$count" = 0 ]; then
  emit "$version+$commit$dirty"
else
  case "$version" in
    *-*) base="$version" ;;                 # past a prerelease: still on its way to that release
    *) base="$(bump "$version" patch)" ;;   # past a release: on the way to the next patch
  esac
  base="${base%%-*}"
  emit "$base-dev.$count+$commit$dirty"
fi
