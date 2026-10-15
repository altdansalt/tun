#!/bin/bash
# bench-rbe.sh: remote execution through tun bazel sessions vs local execution in the session,
# against the private server of scripts/dev-tun.sh plus $RUNNERS local `tun runner` processes
# with one slot each (like the shared deployment's runners):
#   tun run --timeout 60m -e DEV_TUN_SERVE_ARGS='--local-slots 1' -c 'scripts/dev-tun.sh ./scripts/bench-rbe.sh genrules|tree [targets...]'
# genrules: N (default 200) independent genrules that sleep 1s, in a scratch workspace:
#   (g1) new session; (g2) again with new action keys (warm session, pure remote execution,
#   --jobs from tun: twice the slots); (g3) remote with --jobs = slots; (g4) local with --jobs =
#   slots; (g5) local with Bazel's default --jobs.
# tree: tun bazel test on this tree (default: the targets that pass in a session):
#   (a) new session, empty caches; (b) --cold, warm remote cache; (c) no-op; (d) one-line
#   change; (e) --cold, every action again (--noremote_accept_cached), remote; (f) the same,
#   local; (g) one-line change, local; (h) --cold local, warm remote cache.
# "Local" means --remote_executor= from the agent: the session still uses the remote cache.
# Each step prints wall time, Bazel's summary lines, the session's Bazel server CPU seconds
# during the step and the hub's executor counts. All output roots stay under TMPDIR.
set -uo pipefail
cd "$(dirname "$0")/.."
mode=${1:?usage: bench-rbe.sh genrules|tree [targets...]}
shift
data=$(ls -d "${TMPDIR:-/tmp}"/tun-ephemeral-* 2>/dev/null | head -1)
[ -n "$data" ] || { echo "bench-rbe.sh: run inside scripts/dev-tun.sh" >&2; exit 2; }
tmp=$(mktemp -d)
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done
  for p in "${pids[@]}"; do wait "$p" 2>/dev/null; done
  rm -rf "$tmp"
}
trap cleanup EXIT
for i in $(seq 1 "${RUNNERS:-2}"); do
  tun runner --hub "$TUN_URL" --name "bench-r$i" --slots 1 --data "$tmp/runner-$i" >"$tmp/runner-$i.log" 2>&1 &
  pids+=("$!")
done
for _ in $(seq 100); do
  n=$(curl -s "$TUN_URL/v1/healthz" | grep -o '"execution":{"slots":[0-9]*' | grep -o '[0-9]*$')
  [ "${n:-0}" -ge "$((${RUNNERS:-2} + 1))" ] && break
  sleep 0.2
done
echo "== executor slots: $n"
# The session's Bazel server: the java process whose output base is the session's.
server_cpu() {
  local pid
  pid=$(pgrep -f 'output_base=/tun/out/base' | while read -r p; do grep -qa 'A-server.jar\|BazelServer' /proc/"$p"/cmdline 2>/dev/null && echo "$p"; done | head -1)
  [ -n "$pid" ] || { echo 0; return; }
  awk -v t="$(getconf CLK_TCK)" '{sub(/.*\) /, ""); print ($12 + $13) / t}' /proc/"$pid"/stat
}
step() {
  local name=$1
  shift
  local start end code c0 c1
  c0=$(server_cpu)
  start=$(date +%s.%N)
  tun bazel --timeout 50m "$@" >"$tmp/$name.out" 2>"$tmp/$name.err"
  code=$?
  end=$(date +%s.%N)
  c1=$(server_cpu)
  printf '== %s: exit %d, wall %.2fs, bazel server cpu %.1fs: tun bazel %s\n' "$name" "$code" "$(awk "BEGIN {print $end - $start}")" \
    "$(awk "BEGIN {d = $c1 - $c0; if (d < 0) d = $c1; print d}")" "$*"
  grep -E '^tun: (job|session|bazel)|INFO: (Elapsed|[0-9]+ process)|Executed .* out of|FAILED|NO STATUS' "$tmp/$name.err" | head -12
  curl -s "$TUN_URL/v1/healthz" | grep -o '"execution":{[^}]*}[^}]*}'
  [ "$code" = 0 ] || grep -E -B2 -A8 -- '^--- FAIL|^FAIL:|panic:|ERROR:' "$tmp/$name.err" | head -40
}
case $mode in
genrules)
  ws=$tmp/genrules
  mkdir -p "$ws"
  echo 'module(name = "bench")' >"$ws/MODULE.bazel"
  echo 9.3.0 >"$ws/.bazelversion"
  for i in $(seq 1 "${N:-200}"); do
    printf 'genrule(name = "g%s", outs = ["o%s"], cmd = "sleep 1; echo %s > $@")\n' "$i" "$i" "$i"
  done >"$ws/BUILD.bazel"
  cd "$ws"
  # A new --action_env value gives every action a new key: all of them execute again.
  step g1 build //...
  step g2 build --action_env=BENCH=2 //...
  step g3 build --action_env=BENCH=3 --jobs="$n" //...
  step g4 build --action_env=BENCH=4 --remote_executor= --jobs="$n" //...
  step g5 build --action_env=BENCH=5 --remote_executor= --jobs=auto //...
  ;;
tree)
  [ -f MODULE.bazel ] || { echo "bench-rbe.sh: no MODULE.bazel here" >&2; exit 2; }
  [ $# -gt 0 ] || set -- -- //... -//internal/client:client_test -//internal/reapi:reapi_test -//scripts:e2e_test
  step a test "$@"
  step b --cold test "$@"
  step c test "$@"
  echo "var benchOneLineRemote = 1" >>internal/server/server.go
  step d test "$@"
  step e --cold test --noremote_accept_cached "$@"
  step f --cold test --noremote_accept_cached --remote_executor= --jobs=auto "$@"
  echo "var benchOneLineLocal = 1" >>internal/server/server.go
  step g test --remote_executor= --jobs=auto "$@"
  step h --cold test --remote_executor= --jobs=auto "$@"
  ;;
*)
  echo "bench-rbe.sh: unknown mode $mode" >&2
  exit 2
  ;;
esac
for i in $(seq 1 "${RUNNERS:-2}"); do tail -3 "$tmp/runner-$i.log"; done
