#!/bin/sh
# FUTO Notes server: move an existing Postgres install to SQLite.
#
#   cd ~/futo-notes   # the directory holding docker-compose.yml and .env
#   curl -fsSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/migrate-to-sqlite.sh | sh
#
# Docker Compose installs only. A binary or systemd install has no compose file
# to rewrite; its manual steps are under "Doing it by hand" in the doc below.
#
# What it does, in order: back up Postgres and your configuration, upgrade the
# server container to the current image while still running on Postgres, pause
# so you can check that your notes sync, copy the database into SQLite, then
# restart on the single-container SQLite compose file.
#
# Nothing here modifies Postgres or your encrypted blob files, so the whole
# thing is reversible: the last thing it prints is the rollback command.
#
# Interactive by default (prompts read from /dev/tty so they work under the
# pipe above). Set FUTO_NOTES_YES=1 to run it unattended.
#
# Env overrides:
#   FUTO_NOTES_DIR           install directory        (default: current directory)
#   FUTO_NOTES_YES           1 skips both prompts
#   FUTO_NOTES_TARGET_IMAGE  image to upgrade to      (default: futotech/notes-server:stable)
#   FUTO_NOTES_RAW_BASE      where to fetch the compose file from

set -eu

DEFAULT_IMAGE="futotech/notes-server:stable"
DEFAULT_RAW_BASE="https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main"
MANUAL_DOC="https://gitlab.futo.org/futo-notes/futo-notes-server/-/blob/main/docs/Postgres%20to%20SQLite%20migration.md"

IMAGE="${FUTO_NOTES_TARGET_IMAGE:-$DEFAULT_IMAGE}"
RAW_BASE="${FUTO_NOTES_RAW_BASE:-$DEFAULT_RAW_BASE}"
INSTALL_DIR="${FUTO_NOTES_DIR:-$(pwd)}"

OVERRIDE_FILE=docker-compose.go-image.yml

# ---- output helpers -------------------------------------------------------

if [ -t 1 ]; then
  BOLD="$(printf '\033[1m')"; GREEN="$(printf '\033[0;32m')"
  RED="$(printf '\033[0;31m')"; DIM="$(printf '\033[0;2m')"; OFF="$(printf '\033[0m')"
else
  BOLD=""; GREEN=""; RED=""; DIM=""; OFF=""
fi

info() { printf '%s\n' "$*"; }
step() { printf '\n%s==>%s %s\n' "$BOLD" "$OFF" "$*"; }
ok()   { printf '%s\xe2\x9c\x93%s %s\n' "$GREEN" "$OFF" "$*"; }
die()  { printf '%serror:%s %s\n' "$RED" "$OFF" "$*" >&2; exit 1; }

# ---- interactivity --------------------------------------------------------

# Under `curl | sh` stdin is the pipe, so prompts must read from /dev/tty. A
# mere `[ -r /dev/tty ]` lies in CI, so actually try to open it read-write.
if [ "${FUTO_NOTES_YES:-}" = "1" ]; then
  INTERACTIVE=0
elif (exec 3<>/dev/tty) 2>/dev/null; then
  INTERACTIVE=1
else
  INTERACTIVE=0
fi

