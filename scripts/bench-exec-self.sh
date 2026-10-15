#!/bin/bash
# Supply m0-build.tar containing M0's Bazel files when the branches are separate.
set -euo pipefail
bench_dir=$(mktemp -d "${TMPDIR:-/tmp}/exec-self.XXXXXX")
trap 'rm -rf "$bench_dir"' EXIT
mkdir "$bench_dir/work"
cp -a . "$bench_dir/work/"
cd "$bench_dir/work"
tar xf m0-build.tar
# Refresh package sources/deps for packages added since the M0 snapshot.
python3 - <<'PY'
from pathlib import Path
for p in Path('internal').iterdir():
 if not p.is_dir(): continue
 sources=sorted(f.name for f in p.glob('*.go') if not f.name.endswith('_test.go'))
 if not sources: continue
 deps=set()
 for name in sources:
  import re
  for dep in re.findall(r'"github.com/altdansalt/tun/(internal/[^"\s]+)"',(p/name).read_text()):deps.add('//'+dep)
 (p/'BUILD.bazel').write_text('load("@rules_go//go:def.bzl", "go_library")\ngo_library(name='+repr(p.name)+', srcs='+repr(sources)+', importpath='+repr('github.com/altdansalt/tun/'+str(p))+', visibility=["//:__subpackages__"], deps='+repr(sorted(deps))+')\n')
PY
start=$(date +%s%N)
timeout -k 10s 600 bazel --output_user_root="$bench_dir/root" --host_jvm_args=-Xmx512m --batch build //... \
 --jobs=4 --remote_executor="${TUN_URL/http:/grpc:}" --remote_header="x-tun-token=$TUN_TOKEN" \
 --noremote_upload_local_results --remote_timeout=600 --verbose_failures >"$bench_dir/log" 2>&1 || { cat "$bench_dir/log"; exit 1; }
end=$(date +%s%N)
echo "self wall_ms=$(((end-start)/1000000))"
rg 'Elapsed time:|processes:' "$bench_dir/log"
