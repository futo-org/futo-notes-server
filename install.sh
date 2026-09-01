#!/bin/sh
# FUTO Notes server installer.
#
#   curl -fsSL https://notes.futo.tech/install-server.sh | sh
#
# Brings up a self-hosted, single-user end-to-end-encrypted sync server with
# Docker Compose: pulls the image, writes a private .env, starts the server,
# and waits for it to report healthy. Notes and blobs are stored encrypted;
# the server never sees their contents. There is no database server to run --
# metadata lives in SQLite inside the data directory.
#
# Interactive by default (prompts read from /dev/tty so they work under the
# pipe above). Every prompt also has an env-var override, so the same script
# runs fully non-interactively:
#
#   FUTO_NOTES_PASSWORD=hunter2 FUTO_NOTES_DIR=/opt/futo-notes \
#     sh -c "$(curl -fsSL https://notes.futo.tech/install-server.sh)"
#
# Env overrides:
#   FUTO_NOTES_DIR          install directory        (default: ~/futo-notes)
#   FUTO_NOTES_PORT         host port to expose      (default: 3005)
#   FUTO_NOTES_PASSWORD     sync password (else prompt)
#   FUTO_NOTES_DATA_DIR     data directory           (default: <dir>/futo-notes-data)
#   FUTO_NOTES_IMAGE        server image to run      (default: futotech/notes-server:stable)
#   FUTO_NOTES_COMPOSE_URL  compose file to fetch    (default: main on GitLab)

set -eu

DEFAULT_IMAGE="futotech/notes-server:stable"
DEFAULT_COMPOSE_URL="https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/docker-compose.production.yml"

IMAGE="${FUTO_NOTES_IMAGE:-$DEFAULT_IMAGE}"
COMPOSE_URL="${FUTO_NOTES_COMPOSE_URL:-$DEFAULT_COMPOSE_URL}"
PORT="${FUTO_NOTES_PORT:-3005}"
INSTALL_DIR="${FUTO_NOTES_DIR:-$HOME/futo-notes}"

# ---- output helpers -------------------------------------------------------

if [ -t 1 ]; then
  BOLD="$(printf '\033[1m')"; GREEN="$(printf '\033[0;32m')"
  RED="$(printf '\033[0;31m')"; DIM="$(printf '\033[0;2m')"; OFF="$(printf '\033[0m')"
else
  BOLD=""; GREEN=""; RED=""; DIM=""; OFF=""
fi

info() { printf '%s\n' "$*"; }
step() { printf '%s==>%s %s\n' "$BOLD" "$OFF" "$*"; }
ok()   { printf '%s\xe2\x9c\x93%s %s\n' "$GREEN" "$OFF" "$*"; }
die()  { printf '%serror:%s %s\n' "$RED" "$OFF" "$*" >&2; exit 1; }

# ---- interactivity --------------------------------------------------------

# We can prompt only if a controlling terminal is reachable. Under `curl | sh`
# stdin is the pipe, so prompts must read from /dev/tty directly. A mere
# `[ -r /dev/tty ]` lies in CI (the node exists but opening it fails with
# "No such device or address"), so actually try to open it read-write.
if (exec 3<>/dev/tty) 2>/dev/null; then
  INTERACTIVE=1
else
  INTERACTIVE=0
fi

# ask <prompt> <default> -> echoes the answer
ask() {
  _prompt="$1"; _default="$2"
  if [ "$INTERACTIVE" != "1" ]; then
    printf '%s' "$_default"; return
  fi
  if [ -n "$_default" ]; then
    printf '%s [%s]: ' "$_prompt" "$_default" >/dev/tty
  else
    printf '%s: ' "$_prompt" >/dev/tty
  fi
  IFS= read -r _ans </dev/tty || _ans=""
  if [ -n "$_ans" ]; then printf '%s' "$_ans"; else printf '%s' "$_default"; fi
}

# ask_secret <prompt> -> echoes the answer, input not displayed
ask_secret() {
  _prompt="$1"
  printf '%s: ' "$_prompt" >/dev/tty
  stty -echo </dev/tty 2>/dev/null || true
  IFS= read -r _sec </dev/tty || _sec=""
  stty echo </dev/tty 2>/dev/null || true
  printf '\n' >/dev/tty
  printf '%s' "$_sec"
}