# confirm <question> -- returns non-zero if the answer was no
confirm() {
  [ "$INTERACTIVE" = "1" ] || return 0
  printf '%s [y/N]: ' "$1" >/dev/tty
  IFS= read -r _answer </dev/tty || _answer=""
  case "$_answer" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

pause() {
  [ "$INTERACTIVE" = "1" ] || return 0
  printf '%s' "$1" >/dev/tty
  IFS= read -r _ignored </dev/tty || true
}

# ---- docker helpers -------------------------------------------------------

compose() { $COMPOSE -f docker-compose.yml -f "$OVERRIDE_FILE" "$@"; }

# container_env <container> <name> -- echoes the value, empty when unset
container_env() {
  $DOCKER inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$1" \
    | sed -n "s/^$2=//p" | head -n 1
}

# host_path_for <container> <path in container> -- echoes the host path that
# backs it, empty when no bind mount covers it. Picks the longest matching
# mount so /data and /data/blobs both resolve correctly.
host_path_for() {
  $DOCKER inspect --format \
    '{{range .Mounts}}{{.Type}}|{{.Destination}}|{{.Source}}{{"\n"}}{{end}}' "$1" \
    | awk -F '|' -v target="$2" '
        $1 == "bind" && (target == $2 || index(target, $2 "/") == 1) {
          if (length($2) > length(best_dest)) { best_dest = $2; best_src = $3 }
        }
        END { if (best_dest != "") printf "%s%s", best_src, substr(target, length(best_dest) + 1) }'
}

wait_healthy() {
  _i=0
  while [ "$_i" -lt 90 ]; do
    if curl -fsS "http://127.0.0.1:$HOST_PORT/health" >/dev/null 2>&1; then
      return 0
    fi
    _i=$((_i + 1))
    sleep 1
  done
  return 1
}

# ---- .env helpers ---------------------------------------------------------

# set_env <key> <value> -- rewrites .env in place, keeping mode 600. Values are
# written double-quoted, escaping the three characters compose's dotenv parser
# interprets: a backslash, a double quote, and a dollar sign.
set_env() {
  _escaped="$(printf '%s' "$2" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/[$]/&&/g')"
  _tmp="$INSTALL_DIR/.env.migrate.$$"
  umask 077
  grep -v "^[[:space:]]*$1=" .env > "$_tmp" || true
  printf '%s="%s"\n' "$1" "$_escaped" >> "$_tmp"
  mv "$_tmp" .env
  chmod 600 .env
  umask 022
}

# ---- preflight ------------------------------------------------------------

step "Checking prerequisites"

command -v curl >/dev/null 2>&1 || die "curl is required."
command -v docker >/dev/null 2>&1 \
  || die "Docker is not installed, and this script converts Docker Compose installs. For a binary or systemd install, follow \"Doing it by hand\" in $MANUAL_DOC"
docker compose version >/dev/null 2>&1 \
  || die "Docker Compose v2 plugin not found. Install it: https://docs.docker.com/compose/install/"

COMPOSE="docker compose"
DOCKER="docker"
# What we tell the user to type: the same command without our own flags.
COMPOSE_HINT="docker compose"
if ! docker info >/dev/null 2>&1; then
  if command -v sudo >/dev/null 2>&1 && sudo docker info >/dev/null 2>&1; then
    DOCKER="sudo docker"
    COMPOSE="sudo docker compose"
    COMPOSE_HINT="sudo docker compose"
    info "${DIM}(using sudo to reach the Docker daemon)${OFF}"
  else
    die "Cannot talk to the Docker daemon. Is it running, and can this user reach it?"
  fi
fi

cd "$INSTALL_DIR" || die "No such directory: $INSTALL_DIR"
INSTALL_DIR="$(pwd)"
[ -f docker-compose.yml ] \
  || die "No docker-compose.yml in $INSTALL_DIR. Run this from your Compose install directory, or set FUTO_NOTES_DIR. For a binary or systemd install, follow \"Doing it by hand\" in $MANUAL_DOC"
[ -f .env ] \
  || die "No .env in $INSTALL_DIR. Run this from your Compose install directory, or set FUTO_NOTES_DIR."
if [ -e "$OVERRIDE_FILE" ]; then
  die "$INSTALL_DIR/$OVERRIDE_FILE already exists. A previous run stopped early; remove it and try again."
fi

# Compose reports container-by-container progress on stderr, which buries this
# script's own output. Quiet it where the flag exists.
if $COMPOSE --progress quiet version >/dev/null 2>&1; then
  COMPOSE="$COMPOSE --progress quiet"
fi

ok "Docker and Compose are available"

step "Inspecting the running install"

$COMPOSE config --services >/dev/null 2>&1 \
  || die "docker compose cannot read $INSTALL_DIR/docker-compose.yml with this .env. Fix that first."
$COMPOSE config --services | grep -qx server \
  || die "docker-compose.yml has no 'server' service, so this does not look like a FUTO Notes install."

# The server has to exist as a container to be inspected, and starting the
# install is a no-op when it is already up.
$COMPOSE up -d >/dev/null || die "docker compose up failed. Bring the install up by hand first."
SERVER_CONTAINER="$($COMPOSE ps -q server)"
[ -n "$SERVER_CONTAINER" ] || die "No server container after 'docker compose up -d'."

DATABASE_URL="$(container_env "$SERVER_CONTAINER" DATABASE_URL)"
case "$DATABASE_URL" in
  postgres://*|postgresql://*) ;;
  sqlite:*)
    ok "This install already runs on SQLite ($DATABASE_URL). Nothing to do."
    exit 0
    ;;
  "") die "The server container has no DATABASE_URL, so its database cannot be identified." ;;
  *) die "Unrecognized DATABASE_URL: $DATABASE_URL" ;;
