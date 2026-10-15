package client

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/reapi"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

// remoteProcs returns n from Bazel's "INFO: N processes: ..., n remote." line (0 if none).
func remoteProcs(stderr string) int {
	m := regexp.MustCompile(`processes: [^\n]*?(\d+) remote[.,\n]`).FindStringSubmatch(stderr)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// A session on a server with executors gets remote execution without asking: its actions run
// in the server's slots (which the session command itself doesn't hold, or with one slot they
// would wait forever) and on runners, and their results land in the shared action cache.
func TestBazelRemoteExecution(t *testing.T) {
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
	cfg := server.Config{Sandbox: sandbox.Bwrap, Cache: store, MaxJobs: 1, MaxSessions: 1,
		RunnerPollWait: 100 * time.Millisecond, RunnerDeadAfter: 5 * time.Second}
	if sandbox.NetUsable() == nil {
		cfg.PrivateNet = true
	}
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" { // in a tun job: its shared bazelisk downloads
		cfg.BazeliskSeed = filepath.Join(x, "bazelisk")
	}
	ts := bazelServer(t, cfg, true)
	var build strings.Builder
	for i := range 4 {
		fmt.Fprintf(&build, "genrule(name = \"g%d\", srcs = [\"in.txt\"], outs = [\"g%d.txt\"], cmd = \"sleep 0.2; cat $< > $@; echo %d >> $@\")\n", i, i, i)
	}
	ws := bazelWS(t, map[string]string{
		"MODULE.bazel":  "module(name = \"t\")\n",
		"BUILD.bazel":   build.String(),
		"in.txt":        "one\n",
		".bazelversion": "9.3.0\n",
	})
	out := func(name string) string {
		t.Helper()
		ss := ts.sessions()
		b, err := os.ReadFile(filepath.Join(ts.data, "sessions", ss[0].ID, "sandbox/out/base/execroot/_main/bazel-out/k8-fastbuild/bin", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// The hub's one slot only: the session command runs beside it.
	r := ts.tun(ws, "bazel", "build", "//...")
	if r.code != 0 || !strings.Contains(r.stderr, "remote execution on 1 slots") || remoteProcs(r.stderr) != 4 || out("g3.txt") != "one\n3\n" {
		t.Fatalf("first build (hub slot): %+v", r)
	}
	if h := ts.health(); h.Execution == nil || h.Execution.Executed[api.LocalRunner] != 4 || h.Execution.Slots != 1 {
		t.Fatalf("health after the first build: %+v", h.Execution)
	}

	// A runner joins; the hub's slot is busy with a v0 job, so every action runs on the runner.
	ts.startRunnerCfg(server.RunnerConfig{Name: "r1", Slots: 2, Sandbox: sandbox.Bwrap})
	ts.waitRunners(1)
	eventually(t, "runner takes actions", func() bool { return ts.health().Execution.Slots == 3 })
	blocker := ts.detach("", "--no-workspace", "--", "sleep", "600")
	ts.waitFor(blocker, func(j api.Job) bool { return j.State == api.Running })
	write(t, filepath.Join(ws, "in.txt"), "two\n", 0o644)
	r = ts.tun(ws, "bazel", "build", "//...")
	if r.code != 0 || remoteProcs(r.stderr) != 4 || out("g0.txt") != "two\n0\n" {
		t.Fatalf("second build (runner): %+v", r)
	}
	if h := ts.health(); h.Execution.Executed["r1"] != 4 || h.Execution.Executed[api.LocalRunner] != 4 {
		t.Fatalf("health after the second build: %+v", h.Execution)
	}
	if r := ts.tun("", "cancel", blocker); r.code != 0 {
		t.Fatalf("cancel: %+v", r)
	}

	// A new session finds the executors' results in the shared action cache.
	r = ts.tun(ws, "bazel", "--cold", "build", "//...")
	if r.code != 0 || !strings.Contains(r.stderr, "4 remote cache hit") || out("g1.txt") != "two\n1\n" {
		t.Fatalf("--cold: %+v", r)
	}
}
