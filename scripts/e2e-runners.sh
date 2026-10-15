#!/bin/bash
# Real-process smoke test of runners: a hub without local slots plus two `tun runner`
# processes. Run via:
#   DEV_TUN_SERVE_ARGS='--local-slots 0 --runner-timeout 3s' scripts/dev-tun.sh ./scripts/e2e-runners.sh
set -euo pipefail
: "${TUN_URL:?run through scripts/dev-tun.sh}"
: "${TUN_TOKEN:?run through scripts/dev-tun.sh}"
tmp=$(mktemp -d)
declare -A pids=()
cleanup() {
  for p in "${pids[@]}"; do kill -9 "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT
fail() {
  echo "e2e-runners: FAIL: $*" >&2
  for f in "$tmp"/*.log; do echo "--- $f" >&2; tail -n 20 "$f" >&2; done
  exit 1
}
start_runner() {
  tun runner --hub "$TUN_URL" --name "$1" --slots 1 --data "$tmp/$1" 2>"$tmp/$1.log" &
  pids[$1]=$!
}
# field ID NAME: a job field (string or number) from tun status.
field() { tun status "$1" | sed -n "s/.*\"$2\":\"\{0,1\}\([^\",]*\).*/\1/p"; }
# wait_for ID STATE-REGEX
wait_for() {
  for ((i = 0; i < 200; i++)); do
    if [[ "$(field "$1" state)" =~ ^($2)$ ]]; then return 0; fi
    sleep .1
  done
  fail "job $1 never reached $2: $(tun status "$1")"
}

start_runner r1
start_runner r2
for ((i = 0; i < 100; i++)); do
  [ "$(tun runners | wc -l)" -eq 2 ] && break
  sleep .1
done
[ "$(tun runners | wc -l)" -eq 2 ] || fail "runners did not register: $(tun runners)"

mkdir "$tmp/ws" && cd "$tmp/ws"
echo data > input
[ "$(tun run -- cat input 2>"$tmp/where")" = data ] || fail "workspace"
grep -Eq "^tun: running on runner r[12]$" "$tmp/where" || fail "no running-on line: $(cat "$tmp/where")"
code=0
tun run -- sh -c 'exit 7' || code=$?
[ "$code" -eq 7 ] || fail "exit code $code"
[ "$(tun run -e SMOKE=works -- sh -c 'printf %s "$SMOKE"')" = works ] || fail "env"
tun run --out result -- sh -c 'echo artifact > result'
[ "$(cat result)" = artifact ] || fail "--out"
id=$(tun ls --limit 1 | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
code=0
tun get -o "$tmp/got" "$id" input 2>"$tmp/get.err" || code=$?
[ "$code" -eq 125 ] && grep -q -- "--out input" "$tmp/get.err" || fail "tun get of an undeclared path: $code $(cat "$tmp/get.err")"

# With network isolation (when bwrap --unshare-net works here, as on the hub), runner jobs
# have their own network namespace: no direct connection to the host's ports, and HTTP out
# only through the egress proxy the runner serves.
if curl -fsS "$TUN_URL/v1/healthz" | grep -q '"network_isolation":true'; then
  [ "$(tun runners | grep -c '"network_isolation":true')" -eq 2 ] || fail "runners without isolation: $(tun runners)"
  port=${TUN_URL##*:}
  out=$(tun run --no-workspace -e "REQ=GET $TUN_URL/v1/healthz HTTP/1.0\r\n\r\n" -c \
    "! (exec 3<>/dev/tcp/127.0.0.1/$port) 2>/dev/null || echo direct-reachable
     exec 3<>/dev/tcp/127.0.0.1/\${HTTP_PROXY##*:} && printf \"\$REQ\" >&3 && cat <&3")
  [[ "$out" == *'"ok":true'* && "$out" != *direct-reachable* ]] || fail "isolated egress: $out"
  echo 'e2e-runners: network isolation checked'
fi

# Warm Bazel sessions route through the hub to a runner, including its cache socket.
if command -v bazel >/dev/null; then
  echo 'module(name="runner_smoke")' > MODULE.bazel
  echo 9.3.0 > .bazelversion
  echo 'genrule(name="hello", outs=["hello.txt"], cmd="echo hello > $@")' > BUILD.bazel
  tun bazel build //:hello 2>"$tmp/bazel.log" || fail "runner bazel: $(cat "$tmp/bazel.log")"
  grep -Eq '^tun: running on runner r[12]$' "$tmp/bazel.log" || fail "bazel placement"
  host=$(tun sessions | sed -n 's/.*"host":"\([^"]*\)".*/\1/p')
  tun bazel build //:hello 2>"$tmp/bazel-warm.log" || fail "warm runner bazel"
  grep -q 'Starting local Bazel server' "$tmp/bazel-warm.log" && fail "lost warm server"
  [ "$(tun sessions | sed -n 's/.*"host":"\([^"]*\)".*/\1/p')" = "$host" ] || fail "bazel affinity"
  id=$(tun ls --limit 1 | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  tun get -o "$tmp/bazel-got" "$id" //:hello || fail "runner session artifact"
  [ "$(cat "$tmp/bazel-got/bazel-bin/hello.txt")" = hello ] || fail "session output content"
  tun logs --stderr "$id" | grep -q 'tun: bazel build exit=0' || fail "retained BEP summary"
  cat > test.bzl <<'STARLARK'
def _impl(ctx):
    f = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(f, "#!/bin/sh\necho runner-test-log\n", is_executable = True)
    return [DefaultInfo(executable = f)]
simple_test = rule(implementation = _impl, test = True)
STARLARK
  # Keep load statements ahead of rule declarations.
  printf 'load(":test.bzl", "simple_test")\nsimple_test(name="smoke_test")\n' > test-build
  cat BUILD.bazel >> test-build
  mv test-build BUILD.bazel
  tun bazel test //:smoke_test 2>"$tmp/test.log" || fail "runner test"
  tid=$(tun ls --limit 1 | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  tun bazel build //:hello 2>/dev/null || fail "command after test"
  if ! tun get -o "$tmp/test-got" "$tid" //:smoke_test; then
    curl -fsS -H "X-Tun-Token: $TUN_TOKEN" "$TUN_URL/v1/jobs/$tid/files?path=." | tar -tvf - >&2
    fail "runner test logs by label"
  fi
  grep -rq runner-test-log "$tmp/test-got/bazel-testlogs" || fail "test log contents"
  tun get -o "$tmp/test-got" "$tid" //:smoke_test.log || fail "test log alias"
  echo 'e2e-runners: bazel affinity, artifacts, test logs and cache socket checked'
fi

# Services use runner slots and an authenticated reverse tunnel, even with netns.
if command -v python3 >/dev/null; then
 service=$(tun run --service --port 8080 -- python3 -u -c 'import http.server,os; http.server.HTTPServer(("127.0.0.1",int(os.environ["PORT"])),http.server.SimpleHTTPRequestHandler).serve_forever()')
 sid=$(head -n1 <<<"$service")
 surl=$(awk 'NR==2 {print $2}' <<<"$service")
 [[ "$(field "$sid" runner)" =~ ^r[12]$ ]] || fail "service ran on hub"
 [ "$(curl -fsS -H "X-Tun-Token: $TUN_TOKEN" "${surl}input")" = data ] || fail "runner service proxy"
 tun cancel "$sid" >/dev/null
 wait_for "$sid" canceled
 echo 'e2e-runners: service placement, readiness and proxy checked'
fi

# Both runners take work at once.
a=$(tun run --detach --no-workspace -- sleep 3)
b=$(tun run --detach --no-workspace -- sleep 3)
wait_for "$a" running
wait_for "$b" running
ra=$(field "$a" runner) rb=$(field "$b" runner)
[[ "$ra" =~ ^r[12]$ && "$rb" =~ ^r[12]$ && "$ra" != "$rb" ]] || fail "placement: $ra $rb"
wait_for "$a" exited
wait_for "$b" exited

# Cancel.
c=$(tun run --detach --no-workspace -- sleep 60)
wait_for "$c" running
tun cancel "$c" >/dev/null
wait_for "$c" canceled

# A runner killed mid-job: the job fails once the hub's --runner-timeout passes, and work
# queued meanwhile runs on the other runner.
d=$(tun run --detach --no-workspace -- sleep 60)
e=$(tun run --detach --no-workspace -- sleep 60)
wait_for "$d" running
wait_for "$e" running
victim=$(field "$d" runner)
kill -9 "${pids[$victim]}"
unset "pids[$victim]"
f=$(tun run --detach --no-workspace -- echo after)
wait_for "$d" failed
tun status "$d" | grep -q "runner $victim lost" || fail "lost job: $(tun status "$d")"
tun cancel "$e" >/dev/null
wait_for "$f" exited
[ "$(tun logs "$f")" = after ] || fail "job after the runner died"

# SIGTERM: the runner drains its job and preserves output and exit status.
g=$(tun run --detach --no-workspace -c 'echo before; sleep 2; echo after; exit 7')
wait_for "$g" running
survivor=$(field "$g" runner)
kill -TERM "${pids[$survivor]}"
wait "${pids[$survivor]}" || true
unset "pids[$survivor]"
wait_for "$g" exited
[ "$(field "$g" exit_code)" = 7 ] || fail "drained exit: $(tun status "$g")"
[ "$(tun logs "$g")" = $'before\nafter' ] || fail "drained output"
echo 'e2e-runners: passed'
