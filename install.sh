#!/usr/bin/env bash
# FUTO Notes server installer.
#
#   curl -sSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/install.sh | bash
#
# Sets up a self-hosted FUTO Notes sync server using Docker. Idempotent — re-run
# to upgrade the image; password and Postgres credentials in .env are reused.
set -euo pipefail

GITLAB_HOST="${GITLAB_HOST:-https://gitlab.futo.org}"
PROJECT_PATH="futo-notes/futo-notes-server"
COMPOSE_REF="${FUTO_NOTES_REF:-main}"
DEFAULT_IMAGE="gitlab.futo.org:5050/futo-notes/futo-notes-server/server:stable"
IMAGE="${FUTO_NOTES_IMAGE:-$DEFAULT_IMAGE}"

INSTALL_DIR="${FUTO_NOTES_INSTALL_DIR:-$PWD}"
DATA_DIR=""
PORT="${FUTO_NOTES_PORT:-3005}"
PASSWORD=""
NON_INTERACTIVE=0

usage() {
  cat <<USAGE
FUTO Notes server installer

Usage: install.sh [options]

  --install-dir DIR   Where to write docker-compose.production.yml and .env
                      (default: current directory)
  --data-dir DIR      Where to store encrypted blobs and Postgres data
                      (default: <install-dir>/data)
  --port N            Host port to expose the server on (default: 3005)
  --password PW       Admin password (skips interactive prompt)
  --non-interactive   Fail rather than prompt; requires --password on a fresh install
  -h, --help          Show this help

Environment overrides:
  FUTO_NOTES_IMAGE   Server image to use (default: $DEFAULT_IMAGE)
  FUTO_NOTES_REF     Git ref to fetch docker-compose.production.yml from (default: main)
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --data-dir)    DATA_DIR="$2";    shift 2 ;;
    --port)        PORT="$2";        shift 2 ;;
    --password)    PASSWORD="$2";    shift 2 ;;
    --non-interactive) NON_INTERACTIVE=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    *) printf '  unknown option: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

msg() { printf '  %s\n' "$*"; }
err() { printf '  error: %s\n' "$*" >&2; }

# ─── Prereq checks ──────────────────────────────────────────────────
for cmd in curl docker; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    err "$cmd is required but not installed."
    [ "$cmd" = docker ] && err "Install Docker from https://docs.docker.com/get-docker/"
    exit 1
  fi
done

if ! docker compose version >/dev/null 2>&1; then
  err "Docker Compose v2 is required (run: docker compose version)."
  err "Install the docker-compose-plugin package."
  exit 1
fi

# ─── Resolve directories ────────────────────────────────────────────
mkdir -p "$INSTALL_DIR"
INSTALL_DIR="$(cd "$INSTALL_DIR" && pwd)"

ENV_FILE="$INSTALL_DIR/.env"
COMPOSE_FILE="$INSTALL_DIR/docker-compose.production.yml"

# Pick the data dir. Precedence: --data-dir flag > existing .env > prompt > default.
DEFAULT_DATA_DIR="$INSTALL_DIR/futo-notes-data"
if [ -z "$DATA_DIR" ] && [ -f "$ENV_FILE" ]; then
  EXISTING_DATA_DIR="$(grep -E '^FUTO_NOTES_DATA_DIR=' "$ENV_FILE" | head -n1 | cut -d= -f2- || true)"
  [ -n "$EXISTING_DATA_DIR" ] && DATA_DIR="$EXISTING_DATA_DIR"
fi
if [ -z "$DATA_DIR" ]; then
  if [ "$NON_INTERACTIVE" = 1 ]; then
    DATA_DIR="$DEFAULT_DATA_DIR"
  else
    TTY=/dev/tty
    if [ -r "$TTY" ] && [ -w "$TTY" ]; then
      printf '  Where should encrypted notes and Postgres data live?\n' >"$TTY"
      printf '  Data directory [%s]: ' "$DEFAULT_DATA_DIR" >"$TTY"
      IFS= read -r DATA_DIR <"$TTY"
      [ -z "$DATA_DIR" ] && DATA_DIR="$DEFAULT_DATA_DIR"
    else
      DATA_DIR="$DEFAULT_DATA_DIR"
    fi
  fi
fi

# Expand a leading ~ and resolve to an absolute path.
case "$DATA_DIR" in
  "~")    DATA_DIR="$HOME" ;;
  "~/"*)  DATA_DIR="$HOME/${DATA_DIR:2}" ;;
esac
mkdir -p "$DATA_DIR/blobs" "$DATA_DIR/postgres"
DATA_DIR="$(cd "$DATA_DIR" && pwd)"

cd "$INSTALL_DIR"

