#!/bin/bash
# Real CLI smoke tests. Run via scripts/dev-tun.sh.
set -euo pipefail
: "${TUN_URL:?run through scripts/dev-tun.sh}"
: "${TUN_TOKEN:?run through scripts/dev-tun.sh}"
suite_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
service=
job=
cleanup() {
  [ -z "$service" ] || tun cancel "$service" >/dev/null 2>&1 || true
  [ -z "$job" ] || tun cancel "$job" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT
cd "$tmp"
[ "$(tun run -- echo hi 2>"$tmp/where")" = hi ]
grep -q "^tun: running on hub$" "$tmp/where"
code=0
tun run -- sh -c 'exit 7' || code=$?
[ "$code" -eq 7 ]
[ "$(tun run -e SMOKE=works -- sh -c 'printf %s "$SMOKE"')" = works ]
mkdir sub
[ "$(tun run -C sub -- sh -c 'basename "$PWD"')" = sub ]
tun run --out result -- sh -c 'echo artifact > result'
[ "$(cat result)" = artifact ]
# Build a tiny HTTP fixture using only the toolchain already needed by CI.
cat > http.go <<'GO'
package main
import ("net/http"; "os")
func main() {
  http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("proxied")) })
  if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), nil); err != nil { panic(err) }
}
GO
go build -o http-fixture http.go
tun run --service --port 8080 -- ./http-fixture > service
service=$(head -n1 service)
url=$(awk 'NR==2 {print $2}' service)
[ "$(curl --fail --silent --show-error -H "X-Tun-Token: $TUN_TOKEN" "$url")" = proxied ]
tun cancel "$service" >/dev/null
job=$(tun run --detach -- sleep 60)
tun cancel "$job" >/dev/null
# Cancel may be acknowledged before the process exits.
for ((i=0; i<100; i++)); do
  tun status "$job" > status
  if grep -Eq '"state":[[:space:]]*"canceled"' status; then break; fi
  sleep .05
done
grep -Eq '"state":[[:space:]]*"canceled"' status
tun ls > jobs
grep -q "$job" jobs
# tun bazel: a genrule workspace (no downloads); the second command finds the server warm.
if command -v bazel >/dev/null; then
  mkdir "$tmp/bz" && cd "$tmp/bz"
  echo 'module(name = "smoke")' > MODULE.bazel
  echo 9.3.0 > .bazelversion
  echo 'genrule(name = "g", outs = ["g.txt"], cmd = "echo hi > $@")' > BUILD.bazel
  tun bazel build //:g 2> "$tmp/first"
  grep -q "Starting local Bazel server" "$tmp/first"
  grep -q "remote cache: tun's, through /tun/sock/cache.sock" "$tmp/first" # tun serve's default
  tun bazel build //:g 2> "$tmp/second"
  if grep -q "Starting local Bazel server" "$tmp/second"; then exit 1; fi # (! is exempt from set -e)
  grep -q "uploaded 0 of 3 files (0B sent)" "$tmp/second"
  grep -Eq "^tun: bazel build exit=0 after .*: 1 targets built$" "$tmp/second"
  code=0
  tun bazel build //:missing 2>/dev/null || code=$?
  [ "$code" -ne 0 ]
  [ "$code" -ne 125 ]
  tun bazel --json build //:g > "$tmp/json" 2>/dev/null
  grep -q '"path":"bazel-bin/g.txt"' "$tmp/json"
  tun sessions | grep -q '"commands":4'
  # A new session (another name: its own output base) gets the action from tun's remote cache.
  tun bazel --session other build //:g 2> "$tmp/other"
  grep -Eq "[0-9]+ remote cache hit" "$tmp/other"
  grep -q "cold: first command of a new session" "$tmp/other"
  # tun sessions stop frees it; its next command says why it starts cold.
  tun sessions stop other | grep -q '"state":"stopping"'
  until [ "$(tun sessions | grep -c '"name":"other"')" = 0 ]; do sleep 0.2; done
  tun bazel --session other build //:g 2> "$tmp/other"
  grep -q "; cold start: its session ended .* ago: stopped by client .* (tun sessions stop)$" "$tmp/other"
  cd "$tmp"
fi
echo 'e2e: passed'

# Real process restart regression.
python3 "$suite_dir/e2e-drain.py"
