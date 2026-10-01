#!/usr/bin/env bash
# Check and build into ./bin.
#
#   ./build.sh        gofmt + vet + tests, then bin/idrac for this machine with
#                     the graphical viewer (needs cgo + OpenGL/X11 headers; falls
#                     back to a viewer-less build if that fails)
#   ./build.sh all    additionally cross-compile viewer-less CLI binaries for
#                     linux/amd64, linux/arm64, darwin/arm64, windows/amd64
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
mkdir -p bin

cross() {
  local os=$1 arch=$2 out=bin/idrac-$1-$2
  [[ $os == windows ]] && out+=.exe
  echo "building $out (no viewer)" >&2
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags='-s -w' -o "$out" ./cmd/idrac
}

gofmt -l . | grep . && { echo "gofmt: files above need formatting" >&2; exit 1; }
go vet ./...
go test ./...

if CGO_ENABLED=1 go vet -tags gui ./... 2>bin/.gui-build.log && CGO_ENABLED=1 go test -tags gui ./pkg/viewer/ \
  && CGO_ENABLED=1 go build -tags gui -trimpath -ldflags='-s -w' -o bin/idrac ./cmd/idrac; then
  echo "built bin/idrac (with viewer)" >&2
else
  echo "viewer build failed (see bin/.gui-build.log); building without it" >&2
  CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/idrac ./cmd/idrac
  echo "built bin/idrac (no viewer)" >&2
fi

if [[ "${1:-}" == all ]]; then
  cross linux amd64
  cross linux arm64
  cross darwin arm64
  cross windows amd64
fi