# ─── Compose file ───────────────────────────────────────────────────
if [ ! -f "$COMPOSE_FILE" ]; then
  msg "Downloading docker-compose.production.yml..."
  COMPOSE_URL="$GITLAB_HOST/$PROJECT_PATH/-/raw/$COMPOSE_REF/docker-compose.production.yml"
  if ! curl -sSfL "$COMPOSE_URL" -o "$COMPOSE_FILE"; then
    err "Failed to download $COMPOSE_URL"
    exit 1
  fi
fi

# ─── Pull server image ──────────────────────────────────────────────
msg "Pulling $IMAGE..."
docker pull -q "$IMAGE" >/dev/null

# ─── Reuse credentials from existing .env if present ────────────────
EXISTING_HASH=""
EXISTING_PG_PW=""
if [ -f "$ENV_FILE" ]; then
  EXISTING_HASH="$(grep -E '^FUTO_NOTES_PASSWORD_HASH=' "$ENV_FILE" | head -n1 | cut -d= -f2- || true)"
  EXISTING_PG_PW="$(grep -E '^POSTGRES_PASSWORD=' "$ENV_FILE" | head -n1 | cut -d= -f2- || true)"
fi

if [ -n "$EXISTING_PG_PW" ]; then
  POSTGRES_PASSWORD="$EXISTING_PG_PW"
else
  POSTGRES_PASSWORD="$(head -c 32 /dev/urandom | od -An -vtx1 | tr -d ' \n')"
fi

# ─── Admin password hash ────────────────────────────────────────────
if [ -n "$EXISTING_HASH" ]; then
  msg "Reusing existing admin password (delete .env to reset)."
  PASSWORD_HASH="$EXISTING_HASH"
else
  if [ -z "$PASSWORD" ]; then
    if [ "$NON_INTERACTIVE" = 1 ]; then
      err "--password is required on a fresh install when --non-interactive is set."
      exit 2
    fi
    TTY=/dev/tty
    if [ ! -r "$TTY" ] || [ ! -w "$TTY" ]; then
      err "Cannot prompt for password (no TTY available)."
      err "Pass --password PW or run install.sh in an interactive shell."
      exit 2
    fi
    while :; do
      printf '  Admin password: ' >"$TTY"
      IFS= read -rs PASSWORD <"$TTY"
      printf '\n' >"$TTY"
      printf '  Confirm:        ' >"$TTY"
      IFS= read -rs CONFIRM <"$TTY"
      printf '\n' >"$TTY"
      if [ -n "$PASSWORD" ] && [ "$PASSWORD" = "$CONFIRM" ]; then
        break
      fi
      err "Passwords did not match (or empty), try again."
    done
  fi

  msg "Hashing password..."
  PASSWORD_HASH="$(docker run --rm "$IMAGE" node dist/index.js hash "$PASSWORD")"
fi

# ─── Write .env ─────────────────────────────────────────────────────
umask 077
cat > "$ENV_FILE" <<EOF
# FUTO Notes server — generated by install.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)
# Keep this file private. POSTGRES_PASSWORD and FUTO_NOTES_PASSWORD_HASH grant full access.
FUTO_NOTES_PORT=$PORT
FUTO_NOTES_DATA_DIR=$DATA_DIR
FUTO_NOTES_IMAGE=$IMAGE
POSTGRES_PASSWORD=$POSTGRES_PASSWORD
FUTO_NOTES_PASSWORD_HASH=$PASSWORD_HASH
EOF
umask 022

# ─── Start the stack ────────────────────────────────────────────────
msg "Starting containers..."
docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" up -d

msg "Waiting for server to become healthy..."
HEALTH_URL="http://localhost:$PORT/health"
for i in $(seq 1 60); do
  if curl -sSf "$HEALTH_URL" >/dev/null 2>&1; then
    msg "Server is healthy."
    break
  fi
  if [ "$i" = 60 ]; then
    err "Server did not become healthy within 60s."
    err "Check logs: docker compose -f $COMPOSE_FILE logs"
    exit 1
  fi
  sleep 1
done

cat <<EOF

  FUTO Notes server is running.

    URL:       http://localhost:$PORT
    Data dir:  $DATA_DIR
    Compose:   $COMPOSE_FILE
    Env file:  $ENV_FILE

  Open FUTO Notes, go to Settings → Sync, and enter the URL above
  with the admin password you just set.

  Useful commands (run from $INSTALL_DIR):
    docker compose -f docker-compose.production.yml ps
    docker compose -f docker-compose.production.yml logs -f
    docker compose -f docker-compose.production.yml pull && \\
      docker compose -f docker-compose.production.yml up -d   # upgrade
    docker compose -f docker-compose.production.yml down

EOF
