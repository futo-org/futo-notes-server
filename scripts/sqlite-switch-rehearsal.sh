#!/usr/bin/env bash
set -euo pipefail

# End-to-end rehearsal for migrate-to-sqlite.sh, the one-command switch a
# self-hoster runs. It builds a Postgres install that deliberately does *not*
# match the shipped compose file -- Postgres in a named volume, an installer-
# style data directory, a dropped LOG_LEVEL variable, an older image tag -- and
# then lets the script convert it, because that is the install shape most likely
# to lose data if the script assumed the shipped layout.
#
# Asserts, after one unattended run: the same host port serves /health from a
# single SQLite container, a session token minted before the switch still
# authenticates, note bytes and collection key material survive, SQLite accepts
# new writes and survives a restart, the backup is real, the Postgres volume is
# untouched, and the printed rollback command puts the install back on Postgres
# with its pre-switch contents.
#
# Run it by hand before a release. Like scripts/compose-rehearsal.sh it is kept
# out of CI: it asserts on host paths and 127.0.0.1, so it needs a local daemon.

usage() {
  echo "usage: $0" >&2
  exit 2
}
[[ $# -eq 0 ]] || usage

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
port=${FUTO_SQLITE_SWITCH_PORT:-3115}
password=sqlite-switch-rehearsal-password
run_id="$(date +%s)-$$"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/futo-notes-sqlite-switch.XXXXXX")
install_dir="$scratch/futo-notes-switch-$run_id"
data_dir="$install_dir/futo-notes-data"
response_file="$scratch/response"
# Two tags of the same build: the install runs "before", the script upgrades to
# "after", so the image swap in phase 1 is a real container replacement.
before_image=futo-notes-switch-before-$run_id:build
after_image=futo-notes-switch-after-$run_id:build
pg_volume="futo-notes-switch-pg-$run_id"
project=
RESPONSE=

icompose() { (cd "$install_dir" && docker compose "$@"); }

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ $status -ne 0 && -d "$install_dir" ]]; then
    echo "sqlite switch rehearsal failed; last server log:" >&2
    icompose logs --no-color --tail 150 >&2 2>/dev/null || true
  fi
  if [[ -d "$install_dir" ]]; then
    icompose down -v --remove-orphans --timeout 20 >/dev/null 2>&1 || true
  fi
  docker volume rm "$pg_volume" >/dev/null 2>&1 || true
  docker rmi "$before_image" "$after_image" >/dev/null 2>&1 || true
  # Postgres wrote nothing to the host here, but the container ran as uid 1000
  # under /data; remove the tree as root to be safe.
  docker run --rm --user 0:0 --volume "$scratch:/scratch" debian:bookworm-slim \
    rm -rf "/scratch/$(basename "$install_dir")" >/dev/null 2>&1 || true
  rm -rf -- "$scratch" 2>/dev/null || echo "could not remove scratch dir $scratch" >&2
  exit "$status"
}
trap cleanup EXIT INT TERM

for command in curl docker jq; do
  if ! command -v "$command" >/dev/null; then
    echo "required command not found: $command" >&2
    exit 1
  fi
done
if ! docker compose version >/dev/null 2>&1; then
  echo "docker compose v2 is required" >&2
  exit 1
fi
if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
  exec 3>&-
  echo "port $port is already in use; free it or set FUTO_SQLITE_SWITCH_PORT" >&2
  exit 1
