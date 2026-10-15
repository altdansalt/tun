#!/bin/bash
# ci.sh: build + test tun. Runs anywhere (bb remote runner, tun itself, a laptop).
# Installs Go into ~/.cache/tun-go if no suitable go is on PATH.
#
# TEST_FLAGS: extra `go test` flags, split on whitespace with xargs quoting rules: '...' and "..."
#   group words and are removed, so TEST_FLAGS='-run "TestA|TestB" -v' and '-run TestA|TestB'
#   both work. Nothing is evaluated: no globbing, variables, command substitution or pipes.
#   With -run, the separate vet and build steps are skipped (go test compiles everything it
#   tests) unless CI_FULL=1, and the run fails if the pattern matched no test in any package.
# PKGS: the packages to vet, build and test (default ./...), split on whitespace, e.g.
#   PKGS=./internal/reapi or PKGS='./internal/server ./internal/client'. With PKGS the
#   end-to-end suites are skipped unless E2E=1.
# E2E=0: skip the end-to-end smoke suite (scripts/e2e.sh) that full runs do at the end;
#   E2E=1: run it even with -run.
# LOAD=N: N busy-loop processes compete for the CPUs while everything runs, to shake out
#   timing assumptions: LOAD=3 TEST_FLAGS='-run TestX -count=20 -cpu 1,4'.
set -euo pipefail
GOVER=${GOVER:-1.27.2}
if ! command -v go >/dev/null || ! go version | grep -q "go1.2[5-9]"; then
  d=$HOME/.cache/tun-go/$GOVER
  if [ ! -x "$d/go/bin/go" ]; then
    mkdir -p "$d"
    curl -fsSL "https://go.dev/dl/go$GOVER.linux-amd64.tar.gz" | tar -xz -C "$d"
  fi
  export PATH=$d/go/bin:$PATH
fi
cd "$(dirname "$0")/.."

# Plain ${TEST_FLAGS} would keep quote characters literally ('-run "A|B"' became the regexp
# `"A|B"`, matching nothing) and glob; eval would run | ( $(...) as shell. xargs only unquotes.
flags=()
if [ -n "${TEST_FLAGS:-}" ]; then
  if ! xargs printf '%s\0' <<<"$TEST_FLAGS" >/dev/null; then
    echo "ci.sh: cannot parse TEST_FLAGS=$TEST_FLAGS (unmatched quote?)" >&2
    exit 2
  fi
  mapfile -d '' flags < <(xargs printf '%s\0' <<<"$TEST_FLAGS")
fi
read -r -a pkgs <<<"${PKGS:-./...}"
if [ "${#pkgs[@]}" = 0 ]; then
  echo "ci.sh: PKGS is empty" >&2
  exit 2
fi
run=0 check=1
[ -n "${PKGS:-}" ] && run_e2e=0 || run_e2e=1
for f in "${flags[@]}"; do
  case $f in
    -run|-run=*|--run|--run=*|-test.run|-test.run=*) run=1 ;;
    -bench*|--bench*|-json|--json|-list*|--list*) check=0 ;; # output we don't parse / no tests by design
  esac
done

if [[ ! "${LOAD:-0}" =~ ^[0-9]+$ ]]; then
  echo "ci.sh: LOAD=$LOAD is not a number" >&2
  exit 2
fi
hogs=()
stop_hogs() { [ "${#hogs[@]}" = 0 ] || kill "${hogs[@]}" 2>/dev/null || true; }
trap stop_hogs EXIT
for _ in $(seq "${LOAD:-0}"); do
  (exec -a tun-ci-load bash -c 'while :; do :; done') &
  hogs+=($!)
done
[ "${LOAD:-0}" = 0 ] || echo "ci.sh: LOAD=$LOAD: $LOAD busy loops running alongside"
go version
if [ "$run" = 0 ] || [ "${CI_FULL:-}" = 1 ]; then
  go vet "${pkgs[@]}"
  go build "${pkgs[@]}"
fi
# End-to-end smoke suite of the real binary (scripts/e2e.sh): on full runs unless E2E=0, and
# with -run or PKGS only if E2E=1.
e2e() {
  if [ "${E2E:-$((run_e2e && !run))}" = 1 ]; then
    time DEV_TUN_CLIENTS=alice,bob scripts/dev-tun.sh ./scripts/e2e.sh
    time DEV_TUN_SERVE_ARGS='--local-slots 0 --runner-timeout 3s' scripts/dev-tun.sh ./scripts/e2e-runners.sh
  fi
}
if [ "$run" = 0 ] || [ "$check" = 0 ]; then
  go test -timeout=10m -race -count=1 "${flags[@]}" "${pkgs[@]}"
  e2e
  exit 0
fi

out=$(mktemp)
trap 'rm -f "$out"; stop_hogs' EXIT
status=0
go test -timeout=10m -race -count=1 "${flags[@]}" "${pkgs[@]}" 2>&1 | tee "$out" || status=$?
# A package whose tests ran prints "ok  pkg  1.2s" without "[no tests to run]" (or FAIL lines).
if [ "$status" = 0 ] && ! awk '/^(ok[[:space:]]|--- |FAIL)/ && !/\[no tests to run\]/ {f=1} END {exit !f}' "$out"; then
  echo "ci.sh: FAIL: TEST_FLAGS=${TEST_FLAGS} matched no tests in any package" >&2
  echo "ci.sh: (go test -list '.*' ./... lists the test names; quote alternations: -run 'A|B')" >&2
  exit 1
fi
[ "$status" = 0 ] && e2e
exit "$status"
