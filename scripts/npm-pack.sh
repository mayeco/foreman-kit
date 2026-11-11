#!/bin/sh
# ==============================================================================
# scripts/npm-pack.sh - Build the npm package of foreman-kit as a local tarball
# ==============================================================================
# usage: scripts/npm-pack.sh [--all | --target <os>/<arch>...] [--prebuilt <dir>] [--public]
#
# Builds the binaries (scripts/build.sh, the same targets: the platform this runs on unless told
# otherwise), lays out the package from npm/ in dist/npm/package with each binary under
# vendor/<os>-<arch>/, and runs npm pack. It prints the path of the tarball, dist/<name>.tgz.
#
#   --prebuilt dir   build nothing: take the binaries of a release from dir, named as GoReleaser
#                    uploads them (foreman-kit-<os>-<arch>)
#   --public         a package npm may publish. Without it the package is private, and npm refuses
#                    to publish it; only the release workflow passes it.
#
# FOREMANKIT_KIT_VERSION sets the version of the package (default: the one of the source). NPM
# chooses the npm that packs it (default: npm).
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
NPM="${NPM:-npm}"
command -v "$NPM" >/dev/null 2>&1 || { echo "npm-pack: $NPM is not on PATH" >&2; exit 1; }

PREBUILT=""
PUBLIC=0
BUILD_ARGS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --prebuilt)
      [ $# -ge 2 ] || { echo "npm-pack: --prebuilt needs a directory" >&2; exit 2; }
      PREBUILT="$2"
      shift
      ;;
    --public) PUBLIC=1 ;;
    --all) BUILD_ARGS="$BUILD_ARGS --all" ;;
    --target)
      [ $# -ge 2 ] || { echo "npm-pack: --target needs <os>/<arch>" >&2; exit 2; }
      BUILD_ARGS="$BUILD_ARGS --target $2"
      shift
      ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "npm-pack: unknown argument $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done

BIN_OUT="$ROOT/dist/bin"
rm -rf "$BIN_OUT"
if [ -n "$PREBUILT" ]; then
  found=0
  for f in "$PREBUILT"/foreman-kit-*-*; do
    [ -f "$f" ] || continue
    platform="${f##*/foreman-kit-}"
    case "$platform" in
      darwin-amd64|darwin-arm64|linux-amd64|linux-arm64) ;;
      *) continue ;;
    esac
    mkdir -p "$BIN_OUT/$platform"
    cp "$f" "$BIN_OUT/$platform/foreman-kit"
    found=$((found + 1))
  done
  [ "$found" -gt 0 ] || { echo "npm-pack: $PREBUILT holds no foreman-kit-<os>-<arch> binary" >&2; exit 1; }
else
  # shellcheck disable=SC2086
  "$ROOT/scripts/build.sh" --out "$BIN_OUT" $BUILD_ARGS >&2
fi

# The version build.sh gave the binaries: the source's, or FOREMANKIT_KIT_VERSION.
VERSION="${FOREMANKIT_KIT_VERSION:-$(sed -n 's/^[[:space:]]*Version[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' "$ROOT/internal/assets/embed.go" | head -n 1)}"

STAGE="$ROOT/dist/npm/package"
rm -rf "$ROOT/dist/npm"
mkdir -p "$STAGE/vendor"
cp -R "$ROOT/npm/bin" "$ROOT/npm/README.md" "$STAGE/"
cp "$ROOT/npm/package.json" "$STAGE/package.json"
for dir in "$BIN_OUT"/*/; do
  platform="$(basename "$dir")"
  mkdir -p "$STAGE/vendor/$platform"
  cp "$dir/foreman-kit" "$STAGE/vendor/$platform/foreman-kit"
  chmod 0755 "$STAGE/vendor/$platform/foreman-kit"
done
chmod 0755 "$STAGE/bin/foreman-kit.js"

# The version of the package is the binary's (4.0.0-go is a valid semver prerelease).
(cd "$STAGE" && "$NPM" pkg set version="$VERSION" >/dev/null)
if [ "$PUBLIC" = 1 ]; then
  (cd "$STAGE" && "$NPM" pkg delete private >/dev/null)
fi

TGZ="$(cd "$STAGE" && "$NPM" pack --silent --pack-destination "$ROOT/dist")"
echo "npm-pack: $(ls "$BIN_OUT" | tr '\n' ' ')-> dist/$TGZ" >&2
echo "$ROOT/dist/$TGZ"
