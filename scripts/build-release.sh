#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
  echo "usage: $0 <version>" >&2
  exit 2
fi

version=$1
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output_dir=${FUTO_RELEASE_DIR:-$repo_root/dist}

mkdir -p "$output_dir"

build() {
  local goos=$1
  local goarch=$2
  local suffix=
  if [[ "$goos" == windows ]]; then
    suffix=.exe
  fi
  local name="futo-notes-server_${version}_${goos}_${goarch}${suffix}"
  echo "building $name"
  (
    cd "$repo_root"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOTOOLCHAIN=auto \
      go build -trimpath -ldflags="-s -w -X main.serverVersion=$version" \
      -o "$output_dir/$name" ./cmd/server
  )
}

build linux amd64
build linux arm64
build darwin amd64
build darwin arm64
build windows amd64

(
  cd "$output_dir"
  sha256sum futo-notes-server_"$version"_* >"futo-notes-server_${version}_checksums.txt"
)
