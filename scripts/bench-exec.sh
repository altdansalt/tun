#!/bin/bash
# Run through tun; remote modes require scripts/dev-tun.sh with --cache-dir=reapi.
set -euo pipefail
bench_dir=$(mktemp -d "${TMPDIR:-/tmp}/exec-bench.XXXXXX")
runner_pids=()
cleanup() {
  for pid in "${runner_pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${runner_pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  rm -rf "$bench_dir"
}
trap cleanup EXIT
mode=${1:-local}
if [[ $mode == runners ]]; then
  for i in 1 2; do
    tun runner --hub "$TUN_URL" --name "exec-bench-$i" --slots 2 --data "$bench_dir/runner-$i" >"$bench_dir/runner-$i.log" 2>&1 &
    runner_pids+=("$!")
  done
fi
mkdir "$bench_dir/work"
touch "$bench_dir/work/MODULE.bazel"
for i in $(seq 1 200); do
  printf 'genrule(name="g%s", outs=["o%s"], cmd="sleep 0.5; echo %s > $@")\n' "$i" "$i" "$i"
done > "$bench_dir/work/BUILD.bazel"
flags=()
if [[ $mode != local ]]; then
 flags+=("--remote_executor=${TUN_URL/http:/grpc:}" "--remote_header=x-tun-token=$TUN_TOKEN" --noremote_upload_local_results --remote_timeout=120)
fi
cd "$bench_dir/work"
start=$(date +%s%N)
timeout -k 10s 240 bazel --ignore_all_rc_files --output_user_root="$bench_dir/root" --host_jvm_args=-Xmx512m --batch build //... \
 --jobs=50 --enable_bzlmod=false "${flags[@]}" >"$bench_dir/log" 2>&1 || { cat "$bench_dir/log"; for f in "$bench_dir"/runner-*.log; do [[ ! -f $f ]] || cat "$f"; done; exit 1; }
end=$(date +%s%N)
echo "$mode wall_ms=$(((end-start)/1000000))"
rg 'Elapsed time:|processes:' "$bench_dir/log"
