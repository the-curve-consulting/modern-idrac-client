#!/usr/bin/env bash
# Build the idrac CLI (only when sources changed) and run it with the given args.
#
#   ./run.sh                             # graphical manager
#   ./run.sh -host 192.0.2.10 info
#   ./run.sh -host 192.0.2.10 kvm        # console window
#   ./run.sh kvm demo                    # console test screen, no iDRAC needed
#   ./run.sh -h                          # usage
#
# Env: IDRAC_BUILD=1 forces a rebuild; IDRAC_NOGUI=1 builds without the viewer
# (pure Go, no cgo/OpenGL needed).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
BIN=./bin/idrac
mkdir -p bin
LDFLAGS="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)"

needs_build() {
  [[ "${IDRAC_BUILD:-0}" == 1 ]] && return 0
  [[ -x "$BIN" ]] || return 0
  # rebuild if any Go source, go.mod or go.sum is newer than the binary
  [[ -n "$(find . -path ./bin -prune -o \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit)" ]]
}

if needs_build; then
  if [[ "${IDRAC_NOGUI:-0}" != 1 ]] && CGO_ENABLED=1 go build -tags gui -trimpath -ldflags="$LDFLAGS" -o "$BIN" ./cmd/idrac 2>bin/.gui-build.log; then
    echo "built $BIN (with viewer)" >&2
  else
    [[ "${IDRAC_NOGUI:-0}" != 1 ]] && echo "viewer build failed (see bin/.gui-build.log); building without it" >&2
    CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o "$BIN" ./cmd/idrac
    echo "built $BIN (no viewer)" >&2
  fi
fi

exec "$BIN" "$@"
