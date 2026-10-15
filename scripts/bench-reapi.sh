#!/bin/bash
# Run inside scripts/dev-tun.sh with --cache-dir=reapi --cache-action-writes=true, or on the host with
# CACHE_URL=grpcs://remote.buildbuddy.dev BUILDBUDDY_API_KEY set. No credentials printed.
set -euo pipefail
bench_dir=$(mktemp -d "${TMPDIR:-/tmp}/reapi-bench.XXXXXX")
trap 'rm -rf "$bench_dir"' EXIT
mkdir "$bench_dir/work"
touch "$bench_dir/work/MODULE.bazel"
for i in $(seq 1 200); do
  printf 'genrule(name="g%s", outs=["o%s"], cmd="echo %s > $@")\n' "$i" "$i" "$i"
done > "$bench_dir/work/BUILD.bazel"
if [[ ${CACHE_URL:-} == grpcs:* ]]; then
  cache_url=$CACHE_URL
  header="x-buildbuddy-api-key=${BUILDBUDDY_API_KEY:?}"
else
  cache_url=${TUN_URL/http:/grpc:}
  header="x-tun-token=${TUN_TOKEN:?}"
fi
cd "$bench_dir/work"
for phase in seed rebuild1 rebuild2; do
  rm -rf "$bench_dir/root"
  start=$(date +%s%N)
  bazel --output_user_root="$bench_dir/root" --host_jvm_args=-Xmx512m --batch build //... \
    --jobs=2 --remote_cache="$cache_url" --remote_header="$header" --remote_timeout=30 \
    --repository_cache="$bench_dir/repos" >"$bench_dir/log" 2>&1 || { cat "$bench_dir/log"; exit 1; }
  end=$(date +%s%N)
  echo "$phase wall_ms=$(((end-start)/1000000))"
  rg 'Elapsed time:|processes:' "$bench_dir/log"
done
