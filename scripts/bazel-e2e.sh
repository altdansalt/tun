#!/bin/bash
# Bazel sh_test entry for the smoke suites (scripts/BUILD.bazel): bazel-e2e.sh TUN GO SUITE runs
# SUITE through scripts/dev-tun.sh with the Bazel-built tun binary TUN, and the hermetic Go
# SDK's GO first on PATH (e2e.sh builds a fixture with go). With a fourth argument BAZEL (the
# pinned Bazel binary), dev-tun.sh seeds the server's bazelisk homes with it (DEV_TUN_BAZEL).
set -euo pipefail
tun=$(readlink -f "$1")
go=$(readlink -f "$2")
export DEV_TUN_BIN=$tun PATH=$(dirname "$go"):$PATH
if [ $# -ge 4 ]; then
  export DEV_TUN_BAZEL=$(readlink -f "$4")
fi
exec "$(dirname "$0")/dev-tun.sh" "$3"