esac

AUTH_MODE="$(container_env "$SERVER_CONTAINER" AUTH_MODE)"
case "$AUTH_MODE" in
  ''|password) ;;
  *) die "This install runs AUTH_MODE=$AUTH_MODE. This script only handles password-mode self-hosted installs." ;;
esac

CONTAINER_PORT="$(container_env "$SERVER_CONTAINER" PORT)"
[ -n "$CONTAINER_PORT" ] || CONTAINER_PORT=3000
HOST_PORT="$($COMPOSE port server "$CONTAINER_PORT" 2>/dev/null | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p' | head -n 1)"
[ -n "$HOST_PORT" ] || die "The server service does not publish container port $CONTAINER_PORT to a host port."

CONTAINER_BLOB_DIR="$(container_env "$SERVER_CONTAINER" BLOB_DIR)"
[ -n "$CONTAINER_BLOB_DIR" ] || CONTAINER_BLOB_DIR=/data/blobs
BLOB_DIR="$(host_path_for "$SERVER_CONTAINER" "$CONTAINER_BLOB_DIR")"
[ -n "$BLOB_DIR" ] \
  || die "The blob directory $CONTAINER_BLOB_DIR is not a bind mount from this host, so your notes would not survive the switch. Move it to a host directory first."
[ "$(basename "$BLOB_DIR")" = blobs ] \
  || die "Your blob files live in $BLOB_DIR. The SQLite compose file expects a directory named 'blobs' inside the data directory. Rename or move it first."
DATA_DIR="$(dirname "$BLOB_DIR")"

POSTGRES_SERVICE=""
for _service in $($COMPOSE config --services); do
  case "$_service" in
    postgres|db|database) POSTGRES_SERVICE="$_service"; break ;;
  esac
done
[ -n "$POSTGRES_SERVICE" ] \
  || die "docker-compose.yml has no Postgres service, but the server points at $DATABASE_URL. Back Postgres up by hand before switching."
POSTGRES_CONTAINER="$($COMPOSE ps -q "$POSTGRES_SERVICE")"
[ -n "$POSTGRES_CONTAINER" ] || die "The $POSTGRES_SERVICE service is not running."
POSTGRES_USER="$(container_env "$POSTGRES_CONTAINER" POSTGRES_USER)"
[ -n "$POSTGRES_USER" ] || POSTGRES_USER=postgres
POSTGRES_DB="$(container_env "$POSTGRES_CONTAINER" POSTGRES_DB)"
[ -n "$POSTGRES_DB" ] || POSTGRES_DB="$POSTGRES_USER"
# Reported at the end so the leftover Postgres data can be found, whether it is
# a host directory or a named volume.
POSTGRES_DATA="$($DOCKER inspect --format \
  '{{range .Mounts}}{{if eq .Destination "/var/lib/postgresql/data"}}{{if .Name}}the Docker volume {{.Name}}{{else}}{{.Source}}{{end}}{{end}}{{end}}' \
  "$POSTGRES_CONTAINER")"
