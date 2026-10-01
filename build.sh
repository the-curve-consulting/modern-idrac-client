#!/usr/bin/env bash
# Build release binaries into ./bin: the host platform by default, or
# `./build.sh all` for linux/amd64, linux/arm64, darwin/arm64, windows/amd64.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
mkdir -p bin

build() {
  local os=$1 arch=$2 out=bin/idrac-$1-$2
  [[ $os == windows ]] && out+=.exe
  echo "building $out" >&2
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags='-s -w' -o "$out" ./cmd/idrac
}

gofmt -l . | grep . && { echo "gofmt: files above need formatting" >&2; exit 1; }
go vet ./...
go test ./...

if [[ "${1:-}" == all ]]; then
  build linux amd64
  build linux arm64
  build darwin arm64
  build windows amd64
else
  CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/idrac ./cmd/idrac
  echo "built bin/idrac" >&2
fi
