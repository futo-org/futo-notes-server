#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo "usage: $0" >&2
  exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
client_repo=${FUTO_NOTES_CLIENT_REPO:-/home/justin/Developer/futo-notes}
postgres_container=${FUTO_POSTGRES_CONTAINER:-futo-notes-postgres}
postgres_host=${FUTO_POSTGRES_HOST:-localhost}
postgres_port=${FUTO_POSTGRES_PORT:-5433}
postgres_password=${FUTO_POSTGRES_PASSWORD:-futo_notes}
port=${FUTO_SQLITE_MIGRATION_PORT:-3075}
run_id="$(date +%s)_$$"
database="futo_notes_sqlite_migration_${run_id}"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/futo-notes-sqlite-migration.XXXXXX")
blob_dir="$scratch/blobs"
sqlite_path="$scratch/notes.db"
server_log="$scratch/server.log"
response_file="$scratch/response"
migration_output="$scratch/migration.out"
server_binary="$scratch/futo-notes-server"
server_pid=
RESPONSE=

mkdir -p "$blob_dir"

admin_psql() {
  if [[ "$postgres_container" == direct ]]; then
    PGPASSWORD="$postgres_password" psql -h "$postgres_host" -p "$postgres_port" \
      -v ON_ERROR_STOP=1 -U futo_notes -d futo_notes "$@"
  else
    docker exec "$postgres_container" psql -v ON_ERROR_STOP=1 -U futo_notes -d futo_notes "$@"
  fi
}

source_scalar() {
  local query=$1
  if [[ "$postgres_container" == direct ]]; then
    PGPASSWORD="$postgres_password" psql -h "$postgres_host" -p "$postgres_port" \
      -v ON_ERROR_STOP=1 -Atq -U futo_notes -d "$database" -c "$query"
  else
    docker exec "$postgres_container" psql -v ON_ERROR_STOP=1 -Atq -U futo_notes -d "$database" -c "$query"
  fi
}

stop_server() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill -INT "$server_pid" 2>/dev/null || true
    for _ in {1..50}; do
      kill -0 "$server_pid" 2>/dev/null || break
      sleep 0.1
    done
    if kill -0 "$server_pid" 2>/dev/null; then
      kill -TERM "$server_pid" 2>/dev/null || true
    fi
    wait "$server_pid" 2>/dev/null || true
  fi
  server_pid=
}

