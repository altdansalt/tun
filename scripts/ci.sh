#!/bin/bash
# ci.sh: build + test tun. Runs anywhere (bb remote runner, tun itself, a laptop).
# Installs Go into ~/.cache/tun-go if no suitable go is on PATH.
set -euo pipefail
GOVER=${GOVER:-1.27.2}
if ! command -v go >/dev/null || ! go version | grep -q "go1.2[5-9]"; then
  d=$HOME/.cache/tun-go/$GOVER
  if [ ! -x "$d/go/bin/go" ]; then
    mkdir -p "$d"
    curl -fsSL "https://go.dev/dl/go$GOVER.linux-amd64.tar.gz" | tar -xz -C "$d"
  fi
  export PATH=$d/go/bin:$PATH
fi
cd "$(dirname "$0")/.."
go version
go vet ./...
go build ./...
go test -race -count=1 ${TEST_FLAGS:-} ./...
