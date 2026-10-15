package client

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/reapi"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

func sessionRunner(t *testing.T, ts *testServer, name string) *testRunner {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bazel")
	if err := os.WriteFile(p, []byte(fakeBazel), 0755); err != nil {
		t.Fatal(err)
	}
	return ts.startRunnerCfg(server.RunnerConfig{Name: name, Slots: 2, Sandbox: sandbox.None, BazelCommand: p})
}

func TestRunnerSessionPlacement(t *testing.T) {
	cfg := hubConfig("")
	cfg.MaxSessions = 1
	ts := startServer(t, cfg)
	r1 := sessionRunner(t, ts, "r1")
	r2 := sessionRunner(t, ts, "r2")
	ts.waitRunners(2)
	ws := bazelWS(t, map[string]string{"hello": "first"})
	run := func(name string) api.Session {
		t.Helper()
		r := ts.tun(ws, "bazel", "--session", name, "build", "//:hello")
		if r.code != 0 || !strings.Contains(r.stderr, "tun: bazel build exit=0") {
			t.Fatalf("%+v", r)
		}
		for _, s := range ts.sessions() {
			if s.Name == name {
				if s.Host != "r1" && s.Host != "r2" {
					t.Fatalf("host: %+v", s)
				}
				return s
			}
		}
		t.Fatal("no session")
		return api.Session{}
	}
	a := run("a")
	r := ts.tun(ws, "bazel", "--session", "a", "cat", "hello")
	if r.code != 0 || !strings.Contains(r.stdout, "first") || !strings.Contains(r.stderr, "warm server") {
		t.Fatalf("affinity: %+v", r)
	}
	if s := ts.sessions()[0]; s.Host != a.Host || s.Commands != 2 {
		t.Fatalf("affinity metadata: %+v", s)
	}
	cold := ts.tun(ws, "bazel", "--session", "a", "--cold", "build", "//:hello")
	if cold.code != 0 || !strings.Contains(cold.stderr, "--cold: discarded") || ts.sessions()[0].Commands != 1 {
		t.Fatalf("cold metadata: %+v sessions=%+v", cold, ts.sessions())
	}
	b := run("b")
	if b.Host == a.Host || len(ts.sessions()) != 2 {
		t.Fatalf("per-host capacity: %+v", ts.sessions())
	}
	run("c")
	if len(ts.sessions()) != 2 {
		t.Fatalf("capacity after eviction: %+v", ts.sessions())
	}
	// Kill the host owning c, then reuse that session identity on the survivor.
	var c api.Session
	for _, s := range ts.sessions() {
		if s.Name == "c" {
			c = s
		}
	}
	if c.Host == "r1" {
		r1.kill()
	} else {
		r2.kill()
	}
	ts.waitRunners(1)
	next := run("c")
	if next.Host == c.Host || next.Commands != 1 {
		t.Fatalf("host loss did not recreate: before=%+v after=%+v", c, next)
	}
}

func TestRunnerSessionCancelTimeoutSummary(t *testing.T) {
	ts := startServer(t, hubConfig(""))
	sessionRunner(t, ts, "r")
	ts.waitRunners(1)
	ws := bazelWS(t, map[string]string{})
	// A real HTTP submission is canceled while executing on the runner.
	r := ts.tun(ws, "bazel", "--timeout", "1s", "sleep", "20")
	if r.code != 124 {
		t.Fatalf("timeout: %+v", r)
	}
	r = ts.tun(ws, "bazel", "test", "//:bad")
	if r.code != 3 || !strings.Contains(r.stderr, "1 of 1 tests failed") {
		t.Fatalf("exit/summary: %+v", r)
	}
	r = ts.tun(ws, "bazel", "--json", "build", "//:ok")
	if r.code != 0 || !strings.Contains(r.stdout, `"success":true`) {
		t.Fatalf("BEP: %+v", r)
	}
	c := ts.client()
	j, _, err := c.createBazel(context.Background(), api.CreateJob{Bazel: &api.BazelJob{Root: "/cancel", Args: []string{"sleep", "20"}}, TimeoutSec: 30}, ws)
	if err != nil {
		t.Fatal(err)
	}
	ts.waitFor(j.ID, func(j api.Job) bool { return j.State == api.Running })
	if _, err := c.Cancel(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	ts.waitFor(j.ID, func(j api.Job) bool { return j.State == api.Canceled })
	// Cancellation releases the same lease for a subsequent command.
	eventually(t, "session released", func() bool {
		for _, s := range ts.sessions() {
			if s.Root == "/cancel" {
				return s.State == "idle"
			}
		}
		return false
	})
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
	seed := ""
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		seed = filepath.Join(x, "bazelisk")
	}
	runner := func(name string) *testRunner {
		return ts.startRunnerCfg(server.RunnerConfig{Name: name, Slots: 1, Sandbox: sandbox.Bwrap, BazeliskSeed: seed, PrivateNet: sandbox.NetUsable() == nil})
	}
	a := runner("a")
	b := runner("b")
	ts.waitRunners(2)
	ws := bazelWS(t, map[string]string{"MODULE.bazel": "module(name=\"t\")\n", ".bazelversion": "9.3.0\n", "BUILD.bazel": "genrule(name=\"hello\",outs=[\"hello.txt\"],cmd=\"echo hello > $@\")\n"})
	r := ts.tun(ws, "bazel", "build", "//:hello")
	if r.code != 0 {
		t.Fatalf("seed: %+v", r)
	}
	before := ts.sessions()[0]
	if before.Host == "a" {
		a.kill()
	} else {
		b.kill()
	}
	ts.waitRunners(1)
	r = ts.tun(ws, "bazel", "build", "//:hello")
	if r.code != 0 || !strings.Contains(r.stderr, "1 remote cache hit") {
		t.Fatalf("cross-host cache: %+v", r)
	}
	if after := ts.sessions()[0]; after.Host == before.Host || after.Commands != 1 {
		t.Fatalf("recreated session: %+v", after)
	}
}

