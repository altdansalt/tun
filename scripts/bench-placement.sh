#!/bin/bash
# Private hub + two runner processes, all inside one tun job. No shared deployment changes.
# tun run --timeout 15m -c 'DEV_TUN_SERVE_ARGS="--local-slots 0 --runner-timeout 3s" scripts/dev-tun.sh scripts/bench-placement.sh'
set -euo pipefail
: "${TUN_URL:?run via scripts/dev-tun.sh}"
tmp=$(mktemp -d)
declare -A pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; wait || true; rm -rf "$tmp"; }
trap cleanup EXIT
for name in a b; do
 tun runner --hub "$TUN_URL" --name "$name" --slots 1 --data "$tmp/$name" 2>"$tmp/$name.log" &
 pids[$name]=$!
done
for ((i=0;i<100;i++)); do [ "$(tun runners | wc -l)" -eq 2 ] && break; sleep .1; done
mkdir "$tmp/ws"; cd "$tmp/ws"
echo 'module(name="placement_bench")' > MODULE.bazel
echo 9.3.0 > .bazelversion
for ((i=0;i<200;i++)); do printf 'genrule(name="a%d", outs=["a%d.txt"], cmd="echo %d > $@")\n' "$i" "$i" "$i"; done > BUILD.bazel
step() {
 local name=$1 session=$2 start end; shift 2
 start=$(date +%s.%N)
 tun bazel --session "$session" build //... "$@" >"$tmp/$name.out" 2>"$tmp/$name.err" || { cat "$tmp/$name.err"; exit 1; }
 end=$(date +%s.%N)
 awk -v n="$name" -v a="$start" -v b="$end" 'BEGIN {printf "== %s wall=%.3fs\n",n,b-a}'
 grep -E '^tun: (job|running|bazel)|INFO: (Elapsed|[0-9]+ process)' "$tmp/$name.err"
 tun sessions
}
step seed alpha
step warm-alpha alpha
step other-host beta
step third-session gamma
step retained-alpha alpha
host=$(tun sessions | sed -n '/"name":"alpha"/s/.*"host":"\([^"]*\)".*/\1/p')
kill -9 "${pids[$host]}"; unset 'pids[$host]'
for ((i=0;i<100;i++)); do [ "$(tun runners | wc -l)" -eq 1 ] && break; sleep .1; done
step host-loss alpha
step warm-recreated alpha
