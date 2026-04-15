#!/bin/sh
# Stonefruit server installer.
# Usage: curl -sSL https://gitlab.futo.org/stonefruit/stonefruit-e2ee-server-poc/-/raw/main/install.sh | sh
set -eu

GITLAB_HOST="${GITLAB_HOST:-https://gitlab.futo.org}"
PROJECT_PATH="stonefruit%2Fstonefruit-e2ee-server-poc"
BIN_DIR="${STONEFRUIT_BIN_DIR:-$HOME/.local/bin}"
BIN_PATH="$BIN_DIR/stonefruit"

msg() { printf '  %s\n' "$*"; }
err() { printf '  error: %s\n' "$*" >&2; }

# ── Checks ─────────────────────────────────────────────────────────────

if ! command -v node >/dev/null 2>&1; then
  err "Node.js is required but not installed."
  err "Install Node.js 20+ from https://nodejs.org/ or your package manager:"
  err "  macOS:  brew install node"
  err "  Linux:  sudo apt install nodejs  (or dnf, or your distro's equivalent)"
  exit 1
fi

NODE_MAJOR=$(node --version | sed 's/^v//' | cut -d. -f1)
if [ "$NODE_MAJOR" -lt 20 ]; then
  err "Node.js 20 or newer is required. Detected: $(node --version)"
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  err "Docker is required but not installed."
  err "Install Docker from https://docs.docker.com/get-docker/"
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  err "curl is required but not installed."
  exit 1
fi

# ── Resolve latest release ─────────────────────────────────────────────

msg "Fetching latest release..."
RELEASES_URL="$GITLAB_HOST/api/v4/projects/$PROJECT_PATH/releases"
RELEASE_JSON=$(curl -sSf "$RELEASES_URL?per_page=1")

if [ -z "$RELEASE_JSON" ] || [ "$RELEASE_JSON" = "[]" ]; then
  err "No releases found at $RELEASES_URL"
  exit 1
fi

# Extract tag_name from first release. Avoids jq dependency.
TAG=$(printf '%s' "$RELEASE_JSON" | node -e '
  let d = "";
  process.stdin.on("data", c => d += c);
  process.stdin.on("end", () => {
    const r = JSON.parse(d);
    if (!r[0] || !r[0].tag_name) { process.exit(1); }
    process.stdout.write(r[0].tag_name);
  });
')

if [ -z "$TAG" ]; then
  err "Could not resolve latest release tag"
  exit 1
fi

msg "Installing CLI $TAG..."
DOWNLOAD_URL="$GITLAB_HOST/api/v4/projects/$PROJECT_PATH/packages/generic/stonefruit-cli/$TAG/stonefruit-cli.mjs"

mkdir -p "$BIN_DIR"
if ! curl -sSfL "$DOWNLOAD_URL" -o "$BIN_PATH.tmp"; then
  err "Failed to download CLI from $DOWNLOAD_URL"
  exit 1
fi
mv "$BIN_PATH.tmp" "$BIN_PATH"
chmod +x "$BIN_PATH"

msg "Installed to $BIN_PATH"

# ── PATH warning ───────────────────────────────────────────────────────

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *)
    msg ""
    msg "Note: $BIN_DIR is not on your PATH."
    msg "Add to your shell profile (e.g. ~/.bashrc or ~/.zshrc):"
    msg "  export PATH=\"\$HOME/.local/bin:\$PATH\""
    msg ""
    ;;
esac

# ── Run setup ──────────────────────────────────────────────────────────

msg ""
msg "Starting setup..."
msg ""
exec "$BIN_PATH" setup
