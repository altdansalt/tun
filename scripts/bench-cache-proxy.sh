#!/bin/bash
# Run via scripts/dev-tun.sh with DEV_TUN_SERVE_ARGS='--local-slots 0'.
# One private runner reaches the private hub through an HTTP/1-only proxy.
set -euo pipefail
tmp=$(mktemp -d)
runner_pid= proxy_pid=
cleanup() {
 for p in "$runner_pid" "$proxy_pid"; do [ -z "$p" ] || kill "$p" 2>/dev/null || true; done
 wait || true
 rm -rf "$tmp"
}
trap cleanup EXIT
cat > "$tmp/proxy.go" <<'GO'
package main
import ("fmt"; "net"; "net/http"; "net/http/httputil"; "net/url"; "os")
func main() {
 u,err:=url.Parse(os.Args[1]);if err!=nil {panic(err)}
 p:=httputil.NewSingleHostReverseProxy(u)
 protocols:=new(http.Protocols);protocols.SetHTTP1(true)
 p.Transport=&http.Transport{Protocols:protocols}
 ln,err:=net.Listen("tcp","127.0.0.1:0");if err!=nil {panic(err)}
 fmt.Println("http://"+ln.Addr().String())
 panic(http.Serve(ln,p))
}
GO
go build -o "$tmp/proxy" "$tmp/proxy.go"
"$tmp/proxy" "$TUN_URL" >"$tmp/url" 2>"$tmp/proxy.log" &
proxy_pid=$!
for ((i=0;i<100;i++)); do [ -s "$tmp/url" ] && break; sleep .1; done
tun runner --hub "$(cat "$tmp/url")" --name cache-proxy --slots 1 --data "$tmp/runner" 2>"$tmp/runner.log" &
runner_pid=$!
for ((i=0;i<100;i++)); do [ "$(tun runners | wc -l)" -eq 1 ] && break; sleep .1; done
step() {
 local name=$1; shift
 echo "=== $name"
 tun bazel --session cache-proxy --cold --json test //internal/workspace:workspace_test "$@"
}
step seed
step warm-remote
step no-cache --remote_cache= --disk_cache=
