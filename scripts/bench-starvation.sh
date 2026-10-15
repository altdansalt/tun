#!/bin/bash
# bench-starvation.sh: why session commands that execute remotely hold no job slot.
#   tun run --timeout 15m -c ./scripts/bench-starvation.sh
# Builds this tree twice: as is, and with session commands taking a slot like any job (the
# behavior before). Each binary serves with one local slot and no runners, and one session
# builds 20 genrules that sleep 1s with remote execution. With the slot taken by the session
# command its actions can never start: tun bazel's --timeout (2m) ends it.
set -uo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cp -r "$root" "$tmp/src"
sed -i 's/slotless := j.bz != nil \&\& s.sessionExecutesRemotely(j.SessionID)/slotless := false/' "$tmp/src/internal/server/jobs.go"
grep -q 'slotless := false' "$tmp/src/internal/server/jobs.go" || { echo "bench-starvation.sh: patch failed" >&2; exit 2; }
(cd "$tmp/src" && go build -o "$tmp/tun-slot" ./cmd/tun) || exit 2
(cd "$root" && go build -o "$tmp/tun-new" ./cmd/tun) || exit 2
ws=$tmp/ws
mkdir -p "$ws"
echo 'module(name = "starve")' >"$ws/MODULE.bazel"
echo 9.3.0 >"$ws/.bazelversion"
for i in $(seq 1 20); do
  printf 'genrule(name = "g%s", outs = ["o%s"], cmd = "sleep 1; echo %s > $@")\n' "$i" "$i" "$i"
done >"$ws/BUILD.bazel"
cat >"$tmp/build.sh" <<'EOF'
cd "$1" || exit 2
start=$(date +%s.%N)
tun bazel --timeout 2m build //... >out 2>err
code=$?
printf '%s: exit %d after %.1fs\n' "$2" "$code" "$(awk "BEGIN {print $(date +%s.%N) - $start}")"
grep -E 'INFO: (Elapsed|[0-9]+ process)|timed out|^tun: (bazel|job .* (exited|timeout))' err
curl -s "$TUN_URL/v1/healthz" | grep -o '"execution":{[^}]*}[^}]*}'
EOF
for v in slot new; do
  label="session command takes a job slot (before)"
  [ "$v" = new ] && label="session command holds no slot (now)"
  DEV_TUN_BIN=$tmp/tun-$v DEV_TUN_SERVE_ARGS='--local-slots 1' "$root/scripts/dev-tun.sh" bash "$tmp/build.sh" "$ws" "$label"
done
