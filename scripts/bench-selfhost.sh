#!/bin/bash
# bench-selfhost.sh: time `tun bazel test //...` on tun's own tree against $TUN_URL, from the
# client (not in a job): a new session, warm no-ops, and one-line edits in internal/server and
# internal/workspace (a log/error string, so the compiled library changes and its dependents
# rebuild and rerun). Prints one line per run: step, wall seconds, Bazel's elapsed, job id, hub
# load (healthz running/queued at the start), the summary line. The edits are reverted at exit.
#   scripts/bench-selfhost.sh [ROUNDS] [extra tun bazel flags/args before test...]
# BENCH_SESSION: session name (default bench). BENCH_ARGS: extra bazel test args (e.g. --config=race).
# BENCH_TESTS=1: also print each run's per-target test lines (PASSED in Ns; cached ones left out).
# A third edit changes a message in internal/client/runner_test.go: only its test target reruns.
set -euo pipefail
cd "$(dirname "$0")/.."
rounds=${1:-3}
session=${BENCH_SESSION:-bench}
read -r -a extra <<< "${BENCH_ARGS:-}"
files=(internal/server/gc.go internal/workspace/sync.go internal/client/runner_test.go)
orig=$(mktemp -d) # outside the tree: tun syncs untracked files too
for f in "${files[@]}"; do mkdir -p "$orig/${f%/*}" && cp "$f" "$orig/$f"; done
restore() { for f in "${files[@]}"; do cp "$orig/$f" "$f"; done; rm -rf "$orig"; }
trap restore EXIT
restore_only() { for f in "${files[@]}"; do cp "$orig/$f" "$f"; done; }
load() {
  curl -s "$TUN_URL/v1/healthz" | python3 -c 'import json,sys; h=json.load(sys.stdin); print("running=%d queued=%d" % (h["running"], h["queued"]))'
}
run() {
  local step=$1; shift
  local l t0 t1 log
  l=$(load)
  log=$(mktemp)
  t0=$(date +%s.%N)
  tun bazel --session "$session" "$@" test "${extra[@]}" //... > "$log" 2>&1 || true
  t1=$(date +%s.%N)
  printf '%-10s wall=%6.1fs bazel=%-8s job=%s hub[%s] %s\n' "$step" "$(echo "$t1 - $t0" | bc)" \
    "$(grep -o 'Elapsed time: [0-9.]*s' "$log" | tail -1 | cut -d' ' -f3)" \
    "$(grep -o '^tun: job [^ ]*' "$log" | head -1 | cut -d' ' -f3)" "$l" \
    "$(grep '^tun: bazel test' "$log" | sed 's/^tun: bazel test //')"
  if [ "${BENCH_TESTS:-}" = 1 ]; then grep -E '^//.* (PASSED|FAILED|TIMEOUT) in ' "$log" | grep -v '(cached)' | sed 's/^/    /' || true; fi
  grep -q '^tun: bazel test exit=0' "$log" || { echo "--- failed run's log tail:"; tail -40 "$log"; }
  rm -f "$log"
}
for i in $(seq "$rounds"); do
  run cold --cold
  run noop
  run noop2
  sed -i "s/tun: gc: %v/tun: gc($i-$$): %v/" internal/server/gc.go
  run edit-srv
  sed -i "s/blob upload over the size limit[^\"]*\"/blob upload over the size limit ($i-$$)\"/" internal/workspace/sync.go
  run edit-ws
  sed -i "s/\"run --out: %+v\"/\"run --out ($i-$$): %+v\"/" internal/client/runner_test.go
  run edit-test
  restore_only
done
