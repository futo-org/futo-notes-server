#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/staging-soak-check.sh <expected-version> <soak-start-rfc3339>

Checks both manifest-managed staging servers for the expected capability
document, database health, container health, zero restarts, and zero application
errors since the soak epoch.

Environment:
  FUTO_SOAK_URLS          Space-separated server origins.
  FUTO_SOAK_SSH_HOSTS     Space-separated root SSH targets, one per URL.
  FUTO_SOAK_ENGINES       Expected engines, one per URL (postgres or sqlite).
  FUTO_SOAK_REMOTE        Set false to skip container/log checks.
  FUTO_SOAK_MIN_DAYS      Require this many complete elapsed soak days.
  FUTO_SOAK_REQUIRE_JOBS  Set true to require evidence for all four jobs.
  FUTO_SOAK_CONTAINER     Podman container to inspect. Defaults to the
                          internal sync server; the hosted instance is a
                          second service on the same hosts, so checking it
                          means naming its container here.
  FUTO_SOAK_AUTH_MODE     Require this auth mode, both in the capability
                          document and in the container's environment. Set it
                          to oidc when checking the hosted instance, so a
                          missing FUTO_SOAK_CONTAINER cannot quietly report on
                          the wrong service.
EOF
}

if [[ $# -ne 2 ]]; then
  usage >&2
  exit 2
fi

expected_version=$1
soak_start=$2

if [[ ! $soak_start =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
  echo "soak start must be UTC RFC3339 (for example 2026-08-21T20:15:00Z)" >&2
  exit 2
fi

for dependency in curl jq date rg; do
  if ! command -v "$dependency" >/dev/null; then
    echo "missing dependency: $dependency" >&2
    exit 2
  fi
done

read -r -a urls <<<"${FUTO_SOAK_URLS:-http://100.76.177.70:3010 http://100.127.246.105:3010}"
read -r -a ssh_hosts <<<"${FUTO_SOAK_SSH_HOSTS:-100.76.177.70 100.127.246.105}"
read -r -a engines <<<"${FUTO_SOAK_ENGINES:-postgres sqlite}"
remote_checks=${FUTO_SOAK_REMOTE:-true}
require_jobs=${FUTO_SOAK_REQUIRE_JOBS:-false}
min_days=${FUTO_SOAK_MIN_DAYS:-0}
container=${FUTO_SOAK_CONTAINER:-inventory_futo-notes_sync-server}
auth_mode=${FUTO_SOAK_AUTH_MODE:-}

# Both of these are expanded into a remote shell command and a jq filter, so
# they are held to the characters a container name and an auth mode actually
# use rather than passed through.
if [[ ! $container =~ ^[A-Za-z0-9_.-]+$ ]]; then
  echo "FUTO_SOAK_CONTAINER must be a container name, got $container" >&2
  exit 2
fi
if [[ -n $auth_mode && ! $auth_mode =~ ^[a-z]+$ ]]; then
  echo "FUTO_SOAK_AUTH_MODE must be dev, password, or oidc, got $auth_mode" >&2
  exit 2
fi

# setting_value prints one NAME=value entry out of a container's environment
# listing. Reading the listing once and picking values out of it keeps a
# second setting from costing a second SSH round trip.
setting_value() {
  local name=$1 listing=$2 setting
  while IFS= read -r setting; do
    if [[ $setting == "$name="* ]]; then
      printf '%s' "${setting#"$name="}"
      return
    fi
  done <<<"$listing"
}

if [[ ! $min_days =~ ^[0-9]+$ ]]; then
  echo "FUTO_SOAK_MIN_DAYS must be a non-negative integer" >&2
  exit 2
fi

if [[ ${#urls[@]} -eq 0 ]]; then
  echo "FUTO_SOAK_URLS must contain at least one origin" >&2
  exit 2
fi
if [[ ${#urls[@]} -ne ${#engines[@]} ]]; then
  echo "FUTO_SOAK_URLS and FUTO_SOAK_ENGINES must have the same number of entries" >&2
  exit 2
fi

if [[ $remote_checks == true ]]; then
  if ! command -v ssh >/dev/null; then
    echo "missing dependency: ssh" >&2
    exit 2
  fi
  if [[ ${#urls[@]} -ne ${#ssh_hosts[@]} ]]; then
    echo "FUTO_SOAK_URLS and FUTO_SOAK_SSH_HOSTS must have the same number of entries" >&2
    exit 2
  fi
elif [[ $remote_checks != false ]]; then
  echo "FUTO_SOAK_REMOTE must be true or false" >&2
  exit 2
fi

soak_start_epoch=$(date -u -d "$soak_start" +%s)
now_epoch=$(date -u +%s)
minimum_seconds=$((min_days * 86400))
elapsed_seconds=$((now_epoch - soak_start_epoch))
if ((elapsed_seconds < minimum_seconds)); then
  echo "soak is only $((elapsed_seconds / 86400)) complete days; $min_days required" >&2
  exit 1
fi

for index in "${!urls[@]}"; do
  url=${urls[$index]%/}
  engine=${engines[$index]}
  if [[ $engine != postgres && $engine != sqlite ]]; then
    echo "FUTO_SOAK_ENGINES entries must be postgres or sqlite, got $engine" >&2
    exit 2
  fi
  echo "checking $url"

  capability=$(curl --fail --silent --show-error --max-time 15 "$url/")
  jq -e --arg version "$expected_version" '
    .name == "futo-notes" and
    .version == $version and
    .mutation_ids.successful_create_outcomes == "durable"
  ' <<<"$capability" >/dev/null
  if [[ -n $auth_mode ]]; then
    jq -e --arg mode "$auth_mode" '.auth_mode == $mode' <<<"$capability" >/dev/null
  fi

  health=$(curl --fail --silent --show-error --max-time 15 "$url/health")
  jq -e '.status == "ok" and .db == "connected"' <<<"$health" >/dev/null

  if [[ $remote_checks == false ]]; then
    continue
  fi

  host=${ssh_hosts[$index]}
  ssh_options=(
    -o BatchMode=yes
    -o ConnectTimeout=10
    -o LogLevel=ERROR
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
  )

  # container is validated above; client-side expansion is intentional.
  # shellcheck disable=SC2029
  state=$(ssh "${ssh_options[@]}" "root@$host" \
    "runuser -l podman -c 'podman inspect $container --format \"started={{.State.StartedAt}}|status={{.State.Status}}|exit={{.State.ExitCode}}|restarts={{.RestartCount}}|health={{if .State.Health}}{{.State.Health.Status}}{{end}}\"'")
  echo "  $state"
  [[ $state == *'|status=running|exit=0|restarts=0|health=healthy' ]]

  # container is validated above; client-side expansion is intentional.
  # shellcheck disable=SC2029
  container_env=$(ssh "${ssh_options[@]}" "root@$host" \
    "runuser -l podman -c 'podman inspect $container --format \"{{range .Config.Env}}{{println .}}{{end}}\"'")
  database_url=$(setting_value DATABASE_URL "$container_env")
  case $engine:$database_url in
    postgres:postgres://*|postgres:postgresql://*|sqlite:sqlite:*) ;;
    *)
      echo "database engine on $host does not match expected $engine" >&2
      exit 1
      ;;
  esac
  echo "  database_engine=$engine"

  if [[ -n $auth_mode ]]; then
    container_auth_mode=$(setting_value AUTH_MODE "$container_env")
    if [[ $container_auth_mode != "$auth_mode" ]]; then
      echo "AUTH_MODE in $container on $host is ${container_auth_mode:-unset}, expected $auth_mode" >&2
      exit 1
    fi
    echo "  auth_mode=$auth_mode"
  fi

  started=${state#started=}
  started=${started%%|status=*}
  started_without_fraction=${started%%.*}
  started_epoch=$(date -u -d "${started_without_fraction}Z" +%s)
  if ((started_epoch > soak_start_epoch)); then
    echo "container on $host restarted after the soak epoch: $started" >&2
    exit 1
  fi

  # soak_start and container are validated above; client-side expansion is
  # intentional.
  # shellcheck disable=SC2029
  logs=$(ssh "${ssh_options[@]}" "root@$host" \
    "runuser -l podman -c 'podman logs --since $soak_start $container'" 2>&1)
  if rg -q 'level=(ERROR|FATAL)|panic:|fatal error:' <<<"$logs"; then
    echo "application errors found on $host since $soak_start" >&2
    rg 'level=(ERROR|FATAL)|panic:|fatal error:' <<<"$logs" >&2
    exit 1
  fi

  if [[ $require_jobs == true ]]; then
    for pattern in 'msg=sessions summary=' 'msg="storage reconciliation" summary=' \
      'msg="mutation results" summary=' 'msg="blob GC" summary='; do
      if ! rg -F -q "$pattern" <<<"$logs"; then
        echo "missing scheduled-job evidence on $host: $pattern" >&2
        exit 1
      fi
    done
  elif [[ $require_jobs != false ]]; then
    echo "FUTO_SOAK_REQUIRE_JOBS must be true or false" >&2
    exit 2
  fi

  # container is validated above; client-side expansion is intentional.
  # shellcheck disable=SC2029
  usage_line=$(ssh "${ssh_options[@]}" "root@$host" \
    "runuser -l podman -c 'podman stats --no-stream --format \"mem={{.MemUsage}} cpu={{.CPU}} pids={{.PIDs}}\" $container'")
  echo "  $usage_line"
done

echo "soak check passed: version=$expected_version elapsed_days=$((elapsed_seconds / 86400))"