fi
if [[ -n "${DOCKER_HOST:-}" && "${DOCKER_HOST}" != unix://* ]]; then
  echo "this rehearsal asserts on host paths and 127.0.0.1, so it needs a local daemon; DOCKER_HOST is $DOCKER_HOST" >&2
  exit 1
fi

pass() { echo "PASS  $1"; }

assert_eq() {
  local label=$1 got=$2 want=$3
  if [[ "$got" != "$want" ]]; then
    echo "FAIL  $label: got '$got', want '$want'" >&2
    exit 1
  fi
  pass "$label"
}

json_request() {
  local method=$1 path=$2 want=$3 token=${4:-} body=${5:-} mutation_id=${6:-}
  local args=(--silent --show-error --output "$response_file" --write-out '%{http_code}' --request "$method")
  args+=(--header 'Content-Type: application/json')
  [[ -n "$token" ]] && args+=(--header "Authorization: Bearer $token")
  [[ -n "$mutation_id" ]] && args+=(--header "Mutation-Id: $mutation_id")
  [[ -n "$body" ]] && args+=(--data "$body")
  local status
  status=$(curl "${args[@]}" "http://127.0.0.1:$port$path")
  RESPONSE=$(<"$response_file")
  if [[ "$status" != "$want" ]]; then
    echo "FAIL  $method $path: got HTTP $status, want $want; body: $RESPONSE" >&2
    exit 1
  fi
}

upload_blob() {
  local token=$1 payload=$2 status
  status=$(curl --silent --show-error --output "$response_file" --write-out '%{http_code}' \
    --request POST --header "Authorization: Bearer $token" \
    --header 'Content-Type: application/octet-stream' --data-binary "$payload" \
    "http://127.0.0.1:$port/api/blobs")
  RESPONSE=$(<"$response_file")
  if [[ "$status" != 201 ]]; then
    echo "FAIL  blob upload: got HTTP $status, want 201; body: $RESPONSE" >&2
    exit 1
  fi
}

assert_blob() {
  local label=$1 token=$2 key=$3 want=$4 status
  status=$(curl --silent --show-error --output "$response_file" --write-out '%{http_code}' \
    --header "Authorization: Bearer $token" "http://127.0.0.1:$port/api/blobs/$key")
  if [[ "$status" != 200 ]]; then
    echo "FAIL  $label: got HTTP $status, want 200" >&2
    exit 1
  fi
  assert_eq "$label" "$(<"$response_file")" "$want"
}

wait_healthy() {
  local label=$1
  for _ in $(seq 1 90); do
    if curl --silent --fail --max-time 2 "http://127.0.0.1:$port/health" >/dev/null; then
      return
    fi
    sleep 1
  done
  echo "FAIL  $label did not become healthy on port $port" >&2
  exit 1
}

server_image() {
  docker inspect --format '{{.Config.Image}}' "$(icompose ps -q server)"
}

container_database_url() {
  docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$(icompose ps -q server)" \
    | sed -n 's/^DATABASE_URL=//p' | head -n 1
}

echo "== build the server image under two tags =="
docker build --tag "$before_image" "$repo_root" >/dev/null
docker tag "$before_image" "$after_image"
echo "built $before_image and $after_image"

echo
echo "== stand up a Postgres install that does not match the shipped layout =="
mkdir -p "$data_dir/blobs"
docker volume create "$pg_volume" >/dev/null

# Not docker-compose.postgres.yml: Postgres data lives in an external named
# volume, and LOG_LEVEL is set the way a TypeScript-era install had it. Copying
# the shipped compose file over this one would point Postgres at an empty
# directory, which is exactly what the script must not do.
cat >"$install_dir/docker-compose.yml" <<YAML
services:
  server:
    image: \${FUTO_NOTES_IMAGE}
    restart: unless-stopped
    environment:
      DATABASE_URL: postgres://futo_notes:\${POSTGRES_PASSWORD}@postgres:5432/futo_notes
      PORT: "3000"
      AUTH_MODE: password
      FUTO_NOTES_PASSWORD: \${FUTO_NOTES_PASSWORD}
      COOKIE_SECURE: "false"
      BLOB_DIR: /data/blobs
      BLOB_GC_ENABLED: "false"
      LOG_LEVEL: info
    ports:
      - "\${FUTO_NOTES_PORT}:3000"
    volumes:
      - \${FUTO_NOTES_DATA_DIR}/blobs:/data/blobs
    depends_on:
      postgres:
        condition: service_healthy

  postgres:
    image: postgres:16
    restart: unless-stopped
    environment:
      POSTGRES_USER: futo_notes
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD}
      POSTGRES_DB: futo_notes
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U futo_notes -d futo_notes"]
      interval: 5s
      timeout: 3s
      retries: 15

volumes:
  pgdata:
    external: true
    name: $pg_volume
YAML

umask 077
cat >"$install_dir/.env" <<ENV
POSTGRES_PASSWORD=sqlite-switch-rehearsal-postgres
FUTO_NOTES_PASSWORD=$password
FUTO_NOTES_IMAGE=$before_image
FUTO_NOTES_PORT=$port
FUTO_NOTES_DATA_DIR=$data_dir
ENV
umask 022

icompose up -d --wait --wait-timeout 180 >/dev/null
project=$(icompose config --format json | jq -er '.name')
wait_healthy "Postgres install"
assert_eq "install starts on Postgres" "$(container_database_url)" \
  "postgres://futo_notes:sqlite-switch-rehearsal-postgres@postgres:5432/futo_notes"
assert_eq "install starts on the pre-upgrade image tag" "$(server_image)" "$before_image"

echo
echo "== seed notes through the Postgres install =="
json_request POST /api/auth/password/login 200 '' "{\"password\":\"$password\"}"
pg_token=$(jq -er '.token' <<<"$RESPONSE")
user_id=$(jq -er '.user.id' <<<"$RESPONSE")
json_request POST /api/collections 201 "$pg_token"
collection_id=$(jq -er '.collection.id' <<<"$RESPONSE")
json_request PUT "/api/collections/$collection_id/key" 200 "$pg_token" \
  '{"key_salt":"switch-rehearsal-salt","key_kdf":{"name":"argon2id","m":65536,"t":3,"p":1},"encrypted_vault_key":"switch-rehearsal-wrapped-key"}'
json_request GET "/api/collections/$collection_id/key" 200 "$pg_token"
key_before=$(jq -cS '.key | del(.key_updated_at)' <<<"$RESPONSE")

note_bytes='pre-switch note bytes'
upload_blob "$pg_token" "$note_bytes"
note_key=$(jq -er '.key' <<<"$RESPONSE")
json_request POST "/api/collections/$collection_id/objects" 201 "$pg_token" \
  "{\"blob_key\":\"$note_key\",\"size_bytes\":${#note_bytes}}" switch-pg-create
note_object=$(jq -er '.object.id' <<<"$RESPONSE")
pre_switch_version=$(jq -er '.collectionVersion' <<<"$RESPONSE")
pass "seeded one note through the Postgres install"

echo
echo "== run migrate-to-sqlite.sh unattended =="
FUTO_NOTES_YES=1 \
FUTO_NOTES_DIR="$install_dir" \
FUTO_NOTES_TARGET_IMAGE="$after_image" \
FUTO_NOTES_RAW_BASE="file://$repo_root" \
  sh "$repo_root/migrate-to-sqlite.sh"

echo
echo "== after the switch =="
wait_healthy "SQLite install"
assert_eq "the same host port serves /health" \
  "$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:$port/health")" 200
