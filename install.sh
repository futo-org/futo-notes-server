#!/bin/sh
# Stonefruit server installer.
# Usage: curl -sSL https://gitlab.futo.org/stonefruit/stonefruit-server/-/raw/main/install.sh | sh
#
# Downloads the stonefruit installer binary (static, no runtime dependencies)
# and runs interactive setup. Requires Docker; no Node, no Python.
set -eu

GITLAB_HOST="${GITLAB_HOST:-https://gitlab.futo.org}"
PROJECT_PATH="stonefruit%2Fstonefruit-server"
INSTALL_DIR="${STONEFRUIT_INSTALL_DIR:-/usr/local/bin}"
BIN_PATH="$INSTALL_DIR/stonefruit"

msg() { printf '  %s\n' "$*"; }
err() { printf '  error: %s\n' "$*" >&2; }

# ── Prereq checks ──────────────────────────────────────────────────────

for cmd in curl uname; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    err "$cmd is required but not installed."
    exit 1
  fi
done

if ! command -v docker >/dev/null 2>&1; then
  err "Docker is required but not installed."
  err "Install Docker from https://docs.docker.com/get-docker/"
  exit 1
fi

# ── Detect platform ────────────────────────────────────────────────────

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux) ;;
  darwin) ;;
  *) err "Unsupported OS: $OS"; exit 1 ;;
esac

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) err "Unsupported arch: $ARCH"; exit 1 ;;
esac

TARGET="${OS}-${ARCH}"

# ── Resolve latest release tag ─────────────────────────────────────────

msg "Fetching latest release..."
RELEASES_URL="$GITLAB_HOST/api/v4/projects/$PROJECT_PATH/releases?per_page=1"
RELEASE_JSON=$(curl -sSf "$RELEASES_URL" || true)

if [ -z "$RELEASE_JSON" ] || [ "$RELEASE_JSON" = "[]" ]; then
  err "No releases found at $RELEASES_URL"
  exit 1
fi

# Pick out the first tag_name. Tolerates arbitrary whitespace in the JSON.
TAG=$(printf '%s' "$RELEASE_JSON" \
  | tr -d '[:space:]' \
  | grep -o '"tag_name":"[^"]*"' \
  | head -n1 \
  | sed 's/"tag_name":"\([^"]*\)"/\1/')

if [ -z "$TAG" ]; then
  err "Could not resolve latest release tag"
  exit 1
fi

# ── Download binary ────────────────────────────────────────────────────

msg "Installing Stonefruit $TAG ($TARGET)..."
DOWNLOAD_URL="$GITLAB_HOST/api/v4/projects/$PROJECT_PATH/packages/generic/stonefruit-installer/$TAG/stonefruit-$TARGET"

TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

if ! curl -sSfL "$DOWNLOAD_URL" -o "$TMP"; then
  err "Failed to download binary from $DOWNLOAD_URL"
  exit 1
fi
chmod +x "$TMP"

# ── Install to $INSTALL_DIR ────────────────────────────────────────────

if [ -w "$INSTALL_DIR" ] || [ "$(id -u)" = 0 ]; then
  mv "$TMP" "$BIN_PATH"
elif command -v sudo >/dev/null 2>&1; then
  msg "Installing to $INSTALL_DIR requires sudo..."
  sudo mv "$TMP" "$BIN_PATH"
  sudo chmod +x "$BIN_PATH"
else
  err "Cannot write to $INSTALL_DIR and sudo is not available."
  err "Set STONEFRUIT_INSTALL_DIR to a writable location, e.g. \$HOME/.local/bin."
  exit 1
fi

trap - EXIT
msg "Installed to $BIN_PATH"

# ── Launch setup ───────────────────────────────────────────────────────

msg ""
exec "$BIN_PATH" setup
