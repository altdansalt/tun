#!/bin/bash
# Real CLI smoke tests. Run via scripts/dev-tun.sh.
set -euo pipefail
: "${TUN_URL:?run through scripts/dev-tun.sh}"
: "${TUN_TOKEN:?run through scripts/dev-tun.sh}"
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
[ "$(tun run -- echo hi)" = hi ]
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
echo 'e2e: passed'
