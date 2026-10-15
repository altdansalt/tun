package client

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Under `bazel test`, TUN_TEST_GO is the runfiles path of the hermetic Go SDK's go binary
// (see BUILD.bazel): put its real bin dir first on PATH, so tests that run `go` in jobs
// (TestBwrapGoBuild, TestNetGoModuleDownload) use it rather than whatever the host has.
// The real path, because the runfiles tree may be hidden inside a job's sandbox.
func init() {
	p := os.Getenv("TUN_TEST_GO")
	if p == "" {
		return
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(os.Getenv("TEST_SRCDIR"), p)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		os.Setenv("PATH", filepath.Dir(real)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
}

// testBazel is the Bazel binary for tests that run real Bazel nested in `bazel test`:
// TUN_TEST_BAZEL, the runfiles path of @bazel_binary (MODULE.bazel), resolved; "" outside Bazel.
func testBazel() string {
	p := os.Getenv("TUN_TEST_BAZEL")
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(os.Getenv("TEST_SRCDIR"), p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return real
}

// bazeliskSeed returns a bazelisk home for tun serve --bazelisk-seed. Under bazel test it is
// made from testBazel (a file named bazel-<version>-<os>-<arch>) in bazelisk's own cache
// layout, so a nested tun bazel needs neither the network nor a cache from outside the test
// sandbox (neither is there: the test env has no proxy variables, XDG_CACHE_HOME or
// BAZELISK_HOME). In a plain tun job it is the job's shared bazelisk cache; else "".
func bazeliskSeed(t *testing.T) string {
	t.Helper()
	bin := testBazel()
	if bin == "" {
		if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
			return filepath.Join(x, "bazelisk")
		}
		return ""
	}
	name := filepath.Base(bin)
	if !strings.HasPrefix(name, "bazel-") {
		t.Fatalf("TUN_TEST_BAZEL %s: want a file named bazel-<version>-<os>-<arch>", bin)
	}
	src, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	home := t.TempDir()
	tmp := filepath.Join(home, "bazel.tmp")
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(dst, h), src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	bindir := filepath.Join(home, "downloads", "sha256", sum, "bin")
	meta := filepath.Join(home, "downloads", "metadata", "bazelbuild")
	for _, d := range []string{bindir, meta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(tmp, filepath.Join(bindir, "bazel")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meta, name), []byte(sum), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}
