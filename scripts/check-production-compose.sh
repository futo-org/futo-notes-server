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
mkdir -p "$CHECK_DIR/data"

cat > "$CHECK_DIR/test.env" <<'EOF'
POSTGRES_PASSWORD=local-db-secret
FUTO_NOTES_PASSWORD="dollar$$ double\" single' backslash\\ combo\\' inner space"
FUTO_NOTES_PASSWORD_HASH=
LOG_LEVEL=debug
EOF
printf '%s\n' "FUTO_NOTES_IMAGE=${FUTO_NOTES_TEST_IMAGE:-futo-notes-server:ci}" >> "$CHECK_DIR/test.env"
printf '%s\n' "FUTO_NOTES_DATA_DIR=$CHECK_DIR/data" >> "$CHECK_DIR/test.env"

COMPOSE="docker compose --project-name futo-notes-config-check-$$ --env-file $CHECK_DIR/test.env -f docker-compose.production.yml"
$COMPOSE config -q
ACTUAL="$($COMPOSE run --rm --no-deps --entrypoint env server)"

cat > "$CHECK_DIR/expected.env" <<'EOF'
FUTO_NOTES_PASSWORD=dollar$ double" single' backslash\ combo\' inner space
DATABASE_URL=postgres://futo_notes:local-db-secret@postgres:5432/futo_notes
LOG_LEVEL=debug
EOF

while IFS= read -r EXPECTED; do
  if ! printf '%s\n' "$ACTUAL" | grep -Fqx "$EXPECTED"; then
    printf 'production Compose did not preserve: %s\n' "$EXPECTED" >&2
    printf '%s\n' "$ACTUAL" >&2
    exit 1
  fi
done < "$CHECK_DIR/expected.env"

echo "Production Compose uses bundled Postgres and preserves documented settings and special-character passwords."
