#!/bin/sh
# ==============================================================================
# install.sh - Install the foreman-kit binary from a GitHub release
# ==============================================================================
#   curl -fsSL https://raw.githubusercontent.com/mayeco/foreman-kit/master/install.sh | sh
#
# Options, after "sh -s --" when piped (curl ... | sh -s -- --version v4.0.0):
#   --version <tag>   the release to install (v4.0.0; default: the latest release)
#   --bin-dir <dir>   where foreman-kit goes (default: ~/.local/bin)
#   --source          build it from this checkout with Go instead (./install.sh --source)
#
# It downloads foreman-kit-<os>-<arch> (macOS or Linux, amd64 or arm64) and the release's
# checksums.txt, checks the binary's SHA-256 against it, and moves it into place only then. The
# release is read, in this order, with GITHUB_TOKEN (or GH_TOKEN) through the API of GitHub, with
# gh when it is installed and logged in, or anonymously (a public repository needs nothing more;
# a token only spares the API's anonymous rate limit, or reads a fork that is private).
#
# Environment: FOREMANKIT_KIT_REF (= --version), FOREMANKIT_KIT_BIN_DIR (= --bin-dir),
# FOREMANKIT_KIT_REPO_SLUG (default: mayeco/foreman-kit), FOREMANKIT_HOME (default: ~/.foreman-kit).
set -eu

REPO="${FOREMANKIT_KIT_REPO_SLUG:-mayeco/foreman-kit}"
REF="${FOREMANKIT_KIT_REF:-latest}"
BIN_DIR="${FOREMANKIT_KIT_BIN_DIR:-$HOME/.local/bin}"
KIT_HOME="${FOREMANKIT_HOME:-$HOME/.foreman-kit}"
TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
SOURCE=0

say() { echo "foreman-kit: $*"; }
die() { echo "foreman-kit: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version)
      [ $# -ge 2 ] || die "--version needs a tag (v4.0.0)"
      REF="$2"
      shift
      ;;
    --bin-dir)
      [ $# -ge 2 ] || die "--bin-dir needs a directory"
      BIN_DIR="$2"
      shift
      ;;
    --source) SOURCE=1 ;;
    -h|--help) sed -n '2,19p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
  shift
done
case "$REF" in
  latest|v*) ;;
  *) REF="v$REF" ;;
esac

mkdir -p "$BIN_DIR" "$KIT_HOME/state"
TARGET="$BIN_DIR/foreman-kit"

# --- From a checkout -----------------------------------------------------------
if [ "$SOURCE" = 1 ]; then
  # shellcheck disable=SC1007
  ROOT="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
  [ -x "$ROOT/scripts/build.sh" ] && [ -d "$ROOT/cmd/foreman-kit" ] || die "--source needs a checkout of foreman-kit ($ROOT is not one)"
  command -v go >/dev/null 2>&1 || die "--source needs Go on PATH"
  OUT="$(mktemp -d "${TMPDIR:-/tmp}/foreman-kit-build.XXXXXX")"
  trap 'rm -rf "$OUT"' EXIT INT TERM
  # build.sh gives the binary the version of the git tags, the commit and the build time.
  "$ROOT/scripts/build.sh" --out "$OUT" >&2
  cp "$OUT"/*/foreman-kit "$TARGET.tmp"
  mv "$TARGET.tmp" "$TARGET"
  say "installed $("$TARGET" version) at $TARGET"
  exit 0
fi

# --- From a release ------------------------------------------------------------
command -v curl >/dev/null 2>&1 || die "curl is needed"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  darwin|linux) ;;
  *) die "unsupported system $OS (macOS or Linux)" ;;
esac
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) die "unsupported architecture $ARCH (amd64 or arm64)" ;;
esac
ASSET="foreman-kit-${OS}-${ARCH}"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/foreman-kit-install.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT INT TERM

# download_api <name> <dest>: through the API, the one way a token reads a private release.
RELEASE_JSON=""
download_api() {
  if [ -z "$RELEASE_JSON" ]; then
    if [ "$REF" = latest ]; then path="releases/latest"; else path="releases/tags/$REF"; fi
    RELEASE_JSON="$TMP/release.json"
    curl -fsSL -H "Authorization: Bearer $TOKEN" -H "Accept: application/vnd.github+json" \
      "https://api.github.com/repos/$REPO/$path" -o "$RELEASE_JSON" \
      || die "cannot read release $REF of $REPO with the token given"
  fi
  # The asset's "url" (…/releases/assets/<id>) comes before its "name" in the API's JSON.
  url="$(tr ',' '\n' < "$RELEASE_JSON" | awk -v want="$1" '
    /"url": *"https:\/\/api\.github\.com\/repos\/.*\/releases\/assets\// { u = $0; sub(/.*"url": *"/, "", u); sub(/".*/, "", u) }
    /"name": *"/ { n = $0; sub(/.*"name": *"/, "", n); sub(/".*/, "", n); if (n == want && u != "") { print u; exit } }
  ')"
  [ -n "$url" ] || die "release $REF of $REPO has no $1"
  curl -fsSL -H "Authorization: Bearer $TOKEN" -H "Accept: application/octet-stream" "$url" -o "$2"
}

download() { # download <name> <dest>
  if [ -n "$TOKEN" ]; then
    download_api "$1" "$2"
  elif command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    if [ "$REF" = latest ]; then
      gh release download --repo "$REPO" --pattern "$1" --output "$2" --clobber
    else
      gh release download "$REF" --repo "$REPO" --pattern "$1" --output "$2" --clobber
    fi
  else
    if [ "$REF" = latest ]; then
      url="https://github.com/$REPO/releases/latest/download/$1"
    else
      url="https://github.com/$REPO/releases/download/$REF/$1"
    fi
    curl -fsSL "$url" -o "$2" \
      || die "cannot download $url (a private repository needs GITHUB_TOKEN, or gh logged in)"
  fi
}

say "downloading $ASSET of release $REF of $REPO..."
download checksums.txt "$TMP/checksums.txt"
download "$ASSET" "$TMP/$ASSET"

want="$(awk -v a="$ASSET" '$2 == a || $2 == "*" a { print $1; exit }' "$TMP/checksums.txt")"
[ -n "$want" ] || die "checksums.txt of release $REF has no $ASSET"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$TMP/$ASSET" | awk '{ print $1 }')"
else
  got="$(shasum -a 256 "$TMP/$ASSET" | awk '{ print $1 }')"
fi
[ "$got" = "$want" ] || die "checksum mismatch for $ASSET: expected $want, got $got"

chmod 0755 "$TMP/$ASSET"
# Copied beside and renamed: a foreman-kit running at the same time keeps its own file.
cp "$TMP/$ASSET" "$TARGET.tmp"
mv "$TARGET.tmp" "$TARGET"

say "installed $("$TARGET" version) at $TARGET"
case ":${PATH:-}:" in
  *":$BIN_DIR:"*) ;;
  *) say "add $BIN_DIR to your PATH (export PATH=\"$BIN_DIR:\$PATH\")" ;;
esac