func TestRunnerSessionStreamingAndSerialization(t *testing.T) {
	cfg := hubConfig("")
	cfg.MaxSessions = 2
	cfg.KillGrace = 500 * time.Millisecond
	ts := startServer(t, cfg)
	sessionRunner(t, ts, "a")
	sessionRunner(t, ts, "b")
	ts.waitRunners(2)
	c := ts.client()
	start := func(name string, args ...string) api.Job {
		t.Helper()
		j, _, err := c.createBazel(context.Background(), api.CreateJob{Bazel: &api.BazelJob{Root: "/stream", Session: name, Args: args}, TimeoutSec: 30}, "")
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	a := start("a", "sh", "echo streaming; sleep 20")
	ts.waitFor(a.ID, func(j api.Job) bool { return j.State == api.Running })
	eventually(t, "output arrives while command runs", func() bool {
		var b bytes.Buffer
		_, err := c.Logs(context.Background(), a.ID, "stdout", 0, false, &b)
		return err == nil && strings.Contains(b.String(), "streaming")
	})
	queued := start("a", "build")
	if j := ts.status(queued.ID); j.State != api.Queued {
		t.Fatalf("same session overlapped: %+v", j)
	}
	for _, name := range []string{"b", "c"} {
		j := start(name, "build")
		if got := ts.waitDone(j.ID); got.State != api.Exited {
			t.Fatalf("other session: %+v", got)
		}
	}
	if ss := ts.sessions(); len(ss) != 3 {
		t.Fatalf("aggregate capacity should exceed two: %+v", ss)
	}
	if _, err := c.Cancel(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	ts.waitDone(a.ID)
	end := ts.waitDone(queued.ID)
	if end.State != api.Exited || end.Runner != ts.status(a.ID).Runner {
		t.Fatalf("queued affinity: %+v", end)
	}
}

func TestRunnerSessionTimeoutDuringSync(t *testing.T) {
	ts := startServer(t, hubConfig(""))
	original := ts.hs.Handler
	ts.hs.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/runner/cas") {
			<-r.Context().Done()
			return
		}
		original.ServeHTTP(w, r)
	})
	sessionRunner(t, ts, "r")
	ts.waitRunners(1)
	ws := bazelWS(t, map[string]string{"input": "data"})
	r := ts.tun(ws, "bazel", "--timeout", "1s", "build")
	if r.code != 124 {
		t.Fatalf("sync must be inside the command timeout: %+v", r)
	}
}

func TestRunnerSessionHubDiskCacheLimit(t *testing.T) {
	cfg := hubConfig("")
	cfg.DiskCacheMax = "123M"
	ts := startServer(t, cfg)
	sessionRunner(t, ts, "r")
	ts.waitRunners(1)
	ws := bazelWS(t, map[string]string{})
	r := ts.tun(ws, "bazel", "sh", `cat "$HOME/../../tun.bazelrc"`)
	if r.code != 0 || !strings.Contains(r.stdout, "--experimental_disk_cache_gc_max_size=123M") {
		t.Fatalf("runner did not apply hub disk cache limit: %+v", r)
	}
}
