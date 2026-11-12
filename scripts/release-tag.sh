#!/bin/sh
# ==============================================================================
# scripts/release-tag.sh - Create the next release tag v<semver> of foreman-kit
# ==============================================================================
# usage: scripts/release-tag.sh [patch|minor|major|<X.Y.Z>] [--pre <id>] [--push] [--dry-run] [--yes]
#
# The version is detected from the tags already there (scripts/version.sh --next): the latest tag
# vX.Y.Z with the part named raised (default: patch). The first tag is v4.0.0.
#
#   patch|minor|major   the part of the latest release to raise (default: patch)
#   X.Y.Z               the version itself, which must be higher than the latest release
#   --pre <id>          a prerelease: the next free <version>-<id>.N (v4.1.0-rc.1, then rc.2, ...)
#   --push              push the tag to origin, which runs the release workflow
#                       (.github/workflows/release.yml: GitHub release and npm package)
#   --dry-run           print the tag it would create and create nothing
#   --yes               do not ask before creating it
#
# It creates an annotated tag on HEAD and refuses a checkout with uncommitted changes, a HEAD that
# already carries a release tag, or a tag that exists already.
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
PART="patch"
EXPLICIT=""
PRE=""
PUSH=0
DRY=0
YES=0

while [ $# -gt 0 ]; do
  case "$1" in
    patch|minor|major) PART="$1" ;;
    [0-9]*.[0-9]*.[0-9]*|v[0-9]*.[0-9]*.[0-9]*) EXPLICIT="${1#v}" ;;
    --pre)
      [ $# -ge 2 ] || { echo "release-tag: --pre needs an identifier (rc, beta, ...)" >&2; exit 2; }
      PRE="$2"
      shift
      ;;
    --push) PUSH=1 ;;
    --dry-run) DRY=1 ;;
    --yes|-y) YES=1 ;;
    -h|--help) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "release-tag: unknown argument $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done

cd "$ROOT"
git rev-parse --git-dir >/dev/null 2>&1 || { echo "release-tag: $ROOT is not a git checkout" >&2; exit 1; }

if [ "$PUSH" = 1 ]; then
  git fetch --tags --quiet origin || { echo "release-tag: cannot fetch the tags of origin" >&2; exit 1; }
fi

semver_gt() { # semver_gt <a> <b>: a > b for X.Y.Z
  [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)" = "$1" ]
}

LATEST="$(git tag --list 'v[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sed 's/^v//' | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1 || true)"

if [ -n "$EXPLICIT" ]; then
  echo "$EXPLICIT" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "release-tag: $EXPLICIT is not X.Y.Z" >&2; exit 2; }
  if [ -n "$LATEST" ] && ! semver_gt "$EXPLICIT" "$LATEST"; then
    echo "release-tag: $EXPLICIT is not higher than the latest release v$LATEST" >&2
    exit 1
  fi
  VERSION="$EXPLICIT"
else
  VERSION="$("$ROOT/scripts/version.sh" --next "$PART")"
fi

if [ -n "$PRE" ]; then
  echo "$PRE" | grep -Eq '^[0-9A-Za-z-]+$' || { echo "release-tag: --pre $PRE is not a valid identifier" >&2; exit 2; }
  n=1
  while git rev-parse -q --verify "refs/tags/v$VERSION-$PRE.$n" >/dev/null; do
    n=$((n + 1))
  done
  VERSION="$VERSION-$PRE.$n"
fi
TAG="v$VERSION"

if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  echo "release-tag: the tag $TAG exists already" >&2
  exit 1
fi
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  if [ "$DRY" = 1 ]; then
    echo "release-tag: warning: the checkout has uncommitted changes; a real run refuses to tag it" >&2
  else
    echo "release-tag: the checkout has uncommitted changes; commit them before tagging" >&2
    exit 1
  fi
fi
on_head="$(git tag --points-at HEAD | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true)"
if [ -n "$on_head" ] && [ -z "$PRE" ]; then
  echo "release-tag: HEAD is already released as $on_head" >&2
  exit 1
fi

COMMIT="$(git rev-parse --short=12 HEAD)"
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
echo "release-tag: latest release ${LATEST:+v$LATEST}$([ -n "$LATEST" ] || echo '(none)') -> $TAG on $COMMIT ($BRANCH)"

if [ "$DRY" = 1 ]; then
  echo "release-tag: dry run, nothing created"
  exit 0
fi

if [ "$YES" != 1 ]; then
  if [ -t 0 ]; then
    printf 'Create the tag %s%s? [y/N] ' "$TAG" "$([ "$PUSH" = 1 ] && echo ' and push it to origin')"
    read -r answer
    case "$answer" in y|Y|yes|YES) ;; *) echo "release-tag: nothing created"; exit 1 ;; esac
  else
    echo "release-tag: not a terminal; pass --yes to create $TAG" >&2
    exit 1
  fi
fi

git tag -a "$TAG" -m "foreman-kit $TAG"
echo "release-tag: created $TAG"

if [ "$PUSH" = 1 ]; then
  git push origin "refs/tags/$TAG"
  echo "release-tag: pushed $TAG; the release workflow builds and publishes it"
else
  echo "release-tag: push it with: git push origin $TAG"
fi
