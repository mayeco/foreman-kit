#!/bin/sh
# ==============================================================================
# scripts/build.sh - Build the static foreman-kit binary for one or more platforms
# ==============================================================================
# usage: scripts/build.sh [--all] [--target <os>/<arch>]... [--out <dir>]
#
#   (no target)        the platform this runs on
#   --all              darwin/amd64, darwin/arm64, linux/amd64, linux/arm64
#   --target os/arch   one platform; repeat it for more
#   --out dir          where the binaries go (default: dist/bin), one directory per platform:
#                      <dir>/<os>-<arch>/foreman-kit
#
# The binary is static (CGO_ENABLED=0) and carries its version, the commit it was built from and
# when (internal/assets: Version, SourceCommit, BuiltAt). FOREMANKIT_KIT_VERSION overrides the version
# the source declares. It needs Go on PATH and nothing else.
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
OUT="$ROOT/dist/bin"
TARGETS=""
ALL="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"

while [ $# -gt 0 ]; do
  case "$1" in
    --all) TARGETS="$ALL" ;;
    --target)
      [ $# -ge 2 ] || { echo "build: --target needs <os>/<arch>" >&2; exit 2; }
      TARGETS="$TARGETS $2"
      shift
      ;;
    --out)
      [ $# -ge 2 ] || { echo "build: --out needs a directory" >&2; exit 2; }
      OUT="$2"
      shift
      ;;
    -h|--help) sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "build: unknown argument $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done

command -v go >/dev/null 2>&1 || { echo "build: go is not on PATH (Go 1.25 or later builds foreman-kit)" >&2; exit 1; }
if [ -z "$TARGETS" ]; then
  TARGETS="$(go env GOOS)/$(go env GOARCH)"
fi

PKG="github.com/mayeco/foreman-kit/internal/assets"
VERSION="${FOREMANKIT_KIT_VERSION:-$(sed -n 's/^[[:space:]]*Version[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' "$ROOT/internal/assets/embed.go" | head -n 1)}"
[ -n "$VERSION" ] || { echo "build: cannot read the version from internal/assets/embed.go" >&2; exit 1; }
COMMIT="$(git -C "$ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
if [ -n "$(git -C "$ROOT" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
  COMMIT="$COMMIT-dirty"
fi
BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X $PKG.Version=$VERSION -X $PKG.SourceCommit=$COMMIT -X $PKG.BuiltAt=$BUILT_AT"

for target in $TARGETS; do
  os="${target%/*}"
  arch="${target#*/}"
  case "$os/$arch" in
    darwin/amd64|darwin/arm64|linux/amd64|linux/arm64) ;;
    *) echo "build: $target is not a supported platform ($ALL)" >&2; exit 2 ;;
  esac
  dest="$OUT/$os-$arch"
  mkdir -p "$dest"
  echo "build: foreman-kit $VERSION ($COMMIT) for $os/$arch -> $dest/foreman-kit"
  (cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" -o "$dest/foreman-kit" ./cmd/foreman-kit)
done