cleanup() {
  status=$?
  trap - EXIT INT TERM
  stop_server
  admin_psql \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$database' AND pid <> pg_backend_pid()" \
    -c "DROP DATABASE IF EXISTS \"$database\"" >/dev/null || true
  if [[ $status -ne 0 ]]; then
    echo "SQLite migration rehearsal failed; last server log:" >&2
    tail -150 "$server_log" >&2 || true
    echo "migration output:" >&2
    cat "$migration_output" >&2 2>/dev/null || true
  fi
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT INT TERM

required_commands=(cargo curl go jq rg)
if [[ "$postgres_container" == direct ]]; then
  required_commands+=(psql)
else
  required_commands+=(docker)
fi
for command in "${required_commands[@]}"; do
  if ! command -v "$command" >/dev/null; then
    echo "required command not found: $command" >&2
    exit 1
  fi
done
if [[ ! -d "$client_repo" ]]; then
  echo "set FUTO_NOTES_CLIENT_REPO to the FUTO Notes client checkout" >&2
  exit 1
fi
if curl --silent --fail --max-time 1 "http://127.0.0.1:$port/health" >/dev/null 2>&1; then
  echo "port $port is already serving a healthy process" >&2
  exit 1
fi

wait_healthy() {
  local label=$1
  for _ in {1..200}; do
    if ! kill -0 "$server_pid" 2>/dev/null; then
      echo "$label exited before becoming healthy" >&2
      exit 1
    fi
    if curl --silent --fail --max-time 1 "http://127.0.0.1:$port/health" >/dev/null; then
      return
    fi
    sleep 0.1
  done
  echo "$label did not become healthy" >&2
  exit 1
}

start_server() {
  local database_url=$1
  local label=$2
  : >"$server_log"
  (
    cd "$repo_root"
    exec env AUTH_MODE=dev PORT="$port" DATABASE_URL="$database_url" BLOB_DIR="$blob_dir" \
      COOKIE_SECURE=false DEV_UI=true BLOB_GC_ENABLED=false "$server_binary"
  ) >"$server_log" 2>&1 &
  server_pid=$!
  wait_healthy "$label"
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
    echo "$method $path: got HTTP $status, want $want; body: $RESPONSE" >&2
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
    echo "blob upload: got HTTP $status, want 201; body: $RESPONSE" >&2
    exit 1
  fi
}

assert_blob() {
  local token=$1
  local key=$2
  local want=$3
  local status
  status=$(curl --silent --show-error --output "$response_file" --write-out '%{http_code}' \
    --header "Authorization: Bearer $token" "http://127.0.0.1:$port/api/blobs/$key")
  if [[ "$status" != 200 || "$(<"$response_file")" != "$want" ]]; then
    echo "blob $key did not survive the migration" >&2
    exit 1
  fi
}

run_rust_case() {
  local test_name=$1
  echo "running Rust client case: $test_name"
  (
    cd "$client_repo"
    FUTO_TEST_SERVER="http://127.0.0.1:$port" \
      cargo test -q -p futo-notes-sync --test server_integration "$test_name" \
        -- --ignored --exact --test-threads=1
  )
}

echo "building Go server"
(cd "$repo_root" && GOTOOLCHAIN=auto go build -o "$server_binary" ./cmd/server)
admin_psql -c "CREATE DATABASE \"$database\"" >/dev/null
postgres_url="postgres://futo_notes:$postgres_password@$postgres_host:$postgres_port/$database"

echo "== populate Postgres =="
start_server "$postgres_url" "Go/Postgres source"
run_rust_case single_note_round_trip_and_cursor_advance
json_request POST /api/auth/dev/login 200 '' \
  '{"email":"sqlite-migration@example.invalid","name":"SQLite Migration"}'
source_token=$(jq -er '.token' <<<"$RESPONSE")
source_user=$(jq -er '.user.id' <<<"$RESPONSE")
json_request POST /api/collections 201 "$source_token"
source_collection=$(jq -er '.collection.id' <<<"$RESPONSE")
json_request PUT "/api/collections/$source_collection/key" 200 "$source_token" \
  '{"key_salt":"migration-salt","key_kdf":{"name":"argon2id","m":65536},"encrypted_vault_key":"migration-wrapped-key"}'

durable_bytes='pre-migration durable bytes'
upload_blob "$source_token" "$durable_bytes"
durable_key=$(jq -er '.key' <<<"$RESPONSE")
json_request POST "/api/collections/$source_collection/objects" 201 "$source_token" \
  "{\"blob_key\":\"$durable_key\",\"size_bytes\":${#durable_bytes}}" migration-durable-create
durable_object=$(jq -er '.object.id' <<<"$RESPONSE")

staged_bytes='pre-migration staged bytes'
upload_blob "$source_token" "$staged_bytes"
staged_key=$(jq -er '.key' <<<"$RESPONSE")
stop_server

echo "== convert the stopped Postgres install =="
DATABASE_URL="$postgres_url" BLOB_DIR="$blob_dir" \
  "$server_binary" migrate-to-sqlite -to "sqlite:$sqlite_path" >"$migration_output"
rg -q 'row counts, collection versions, blob bytes, integrity, foreign keys: ok' "$migration_output"
rg -q 'SQLite copy complete:' "$migration_output"

echo "== verify and write through SQLite =="
start_server "sqlite:$sqlite_path" "Go/SQLite target"
json_request GET /api/auth 200 "$source_token"
if [[ "$(jq -er '.user.id' <<<"$RESPONSE")" != "$source_user" ]]; then
  echo "pre-migration session changed user" >&2
  exit 1
fi
json_request GET "/api/collections/$source_collection/key" 200 "$source_token"
json_request GET "/api/collections/$source_collection/objects/$durable_object" 200 "$source_token"
assert_blob "$source_token" "$durable_key" "$durable_bytes"

json_request POST "/api/collections/$source_collection/objects" 201 "$source_token" \
  "{\"blob_key\":\"$staged_key\",\"size_bytes\":${#staged_bytes}}" migration-claim-staged
claimed_object=$(jq -er '.object.id' <<<"$RESPONSE")
if [[ -z "$claimed_object" ]]; then
  echo "staged blob was not claimable after migration" >&2
  exit 1
fi

updated_bytes='post-migration updated bytes'
upload_blob "$source_token" "$updated_bytes"
updated_key=$(jq -er '.key' <<<"$RESPONSE")
json_request PUT "/api/collections/$source_collection/objects/$durable_object" 200 "$source_token" \
  "{\"version\":2,\"blob_key\":\"$updated_key\",\"size_bytes\":${#updated_bytes}}" migration-sqlite-update
assert_blob "$source_token" "$updated_key" "$updated_bytes"

run_rust_case single_note_round_trip_and_cursor_advance
run_rust_case update_propagates
for job in sessions reconciliation mutation-results blob-gc; do
  json_request POST "/dev/jobs/$job" 200
done
stop_server

if [[ "$(source_scalar "SELECT version FROM objects WHERE id = '$durable_object'")" != 1 ]]; then
  echo "the untouched Postgres source changed after SQLite writes" >&2
  exit 1
fi

echo "Postgres-to-SQLite migration rehearsal passed"
