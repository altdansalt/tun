#!/bin/bash
# bb-ci.sh: run scripts/ci.sh on a BuildBuddy remote runner via `bb remote`.
# Mirrors the local working tree (committed + uncommitted changes) onto the runner.
# Usage: scripts/bb-ci.sh [extra script...]   e.g. TEST_FLAGS='-run TestFoo' scripts/bb-ci.sh
set -euo pipefail
cd "$(dirname "$0")/.."
git remote get-url public >/dev/null 2>&1 || git remote add public https://github.com/altdansalt/tun.git
git config buildbuddy.remote-bazel-remote-name public
export BUILDBUDDY_API_KEY=${BUILDBUDDY_API_KEY:-$(cat ~/bb-token)}
# The remote runner's bazel wrapper rewrites remote-cache/proxy options. Use
# Bazelisk directly so cache/socket tests exercise Bazel's native transport.
setup=$(cat <<'REMOTE'
if [ -x /usr/local/bin/bazelisk ]; then
  tun_ci_bin=$(mktemp -d)
  ln -s /usr/local/bin/bazelisk "$tun_ci_bin/bazel"
  export PATH="$tun_ci_bin:$PATH"
  export USE_BAZEL_VERSION=9.3.0
fi
REMOTE
)
script=${*:-"TEST_FLAGS=$(printf %q "${TEST_FLAGS:-}") ./scripts/ci.sh"}
script="$setup
$script"
exec bb remote --remote_runner=grpcs://remote.buildbuddy.dev --timeout=20m --script="$script"
