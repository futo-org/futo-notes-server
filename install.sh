#!/bin/sh
# FUTO Notes server installer.
#
#   curl -fsSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/install.sh | sh
#
# Brings up a self-hosted, single-user E2EE sync server with Docker Compose:
# pulls the image, writes a private .env, starts Postgres +
# the server, and waits for it to come up.
#
# Interactive by default (prompts read from /dev/tty so they work under the
# pipe above). Every prompt also has an env-var override, so the same script
# runs fully non-interactively for CI/automation:
#
#   FUTO_ADMIN_PASSWORD=hunter2 FUTO_NOTES_DIR=/opt/futo-notes \
#     sh -c "$(curl -fsSL .../install.sh)"
#
# Env overrides:
#   FUTO_NOTES_DIR        install directory            (default: ~/futo-notes)
#   FUTO_NOTES_PORT       host port to expose          (default: 3005)
#   FUTO_ADMIN_PASSWORD   admin password (else prompt)
#   FUTO_NOTES_IMAGE      server image to run          (default: stable)
#   FUTO_NOTES_DATA_DIR   blob + Postgres data dir     (default: <dir>/futo-notes-data)
#   POSTGRES_PASSWORD     DB password                  (default: random)
#   FUTO_NOTES_COMPOSE_URL  compose file to fetch      (default: main on GitLab)

set -eu

DEFAULT_IMAGE="gitlab.futo.org:5050/futo-notes/futo-notes-server/server:stable"
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
ok()   { printf '%s✓%s %s\n' "$GREEN" "$OFF" "$*"; }
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
  [ -n "$_ans" ] && printf '%s' "$_ans" || printf '%s' "$_default"
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

gen_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    LC_ALL=C tr -dc 'a-f0-9' </dev/urandom | dd bs=1 count=64 2>/dev/null
  fi
}

# ---- preflight ------------------------------------------------------------

step "Checking prerequisites"

command -v docker >/dev/null 2>&1 || die "Docker is not installed. See https://docs.docker.com/engine/install/"

if docker compose version >/dev/null 2>&1; then
  COMPOSE="docker compose"
else
  die "Docker Compose v2 plugin not found. Install it: https://docs.docker.com/compose/install/"
fi

# The daemon may require elevated privileges (user not in the docker group).
DOCKER="docker"
if ! docker info >/dev/null 2>&1; then
  if command -v sudo >/dev/null 2>&1 && sudo docker info >/dev/null 2>&1; then
    DOCKER="sudo docker"
    COMPOSE="sudo docker compose"
    info "${DIM}(using sudo to reach the Docker daemon)${OFF}"
  else
    die "Cannot talk to the Docker daemon. Is it running, and can this user reach it?"
  fi
fi

command -v curl >/dev/null 2>&1 || die "curl is required."

ok "Docker and Compose are available"

# ---- gather configuration -------------------------------------------------

step "Configuration"

INSTALL_DIR="$(ask 'Install directory' "$INSTALL_DIR")"
PORT="$(ask 'Port to expose the server on' "$PORT")"

if [ -n "${FUTO_ADMIN_PASSWORD:-}" ]; then
  ADMIN_PASSWORD="$FUTO_ADMIN_PASSWORD"
elif [ "$INTERACTIVE" = "1" ]; then
  while :; do
    ADMIN_PASSWORD="$(ask_secret 'Admin password')"
    [ -z "$ADMIN_PASSWORD" ] && { info "Password cannot be empty."; continue; }
    _confirm="$(ask_secret 'Confirm admin password')"
    [ "$ADMIN_PASSWORD" = "$_confirm" ] && break
    info "Passwords did not match — try again."
  done
else
  die "FUTO_ADMIN_PASSWORD must be set when running non-interactively."
fi

# Docker Compose .env values cannot safely contain physical line breaks. Keep
# the supported form explicit instead of silently changing the credential.
CR="$(printf '\r')"
case "$ADMIN_PASSWORD" in
  *'
'*|*"$CR"*) die "Admin password must not contain newline characters." ;;
esac

DATA_DIR="${FUTO_NOTES_DATA_DIR:-$INSTALL_DIR/futo-notes-data}"
PG_PASSWORD="${POSTGRES_PASSWORD:-$(gen_secret)}"

# ---- install --------------------------------------------------------------

step "Installing into $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"
cd "$INSTALL_DIR"

info "Fetching compose file"
curl -fsSL "$COMPOSE_URL" -o docker-compose.yml || die "Could not download compose file from $COMPOSE_URL"

info "Pulling server image (this may take a minute)"
$DOCKER pull "$IMAGE" >/dev/null || die "Could not pull image $IMAGE"

info "Writing .env"
# Compose double-quoted dotenv values can represent every supported password
# character. Escape the three characters Compose interprets; single quotes and
# spaces remain literal.
ESCAPED_ADMIN_PASSWORD="$(printf '%s' "$ADMIN_PASSWORD" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/[$]/&&/g')"
umask 077
cat > .env <<EOF
# Generated by install.sh — keep this file private.
# Back up this file together with $DATA_DIR to back up your server.
FUTO_NOTES_PASSWORD="$ESCAPED_ADMIN_PASSWORD"
POSTGRES_PASSWORD=$PG_PASSWORD
FUTO_NOTES_PORT=$PORT
FUTO_NOTES_DATA_DIR=$DATA_DIR
FUTO_NOTES_IMAGE=$IMAGE
EOF
chmod 600 .env
umask 022

step "Starting the server"
$COMPOSE up -d || die "docker compose up failed"

# ---- wait for health ------------------------------------------------------

info "Waiting for the server to become healthy"
HEALTHY=0
i=0
while [ "$i" -lt 60 ]; do
  if curl -fsS "http://localhost:$PORT/health" >/dev/null 2>&1; then
    HEALTHY=1; break
  fi
  i=$((i + 1))
  sleep 1
done

if [ "$HEALTHY" != "1" ]; then
  printf '%s\n' "${RED}Server did not report healthy within 60s.${OFF}" >&2
  info "Recent logs:"
  $COMPOSE logs --tail 30 || true
  die "Check the logs above. Re-run '$COMPOSE up -d' once the issue is resolved."
fi

# ---- done -----------------------------------------------------------------

printf '\n'
ok "FUTO Notes server is up and running."
printf '\n'
info "${BOLD}Connect the app${OFF}"
info "  Open FUTO Notes → Settings → Sync and enter:"
info "    Server URL:  http://<this-server's-IP-or-hostname>:$PORT"
info "    Password:    the admin password you just set"
info "  (Use http://localhost:$PORT only when the app runs on this server.)"
printf '\n'
info "${BOLD}Manage it${OFF} (from $INSTALL_DIR)"
info "    $COMPOSE ps                 # status"
info "    $COMPOSE logs -f            # follow logs"
info "    $COMPOSE pull && $COMPOSE up -d   # upgrade"
info "    $COMPOSE down               # stop (data is preserved)"
printf '\n'
info "${BOLD}Your data${OFF}"
info "    Lives in $DATA_DIR — back it up along with $INSTALL_DIR/.env"
printf '\n'
info "${BOLD}Before exposing this to the internet${OFF}"
info "  • Put a TLS reverse proxy in front (Caddy, nginx, or Tailscale Funnel)."
info "  • The server rate-limits password login. If you use a reverse proxy, set"
info "    TRUST_PROXY=true so limits apply to each client's forwarded IP address."
printf '\n'
