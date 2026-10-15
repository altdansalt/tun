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
# In a tun job: seed tun bazel's per-client bazelisk homes from the job's (shared) download cache.
if [ -n "${XDG_CACHE_HOME:-}" ] && [ -d "$XDG_CACHE_HOME/bazelisk/downloads" ]; then
  seed=(--bazelisk-seed "$XDG_CACHE_HOME/bazelisk")
fi
# shellcheck disable=SC2086
"$tmp/tun" serve --ephemeral --exit-on-eof "${seed[@]}" ${DEV_TUN_SERVE_ARGS:-} <"$tmp/stdin" >"$tmp/env" 3>&- &
pid=$!
{
  IFS= read -r url
  IFS= read -r token
  IFS= read -r data
} <"$tmp/env"
[[ "$url" == TUN_URL=http://127.0.0.1:* && "$token" == TUN_TOKEN=?* && "$data" == TUN_DATA=/* ]]
export TUN_URL="${url#TUN_URL=}" TUN_TOKEN="${token#TUN_TOKEN=}" TUN_DATA="${data#TUN_DATA=}"
# DEV_TUN_CLIENTS=alice,bob: issue a client token per name, exported as TUN_TOKEN_alice etc.
# (TUN_TOKEN stays the admin token).
IFS=, read -r -a clients <<<"${DEV_TUN_CLIENTS:-}"
for c in "${clients[@]}"; do
  if [[ ! "$c" =~ ^[A-Za-z0-9_]+$ ]]; then
    echo "dev-tun.sh: DEV_TUN_CLIENTS: client name '$c' must be letters, digits and _" >&2
    exit 125
  fi
  t=$("$tmp/tun" token --data "$TUN_DATA" new "$c")
  export "TUN_TOKEN_$c=$t"
done
export PATH="$tmp:$PATH"
"$@" 3>&-
