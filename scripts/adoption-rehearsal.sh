#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 [latest|old|all]" >&2
  exit 2
}

variant=${1:-all}
case "$variant" in
  latest|old|all) ;;
  *) usage ;;
esac
[[ $# -le 1 ]] || usage

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ts_repo=${FUTO_TS_SERVER_REPO:-/home/justin/Developer/futo-notes-server}
client_repo=${FUTO_NOTES_CLIENT_REPO:-/home/justin/Developer/futo-notes}
postgres_container=${FUTO_POSTGRES_CONTAINER:-futo-notes-postgres}
postgres_host=${FUTO_POSTGRES_HOST:-localhost}
postgres_port=${FUTO_POSTGRES_PORT:-5433}
postgres_password=${FUTO_POSTGRES_PASSWORD:-futo_notes}
old_ts_commit=${FUTO_OLD_TS_COMMIT:-877fe8d20120f4b350572b5aec3479e62f92b2d2}
port=${FUTO_ADOPTION_PORT:-3065}
# Must match crates/futo-notes-sync/tests/common/mod.rs so the same Rust client
# cases can exercise password-mode adoption.
password=integration-test-password
run_id="$(date +%s)_$$"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/futo-notes-adoption.XXXXXX")
blob_dir="$scratch/blobs"
server_log="$scratch/server.log"
response_file="$scratch/response"
go_server="$scratch/futo-notes-server"
server_pid=
active_database=
RESPONSE=

mkdir -p "$blob_dir"

cleanup() {
  status=$?
  trap - EXIT INT TERM
  stop_server
  if [[ -n "$active_database" ]]; then
    drop_database "$active_database" || true
  fi
  if [[ $status -ne 0 ]]; then
    echo "adoption rehearsal failed; last server log:" >&2
    tail -150 "$server_log" >&2 || true
  fi
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT INT TERM

required_commands=(bun cargo curl git go jq rg tar)
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
if [[ ! -d "$ts_repo" || ! -d "$client_repo" ]]; then
  echo "set FUTO_TS_SERVER_REPO and FUTO_NOTES_CLIENT_REPO to local checkouts" >&2
  exit 1
fi
if curl --silent --fail --max-time 1 "http://127.0.0.1:$port/health" >/dev/null 2>&1; then
  echo "port $port is already serving a healthy process; stop it or set FUTO_ADOPTION_PORT" >&2
  exit 1
fi

admin_psql() {
  if [[ "$postgres_container" == direct ]]; then
    PGPASSWORD="$postgres_password" psql -h "$postgres_host" -p "$postgres_port" \
      -v ON_ERROR_STOP=1 -U futo_notes -d futo_notes "$@"
  else
    docker exec "$postgres_container" psql -v ON_ERROR_STOP=1 -U futo_notes -d futo_notes "$@"
  fi
}

db_psql() {
  if [[ "$postgres_container" == direct ]]; then
    PGPASSWORD="$postgres_password" psql -h "$postgres_host" -p "$postgres_port" \
      -v ON_ERROR_STOP=1 -U futo_notes -d "$active_database" "$@"
  else
    docker exec -i "$postgres_container" psql -v ON_ERROR_STOP=1 -U futo_notes -d "$active_database" "$@"
  fi
}

scalar() {
  db_psql -Atq -c "$1" | tr -d '[:space:]'
}

assert_eq() {
  local label=$1
  local got=$2
  local want=$3
  if [[ "$got" != "$want" ]]; then
    echo "$label: got '$got', want '$want'" >&2
    exit 1
  fi
}

create_database() {
  active_database=$1
  admin_psql -c "CREATE DATABASE \"$active_database\"" >/dev/null
}

drop_database() {
  local database=$1
  admin_psql \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$database' AND pid <> pg_backend_pid()" \
    -c "DROP DATABASE IF EXISTS \"$database\"" >/dev/null
  active_database=
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

start_ts() {
  local source=$1
  local label=$2
  : >"$server_log"
  (
    cd "$source"
    exec env AUTH_MODE=password PORT="$port" DATABASE_URL="postgres://futo_notes:$postgres_password@$postgres_host:$postgres_port/$active_database" \
      BLOB_DIR="$blob_dir" COOKIE_SECURE=false BLOB_GC_ENABLED=false LOG_LEVEL=warn \
      FUTO_NOTES_PASSWORD= FUTO_NOTES_PASSWORD_HASH="$password_hash" bun src/index.ts
  ) >"$server_log" 2>&1 &
  server_pid=$!
  wait_healthy "$label"
}

start_go() {
  : >"$server_log"
  (
    cd "$repo_root"
    exec env AUTH_MODE=password PORT="$port" DATABASE_URL="postgres://futo_notes:$postgres_password@$postgres_host:$postgres_port/$active_database" \
      BLOB_DIR="$blob_dir" COOKIE_SECURE=false BLOB_GC_ENABLED=false DEV_UI=true \
      FUTO_NOTES_PASSWORD= FUTO_NOTES_PASSWORD_HASH="$password_hash" "$go_server"
  ) >"$server_log" 2>&1 &
  server_pid=$!
  wait_healthy "Go server"
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
  assert_eq "download $key status" "$status" 200
  assert_eq "download $key bytes" "$(<"$response_file")" "$want"
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

seed_ts_traffic() {
  run_rust_case single_note_round_trip_and_cursor_advance
  run_rust_case update_propagates
  run_rust_case delete_propagates_as_tombstone

  json_request POST /api/auth/password/login 200 '' "{\"password\":\"$password\"}"
  ts_token=$(jq -er '.token' <<<"$RESPONSE")
  user_id=$(jq -er '.user.id' <<<"$RESPONSE")
  json_request POST /api/collections 200 "$ts_token"
  collection_id=$(jq -er '.collection.id' <<<"$RESPONSE")
  json_request GET "/api/collections/$collection_id/key" 200 "$ts_token"
  key_before=$(jq -cS '.key | del(.key_updated_at)' <<<"$RESPONSE")
  if [[ "$key_before" == null ]]; then
    echo "Rust client did not establish collection key material" >&2
    exit 1
  fi

  upload_blob "$ts_token" 'aged retained bytes'
  retained_key=$(jq -er '.key' <<<"$RESPONSE")
  json_request POST "/api/collections/$collection_id/objects" 201 "$ts_token" \
    "{\"blob_key\":\"$retained_key\",\"size_bytes\":19}" rehearsal-create-durable
  object_a=$(jq -er '.object.id' <<<"$RESPONSE")
  pre_swap_cursor=$(jq -er '.collectionVersion' <<<"$RESPONSE")

  upload_blob "$ts_token" 'continuity blob bytes'
  continuity_key=$(jq -er '.key' <<<"$RESPONSE")
  json_request PUT "/api/collections/$collection_id/objects/$object_a" 200 "$ts_token" \
    "{\"version\":2,\"blob_key\":\"$continuity_key\",\"size_bytes\":21}" rehearsal-update-aged

  upload_blob "$ts_token" 'delete candidate bytes'
  delete_key=$(jq -er '.key' <<<"$RESPONSE")
  json_request POST "/api/collections/$collection_id/objects" 201 "$ts_token" \
    "{\"blob_key\":\"$delete_key\",\"size_bytes\":22}" rehearsal-create-delete
  object_b=$(jq -er '.object.id' <<<"$RESPONSE")
  json_request DELETE "/api/collections/$collection_id/objects/$object_b?version=1" 200 "$ts_token" '' rehearsal-delete-fresh

  upload_blob "$ts_token" 'aged staged bytes'
  staged_key=$(jq -er '.key' <<<"$RESPONSE")
}

plant_aged_rows() {
  if [[ "$(scalar "SELECT count(*) FROM blob_ledger WHERE blob_key = '$retained_key'")" != 1 ]]; then
    echo "retained fixture ledger row is missing before aging: $retained_key" >&2
    db_psql -At -c "SELECT blob_key, state FROM blob_ledger WHERE user_id = '$user_id' ORDER BY created_at DESC LIMIT 10" >&2
    exit 1
  fi
  db_psql -q <<SQL
UPDATE blob_ledger
SET state = 'retained', object_id = NULL, object_version = NULL,
    state_changed_at = now() - interval '366 days'
WHERE blob_key = '$retained_key';

INSERT INTO blob_ledger (blob_key, user_id, size_bytes, state, state_changed_at)
VALUES ('$staged_key', '$user_id', 17, 'staged', now() - interval '25 hours')
ON CONFLICT (blob_key) DO UPDATE
SET state = 'staged', state_changed_at = excluded.state_changed_at;

INSERT INTO mutation_results
  (user_id, mutation_id, kind, collection_id, result, created_at)
VALUES
  ('$user_id', 'rehearsal-create-durable', 'create', '$collection_id', '{"status":"201"}', now() - interval '31 days'),
  ('$user_id', 'rehearsal-update-aged', 'update', '$collection_id', '{"status":"409","error":"version conflict"}', now() - interval '31 days'),
  ('$user_id', 'rehearsal-pending-aged', 'update', '$collection_id', '{"status":"pending"}', now() - interval '25 hours'),
  ('$user_id', 'rehearsal-delete-fresh', 'delete', '$collection_id', '{"status":"200"}', now())
ON CONFLICT (user_id, mutation_id) DO UPDATE
SET kind = excluded.kind, collection_id = excluded.collection_id,
    result = excluded.result, created_at = excluded.created_at;

INSERT INTO sessions (id, user_id, access_token_hash, expires_at)
VALUES ('11111111-1111-4111-8111-111111111111', '$user_id', decode(repeat('ab', 32), 'hex'), now() - interval '1 hour')
ON CONFLICT (id) DO UPDATE SET expires_at = excluded.expires_at;
SQL

  local aged_retained
  aged_retained=$(scalar "SELECT count(*) FROM blob_ledger WHERE blob_key = '$retained_key' AND state = 'retained' AND state_changed_at < now() - interval '365 days'")
  if [[ "$aged_retained" != 1 ]]; then
    echo "retained row did not age as expected" >&2
    db_psql -x -c "SELECT blob_key, state, state_changed_at, now(), now() - interval '365 days' AS cutoff FROM blob_ledger WHERE blob_key = '$retained_key'" >&2
    exit 1
  fi
  assert_eq "aged staged row" "$(scalar "SELECT count(*) FROM blob_ledger WHERE blob_key = '$staged_key' AND state = 'staged' AND state_changed_at < now() - interval '24 hours'")" 1
  assert_eq "all GC-eligible rows" "$(scalar "SELECT count(*) FROM blob_ledger WHERE (state = 'staged' AND state_changed_at < now() - interval '24 hours') OR (state = 'retained' AND state_changed_at < now() - interval '365 days') OR state = 'purgeable'")" 2
}

verify_swap_continuity() {
  json_request GET /api/auth 200 "$ts_token"
  assert_eq "TS session user after Go swap" "$(jq -er '.user.id' <<<"$RESPONSE")" "$user_id"

  json_request POST /api/auth/password/login 200 '' "{\"password\":\"$password\"}"
  go_token=$(jq -er '.token' <<<"$RESPONSE")
  json_request GET "/api/collections/$collection_id/key" 200 "$go_token"
  assert_eq "collection key after swap" "$(jq -cS '.key | del(.key_updated_at)' <<<"$RESPONSE")" "$key_before"

  json_request GET "/api/collections/$collection_id/objects?sinceVersion=$pre_swap_cursor" 200 "$ts_token"
  assert_eq "pre-swap cursor delta size" "$(jq -er '.objects | length' <<<"$RESPONSE")" 2
  assert_eq "updated object in cursor delta" "$(jq -er --arg id "$object_a" '[.objects[] | select(.id == $id and .version == "2" and .deleted == false)] | length' <<<"$RESPONSE")" 1
  assert_eq "deleted object in cursor delta" "$(jq -er --arg id "$object_b" '[.objects[] | select(.id == $id and .deleted == true)] | length' <<<"$RESPONSE")" 1
  assert_blob "$ts_token" "$continuity_key" 'continuity blob bytes'
  run_rust_case single_note_round_trip_and_cursor_advance
}

audit_jobs() {
  mkdir -p "$blob_dir/$user_id"
  printf '%s' 'reconciliation bytes' >"$blob_dir/$user_id/reconciliation-fixture"

  json_request POST /dev/jobs/sessions 200
  assert_eq "session reaper result" "$(jq -cS . <<<"$RESPONSE")" '{"reaped":1}'

  json_request POST /dev/jobs/reconciliation 200
  assert_eq "storage reconciliation result" "$(jq -cS . <<<"$RESPONSE")" '{"adopted":1,"cap_hit":false,"skipped":0}'

  json_request POST /dev/jobs/mutation-results 200
  assert_eq "mutation expiry result" "$(jq -cS . <<<"$RESPONSE")" '{"other_expired":1,"pending_expired":1}'

  json_request POST /dev/jobs/blob-gc 200
  assert_eq "blob GC result" "$(jq -cS . <<<"$RESPONSE")" '{"files_removed":2,"rows_purged":2}'

  assert_eq "GC-eligible rows after collection" "$(scalar "SELECT count(*) FROM blob_ledger WHERE (state = 'staged' AND state_changed_at < now() - interval '24 hours') OR (state = 'retained' AND state_changed_at < now() - interval '365 days') OR state = 'purgeable'")" 0
  assert_eq "durable create result retained" "$(scalar "SELECT count(*) FROM mutation_results WHERE user_id = '$user_id' AND mutation_id = 'rehearsal-create-durable'")" 1
  assert_eq "fresh mutation result retained" "$(scalar "SELECT count(*) FROM mutation_results WHERE user_id = '$user_id' AND mutation_id = 'rehearsal-delete-fresh'")" 1
  assert_eq "reconciled row retained" "$(scalar "SELECT count(*) FROM blob_ledger WHERE blob_key = '$user_id/reconciliation-fixture' AND state = 'staged'")" 1
  [[ ! -e "$blob_dir/$retained_key" ]] || { echo "GC left aged retained file" >&2; exit 1; }
  [[ ! -e "$blob_dir/$staged_key" ]] || { echo "GC left aged staged file" >&2; exit 1; }
  assert_blob "$ts_token" "$continuity_key" 'continuity blob bytes'
}

write_through_go() {
  rollback_cursor=$(scalar "SELECT current_version FROM collections WHERE id = '$collection_id'")
  upload_blob "$go_token" 'go create bytes'
  go_create_key=$(jq -er '.key' <<<"$RESPONSE")
  json_request POST "/api/collections/$collection_id/objects" 201 "$go_token" \
    "{\"blob_key\":\"$go_create_key\",\"size_bytes\":15}" rehearsal-go-create
  go_object=$(jq -er '.object.id' <<<"$RESPONSE")

  upload_blob "$go_token" 'go update bytes'
  go_update_key=$(jq -er '.key' <<<"$RESPONSE")
  json_request PUT "/api/collections/$collection_id/objects/$go_object" 200 "$go_token" \
    "{\"version\":2,\"blob_key\":\"$go_update_key\",\"size_bytes\":15}" rehearsal-go-update
  json_request DELETE "/api/collections/$collection_id/objects/$go_object?version=2" 200 "$go_token" '' rehearsal-go-delete
}

verify_rollback() {
  json_request GET /api/auth 200 "$ts_token"
  assert_eq "TS session user after rollback" "$(jq -er '.user.id' <<<"$RESPONSE")" "$user_id"
  json_request GET "/api/collections/$collection_id/objects?sinceVersion=$rollback_cursor" 200 "$ts_token"
  assert_eq "rollback cursor delta size" "$(jq -er '.objects | length' <<<"$RESPONSE")" 1
  assert_eq "Go tombstone visible to TS" "$(jq -er --arg id "$go_object" '[.objects[] | select(.id == $id and .version == "3" and .deleted == true)] | length' <<<"$RESPONSE")" 1
  assert_eq "rollback migration count" "$(scalar 'SELECT count(*) FROM kysely_migration')" 11
  run_rust_case single_note_round_trip_and_cursor_advance
}

prepare_old_source() {
  old_ts_source="$scratch/ts-old"
  mkdir -p "$old_ts_source"
  git -C "$ts_repo" archive "$old_ts_commit" | tar -x -C "$old_ts_source"
  ln -s "$ts_repo/node_modules" "$old_ts_source/node_modules"
}

rehearse() {
  local kind=$1
  local ts_source=$ts_repo
  local before_migrations=11
  if [[ "$kind" == old ]]; then
    ts_source=$old_ts_source
    before_migrations=8
  fi

  echo "rehearsing $kind TypeScript adoption"
  active_database="futo_notes_adoption_${kind}_${run_id}"
  rm -rf -- "$blob_dir"
  mkdir -p "$blob_dir"
  create_database "$active_database"
  start_ts "$ts_source" "$kind TypeScript server"
  assert_eq "$kind TS migration count" "$(scalar 'SELECT count(*) FROM kysely_migration')" "$before_migrations"
  seed_ts_traffic
  stop_server

  if [[ "$kind" == old ]]; then
    db_psql -q -c "UPDATE orphaned_blobs SET orphaned_at = now() - interval '366 days' WHERE blob_key = '$retained_key'"
  else
    plant_aged_rows
  fi

  start_go
  assert_eq "$kind Go migration count" "$(scalar 'SELECT count(*) FROM kysely_migration')" 11
  if [[ "$kind" == latest ]]; then
    if rg -q 'applied database migrations' "$server_log"; then
      echo "Go unexpectedly applied migrations to the latest TS database" >&2
      exit 1
    fi
  else
    assert_eq "old-version migrations applied" \
      "$(scalar "SELECT string_agg(name, ',' ORDER BY name) FROM kysely_migration WHERE name >= '009_'")" \
      '009_restore_plural_collections,010_authoritative_blob_ledger,011_mutation_results'
    if ! rg -q 'count=3 migrations="009_restore_plural_collections, 010_authoritative_blob_ledger, 011_mutation_results"' "$server_log"; then
      echo "Go did not report exactly migrations 009-011 for old TS adoption" >&2
      exit 1
    fi
    plant_aged_rows
  fi

  verify_swap_continuity
  audit_jobs
  write_through_go
  stop_server

  start_ts "$ts_repo" "rollback TypeScript server"
  verify_rollback
  stop_server
  drop_database "$active_database"
  echo "$kind adoption and rollback passed"
}

echo "building Go server"
(cd "$repo_root" && GOTOOLCHAIN=auto go build -o "$go_server" ./cmd/server)
password_hash=$(cd "$ts_repo" && bun src/index.ts hash "$password")
if [[ "$password_hash" != scrypt:* ]]; then
  echo "TypeScript server did not mint a scrypt hash" >&2
  exit 1
fi

if [[ "$variant" == old || "$variant" == all ]]; then
  prepare_old_source
fi
if [[ "$variant" == latest || "$variant" == all ]]; then
  rehearse latest
fi
if [[ "$variant" == old || "$variant" == all ]]; then
  rehearse old
fi

echo "adoption rehearsal passed ($variant)"
