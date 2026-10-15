package client

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/reapi"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

// TestBazelReal runs the real Bazel (skipped without bazel and bwrap): a genrule workspace
// needs no downloads. The second command must find the server warm and its analysis cached.
func TestBazelReal(t *testing.T) {
	if _, err := exec.LookPath("bazel"); err != nil {
		t.Skip("bazel not on PATH")
	}
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap unusable (without it a Bazel server would outlive the test)")
	}
	store, err := reapi.Open(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.Config{Sandbox: sandbox.Bwrap, Cache: store}
	if sandbox.NetUsable() == nil {
		cfg.PrivateNet = true
	}
	cfg.BazeliskSeed = bazeliskSeed(t)
	ts := bazelServer(t, cfg, true)
	ws := bazelWS(t, map[string]string{
		"MODULE.bazel":  "module(name = \"t\")\n",
		"BUILD.bazel":   "genrule(name = \"hello\", srcs = [\"in.txt\"], outs = [\"hello.txt\"], cmd = \"cat $< > $@\")\n",
		"in.txt":        "one\n",
		".bazelversion": "9.3.0\n", // what the tun hosts have cached
	})
	out := func() string {
		t.Helper()
		ss := ts.sessions()
		b, err := os.ReadFile(filepath.Join(ts.data, "sessions", ss[0].ID, "sandbox/out/base/execroot/_main/bazel-out/k8-fastbuild/bin/hello.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	r := ts.tun(ws, "bazel", "build", "//:hello")
	if r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") || out() != "one\n" ||
		!regexp.MustCompile(`tun: bazel build exit=0 after [0-9.]+s: 1 targets built\n`).MatchString(r.stderr) {
		t.Fatalf("first: %+v", r)
	}
	r = ts.tun(ws, "bazel", "build", "//:hello")
	if r.code != 0 || strings.Contains(r.stderr, "Starting local Bazel server") || !strings.Contains(r.stderr, "(0 packages loaded, 0 targets configured)") {
		t.Fatalf("second (warm): %+v", r)
	}
	write(t, filepath.Join(ws, "in.txt"), "two\n", 0o644)
	r = ts.tun(ws, "bazel", "build", "//:hello")
	if r.code != 0 || strings.Contains(r.stderr, "Starting local Bazel server") || out() != "two\n" {
		t.Fatalf("third (changed input): %+v", r)
	}
	if r = ts.tun(ws, "bazel", "build", "//:nope"); r.code == 0 || !strings.Contains(r.stderr, "no such target") ||
		!strings.Contains(r.stderr, "tun: bazel build exit=1 after") {
		t.Fatalf("bad target: %+v", r)
	}
	r = ts.tun(ws, "bazel", "--json", "build", "//:hello")
	var sum struct {
		Success bool
		Targets []struct {
			Label   string
			Outputs []struct{ Path string }
		}
	}
	if err := json.Unmarshal([]byte(r.stdout), &sum); err != nil || !sum.Success || len(sum.Targets) != 1 || len(sum.Targets[0].Outputs) != 1 ||
		sum.Targets[0].Outputs[0].Path != "bazel-bin/hello.txt" {
		t.Fatalf("--json: %v: %+v", err, r)
	}
	// A new session (as on a new host): with a remote cache sessions have no disk cache, so the
	// hit comes through the session's socket.
	r = ts.tun(ws, "bazel", "--cold", "build", "//:hello")
	if r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") || !strings.Contains(r.stderr, "1 remote cache hit") || out() != "two\n" {
		t.Fatalf("--cold with a warm remote cache: %+v", r)
	}
}
