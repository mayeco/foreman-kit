#!/bin/sh
# ==============================================================================
# scripts/npm-install.sh - Build foreman-kit and install it globally with npm
# ==============================================================================
# usage: scripts/npm-install.sh [--prefix <dir>] [--all | --target <os>/<arch>...]
#
# Packs the binary of this platform (scripts/npm-pack.sh, or the targets given) and installs the
# tarball with npm install -g, from the file: nothing is published, and the launcher runs the binary
# the package carries (it downloads one from the GitHub release only on a platform it carries none for).
#
#   --prefix dir   install under dir (dir/bin/foreman-kit) instead of npm's global prefix
#   NPM=<path>     the npm to install with (default: npm). The global prefix of an npm a version
#                  manager provides (mise, nvm, fnm) belongs to one version of Node, and a process
#                  started with another one does not find foreman-kit there: the agents and the
#                  daemon need it on a PATH that stays. Homebrew's npm installs into /opt/homebrew.
#
# It then checks that the foreman-kit installed runs, and says whether it is the one PATH finds.
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
NPM="${NPM:-npm}"
PREFIX=""
PACK_ARGS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --prefix)
      [ $# -ge 2 ] || { echo "npm-install: --prefix needs a directory" >&2; exit 2; }
      PREFIX="$2"
      shift
      ;;
    --all) PACK_ARGS="$PACK_ARGS --all" ;;
    --target)
      [ $# -ge 2 ] || { echo "npm-install: --target needs <os>/<arch>" >&2; exit 2; }
      PACK_ARGS="$PACK_ARGS --target $2"
      shift
      ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "npm-install: unknown argument $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done
command -v "$NPM" >/dev/null 2>&1 || { echo "npm-install: $NPM is not on PATH" >&2; exit 1; }

# shellcheck disable=SC2086
TGZ="$(NPM="$NPM" "$ROOT/scripts/npm-pack.sh" $PACK_ARGS)"

if [ -n "$PREFIX" ]; then
  mkdir -p "$PREFIX"
  "$NPM" install --global --prefix "$PREFIX" --no-audit --no-fund "$TGZ"
  BIN_DIR="$PREFIX/bin"
else
  "$NPM" install --global --no-audit --no-fund "$TGZ"
  BIN_DIR="$("$NPM" prefix --global)/bin"
fi

INSTALLED="$BIN_DIR/foreman-kit"
echo "npm-install: $("$INSTALLED" version) at $INSTALLED"
FOUND="$(command -v foreman-kit 2>/dev/null || true)"
if [ "$FOUND" != "$INSTALLED" ]; then
  if [ -n "$FOUND" ]; then
    echo "npm-install: PATH finds another foreman-kit first: $FOUND" >&2
  fi
  echo "npm-install: put $BIN_DIR first on PATH to use this one" >&2
fi
case "$BIN_DIR" in
  */mise/installs/*|*/.nvm/*|*/fnm/*|*/.volta/*)
    echo "npm-install: $BIN_DIR belongs to one version of Node; agents started with another one will not find foreman-kit there (NPM=/opt/homebrew/bin/npm installs where it stays)" >&2
    ;;
esac