[ -n "$POSTGRES_DATA" ] || POSTGRES_DATA="wherever your Postgres service stores it"

if [ -e "$DATA_DIR/db/notes.db" ]; then
  die "$DATA_DIR/db/notes.db already exists. Move it aside if you want to redo the copy."
fi

# Captured now because the server container is replaced twice below, and the
# new compose file declares far less than the old one did.
KEEP_PASSWORD="$(container_env "$SERVER_CONTAINER" FUTO_NOTES_PASSWORD)"
KEEP_PASSWORD_HASH="$(container_env "$SERVER_CONTAINER" FUTO_NOTES_PASSWORD_HASH)"
KEEP_COOKIE_SECURE="$(container_env "$SERVER_CONTAINER" COOKIE_SECURE)"
KEEP_BLOB_GC="$(container_env "$SERVER_CONTAINER" BLOB_GC_ENABLED)"

ok "Server on port $HOST_PORT, notes in $BLOB_DIR, database $POSTGRES_DB in the $POSTGRES_SERVICE service"

BACKUP_NAME="before-sqlite-$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="$INSTALL_DIR/$BACKUP_NAME"

info ""
info "About to switch this install to SQLite:"
info "  install directory   $INSTALL_DIR"
info "  data directory      $DATA_DIR"
info "  new database file   $DATA_DIR/db/notes.db"
info "  backup directory    $BACKUP_DIR"
info ""
info "The server restarts twice, so expect a minute of downtime. Postgres and"
info "your encrypted note files are only read, never modified."
if ! confirm "Continue?"; then
  die "Nothing was changed."
fi

# ---- back up --------------------------------------------------------------

step "Backing up"

mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"
cp .env "$BACKUP_DIR/.env"
cp docker-compose.yml "$BACKUP_DIR/docker-compose.yml"

$COMPOSE stop server >/dev/null
$COMPOSE exec -T "$POSTGRES_SERVICE" \
  pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc > "$BACKUP_DIR/postgres.dump" \
  || die "pg_dump failed. Nothing was changed except a stopped server; start it again with '$COMPOSE_HINT up -d'."
[ -s "$BACKUP_DIR/postgres.dump" ] || die "pg_dump wrote an empty file to $BACKUP_DIR/postgres.dump."

ok "Postgres dump, .env and docker-compose.yml saved to $BACKUP_DIR"
info "${DIM}Note files are not copied: nothing in this switch writes to $BLOB_DIR.${OFF}"

# ---- phase 1: current server, still on Postgres ---------------------------

step "Upgrading the server container (still on Postgres)"

cat > "$OVERRIDE_FILE" <<YAML
# Written by migrate-to-sqlite.sh. Points the server service at the current
# image while leaving every volume and variable of your install alone.
services:
  server:
    image: $IMAGE
YAML

# An offline host may already hold the image; only a missing image is fatal.
compose pull server >/dev/null 2>&1 \
  || $DOCKER image inspect "$IMAGE" >/dev/null 2>&1 \
  || die "Could not pull $IMAGE and it is not present locally."

compose up -d >/dev/null || die "docker compose up failed. Restore with: cp $BACKUP_DIR/docker-compose.yml docker-compose.yml && rm -f $OVERRIDE_FILE && $COMPOSE_HINT up -d"
wait_healthy || {
  compose logs --tail 30 server || true
  die "The server did not report healthy on port $HOST_PORT. Restore with: rm -f $OVERRIDE_FILE && $COMPOSE_HINT up -d"
}

