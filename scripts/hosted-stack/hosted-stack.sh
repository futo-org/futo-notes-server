#!/usr/bin/env bash
# Runs the hosted FUTO Notes service for client tests: Postgres and MinIO in
# Docker, and the released api, storage (one per region) and mock-oidc
# binaries on this machine. TEST USE ONLY: everything signs with the public
# test keys in test-only-keys/.
#
#   hosted-stack.sh up     start everything, wait until it is ready, print the endpoints
#   hosted-stack.sh env    print the endpoints of the running stack
#   hosted-stack.sh down   stop everything `up` started
#
# The endpoints are KEY=value lines on stdout, for a harness to source:
#   HOSTED_STACK_API_URL               the API, the app's server URL
#   HOSTED_STACK_ISSUER                the mock OIDC issuer
#   HOSTED_STACK_REGIONS               region ids, comma-separated
#   HOSTED_STACK_STORAGE_URL_<REGION>  each region's storage service, e.g.
#                                      HOSTED_STACK_STORAGE_URL_US_EAST
# Progress and errors go to stderr.
#
# `up` runs these binaries, which must be set:
#   HOSTED_STACK_API_BIN, HOSTED_STACK_STORAGE_BIN, HOSTED_STACK_MOCK_OIDC_BIN
# Ports, all optional. The defaults stay clear of 3005, which the apps keep
# for themselves, and of the server repo's own dev ports:
#   HOSTED_STACK_API_PORT       3125
#   HOSTED_STACK_STORAGE_PORT   3115; the three regions take it and the next two
#   HOSTED_STACK_OIDC_PORT      8192
#   HOSTED_STACK_POSTGRES_PORT  15532
#   HOSTED_STACK_MINIO_PORT     19000
# The API runs with DEV_UI and the mock web payment provider, so billing is
# on: a test user writes only once subscribed, through the mock checkout
# (POST /api/billing/checkout, then POST the URL it returns).
# HOSTED_STACK_ALLOWED_EMAIL_DOMAINS, optional and empty by default, is the
# API's ALLOWED_EMAIL_DOMAINS: the comma-separated email domains new accounts
# may sign up with. Empty lets anyone sign up.
# HOSTED_STACK_DIR holds the stack's state and its service logs
# (logs/<service>.log), by default $TMPDIR/futo-notes-hosted-stack. Give every
# command the same one.
# HOSTED_STACK_PROJECT names the stack's containers, by default
# futo-notes-hosted-stack. Two stacks run side by side with different
# projects, directories and ports.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
keys=$here/test-only-keys
tmp=${TMPDIR:-/tmp}
state=${HOSTED_STACK_DIR:-${tmp%/}/futo-notes-hosted-stack}
project=${HOSTED_STACK_PROJECT:-futo-notes-hosted-stack}

api_port=${HOSTED_STACK_API_PORT:-3125}
storage_port=${HOSTED_STACK_STORAGE_PORT:-3115}
oidc_port=${HOSTED_STACK_OIDC_PORT:-8192}
export HOSTED_STACK_POSTGRES_PORT=${HOSTED_STACK_POSTGRES_PORT:-15532}
export HOSTED_STACK_MINIO_PORT=${HOSTED_STACK_MINIO_PORT:-19000}

regions=(us-east eu-central ap-southeast)
region_names=("US East" "EU Central" "Asia Pacific Southeast")

