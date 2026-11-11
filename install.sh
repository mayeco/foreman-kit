#!/bin/sh
# ==============================================================================
# install.sh - Bootstrap installer for the Foreman Go binary
# ==============================================================================
# Install with:
#   curl -fsSL https://raw.githubusercontent.com/mayeco/foreman-kit/master/install.sh | sh
#
# Downloads the prebuilt static binary for darwin/linux (amd64/arm64) from
# GitHub Releases into ~/.local/bin/foreman-kit, or builds it from source with
# `CGO_ENABLED=0 go build ./cmd/foreman-kit` when run from a local checkout.
set -eu

BIN_DIR="${FOREMANKIT_KIT_BIN_DIR:-$HOME/.local/bin}"
KIT_HOME="${FOREMANKIT_HOME:-$HOME/.foreman-kit}"
REPO="${FOREMANKIT_KIT_REPO_SLUG:-mayeco/foreman-kit}"
REF="${FOREMANKIT_KIT_REF:-latest}"

mkdir -p "$BIN_DIR" "$KIT_HOME/state"
TARGET="$BIN_DIR/foreman-kit"

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd || pwd)"
if [ -f "$SCRIPT_DIR/go.mod" ] && [ -d "$SCRIPT_DIR/cmd/foreman-kit" ] && command -v go >/dev/null 2>&1; then
  echo "foreman-kit: building static binary from $SCRIPT_DIR..."
  (cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -o "$TARGET" ./cmd/foreman-kit)
  chmod +x "$TARGET"
else
  OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *)
      echo "foreman-kit: unsupported architecture: $ARCH" >&2
      exit 1
      ;;
  esac
  ASSET="foreman-kit-${OS}-${ARCH}"
  if [ "$REF" = "latest" ]; then
    URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
  else
    URL="https://github.com/${REPO}/releases/download/${REF}/${ASSET}"
  fi
  echo "foreman-kit: downloading $URL -> $TARGET..."
  curl -fsSL "$URL" -o "${TARGET}.tmp"
  chmod +x "${TARGET}.tmp"
  mv "${TARGET}.tmp" "$TARGET"
fi

echo "foreman-kit: installed $($TARGET version)"
case ":${PATH:-}:" in
  *":$BIN_DIR:"*) ;;
  *) echo "foreman-kit: add $BIN_DIR to your PATH (export PATH=\"$BIN_DIR:\$PATH\")" ;;
esac