# absolute <path> -> echoes the path resolved against the current directory
absolute() {
  case "$1" in
    /*) printf '%s' "$1" ;;
    *)  printf '%s/%s' "$(pwd)" "${1#./}" ;;
  esac
}

# ---- preflight ------------------------------------------------------------

step "Checking prerequisites"

command -v curl >/dev/null 2>&1 || die "curl is required."

command -v docker >/dev/null 2>&1 \
  || die "Docker is not installed. See https://docs.docker.com/engine/install/"

docker compose version >/dev/null 2>&1 \
  || die "Docker Compose v2 plugin not found. Install it: https://docs.docker.com/compose/install/"

COMPOSE="docker compose"
DOCKER="docker"

# The daemon may require elevated privileges (user not in the docker group).
if ! docker info >/dev/null 2>&1; then
  if command -v sudo >/dev/null 2>&1 && sudo docker info >/dev/null 2>&1; then
    DOCKER="sudo docker"
    COMPOSE="sudo docker compose"
    info "${DIM}(using sudo to reach the Docker daemon)${OFF}"
  else
    die "Cannot talk to the Docker daemon. Is it running, and can this user reach it?"
  fi
fi

ok "Docker and Compose are available"

# ---- gather configuration -------------------------------------------------

step "Configuration"

INSTALL_DIR="$(ask 'Install directory' "$INSTALL_DIR")"
[ -n "$INSTALL_DIR" ] || die "Install directory cannot be empty."
INSTALL_DIR="$(absolute "$INSTALL_DIR")"

PORT="$(ask 'Port to expose the server on' "$PORT")"
case "$PORT" in
  ''|*[!0-9]*) die "Port must be a number, got '$PORT'." ;;
esac

if [ -e "$INSTALL_DIR/.env" ]; then
  info "$INSTALL_DIR/.env already exists, so this looks like an existing install."
  info "To upgrade it instead, run:"
  info "    cd $INSTALL_DIR && $COMPOSE pull && $COMPOSE up -d"
  die "Refusing to overwrite $INSTALL_DIR/.env."
fi

if [ -n "${FUTO_NOTES_PASSWORD:-}" ]; then
  PASSWORD="$FUTO_NOTES_PASSWORD"
elif [ "$INTERACTIVE" = "1" ]; then
  while :; do
    PASSWORD="$(ask_secret 'Sync password')"
    if [ -z "$PASSWORD" ]; then
      info "Password cannot be empty."
      continue
    fi
    _confirm="$(ask_secret 'Confirm sync password')"
    [ "$PASSWORD" = "$_confirm" ] && break
    info "Passwords did not match - try again."
  done
else
  die "FUTO_NOTES_PASSWORD must be set when running non-interactively."
fi
[ -n "$PASSWORD" ] || die "FUTO_NOTES_PASSWORD cannot be empty."

# Compose .env values cannot represent a physical line break. Reject rather
# than silently storing a different credential than the one supplied.
CR="$(printf '\r')"
case "$PASSWORD" in
  *'
'*|*"$CR"*) die "Sync password must not contain newline characters." ;;
esac

DATA_DIR="${FUTO_NOTES_DATA_DIR:-$INSTALL_DIR/futo-notes-data}"
DATA_DIR="$(absolute "$DATA_DIR")"

# ---- install --------------------------------------------------------------

step "Installing into $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"
mkdir -p "$DATA_DIR"
cd "$INSTALL_DIR"

info "Fetching compose file"
curl -fsSL "$COMPOSE_URL" -o docker-compose.yml \
  || die "Could not download compose file from $COMPOSE_URL"

info "Writing .env"
# Compose double-quoted dotenv values can carry every allowed password
# character. Escape the three the parser interprets: a backslash, a double
# quote, and a dollar sign (doubled to mean a literal '$').
ESCAPED_PASSWORD="$(printf '%s' "$PASSWORD" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/[$]/&&/g')"
umask 077
cat > .env <<EOF
# Generated by install.sh - keep this file private.
# Back up this file together with $DATA_DIR to back up your server.
FUTO_NOTES_PASSWORD="$ESCAPED_PASSWORD"
FUTO_NOTES_DATA_DIR=$DATA_DIR
FUTO_NOTES_PORT=$PORT
FUTO_NOTES_IMAGE=$IMAGE
EOF
chmod 600 .env
umask 022

step "Pulling the server image (this may take a minute)"
$COMPOSE pull || die "docker compose pull failed"

step "Starting the server"
$COMPOSE up -d || die "docker compose up failed"

# ---- wait for health ------------------------------------------------------

info "Waiting for the server to become healthy"
HEALTHY=0
i=0
while [ "$i" -lt 60 ]; do
  if curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    HEALTHY=1; break
  fi
  i=$((i + 1))
  sleep 1
done

if [ "$HEALTHY" != "1" ]; then
  printf '%s\n' "${RED}Server did not report healthy within 60s.${OFF}" >&2
  info "Recent logs:"
  $COMPOSE logs --tail 30 || true
  info "Full logs: cd $INSTALL_DIR && $COMPOSE logs"
  die "Fix the problem above, then run '$COMPOSE up -d' from $INSTALL_DIR."
fi

# ---- done -----------------------------------------------------------------

HOST_ADDR="localhost"
if command -v hostname >/dev/null 2>&1; then
  _ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
  [ -n "$_ip" ] && HOST_ADDR="$_ip"
fi

printf '\n'
ok "FUTO Notes server is up and running."
printf '\n'
info "  Install directory:  $INSTALL_DIR"
info "  Data directory:     $DATA_DIR"
printf '\n'
info "${BOLD}Connect the app${OFF}"
info "  FUTO Notes -> Settings -> Self-hosted sync -> Server URL:"
info "      http://$HOST_ADDR:$PORT"
info "  Then sign in with the sync password you just set."
printf '\n'
info "${BOLD}Manage it${OFF} (from $INSTALL_DIR)"
info "    $COMPOSE ps                        # status"
info "    $COMPOSE logs -f                   # follow logs"
info "    $COMPOSE pull && $COMPOSE up -d    # upgrade"
info "    $COMPOSE down                      # stop (data is preserved)"
printf '\n'
info "${BOLD}Next steps${OFF}"
info "  1. Back up $DATA_DIR (and this install's .env). That directory is the"
info "     whole server: SQLite metadata and encrypted blobs."
info "  2. To reach this server from outside your LAN, put TLS in front of it"
info "     with Tailscale Funnel or Caddy. See the \"HTTPS for remote access\""
info "     section of the README:"
info "     https://gitlab.futo.org/futo-notes/futo-notes-server#https-for-remote-access"
printf '\n'