assert_eq "the server now runs on SQLite" "$(container_database_url)" sqlite:/data/db/notes.db
assert_eq "the server runs the upgraded image" "$(server_image)" "$after_image"
assert_eq "one container is left in the project" "$(icompose ps -q | wc -l)" 1
assert_eq "the database file landed in the data directory" \
  "$([[ -s "$data_dir/db/notes.db" ]] && echo yes || echo no)" yes
assert_eq "the working override file was removed" \
  "$([[ -e "$install_dir/docker-compose.go-image.yml" ]] && echo yes || echo no)" no
assert_eq ".env records the detected data directory" \
  "$(sed -n 's/^FUTO_NOTES_DATA_DIR=//p' "$install_dir/.env" | tr -d '"')" "$data_dir"

backup_dir=$(find "$install_dir" -maxdepth 1 -type d -name 'before-sqlite-*' | head -n 1)
assert_eq "a backup directory was created" "$([[ -n "$backup_dir" ]] && echo yes || echo no)" yes
assert_eq "the backup holds a non-empty Postgres dump" \
  "$([[ -s "$backup_dir/postgres.dump" ]] && echo yes || echo no)" yes
assert_eq "the backup holds the original compose file" \
  "$(grep -c 'external: true' "$backup_dir/docker-compose.yml")" 1
