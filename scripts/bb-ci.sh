#!/bin/bash
# bb-ci.sh: run scripts/ci.sh on a BuildBuddy remote runner via `bb remote`.
# Mirrors the local working tree (committed + uncommitted changes) onto the runner.
# Usage: scripts/bb-ci.sh [extra script...]   e.g. TEST_FLAGS='-run TestFoo' scripts/bb-ci.sh
set -euo pipefail
cd "$(dirname "$0")/.."
git remote get-url public >/dev/null 2>&1 || git remote add public https://github.com/altdansalt/tun.git
git config buildbuddy.remote-bazel-remote-name public
export BUILDBUDDY_API_KEY=${BUILDBUDDY_API_KEY:-$(cat ~/bb-token)}
script=${*:-"TEST_FLAGS=$(printf %q "${TEST_FLAGS:-}") ./scripts/ci.sh"}
exec bb remote --remote_runner=grpcs://remote.buildbuddy.dev --timeout=20m --script="$script"
