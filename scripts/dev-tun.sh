#!/bin/bash
# Build a private tun server and run a command against it.
set -euo pipefail
if [ "$#" -eq 0 ]; then
  echo "usage: scripts/dev-tun.sh COMMAND [ARG...]" >&2
  exit 125
fi
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
pid=
cleanup() {
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# DEV_TUN_BIN: use this tun binary instead of building the working tree (Bazel's e2e tests).
if [ -n "${DEV_TUN_BIN:-}" ]; then
  cp "$DEV_TUN_BIN" "$tmp/tun"
else
  (cd "$root" && go build -o "$tmp/tun" ./cmd/tun)
fi
# A FIFO makes readiness explicit and EOF shuts the server down if this wrapper dies.
mkfifo "$tmp/env" "$tmp/stdin"
exec 3<>"$tmp/stdin"
# DEV_TUN_SERVE_ARGS: extra tun serve flags (word-split), e.g. '--local-slots 0'.
seed=()
if [ -n "${DEV_TUN_BAZEL:-}" ]; then
  # DEV_TUN_BAZEL: a Bazel binary named bazel-<version>-<os>-<arch> (Bazel's e2e tests: no network
  # or download cache inside the test sandbox). Seed tun bazel's bazelisk homes with it, laid out
  # like bazelisk's own cache.
  sum=$(sha256sum "$DEV_TUN_BAZEL" | cut -d' ' -f1)
  mkdir -p "$tmp/bazelisk/downloads/sha256/$sum/bin" "$tmp/bazelisk/downloads/metadata/bazelbuild"
  cp "$DEV_TUN_BAZEL" "$tmp/bazelisk/downloads/sha256/$sum/bin/bazel"
  chmod 755 "$tmp/bazelisk/downloads/sha256/$sum/bin/bazel"
  printf %s "$sum" > "$tmp/bazelisk/downloads/metadata/bazelbuild/$(basename "$DEV_TUN_BAZEL")"
  seed=(--bazelisk-seed "$tmp/bazelisk")
elif [ -n "${XDG_CACHE_HOME:-}" ] && [ -d "$XDG_CACHE_HOME/bazelisk/downloads" ]; then
  # In a tun job: seed them from the job's (shared) download cache.
  seed=(--bazelisk-seed "$XDG_CACHE_HOME/bazelisk")
fi
# shellcheck disable=SC2086
"$tmp/tun" serve --ephemeral --exit-on-eof "${seed[@]}" ${DEV_TUN_SERVE_ARGS:-} <"$tmp/stdin" >"$tmp/env" 3>&- &
pid=$!
{
  IFS= read -r url
  IFS= read -r token
} <"$tmp/env"
[[ "$url" == TUN_URL=http://127.0.0.1:* && "$token" == TUN_TOKEN=?* ]]
export TUN_URL="${url#TUN_URL=}" TUN_TOKEN="${token#TUN_TOKEN=}"
export PATH="$tmp:$PATH"
"$@" 3>&-
