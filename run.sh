#!/usr/bin/env bash
# Build the idrac CLI (only when sources changed) and run it with the given args.
#
#   ./run.sh -host r710 info
#   ./run.sh -host r710 kvm screenshot shot.png
#   ./run.sh                      # prints usage
#
# Env: IDRAC_BUILD=1 forces a rebuild; GOFLAGS/CGO_ENABLED pass through to go.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
BIN=./bin/idrac
mkdir -p bin

needs_build() {
  [[ "${IDRAC_BUILD:-0}" == 1 ]] && return 0
  [[ -x "$BIN" ]] || return 0
  # rebuild if any Go source, go.mod or go.sum is newer than the binary
  [[ -n "$(find . -path ./bin -prune -o \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit)" ]]
}

if needs_build; then
  echo "building $BIN ..." >&2
  CGO_ENABLED="${CGO_ENABLED:-0}" go build -trimpath -ldflags='-s -w' -o "$BIN" ./cmd/idrac
fi

exec "$BIN" "$@"
