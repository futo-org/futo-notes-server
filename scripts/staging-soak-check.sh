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

  state=$(ssh "${ssh_options[@]}" "root@$host" \
    "runuser -l podman -c 'podman inspect inventory_futo-notes_sync-server --format \"started={{.State.StartedAt}}|status={{.State.Status}}|exit={{.State.ExitCode}}|restarts={{.RestartCount}}|health={{if .State.Health}}{{.State.Health.Status}}{{end}}\"'")
  echo "  $state"
  [[ $state == *'|status=running|exit=0|restarts=0|health=healthy' ]]

  database_url=$(ssh "${ssh_options[@]}" "root@$host" \
    "runuser -l podman -c 'podman inspect inventory_futo-notes_sync-server --format \"{{range .Config.Env}}{{println .}}{{end}}\"'" \
    | while IFS= read -r setting; do
        if [[ $setting == DATABASE_URL=* ]]; then
          printf '%s' "${setting#DATABASE_URL=}"
          break
        fi
      done)
  case $engine:$database_url in
    postgres:postgres://*|postgres:postgresql://*|sqlite:sqlite:*) ;;
    *)
      echo "database engine on $host does not match expected $engine" >&2
      exit 1
      ;;
  esac
  echo "  database_engine=$engine"

  started=${state#started=}
  started=${started%%|status=*}
  started_without_fraction=${started%%.*}
  started_epoch=$(date -u -d "${started_without_fraction}Z" +%s)
  if ((started_epoch > soak_start_epoch)); then
    echo "container on $host restarted after the soak epoch: $started" >&2
    exit 1
  fi

  # soak_start is format-validated above; client-side expansion is intentional.
  # shellcheck disable=SC2029
  logs=$(ssh "${ssh_options[@]}" "root@$host" \
    "runuser -l podman -c 'podman logs --since $soak_start inventory_futo-notes_sync-server'" 2>&1)
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

  usage_line=$(ssh "${ssh_options[@]}" "root@$host" \
    "runuser -l podman -c 'podman stats --no-stream --format \"mem={{.MemUsage}} cpu={{.CPU}} pids={{.PIDs}}\" inventory_futo-notes_sync-server'")
  echo "  $usage_line"
done

echo "soak check passed: version=$expected_version elapsed_days=$((elapsed_seconds / 86400))"