ok "Running $IMAGE against your existing Postgres database"
info ""
info "Open the app and check that your notes are there and an edit syncs."
info "Nothing has been converted yet, so this is the safe place to stop:"
info "  ${DIM}rm -f $OVERRIDE_FILE && $COMPOSE_HINT up -d${OFF}"
pause "Press Enter to copy the database into SQLite, or Ctrl-C to stop here: "

# ---- phase 2: copy Postgres into SQLite -----------------------------------

step "Copying the database into SQLite"

mkdir -p "$DATA_DIR/db"
$COMPOSE stop server >/dev/null
compose run --rm --volume "$DATA_DIR/db:/data/db" server \
  futo-notes-server migrate-to-sqlite -to sqlite:/data/db/notes.db \
  || die "The copy failed and removed its half-written file. Postgres is untouched; go back with: rm -f $OVERRIDE_FILE && $COMPOSE_HINT up -d"

ok "Wrote $DATA_DIR/db/notes.db"

# ---- phase 3: switch to the SQLite compose file ---------------------------

step "Switching to the single-container SQLite server"

compose down >/dev/null
rm -f "$OVERRIDE_FILE"

curl -fsSL "$RAW_BASE/docker-compose.production.yml" -o docker-compose.yml.sqlite \
  || die "Could not download docker-compose.production.yml from $RAW_BASE. Your old compose file is still in place."
mv docker-compose.yml.sqlite docker-compose.yml

# Carry over what the server was actually running with, so nothing depends on
# variables the old compose file declared and the new one does not.
set_env FUTO_NOTES_DATA_DIR "$DATA_DIR"
set_env FUTO_NOTES_PORT "$HOST_PORT"
set_env FUTO_NOTES_IMAGE "$IMAGE"
if [ -n "$KEEP_PASSWORD" ]; then set_env FUTO_NOTES_PASSWORD "$KEEP_PASSWORD"; fi
if [ -n "$KEEP_PASSWORD_HASH" ]; then set_env FUTO_NOTES_PASSWORD_HASH "$KEEP_PASSWORD_HASH"; fi
if [ -n "$KEEP_COOKIE_SECURE" ]; then set_env COOKIE_SECURE "$KEEP_COOKIE_SECURE"; fi
if [ -n "$KEEP_BLOB_GC" ]; then set_env BLOB_GC_ENABLED "$KEEP_BLOB_GC"; fi

$COMPOSE up -d >/dev/null || die "docker compose up failed. Roll back with: cp $BACKUP_DIR/docker-compose.yml $BACKUP_DIR/.env . && $COMPOSE_HINT up -d"
wait_healthy || {
  $COMPOSE logs --tail 30 server || true
  die "The SQLite server did not report healthy on port $HOST_PORT. Roll back with: cp $BACKUP_DIR/docker-compose.yml $BACKUP_DIR/.env . && $COMPOSE_HINT up -d"
}

# ---- done -----------------------------------------------------------------

printf '\n'
ok "This server now runs on SQLite."
printf '\n'
info "  Database:  $DATA_DIR/db/notes.db"
info "  Notes:     $BLOB_DIR"
info "  Backup:    $BACKUP_DIR"
printf '\n'
info "Open the app, edit a note, and confirm it reaches another device."
printf '\n'
info "${BOLD}Back it up${OFF} by copying $DATA_DIR together with $INSTALL_DIR/.env."
info "There is no database server to dump any more."
printf '\n'
info "${BOLD}To go back to Postgres${OFF} (its data is exactly as you left it):"
info "    cd $INSTALL_DIR"
info "    $COMPOSE_HINT down"
info "    cp $BACKUP_NAME/docker-compose.yml $BACKUP_NAME/.env ."
info "    $COMPOSE_HINT up -d"
info "Edits made on SQLite stay in $DATA_DIR/db, so keep that directory."
printf '\n'
info "Your Postgres data is untouched in $POSTGRES_DATA."
info "Once you are sure you will not go back, delete it and the"
info "POSTGRES_PASSWORD line in .env."
printf '\n'
