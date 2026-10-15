package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/reapi"
	"github.com/altdansalt/tun/internal/sandbox"
)

// A separate test process exercises the same Runner as the CLI, including HTTP
// polling and the per-executor disk cache. It never uses the shared main runners.
func TestActionRunnerProcess(t *testing.T) {
	value := os.Getenv("TUN_TEST_ACTION_RUNNER")
	if value == "" {
		return
	}
	var cfg RunnerConfig
	if err := json.Unmarshal([]byte(value), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Logf = t.Logf
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Run()
}
func TestBazelRunnerExecution(t *testing.T) {
	if _, err := exec.LookPath("bazel"); err != nil {
		t.Skip("bazel missing")
	}
	mode := sandbox.None
	if sandbox.BwrapUsable() {
		mode = sandbox.Bwrap
	}
	s, err := New(Config{DataDir: t.TempDir(), HubOnly: true, Sandbox: mode, RunnerPollWait: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	store, err := reapi.Open(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	s.actionStore = store
	execution := reapi.NewExecution(store, -1, s.executeAction)
	defer execution.Close()
	cache := &reapi.Handler{Store: store, Token: s.Token(), Execution: execution}
	apiHandler := s.Handler()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Protocols: reapi.Protocols(), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			cache.ServeHTTP(w, r)
		} else {
			apiHandler.ServeHTTP(w, r)
		}
	})}
	go hs.Serve(ln)
	defer hs.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cfg, _ := json.Marshal(map[string]any{"Hub": "http://" + ln.Addr().String(), "Token": s.RunnerToken(), "Name": "action-test", "Slots": 2, "DataDir": t.TempDir(), "Sandbox": mode})
	proc := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestActionRunnerProcess$")
	proc.Env = append(os.Environ(), "TUN_TEST_ACTION_RUNNER="+string(cfg))
	proc.Stdout = os.Stderr
	proc.Stderr = os.Stderr
	if err = proc.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { cancel(); proc.Wait() }()
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "MODULE.bazel"), nil, 0600)
	os.WriteFile(filepath.Join(work, "BUILD.bazel"), []byte(`genrule(name="a", outs=["a.txt"], cmd="echo from-runner > $@")
genrule(name="b", srcs=[":a"], outs=["b.txt"], cmd="cat $(location :a) > $@")
genrule(name="bad", outs=["bad.txt"], cmd="echo runner-action-failure >&2; exit 1")`), 0600)
	run := func(target string, success bool) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bazel", "--output_user_root="+t.TempDir(), "--host_jvm_args=-Xmx512m", "--batch", "build", target, "--jobs=2", "--remote_executor=grpc://"+ln.Addr().String(), "--remote_header=x-tun-token="+s.Token(), "--remote_timeout=20", "--noremote_upload_local_results", "--enable_bzlmod=false")
		cmd.Dir = work
		cmd.WaitDelay = 3 * time.Second
		out, err := cmd.CombinedOutput()
		if (err == nil) != success {
			t.Fatalf("bazel: %v\n%s", err, out)
		}
		return string(out)
	}
	out := run("//:b", true)
	if !strings.Contains(out, "remote") {
		t.Fatal(out)
	}
	b, err := os.ReadFile(filepath.Join(work, "bazel-bin", "b.txt"))
	if err != nil || string(b) != "from-runner\n" {
		t.Fatalf("%q %v", b, err)
	}
	out = run("//:b", true)
	if !strings.Contains(out, "remote cache hit") {
		t.Fatal(out)
	}
	out = run("//:bad", false)
	if !strings.Contains(out, "runner-action-failure") {
		t.Fatal(out)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		t.Fatal("no actions scheduled")
	}
	for _, j := range s.jobs {
		if j.Runner != "action-test" {
			t.Fatalf("action on %s", j.Runner)
		}
	}
}
