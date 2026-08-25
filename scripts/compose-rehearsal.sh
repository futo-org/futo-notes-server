#!/usr/bin/env bash
set -euo pipefail

# Container-level adoption rehearsal. scripts/adoption-rehearsal.sh proves the
# swap with bare processes; this proves the *image and Compose contract* a Docker
# self-hoster actually upgrades through: one Compose project, one image tag, and
# the volumes stay put while the tag is re-pointed from TypeScript to Go and back.
#
# The swap is performed only by re-tagging the tag the Compose file names, which
# is the closest local stand-in for `docker compose pull && docker compose up -d`.
# The Compose file and .env are identical across all three phases.
#
# Run it by hand before a release. It is deliberately absent from .gitlab-ci.yml:
# the runners use docker-in-docker, where a bind mount named by this script would
# be created inside the daemon container rather than shared with the job, so the
# volume and uid-1000 asserts would inspect an empty directory. See the guard on
# DOCKER_HOST below.

usage() {
  echo "usage: $0" >&2
  exit 2
}
[[ $# -eq 0 ]] || usage

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ts_repo=${FUTO_TS_SERVER_REPO:-/home/justin/Developer/futo-notes-server}
port=${FUTO_COMPOSE_REHEARSAL_PORT:-3205}
wait_timeout=${FUTO_COMPOSE_WAIT_TIMEOUT:-180}
ts_image=futo-notes-rehearsal-ts:build
go_image=futo-notes-rehearsal-go:build
# The tag the Compose file resolves. Re-tagged in place to perform each swap.
swap_tag=futo-notes-rehearsal:stable
password=compose-rehearsal-password
run_id="$(date +%s)_$$"
project="futo-notes-compose-${run_id}"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/futo-notes-compose.XXXXXX")
data_dir="$scratch/data"
overlay="$scratch/compose.overlay.yml"
env_file="$scratch/rehearsal.env"
response_file="$scratch/response"
base_compose="$repo_root/docker-compose.postgres.yml"
project_started=false
current_container=
RESPONSE=

compose() {
  local args=(-p "$project" -f "$base_compose")
  if [[ -n "$overlay" ]]; then
    args+=(-f "$overlay")
  fi
  docker compose "${args[@]}" --env-file "$env_file" "$@"
}

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ $status -ne 0 && "$project_started" == true ]]; then
    echo "compose rehearsal failed; last server log:" >&2
    compose logs --no-color --tail 150 server >&2 2>/dev/null || true
    echo "compose rehearsal failed; last postgres log:" >&2
    compose logs --no-color --tail 40 postgres >&2 2>/dev/null || true
  fi
  if [[ "$project_started" == true ]]; then
    compose down -v --remove-orphans --timeout 20 >/dev/null 2>&1 || true
  fi
  docker rmi "$swap_tag" >/dev/null 2>&1 || true
  # Postgres writes its data dir as uid 999, which this user cannot unlink.
  if [[ -d "$data_dir" ]]; then
    docker run --rm --user 0:0 --volume "$scratch:/scratch" postgres:16 \
      rm -rf /scratch/data >/dev/null 2>&1 || true
  fi
  rm -rf -- "$scratch" 2>/dev/null || echo "could not remove scratch dir $scratch" >&2
  exit "$status"
}
trap cleanup EXIT INT TERM

for command in curl docker jq stat; do
  if ! command -v "$command" >/dev/null; then
    echo "required command not found: $command" >&2
    exit 1
  fi
done
if ! docker compose version >/dev/null 2>&1; then
  echo "docker compose v2 is required" >&2
  exit 1
fi
if [[ ! -d "$ts_repo" ]]; then
  echo "set FUTO_TS_SERVER_REPO to a local TypeScript server checkout" >&2
  exit 1
fi
if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
  exec 3>&-
  echo "port $port is already in use; free it or set FUTO_COMPOSE_REHEARSAL_PORT" >&2
  exit 1
