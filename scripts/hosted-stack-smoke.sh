#!/usr/bin/env bash
# Starts the hosted test stack from a release's artifacts, the way an app's CI
# does, and runs the client smoke test (e2e/hosted TestReleasedStack) against
# it. <release-dir> is what scripts/build-release.sh <version> wrote.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <release-dir> <version>" >&2
  exit 2
fi

release=$(cd "$1/hosted" && pwd)
version=$2
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
platform="$(go env GOOS)_$(go env GOARCH)"

(cd "$release" && sha256sum -c "futo-notes-hosted_${version}_checksums.txt" >/dev/null)

work=$(mktemp -d)
tar -xzf "$release/futo-notes-hosted-stack_${version}.tar.gz" -C "$work"
launcher=$work/futo-notes-hosted-stack/hosted-stack.sh
export HOSTED_STACK_DIR=$work/state
export HOSTED_STACK_API_BIN=$release/futo-notes-api_${version}_$platform
export HOSTED_STACK_STORAGE_BIN=$release/futo-notes-storage_${version}_$platform
export HOSTED_STACK_MOCK_OIDC_BIN=$release/futo-notes-mock-oidc_${version}_$platform

cleanup() {
  "$launcher" down
  rm -rf "$work"
}
trap cleanup EXIT

"$launcher" up >"$work/stack.env"
set -a
# shellcheck source=/dev/null
. "$work/stack.env"
set +a

if ! (cd "$repo_root" && go test -count=1 -run '^TestReleasedStack$' -v ./e2e/hosted); then
  for f in "$HOSTED_STACK_DIR"/logs/*.log; do
    echo "--- $(basename "$f" .log)" >&2
    tail -n 30 "$f" >&2
  done
  exit 1
fi
