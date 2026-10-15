package client

import (
	"os"
	"path/filepath"
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
