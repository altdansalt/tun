#!/bin/bash
# bench-session.sh: time `tun bazel test` on this tree (it needs BUILD files, see docs/bazel.md)
# against the private server of scripts/dev-tun.sh, whose remote cache is on by default:
#   tun run --timeout 50m -c 'scripts/dev-tun.sh ./scripts/bench-session.sh [targets...]'
# Steps: (a) new session, empty caches; (b1) --cold, warm remote and disk cache; (b3) --cold
# with only the disk cache (--remote_cache= from the agent); (b2) --cold with only the remote
# cache (the client's disk cache emptied: a session on a new host); (c) no-op;
# (d) a one-line change in internal/server. Prints one line per step plus Bazel's own
# summary lines. Output roots stay in the private server's data dir under TMPDIR.
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f MODULE.bazel ] && [ -f internal/server/BUILD.bazel ] || { echo "bench-session.sh: no BUILD files here" >&2; exit 2; }
[ $# -gt 0 ] || set -- //...
data=$(ls -d "${TMPDIR:-/tmp}"/tun-ephemeral-* 2>/dev/null | head -1)
[ -n "$data" ] || { echo "bench-session.sh: run inside scripts/dev-tun.sh" >&2; exit 2; }
out=$(mktemp -d)
step() {
  local name=$1
  shift
  local start end code
  start=$(date +%s.%N)
  tun bazel "$@" >"$out/$name.out" 2>"$out/$name.err"
  code=$?
  end=$(date +%s.%N)
  printf '== %s: exit %d, wall %.2fs: tun bazel %s\n' "$name" "$code" "$(awk "BEGIN {print $end - $start}")" "$*"
  grep -E '^tun: (job|sync|session|bazel)|INFO: (Elapsed|[0-9]+ process)|Executed .* out of|FAILED|NO STATUS' "$out/$name.err" | head -20
}
step a test "$@"
step b1 --cold test "$@"
step b3 --cold test --remote_cache= "$@"
for d in "$data"/bazel/*/disk; do rm -rf "$d" && mkdir "$d"; done
step b2 --cold test "$@"
step c test "$@"
echo "// bench-session.sh: one-line change" >>internal/server/server.go
step d test "$@"
step d2 test "$@" # and its no-op
du -sh "$data"/reapi "$data"/bazel/*/disk 2>/dev/null