fi
# Needs a daemon on this machine. Against a remote daemon (docker-in-docker, a
# Docker context over SSH) the bind mount resolves in the daemon's filesystem, so
# the volume and uid-1000 asserts would read an empty directory here, and the
# published port would not be on 127.0.0.1 either.
if [[ -n "${DOCKER_HOST:-}" && "${DOCKER_HOST}" != unix://* ]]; then
  echo "this rehearsal asserts on host paths and 127.0.0.1, so it needs a local daemon; DOCKER_HOST is $DOCKER_HOST" >&2
  exit 1
fi

pass() {
  echo "PASS  $1"
}

assert_eq() {
  local label=$1
  local got=$2
  local want=$3
  if [[ "$got" != "$want" ]]; then
    echo "FAIL  $label: got '$got', want '$want'" >&2
    exit 1
  fi
  pass "$label"
}

json_request() {
  local method=$1
  local path=$2
  local want=$3
  local token=${4:-}
  local body=${5:-}
  local mutation_id=${6:-}
  local args=(--silent --show-error --output "$response_file" --write-out '%{http_code}' --request "$method")
  args+=(--header 'Content-Type: application/json')
  if [[ -n "$token" ]]; then
    args+=(--header "Authorization: Bearer $token")
  fi
  if [[ -n "$mutation_id" ]]; then
    args+=(--header "Mutation-Id: $mutation_id")
  fi
  if [[ -n "$body" ]]; then
    args+=(--data "$body")
  fi
  local status
  status=$(curl "${args[@]}" "http://127.0.0.1:$port$path")
  RESPONSE=$(<"$response_file")
  if [[ "$status" != "$want" ]]; then
    echo "FAIL  $method $path: got HTTP $status, want $want; body: $RESPONSE" >&2
    exit 1
  fi
}

upload_blob() {
  local token=$1
  local payload=$2
  local status
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
  local label=$1
  local token=$2
  local key=$3
  local want=$4
  local status
  status=$(curl --silent --show-error --output "$response_file" --write-out '%{http_code}' \
    --header "Authorization: Bearer $token" "http://127.0.0.1:$port/api/blobs/$key")
  if [[ "$status" != 200 ]]; then
    echo "FAIL  $label: got HTTP $status, want 200" >&2
    exit 1
  fi
  assert_eq "$label" "$(<"$response_file")" "$want"
}

server_log() {
  compose logs --no-color --no-log-prefix server
}

assert_health() {
  local label=$1
  local status
  status=$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 5 \
    "http://127.0.0.1:$port/health")
  assert_eq "$label mapped host port $port serves /health" "$status" 200
  assert_eq "$label container healthcheck" \
    "$(docker inspect --format '{{.State.Health.Status}}' "$current_container")" healthy
}

# Brings the server service up on whatever image $swap_tag currently points at and
# records the new container id, which must differ from the previous one: that is
# the proof Compose really replaced the container rather than restarting it.
bring_up_server() {
  local label=$1
  local previous=$current_container
  compose up -d --wait --wait-timeout "$wait_timeout" >/dev/null
  current_container=$(compose ps -q server)
  if [[ -z "$current_container" ]]; then
    echo "FAIL  $label: no server container after compose up" >&2
    exit 1
  fi
  if [[ -n "$previous" ]]; then
    if [[ "$current_container" == "$previous" ]]; then
      echo "FAIL  $label: compose reused container $previous instead of recreating it" >&2
      exit 1
    fi
    pass "$label image change recreated the server container"
  fi
  assert_health "$label"
}

echo "== build images =="
docker build --tag "$ts_image" "$ts_repo" >/dev/null
echo "built TypeScript image $ts_image"
docker build --tag "$go_image" "$repo_root" >/dev/null
echo "built Go image $go_image"

# Mint the admin hash with the TypeScript image's own CLI, exactly as
# .env.production.example tells a self-hoster to. Go must verify this hash after
# the swap without a re-login or a rehash.
password_hash=$(docker run --rm "$ts_image" bun dist/index.js hash "$password")
if [[ "$password_hash" != scrypt:* ]]; then
  echo "TypeScript image did not mint a scrypt hash: $password_hash" >&2
  exit 1
fi

mkdir -p "$data_dir/blobs" "$data_dir/postgres"

# The only divergence from the preserved docker-compose.postgres.yml. A hoster
# upgrading in place still has the TypeScript-era Compose file on disk, and that
# one sets LOG_LEVEL — a variable the Go server deliberately drops. Layering it
# here rehearses the real upgrade and lets us assert the boot warning.
cat >"$overlay" <<'YAML'
services:
  server:
    environment:
      LOG_LEVEL: ${LOG_LEVEL:-info}
YAML

cat >"$env_file" <<ENV
POSTGRES_PASSWORD=compose-rehearsal-postgres
FUTO_NOTES_PASSWORD=
FUTO_NOTES_PASSWORD_HASH=$password_hash
FUTO_NOTES_IMAGE=$swap_tag
FUTO_NOTES_PORT=$port
FUTO_NOTES_DATA_DIR=$data_dir
LOG_LEVEL=info
ENV

echo
echo "== phase 1: TypeScript image up in project $project =="
docker tag "$ts_image" "$swap_tag"
project_started=true
bring_up_server "TypeScript"

echo
echo "== phase 2: seed real traffic against the TypeScript container =="
json_request POST /api/auth/password/login 200 '' "{\"password\":\"$password\"}"
ts_token=$(jq -er '.token' <<<"$RESPONSE")
user_id=$(jq -er '.user.id' <<<"$RESPONSE")
pass "TypeScript login issued a session token"

json_request POST /api/collections 201 "$ts_token"
collection_id=$(jq -er '.collection.id' <<<"$RESPONSE")
json_request PUT "/api/collections/$collection_id/key" 200 "$ts_token" \
  '{"key_salt":"compose-rehearsal-salt","key_kdf":{"name":"argon2id","m":65536,"t":3,"p":1},"encrypted_vault_key":"compose-rehearsal-wrapped-key"}'
json_request GET "/api/collections/$collection_id/key" 200 "$ts_token"
key_before=$(jq -cS '.key | del(.key_updated_at)' <<<"$RESPONSE")
assert_eq "TypeScript stored collection key material" \
  "$(jq -er '.key.key_salt' <<<"$RESPONSE")" compose-rehearsal-salt

durable_bytes='pre-swap durable bytes'
upload_blob "$ts_token" "$durable_bytes"
durable_key=$(jq -er '.key' <<<"$RESPONSE")
json_request POST "/api/collections/$collection_id/objects" 201 "$ts_token" \
  "{\"blob_key\":\"$durable_key\",\"size_bytes\":${#durable_bytes}}" compose-ts-create-a
object_a=$(jq -er '.object.id' <<<"$RESPONSE")
pre_swap_cursor=$(jq -er '.collectionVersion' <<<"$RESPONSE")

continuity_bytes='pre-swap continuity bytes'
upload_blob "$ts_token" "$continuity_bytes"
continuity_key=$(jq -er '.key' <<<"$RESPONSE")
json_request PUT "/api/collections/$collection_id/objects/$object_a" 200 "$ts_token" \
  "{\"version\":2,\"blob_key\":\"$continuity_key\",\"size_bytes\":${#continuity_bytes}}" compose-ts-update-a

tombstone_bytes='pre-swap tombstone bytes'
upload_blob "$ts_token" "$tombstone_bytes"
tombstone_key=$(jq -er '.key' <<<"$RESPONSE")
json_request POST "/api/collections/$collection_id/objects" 201 "$ts_token" \
  "{\"blob_key\":\"$tombstone_key\",\"size_bytes\":${#tombstone_bytes}}" compose-ts-create-b
object_b=$(jq -er '.object.id' <<<"$RESPONSE")
json_request DELETE "/api/collections/$collection_id/objects/$object_b?version=1" 200 "$ts_token" '' compose-ts-delete-b
pre_swap_head=$(jq -er '.collectionVersion' <<<"$RESPONSE")
pass "TypeScript seeded a cursor history through version $pre_swap_head"

assert_eq "TypeScript blob landed in the mounted volume" \
  "$([[ -f "$data_dir/blobs/$continuity_key" ]] && echo yes || echo no)" yes
assert_eq "TypeScript blob file is owned by uid 1000" \
  "$(stat -c '%u' "$data_dir/blobs/$continuity_key")" 1000

echo
echo "== phase 3: swap the tag to the Go image, same project and volumes =="
compose stop --timeout 20 server >/dev/null
docker tag "$go_image" "$swap_tag"
bring_up_server "Go"

echo
echo "== phase 4: post-swap asserts =="
go_log=$(server_log)
if grep -q 'applied database migrations' <<<"$go_log"; then
  echo "FAIL  Go applied migrations to a database written by the current TypeScript server" >&2
  grep 'applied database migrations' <<<"$go_log" >&2
  exit 1
fi
pass "Go applied zero migrations to the TypeScript-written database"
if ! grep -q 'LOG_LEVEL is ignored' <<<"$go_log"; then
  echo "FAIL  Go did not warn about the dropped LOG_LEVEL variable" >&2
  exit 1
fi
pass "Go warned that LOG_LEVEL is ignored"

json_request GET /api/auth 200 "$ts_token"
assert_eq "TypeScript-issued token still authenticates" "$(jq -er '.user.id' <<<"$RESPONSE")" "$user_id"

json_request POST /api/auth/password/login 200 '' "{\"password\":\"$password\"}"
go_token=$(jq -er '.token' <<<"$RESPONSE")
assert_eq "fresh login verifies the TypeScript-minted scrypt hash" \
  "$(jq -er '.user.id' <<<"$RESPONSE")" "$user_id"

json_request GET "/api/collections/$collection_id/key" 200 "$go_token"
assert_eq "collection key material survives the swap" \
  "$(jq -cS '.key | del(.key_updated_at)' <<<"$RESPONSE")" "$key_before"

json_request GET "/api/collections/$collection_id/objects?sinceVersion=$pre_swap_cursor" 200 "$ts_token"
assert_eq "pre-swap cursor delta size" "$(jq -er '.objects | length' <<<"$RESPONSE")" 2
assert_eq "updated object in the pre-swap delta" \
  "$(jq -er --arg id "$object_a" '[.objects[] | select(.id == $id and .version == "2" and .deleted == false)] | length' <<<"$RESPONSE")" 1
assert_eq "tombstone in the pre-swap delta" \
  "$(jq -er --arg id "$object_b" '[.objects[] | select(.id == $id and .deleted == true)] | length' <<<"$RESPONSE")" 1

assert_blob "pre-swap blob bytes round-trip through Go" "$go_token" "$continuity_key" "$continuity_bytes"

go_blob_bytes='post-swap go blob bytes'
upload_blob "$go_token" "$go_blob_bytes"
go_blob_key=$(jq -er '.key' <<<"$RESPONSE")
assert_eq "Go blob landed in the mounted volume" \
  "$([[ -f "$data_dir/blobs/$go_blob_key" ]] && echo yes || echo no)" yes
assert_eq "Go blob file is owned by uid 1000" \
  "$(stat -c '%u' "$data_dir/blobs/$go_blob_key")" 1000
assert_blob "Go blob bytes round-trip" "$go_token" "$go_blob_key" "$go_blob_bytes"

echo
echo "== phase 5: write through Go =="
rollback_cursor=$pre_swap_head
json_request POST "/api/collections/$collection_id/objects" 201 "$go_token" \
  "{\"blob_key\":\"$go_blob_key\",\"size_bytes\":${#go_blob_bytes}}" compose-go-create
go_object=$(jq -er '.object.id' <<<"$RESPONSE")

go_update_bytes='post-swap go update bytes'
upload_blob "$go_token" "$go_update_bytes"
go_update_key=$(jq -er '.key' <<<"$RESPONSE")
json_request PUT "/api/collections/$collection_id/objects/$go_object" 200 "$go_token" \
  "{\"version\":2,\"blob_key\":\"$go_update_key\",\"size_bytes\":${#go_update_bytes}}" compose-go-update

go_kept_bytes='post-swap go retained bytes'
upload_blob "$go_token" "$go_kept_bytes"
go_kept_key=$(jq -er '.key' <<<"$RESPONSE")
json_request POST "/api/collections/$collection_id/objects" 201 "$go_token" \
  "{\"blob_key\":\"$go_kept_key\",\"size_bytes\":${#go_kept_bytes}}" compose-go-create-kept
go_kept_object=$(jq -er '.object.id' <<<"$RESPONSE")

json_request DELETE "/api/collections/$collection_id/objects/$go_object?version=2" 200 "$go_token" '' compose-go-delete
post_go_head=$(jq -er '.collectionVersion' <<<"$RESPONSE")
pass "Go created, updated and deleted through version $post_go_head"

echo
echo "== phase 6: roll back to the TypeScript image =="
compose stop --timeout 20 server >/dev/null
docker tag "$ts_image" "$swap_tag"
bring_up_server "rollback TypeScript"

json_request GET /api/auth 200 "$ts_token"
assert_eq "token still authenticates after rollback" "$(jq -er '.user.id' <<<"$RESPONSE")" "$user_id"

json_request GET "/api/collections/$collection_id/objects?sinceVersion=$rollback_cursor" 200 "$ts_token"
assert_eq "rollback cursor delta size" "$(jq -er '.objects | length' <<<"$RESPONSE")" 2
assert_eq "Go tombstone visible to TypeScript" \
  "$(jq -er --arg id "$go_object" '[.objects[] | select(.id == $id and .version == "3" and .deleted == true)] | length' <<<"$RESPONSE")" 1
assert_eq "Go-created object visible to TypeScript" \
  "$(jq -er --arg id "$go_kept_object" '[.objects[] | select(.id == $id and .version == "1" and .deleted == false)] | length' <<<"$RESPONSE")" 1
assert_blob "Go-written blob bytes round-trip through TypeScript" "$ts_token" "$go_kept_key" "$go_kept_bytes"

rollback_bytes='post-rollback ts bytes'
upload_blob "$ts_token" "$rollback_bytes"
rollback_key=$(jq -er '.key' <<<"$RESPONSE")
json_request POST "/api/collections/$collection_id/objects" 201 "$ts_token" \
  "{\"blob_key\":\"$rollback_key\",\"size_bytes\":${#rollback_bytes}}" compose-ts-rollback-create
assert_eq "TypeScript still accepts writes after rollback" \
  "$(jq -er '.object.version' <<<"$RESPONSE")" 1
assert_blob "post-rollback blob bytes round-trip" "$ts_token" "$rollback_key" "$rollback_bytes"

echo
echo "== phase 7: tear down the upgrade project and boot a shipped SQLite new install =="
compose down -v --remove-orphans --timeout 20 >/dev/null
project_started=false
project="${project}-sqlite"
base_compose="$repo_root/docker-compose.production.yml"
overlay=
env_file="$scratch/sqlite.env"
data_dir="$scratch/sqlite-data"
current_container=
mkdir -p "$data_dir"
cat >"$env_file" <<ENV
FUTO_NOTES_PASSWORD=$password
FUTO_NOTES_IMAGE=$go_image
FUTO_NOTES_PORT=$port
FUTO_NOTES_DATA_DIR=$data_dir
ENV

project_started=true
bring_up_server "SQLite new install"
assert_eq "new install image selects SQLite" \
  "$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$current_container" | grep '^DATABASE_URL=' | cut -d= -f2-)" \
  sqlite:/data/db/notes.db