assert_eq "the Postgres volume still exists" \
  "$(docker volume inspect "$pg_volume" --format '{{.Name}}')" "$pg_volume"

json_request GET /api/auth 200 "$pg_token"
assert_eq "a token minted before the switch still authenticates" \
  "$(jq -er '.user.id' <<<"$RESPONSE")" "$user_id"
json_request GET "/api/collections/$collection_id/key" 200 "$pg_token"
assert_eq "collection key material survives the switch" \
  "$(jq -cS '.key | del(.key_updated_at)' <<<"$RESPONSE")" "$key_before"
json_request GET "/api/collections/$collection_id/objects/$note_object" 200 "$pg_token"
assert_eq "the seeded note survives the switch" "$(jq -er '.object.id' <<<"$RESPONSE")" "$note_object"
assert_blob "note bytes round-trip after the switch" "$pg_token" "$note_key" "$note_bytes"

json_request POST /api/auth/password/login 200 '' "{\"password\":\"$password\"}"
sqlite_token=$(jq -er '.token' <<<"$RESPONSE")
pass "the sync password carried over into the new .env"

sqlite_bytes='post-switch sqlite bytes'
upload_blob "$sqlite_token" "$sqlite_bytes"
sqlite_key=$(jq -er '.key' <<<"$RESPONSE")
json_request POST "/api/collections/$collection_id/objects" 201 "$sqlite_token" \
  "{\"blob_key\":\"$sqlite_key\",\"size_bytes\":${#sqlite_bytes}}" switch-sqlite-create
sqlite_object=$(jq -er '.object.id' <<<"$RESPONSE")
json_request GET "/api/collections/$collection_id/objects?sinceVersion=$pre_switch_version" 200 "$sqlite_token"
assert_eq "SQLite continues the collection version history" \
  "$(jq -er --arg id "$sqlite_object" '[.objects[] | select(.id == $id)] | length' <<<"$RESPONSE")" 1

icompose restart --timeout 20 server >/dev/null
wait_healthy "SQLite restart"
json_request GET "/api/collections/$collection_id/objects/$sqlite_object" 200 "$sqlite_token"
assert_blob "SQLite writes survive a restart" "$sqlite_token" "$sqlite_key" "$sqlite_bytes"

echo
echo "== a second run is a no-op =="
second_run=$(FUTO_NOTES_YES=1 FUTO_NOTES_DIR="$install_dir" \
  FUTO_NOTES_TARGET_IMAGE="$after_image" FUTO_NOTES_RAW_BASE="file://$repo_root" \
  sh "$repo_root/migrate-to-sqlite.sh")
if ! grep -q 'already runs on SQLite' <<<"$second_run"; then
  echo "FAIL  a second run did not recognize the SQLite install:" >&2
  echo "$second_run" >&2
  exit 1
fi
pass "a second run stops at 'already runs on SQLite'"
assert_eq "a second run created no extra backup" \
  "$(find "$install_dir" -maxdepth 1 -type d -name 'before-sqlite-*' | wc -l)" 1

echo
echo "== the printed rollback puts it back on Postgres =="
icompose down --timeout 20 >/dev/null
cp "$backup_dir/docker-compose.yml" "$backup_dir/.env" "$install_dir/"
icompose up -d --wait --wait-timeout 180 >/dev/null
wait_healthy "rolled-back Postgres install"
assert_eq "rollback runs on Postgres again" "$(container_database_url)" \
  "postgres://futo_notes:sqlite-switch-rehearsal-postgres@postgres:5432/futo_notes"
json_request GET "/api/collections/$collection_id/objects/$note_object" 200 "$pg_token"
assert_eq "Postgres still holds the pre-switch note" \
  "$(jq -er '.object.id' <<<"$RESPONSE")" "$note_object"
assert_blob "Postgres still serves the pre-switch note bytes" "$pg_token" "$note_key" "$note_bytes"
json_request GET "/api/collections/$collection_id/objects/$sqlite_object" 404 "$pg_token"
pass "Postgres was never written to during the switch"

echo
echo "sqlite switch rehearsal passed"
