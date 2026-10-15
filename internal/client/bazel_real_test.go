package client

import (
	"encoding/json"
	"net/http"
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

// Two real session sandboxes, independent output/repository caches, one hub REAPI
// cache. The cold rebuild must get its action result through runner-side forwarding.
func TestRunnerSessionCacheAcrossHosts(t *testing.T) {
	if _, err := exec.LookPath("bazel"); err != nil {
		t.Skip("bazel not installed")
	}
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap unavailable")
	}
	store, err := reapi.Open(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	cfg := hubConfig("")
	cfg.Cache = store
	ts := startServer(t, cfg)
	seed := bazeliskSeed(t)
	runner := func(name string) *testRunner {
		return ts.startRunnerCfg(server.RunnerConfig{Name: name, Slots: 1, Sandbox: sandbox.Bwrap, BazeliskSeed: seed, PrivateNet: sandbox.NetUsable() == nil})
	}
	a := runner("a")
	b := runner("b")
	ts.waitRunners(2)
	ws := bazelWS(t, map[string]string{"MODULE.bazel": "module(name=\"t\")\n", ".bazelversion": "9.3.0\n", "BUILD.bazel": "genrule(name=\"hello\",outs=[\"hello.txt\"],cmd=\"echo hello > $@\")\n"})
	alice, err := server.IssueClientToken(ts.data, "alice")
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) result { return ts.tunEnv(ws, map[string]string{"TUN_TOKEN": alice}, args...) }

	r := run("bazel", "build", "//:hello")
	if r.code != 0 {
		t.Fatalf("seed: %+v", r)
	}
	before := ts.sessions()[0]
	if before.Client != "alice" {
		t.Fatalf("token identity lost: %+v", before)
	}
	original := before.LastJob
	// The runner stages artifacts on the hub before releasing the session lock.
	got := t.TempDir()
	download := ts.tunEnv(ws, map[string]string{"TUN_TOKEN": alice}, "get", "-o", got, original, "//:hello")
	if download.code != 0 {
		t.Fatalf("artifact: %+v", download)
	}
	data, err := os.ReadFile(filepath.Join(got, "bazel-bin", "hello.txt"))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("artifact %q %v", data, err)
	}

	if before.Host == "a" {
		a.kill()
	} else {
		b.kill()
	}
	ts.waitRunners(1)
	r = run("bazel", "build", "//:hello")
	if r.code != 0 || !strings.Contains(r.stderr, "1 remote cache hit") {
		t.Fatalf("cross-host cache: %+v", r)
	}
	if after := ts.sessions()[0]; after.Host == before.Host || after.Commands != 1 {
		t.Fatalf("recreated session: %+v", after)
	}
	bob, err := server.IssueClientToken(ts.data, "bob")
	if err != nil {
		t.Fatal(err)
	}
	denied := ts.tunEnv(ws, map[string]string{"TUN_TOKEN": bob}, "get", "-o", got, original, "//:hello")
	if denied.code == 0 {
		t.Fatal("bob downloaded alice's artifact")
	}
	cold := ts.tunEnv(ws, map[string]string{"TUN_TOKEN": bob}, "bazel", "build", "//:hello")
	if cold.code != 0 || strings.Contains(cold.stderr, "1 remote cache hit") {
		t.Fatalf("client cache namespace: %+v", cold)
	}
	// The original command stays downloadable after its runner has disappeared.
	download = ts.tunEnv(ws, map[string]string{"TUN_TOKEN": alice}, "get", "-o", got, original, "//:hello")
	if download.code != 0 {
		t.Fatalf("artifact after host loss: %+v", download)
	}
	var summary map[string]any
	req, _ := http.NewRequest("GET", ts.url+"/v1/jobs/"+original+"/bep", nil)
	req.Header.Set("X-Tun-Token", alice)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil || summary["success"] != true {
		t.Fatalf("retained BEP: %v %v", summary, err)
	}
}
