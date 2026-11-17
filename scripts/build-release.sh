#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
  echo "usage: $0 <version>" >&2
  exit 2
fi

version=$1
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output_dir=${FUTO_RELEASE_DIR:-$repo_root/dist}
# The hosted binaries and test-stack bundle are published as their own package
# (.gitlab-ci.yml), so the self-hosted release keeps exactly its files.
hosted_dir=$output_dir/hosted

mkdir -p "$output_dir" "$hosted_dir"

build() {
  local cmd=$1
  local goos=$2
  local goarch=$3
  local goarm=${4:-}
  local arch=$goarch${goarm:+v$goarm}
  local suffix=
  if [[ "$goos" == windows ]]; then
    suffix=.exe
  fi
  local dir=$hosted_dir
  local ldflags="-s -w -X main.version=$version"
  if [[ "$cmd" == server ]]; then
    dir=$output_dir
    ldflags="-s -w -X main.serverVersion=$version"
  fi
  local name="futo-notes-${cmd}_${version}_${goos}_${arch}${suffix}"
  echo "building $name"
  (
    cd "$repo_root"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOARM="$goarm" GOTOOLCHAIN=auto \
      go build -trimpath -ldflags="$ldflags" \
      -o "$dir/$name" "./cmd/$cmd"
  )
}

for cmd in server api storage mock-oidc; do
  build "$cmd" linux amd64
  build "$cmd" linux arm64
  # 32-bit ARMv6: original Raspberry Pi Zero / Zero W and 32-bit Pi OS.
  build "$cmd" linux arm 6
  build "$cmd" darwin amd64
  build "$cmd" darwin arm64
  build "$cmd" windows amd64
done

(
  cd "$output_dir"
  sha256sum futo-notes-server_"$version"_* >"futo-notes-server_${version}_checksums.txt"
)

# The test-stack bundle: launcher, compose file and the committed test keys.
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
bundle=$stage/futo-notes-hosted-stack
mkdir -p "$bundle/test-only-keys"
cp "$repo_root"/scripts/hosted-stack/* "$bundle/"
cp "$repo_root"/dev/hosted-keys/* "$bundle/test-only-keys/"
tar -czf "$hosted_dir/futo-notes-hosted-stack_${version}.tar.gz" -C "$stage" futo-notes-hosted-stack

(
  cd "$hosted_dir"
  sha256sum futo-notes-{api,storage,mock-oidc}_"$version"_* "futo-notes-hosted-stack_${version}.tar.gz" \
    >"futo-notes-hosted_${version}_checksums.txt"
)
