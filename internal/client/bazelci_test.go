package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/server"
)

// TestBazelCIScript runs scripts/bazel-ci.sh through tun run, the way agents do, with a fake
// bazel that prints its arguments ($TMPDIR and $XDG_CACHE_HOME shown as <TMP> and <XDG>) and
// leaves a read-only dir in its output root, like real Bazel.
func TestBazelCIScript(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	script, err := os.ReadFile("../../scripts/bazel-ci.sh")
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	for _, d := range []string{"scripts", "bin", "a"} {
		os.MkdirAll(filepath.Join(ws, d), 0o755)
	}
	os.WriteFile(filepath.Join(ws, "scripts/bazel-ci.sh"), script, 0o755)
	os.WriteFile(filepath.Join(ws, "bin/bazel"), []byte(`#!/bin/bash
out=bazel:
for a in "$@"; do a=${a//$TMPDIR/<TMP>}; out+=" ${a//$XDG_CACHE_HOME/<XDG>}"; done
echo "$out"
root=${1#--output_user_root=}
mkdir -p "$root/ro" && touch "$root/ro/f" && chmod a-w "$root/ro"
[ "$2" != test ] || exit ${FAKE_CODE:-0}
`), 0o755)
	os.WriteFile(filepath.Join(ws, "a/a.go"), []byte("package a\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "a/a_test.go"), []byte("package a\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "a/BUILD.bazel"), []byte(`go_library(srcs = ["a.go"])
go_test(srcs = ["a_test.go"])
`), 0o644)
	run := func(args string, env ...string) result {
		a := []string{"run"}
		for _, e := range env {
			a = append(a, "-e", e)
		}
		return ts.tun(ws, append(a, "-c", `PATH=$PWD/bin:$PATH ./scripts/bazel-ci.sh `+args+
			`; s=$?; [ -e "$TMPDIR/tun-bazel-root" ] || echo root-deleted; exit $s`)...)
	}
	const root = "bazel: --output_user_root=<TMP>/tun-bazel-root"
	caches := " --repository_cache=<XDG>/tun-bazel/repo --disk_cache=<XDG>/tun-bazel/disk --experimental_disk_cache_gc_max_size=3G"

	want := root + " test" + caches + " //...\n" + root + " shutdown\nroot-deleted\n"
	if r := run(""); r.code != 0 || r.stdout != want {
		t.Errorf("default: want stdout %q, got %+v", want, r)
	}
	// Arguments replace //..., unchanged; bazel's exit status is the script's.
	want = root + " test" + caches + " --test_filter=TestX //a:a_test\n" + root + " shutdown\nroot-deleted\n"
	if r := run("--test_filter=TestX //a:a_test", "FAKE_CODE=3"); r.code != 3 || r.stdout != want {
		t.Errorf("args and exit status: want 3 and %q, got %+v", want, r)
	}
	want = root + " test --repository_cache=<XDG>/tun-bazel/repo //...\n" + root + " shutdown\nroot-deleted\n"
	if r := run("", "BAZEL_DISK_CACHE=0"); r.code != 0 || r.stdout != want {
		t.Errorf("BAZEL_DISK_CACHE=0: want %q, got %+v", want, r)
	}
	// A Go file missing from its BUILD.bazel would silently not be built or run: fail first.
	os.WriteFile(filepath.Join(ws, "a/b_test.go"), []byte("package a\n"), 0o644)
	if r := run(""); r.code != 1 || strings.Contains(r.stdout, "bazel:") ||
		!strings.Contains(r.stderr, "not in BUILD.bazel (run: bazel run //:gazelle): ./a/b_test.go\n") {
		t.Errorf("missing file: %+v", r)
	}
}
