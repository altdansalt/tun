#!/bin/bash
# Measure what per-job network namespaces cost: job startup (N trivial jobs) and a Go build
# that downloads modules, on two private servers built from this tree, --net host vs private.
# Usage (on tun): tun run -c 'scripts/bench-net.sh [N]'
set -euo pipefail
n=${1:-20}
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
pids=()
trap 'exec 3>&-; kill "${pids[@]}" 2>/dev/null; wait; rm -rf "$tmp"' EXIT
(cd "$root" && go build -o "$tmp/tun" ./cmd/tun)
mkfifo "$tmp/stdin"
exec 3<>"$tmp/stdin" # the servers exit when this closes
for net in host private; do
  mkfifo "$tmp/env.$net"
  "$tmp/tun" serve --ephemeral --exit-on-eof --max-jobs 1 --net "$net" <"$tmp/stdin" >"$tmp/env.$net" 2>"$tmp/err.$net" 3>&- &
  pids+=($!)
  { read -r "url_$net"; read -r "token_$net"; } <"$tmp/env.$net"
done
mkdir "$tmp/ws"
cat >"$tmp/ws/go.mod" <<'EOF'
module bench

go 1.21

require (
	github.com/google/uuid v1.6.0
	golang.org/x/text v0.21.0
)
EOF
cat >"$tmp/ws/main.go" <<'EOF'
package main

import (
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/text/language"
)

func main() { fmt.Println(uuid.NewSHA1(uuid.Nil, nil) != uuid.Nil, language.English) }
EOF
use() { local u=url_$1 t=token_$1; export TUN_URL=${!u#TUN_URL=} TUN_TOKEN=${!t#TUN_TOKEN=}; }
runsec() { "$tmp/tun" run --json "$@" 2>/dev/null | sed -n 's/.*"exit_code":\([0-9]*\).*"run_sec":\([0-9.]*\).*/\1 \2/p'; }
stats() { sort -n | awk '{a[NR]=$1; s+=$1} END {printf "mean %.3fs median %.3fs min %.3fs max %.3fs (n=%d)\n", s/NR, a[int((NR+1)/2)], a[1], a[NR], NR}'; }
for net in host private; do
  use $net
  runsec --no-workspace -- true >/dev/null # warm up
  for _ in $(seq "$n"); do runsec --no-workspace -- true | cut -d' ' -f2; done | stats | sed "s/^/startup run_sec  $net: /"
  for _ in $(seq "$n"); do
    s=$(date +%s.%N); "$tmp/tun" run --no-workspace -- true 2>/dev/null; e=$(date +%s.%N)
    echo "$e - $s" | bc
  done | stats | sed "s/^/startup wall     $net: /"
done
# Module downloads into a job-private GOMODCACHE each time (GOCACHE stays shared, warmed first).
for i in 1 2 3; do
  for net in host private; do
    use $net
    r=$(cd "$tmp/ws" && runsec -e GOTOOLCHAIN=local -e GOFLAGS=-mod=mod -c 'GOMODCACHE=$HOME/mod go build -o /dev/null . && chmod -R u+w $HOME/mod')
    [ "$i" = 1 ] && echo "go build (warm-up) $net: exit/run_sec $r" && continue
    echo "go build + module download $net: exit/run_sec $r"
  done
done
