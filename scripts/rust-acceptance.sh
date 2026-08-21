#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <ts|go>" >&2
  exit 2
}

target=${1:-}
case "$target" in
  ts|go) ;;
  *) usage ;;
esac
[[ $# -eq 1 ]] || usage

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ts_repo=${FUTO_TS_SERVER_REPO:-/home/justin/Developer/futo-notes-server}
client_repo=${FUTO_NOTES_CLIENT_REPO:-/home/justin/Developer/futo-notes}
postgres_container=${FUTO_POSTGRES_CONTAINER:-futo-notes-postgres}
postgres_host=${FUTO_POSTGRES_HOST:-localhost}
postgres_port=${FUTO_POSTGRES_PORT:-5433}
postgres_password=${FUTO_POSTGRES_PASSWORD:-futo_notes}
run_id="$(date +%s)_$$"
database="futo_notes_rust_accept_${target}_${run_id}"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/futo-notes-rust-accept.XXXXXX")
blob_dir="$scratch/blobs"
server_log="$scratch/server.log"
server_pid=

mkdir -p "$blob_dir"

admin_psql() {
  if [[ "$postgres_container" == direct ]]; then
    PGPASSWORD="$postgres_password" psql -h "$postgres_host" -p "$postgres_port" \
      -v ON_ERROR_STOP=1 -U futo_notes -d futo_notes "$@"
  else
    docker exec "$postgres_container" psql -v ON_ERROR_STOP=1 -U futo_notes -d futo_notes "$@"
  fi
}

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill -INT "$server_pid" 2>/dev/null || true
    for _ in {1..15}; do
      kill -0 "$server_pid" 2>/dev/null || break
      sleep 0.2
    done
    kill -KILL "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  admin_psql \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$database' AND pid <> pg_backend_pid()" \
    -c "DROP DATABASE IF EXISTS \"$database\"" >/dev/null || true
  if [[ $status -ne 0 ]]; then
    echo "server log ($server_log):" >&2
    tail -100 "$server_log" >&2 || true
  fi
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT INT TERM

if curl --silent --fail --max-time 1 http://127.0.0.1:3055/health >/dev/null 2>&1; then
  echo "port 3055 is already serving a healthy process; stop it before running acceptance" >&2
  exit 1
fi

admin_psql \
  -c "CREATE DATABASE \"$database\"" >/dev/null

database_url="postgres://futo_notes:$postgres_password@$postgres_host:$postgres_port/$database"
if [[ "$target" == go ]]; then
  (cd "$repo_root" && GOTOOLCHAIN=auto go build -o "$scratch/server" ./cmd/server)
  (
    cd "$repo_root"
    exec env AUTH_MODE=dev PORT=3055 DATABASE_URL="$database_url" BLOB_DIR="$blob_dir" \
      COOKIE_SECURE=false DEV_UI=false "$scratch/server"
  ) >"$server_log" 2>&1 &
else
  (
    cd "$ts_repo"
    exec env AUTH_MODE=dev PORT=3055 DATABASE_URL="$database_url" BLOB_DIR="$blob_dir" \
      COOKIE_SECURE=false BLOB_GC_ENABLED=false LOG_LEVEL=warn bun src/index.ts
  ) >"$server_log" 2>&1 &
fi
server_pid=$!

healthy=false
for _ in {1..150}; do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    echo "$target server exited before becoming healthy" >&2
    exit 1
  fi
  if curl --silent --fail --max-time 1 http://127.0.0.1:3055/health >/dev/null; then
    healthy=true
    break
  fi
  sleep 0.2
done
if [[ "$healthy" != true ]]; then
  echo "$target server did not become healthy" >&2
  exit 1
fi

echo "running Rust acceptance suite against $target server"
(
  cd "$client_repo"
  FUTO_TEST_SERVER=http://127.0.0.1:3055 \
    cargo test -p futo-notes-sync --test server_integration --test sse_live \
      -- --ignored --test-threads=1
)