# Postgres and MinIO publish their ports on loopback. Under Docker-in-Docker
# (DOCKER_HOST=tcp://docker:2376 in GitLab CI) the published ports are on the
# Docker host instead, so they must listen beyond it.
container_host=127.0.0.1
export HOSTED_STACK_BIND=127.0.0.1
if [[ ${DOCKER_HOST:-} =~ ^tcp://([^:/]+) ]]; then
  container_host=${BASH_REMATCH[1]}
  HOSTED_STACK_BIND=0.0.0.0
fi
postgres_addr=$container_host:$HOSTED_STACK_POSTGRES_PORT
minio_addr=$container_host:$HOSTED_STACK_MINIO_PORT

log() { echo "hosted-stack: $*" >&2; }
die() {
  log "$*"
  exit 1
}

compose() {
  docker compose -p "$project" -f "$here/compose.yml" "$@" >&2
}

# HOSTED_STACK_JOIN_CONTAINER is the id of a container this launcher runs in,
# on the same Docker daemon as the stack: a CI job given the host's Docker
# socket. The host's firewall may drop traffic from the job's network to ports
# published on another, so the job joins the stack's network instead and
# reaches Postgres and MinIO there, on their own ports.
join_stack_network() {
  local postgres minio network
  postgres=$(docker compose -p "$project" -f "$here/compose.yml" ps -q postgres)
  minio=$(docker compose -p "$project" -f "$here/compose.yml" ps -q minio)
  network=$(docker inspect -f '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}{{end}}' "$postgres")
  echo "$network" >"$state/network"
  docker network connect "$network" "$HOSTED_STACK_JOIN_CONTAINER"
  postgres_addr=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$postgres"):5432
  minio_addr=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$minio"):9000
}

absolute() {
  echo "$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
}

# start <service> <binary> [VAR=value...] runs the binary in the background
# with only the given environment, so nothing the caller exported configures it.
start() {
  local name=$1 bin=$2
  shift 2
  env -i "$@" "$bin" </dev/null >"$state/logs/$name.log" 2>&1 &
  echo "$! $bin" >"$state/pids/$name"
}

cmdline() {
  if [[ -r /proc/$1/cmdline ]]; then
    tr '\0' ' ' <"/proc/$1/cmdline"
  else
    ps -o command= -p "$1" 2>/dev/null || true
  fi
}

# ours <pid> <binary> holds while the pid is alive and still runs the binary
# `up` started, so a reused pid is never signalled. A zombie has no command
# line, so an orphan its new parent never reaps counts as stopped.
ours() {
  kill -0 "$1" 2>/dev/null || return 1
  [[ $(cmdline "$1") == "$2"* ]]
}

stop_services() {
  local pidfile pid bin
  for pidfile in "$state"/pids/*; do
    [[ -f $pidfile ]] || continue
    read -r pid bin <"$pidfile"
    if ours "$pid" "$bin"; then
      kill -TERM "$pid" 2>/dev/null || true
    fi
  done
  for pidfile in "$state"/pids/*; do
    [[ -f $pidfile ]] || continue
    read -r pid bin <"$pidfile"
    for _ in $(seq 1 150); do
      ours "$pid" "$bin" || break
      sleep 0.1
    done
    if ours "$pid" "$bin"; then
      log "$(basename "$pidfile") did not stop within 15 s; killing it"
      kill -KILL "$pid" 2>/dev/null || true
    fi
    rm -f "$pidfile"
  done
}

wait_ready() {
  local name=$1 url=$2 pid bin
  read -r pid bin <"$state/pids/$name"
  for _ in $(seq 1 600); do
    if curl -sf -o /dev/null --max-time 2 "$url"; then
      return 0
    fi
    ours "$pid" "$bin" || die "$name exited before it was ready"
    sleep 0.1
  done
  die "$name was not ready at $url after 60 s"
}

pem_json() {
  awk '{ printf "%s\\n", $0 }' "$1"
}

down() {
  stop_services
  if [[ -f $state/network ]]; then
    docker network disconnect -f "$(cat "$state/network")" "$HOSTED_STACK_JOIN_CONTAINER" || true
  fi
  compose down --volumes --remove-orphans || true
  rm -rf "$state/pids" "$state/logs"
  rm -f "$state/env" "$state/regions.json" "$state/network"
  rmdir "$state" 2>/dev/null || true
  log "down"
}

up_failed() {
  local f
  log "up failed; the last lines of each service log:"
  for f in "$state"/logs/*.log; do
    [[ -f $f ]] || continue
    echo "--- $(basename "$f" .log)" >&2
    tail -n 30 "$f" >&2
  done
  down
}

up() {
  [[ ! -e $state/env ]] || die "a stack is already up (state in $state); run '$0 down' first"
  local var
  for var in HOSTED_STACK_API_BIN HOSTED_STACK_STORAGE_BIN HOSTED_STACK_MOCK_OIDC_BIN; do
    [[ -n ${!var:-} && -x ${!var} ]] || die "$var must name the released binary, got '${!var:-}'"
  done
  local api_bin storage_bin oidc_bin
  api_bin=$(absolute "$HOSTED_STACK_API_BIN")
  storage_bin=$(absolute "$HOSTED_STACK_STORAGE_BIN")
  oidc_bin=$(absolute "$HOSTED_STACK_MOCK_OIDC_BIN")

  mkdir -p "$state/pids" "$state/logs"
  trap up_failed EXIT

  log "starting Postgres and MinIO"
  compose up -d --wait postgres minio
  compose run --rm minio-init
  if [[ -n ${HOSTED_STACK_JOIN_CONTAINER:-} ]]; then
    join_stack_network
  fi

  local api_url=http://127.0.0.1:$api_port
  local issuer=http://127.0.0.1:$oidc_port
  log "starting mock-oidc at $issuer"
  start mock-oidc "$oidc_bin" \
    MOCK_OIDC_PORT="$oidc_port" \
    MOCK_OIDC_ISSUER="$issuer" \
    MOCK_OIDC_CLIENT_ID=notes-test \
    MOCK_OIDC_CLIENT_SECRET=notes-test-secret \
    MOCK_OIDC_REDIRECT_URIS="$api_url/api/auth/oidc/callback" \
    MOCK_OIDC_POST_LOGOUT_REDIRECT_URIS="$api_url/" \
    LOG_LEVEL=debug

  local i region storage_urls=() sep=
  echo "[" >"$state/regions.json"
  for i in "${!regions[@]}"; do
    region=${regions[$i]}
    storage_urls[i]=http://127.0.0.1:$((storage_port + i))
    log "starting storage region $region at ${storage_urls[i]}"
    start "storage-$region" "$storage_bin" \
      REGION_ID="$region" \
      STORAGE_PORT=$((storage_port + i)) \
      S3_ENDPOINT="http://$minio_addr" \
      S3_BUCKET="notes-$region" \
      S3_ACCESS_KEY_ID=minio \
      S3_SECRET_ACCESS_KEY=miniominio \
      S3_FORCE_PATH_STYLE=true \
      API_PUBLIC_KEYS="$(cat "$keys/api.pub.pem")" \
      RECEIPT_SIGNING_KEY="$(cat "$keys/$region.key.pem")" \
      DRAIN_DELAY_MS=0 \
      LOG_LEVEL=debug
    printf '%s{"id": "%s", "name": "%s", "description": "Test region %s.", "endpoint": "%s", "receipt_public_keys": "%s"}\n' \
      "$sep" "$region" "${region_names[i]}" "$region" "${storage_urls[i]}" "$(pem_json "$keys/$region.pub.pem")" \
      >>"$state/regions.json"
    sep=,
  done
  echo "]" >>"$state/regions.json"

  local api_env=(
    API_PORT="$api_port"
    PUBLIC_URL="$api_url"
    POSTGRES_HOST="${postgres_addr%:*}"
    POSTGRES_PORT="${postgres_addr##*:}"
    POSTGRES_DATABASE=notes
    POSTGRES_USERNAME=postgres
    POSTGRES_PASSWORD=postgres
    OIDC_ISSUER="$issuer"
    OIDC_CLIENT_ID=notes-test
    OIDC_CLIENT_SECRET=notes-test-secret
    APP_REDIRECT_SCHEMES="tech.futo.notes,tech.futo.notes.dev"
    ALLOWED_EMAIL_DOMAINS="${HOSTED_STACK_ALLOWED_EMAIL_DOMAINS:-}"
    REGIONS_FILE="$state/regions.json"
    API_SIGNING_KEY="$(cat "$keys/api.key.pem")"
    STORAGE_REFRESH_ROTATION_KEY="$(cat "$keys/storage-refresh-rotation.key")"
    COOKIE_SECURE=false
    DEV_UI=true
    BILLING_WEB_PROVIDER=mock
    BILLING_MOCK_WEBHOOK_SECRET=test-only-mock-webhook-secret
    DRAIN_DELAY_MS=0
    LOG_LEVEL=debug
  )
  log "migrating the database"
  env -i "${api_env[@]}" "$api_bin" migrate </dev/null >"$state/logs/api-migrate.log" 2>&1 ||
    die "api migrate failed"
  log "starting the API at $api_url"
  start api "$api_bin" "${api_env[@]}"

  wait_ready mock-oidc "$issuer/.well-known/openid-configuration"
  for i in "${!regions[@]}"; do
    wait_ready "storage-${regions[i]}" "${storage_urls[i]}/readyz"
  done
  wait_ready api "$api_url/readyz"

  {
    echo "HOSTED_STACK_API_URL=$api_url"
    echo "HOSTED_STACK_ISSUER=$issuer"
    echo "HOSTED_STACK_REGIONS=$(
      IFS=,
      echo "${regions[*]}"
    )"
    for i in "${!regions[@]}"; do
      echo "HOSTED_STACK_STORAGE_URL_$(echo "${regions[i]}" | tr a-z- A-Z_)=${storage_urls[i]}"
    done
  } >"$state/env"
  trap - EXIT
  log "ready"
  cat "$state/env"
}

case ${1:-} in
up) up ;;
env)
  [[ -f $state/env ]] || die "no stack is up (state in $state)"
  cat "$state/env"
  ;;
down) down ;;
*)
  echo "usage: $0 up|env|down" >&2
  exit 2
  ;;
esac
