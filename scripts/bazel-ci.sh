#!/bin/bash
# bazel-ci.sh: build + test tun with Bazel, the way an agent runs it in a tun job:
#   tun run -- ./scripts/bazel-ci.sh [bazel test args]
# No arguments: bazel test //... ; otherwise the arguments go to `bazel test` unchanged, so name
# the targets too (./scripts/bazel-ci.sh --test_filter=TestEphemeral //internal/server:server_test).
# First it checks that the BUILD files are what gazelle generates (GAZELLE=0 skips that).
#
# Caches (tun v0 jobs get a fresh path every time, so nothing in-memory survives a job):
# - output base: under $TMPDIR (private to the job, deleted with it). Bazel's default would be
#   $XDG_CACHE_HOME/bazel, tun's shared cache dir, keyed by the per-job workspace path: every
#   job would leave 100MB+ behind there.
# - repository cache ($XDG_CACHE_HOME/tun-bazel/repo): downloads (Go SDK, rules), content-
#   addressed and verified by Bazel, so sharing it between jobs is safe.
# - disk cache ($XDG_CACHE_HOME/tun-bazel/disk): action outputs. Trust caveat: in tun v0 every job
#   can write the shared cache dir, so one job can plant results that later jobs use as
#   their build outputs (or test results). Fine among cooperating agents of one owner; set
#   BAZEL_DISK_CACHE=0 to build without it. docs/bazel.md: moves to an executor-written cache.
set -euo pipefail
cd "$(dirname "$0")/.."
cache=${TUN_BAZEL_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/tun-bazel}
flags=(--repository_cache="$cache/repo")
if [ "${BAZEL_DISK_CACHE:-1}" != 0 ]; then
  flags+=(--disk_cache="$cache/disk" --experimental_disk_cache_gc_max_size=10G)
fi
[ $# -gt 0 ] || set -- //...
bazel=(bazel --output_user_root="${TMPDIR:-/tmp}/tun-bazel-root")
# BUILD files must match the Go sources: a new _test.go missing from BUILD.bazel would simply
# not run. GAZELLE=0 skips the check.
if [ "${GAZELLE:-1}" != 0 ] && ! "${bazel[@]}" run "${flags[@]}" //:gazelle -- -mode=diff; then
  echo "bazel-ci.sh: FAIL: BUILD files are stale; run: bazel run //:gazelle (diff above)" >&2
  exit 1
fi
exec "${bazel[@]}" test "${flags[@]}" "$@"
