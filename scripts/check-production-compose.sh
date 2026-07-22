#!/bin/sh
set -eu

CHECK_DIR="$(mktemp -d)"
COMPOSE=""
cleanup() {
  if [ -n "$COMPOSE" ]; then
    $COMPOSE down --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -rf "$CHECK_DIR"
}
trap cleanup EXIT HUP INT TERM
mkdir -p "$CHECK_DIR/data" "$CHECK_DIR/config"
: > "$CHECK_DIR/config/postgres-ca.pem"

cat > "$CHECK_DIR/test.env" <<'EOF'
POSTGRES_PASSWORD=local-db-secret
FUTO_NOTES_PASSWORD="dollar$$ double\" single' backslash\\ combo\\' inner space"
FUTO_NOTES_PASSWORD_HASH=
DATABASE_URL=postgres://remote:secret@db.example:5432/notes
TRUST_PROXY=true
AUTH_RATE_LIMIT=17
AUTH_RATE_LIMIT_WINDOW_MS=12345
MAX_BATCH_BYTES=22000000
MAX_BLOB_BYTES=88000000
COOKIE_SECURE=false
BLOB_GC_ENABLED=false
BLOB_RETENTION_DAYS=97
BLOB_GC_INTERVAL_MS=456789
DB_POOL_MAX=23
DB_POOL_IDLE_TIMEOUT_MS=7654
DB_SSL=true
DB_SSL_CA=/config/postgres-ca.pem
DB_SSL_INSECURE=true
EOF
printf '%s\n' "FUTO_NOTES_IMAGE=${FUTO_NOTES_TEST_IMAGE:-futo-notes-server:ci}" >> "$CHECK_DIR/test.env"
printf '%s\n' "FUTO_NOTES_DATA_DIR=$CHECK_DIR/data" >> "$CHECK_DIR/test.env"
printf '%s\n' "FUTO_NOTES_CONFIG_DIR=$CHECK_DIR/config" >> "$CHECK_DIR/test.env"

COMPOSE="docker compose --project-name futo-notes-config-check-$$ --env-file $CHECK_DIR/test.env -f docker-compose.production.yml"
$COMPOSE config -q
ACTUAL="$($COMPOSE run --rm --no-deps --entrypoint env server)"

cat > "$CHECK_DIR/expected.env" <<'EOF'
FUTO_NOTES_PASSWORD=dollar$ double" single' backslash\ combo\' inner space
DATABASE_URL=postgres://remote:secret@db.example:5432/notes
TRUST_PROXY=true
AUTH_RATE_LIMIT=17
AUTH_RATE_LIMIT_WINDOW_MS=12345
MAX_BATCH_BYTES=22000000
MAX_BLOB_BYTES=88000000
COOKIE_SECURE=false
BLOB_GC_ENABLED=false
BLOB_RETENTION_DAYS=97
BLOB_GC_INTERVAL_MS=456789
DB_POOL_MAX=23
DB_POOL_IDLE_TIMEOUT_MS=7654
DB_SSL=true
DB_SSL_CA=/config/postgres-ca.pem
DB_SSL_INSECURE=true
EOF

while IFS= read -r EXPECTED; do
  if ! printf '%s\n' "$ACTUAL" | grep -Fqx "$EXPECTED"; then
    printf 'production Compose did not preserve: %s\n' "$EXPECTED" >&2
    printf '%s\n' "$ACTUAL" >&2
    exit 1
  fi
done < "$CHECK_DIR/expected.env"

echo "Production Compose preserves documented settings and special-character passwords."
