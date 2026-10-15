#!/usr/bin/env bash
# Run from the repository root via tun run; output bases must stay job-local.
set -eu
out="$PWD/internal/bep/testdata"
work="$TMPDIR/bep-work"
mkdir -p "$work"
cd "$work"
echo 9.3.0 > .bazelversion
cat > MODULE.bazel <<'MODULE'
module(name = "bep_fixture")
bazel_dep(name = "rules_shell", version = "0.6.1")
MODULE
cat > BUILD.bazel <<'BUILD'
load("@rules_shell//shell:sh_test.bzl", "sh_test")
genrule(name="output", outs=["hello.txt"], cmd="echo hello > $@")
genrule(name="compile_error", outs=["bad.o"], cmd="echo 'int main( {' > broken.c; /usr/bin/cc -c broken.c -o $@")
sh_test(name="pass", srcs=["pass.sh"])
sh_test(name="fail", srcs=["fail.sh"])
sh_test(name="flaky", srcs=["flaky.sh"])
sh_test(name="sharded", srcs=["pass.sh"], shard_count=3)
sh_test(name="timeout", srcs=["timeout.sh"], timeout="short")
BUILD
printf '#!/bin/sh\nexit 0\n' > pass.sh
printf '#!/bin/sh\necho deliberate_failure >&2\nexit 1\n' > fail.sh
printf '#!/bin/sh\n[ "${TEST_RUN_NUMBER:-1}" -gt 1 ]\n' > flaky.sh
# Use a job-local marker and standalone execution for deterministic retry coverage.
cat > flaky.sh <<'SH'
#!/bin/sh
marker="$BEP_FLAKY_MARKER"
if [ -e "$marker" ]; then exit 0; fi
touch "$marker"
exit 1
SH
printf '#!/bin/sh\nsleep 10\n' > timeout.sh
# A sharded test must acknowledge the sharding protocol.
sed -i '2i [ -z "${TEST_SHARD_STATUS_FILE:-}" ] || touch "$TEST_SHARD_STATUS_FILE"' pass.sh
chmod +x *.sh
run() {
 name="$1"; shift
 set +e
 bazel --output_user_root="$TMPDIR/bep-bazel" "$@" --build_event_json_file="$out/$name.jsonl" --jobs=2 --noshow_progress > "$out/$name.stdout" 2> "$out/$name.stderr"
 code=$?
 set -e
 echo "$name exit=$code"
}
run success build //:output
run build_failure build //:compile_error
run test_failure test //:fail
run flaky test //:flaky --flaky_test_attempts=3 --test_strategy=standalone --test_env="BEP_FLAKY_MARKER=$TMPDIR/flaky-marker"
run sharded test //:sharded
run timeout test //:timeout --test_timeout=1
run analysis_failure build //:missing
# query does not support the BEP flag: preserve its empty stream and process result.
set +e
bazel --output_user_root="$TMPDIR/bep-bazel" query //... > "$out/query.stdout" 2> "$out/query.stderr"
echo "query exit=$?"
set -e
: > "$out/query.jsonl"