json_request POST /api/auth/password/login 200 '' "{\"password\":\"$password\"}"
sqlite_token=$(jq -er '.token' <<<"$RESPONSE")
json_request POST /api/collections 201 "$sqlite_token"
sqlite_collection=$(jq -er '.collection.id' <<<"$RESPONSE")
sqlite_bytes='sqlite new-install bytes'
upload_blob "$sqlite_token" "$sqlite_bytes"
sqlite_key=$(jq -er '.key' <<<"$RESPONSE")
json_request POST "/api/collections/$sqlite_collection/objects" 201 "$sqlite_token" \
  "{\"blob_key\":\"$sqlite_key\",\"size_bytes\":${#sqlite_bytes}}" compose-sqlite-create
sqlite_object=$(jq -er '.object.id' <<<"$RESPONSE")

assert_eq "SQLite database landed in the mounted /data volume" \
  "$([[ -s "$data_dir/db/notes.db" ]] && echo yes || echo no)" yes
assert_eq "SQLite blob landed in the mounted /data volume" \
  "$([[ -f "$data_dir/blobs/$sqlite_key" ]] && echo yes || echo no)" yes
assert_eq "SQLite database file is owned by uid 1000" \
  "$(stat -c '%u' "$data_dir/db/notes.db")" 1000

compose restart --timeout 20 server >/dev/null
compose up -d --wait --wait-timeout "$wait_timeout" >/dev/null
current_container=$(compose ps -q server)
assert_health "SQLite restart"
json_request GET /api/auth 200 "$sqlite_token"
json_request GET "/api/collections/$sqlite_collection/objects/$sqlite_object" 200 "$sqlite_token"
assert_eq "SQLite metadata survives restart" "$(jq -er '.object.id' <<<"$RESPONSE")" "$sqlite_object"
assert_blob "SQLite blob survives restart" "$sqlite_token" "$sqlite_key" "$sqlite_bytes"

echo
echo "compose rehearsal passed: TypeScript -> Go -> TypeScript plus SQLite new install"
