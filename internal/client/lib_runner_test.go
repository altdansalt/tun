// Shared test helpers (:testlib): runners against the in-process hub.

package client

import (
	"fmt"
	"time"

	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
	"github.com/altdansalt/tun/internal/workspace"
)

// hubConfig is a pure hub (no local slots) with short runner timeouts for tests.
func hubConfig(data string) server.Config {
	return server.Config{DataDir: data, HubOnly: true, RunnerPollWait: 100 * time.Millisecond, RunnerDeadAfter: time.Second}
}

type testRunner struct {
	r    *server.Runner
	done chan struct{}
}

func (ts *testServer) startRunner(name string, slots int) *testRunner {
	return ts.startRunnerCfg(server.RunnerConfig{Name: name, Slots: slots, Sandbox: sandbox.None})
}

func (ts *testServer) startRunnerCfg(cfg server.RunnerConfig) *testRunner {
	t := ts.t
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { workspace.RemoveAll(dir) })
	cfg.Hub, cfg.Token, cfg.DataDir, cfg.KillGrace, cfg.Logf = ts.url, ts.srv.Token(), dir, 50*time.Millisecond, t.Logf
	r, err := server.NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tr := &testRunner{r: r, done: make(chan struct{})}
	go func() { r.Run(); close(tr.done) }()
	t.Cleanup(tr.kill)
	return tr
}

// kill stops the runner like a crash.
func (tr *testRunner) kill() {
	tr.r.Kill()
	<-tr.done
}

func (ts *testServer) waitRunners(n int) {
	ts.t.Helper()
	eventually(ts.t, fmt.Sprintf("%d runners", n), func() bool { return len(ts.health().Runners) == n })
}
