package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
	"github.com/altdansalt/tun/internal/workspace"
)

// hubConfig is a pure hub (no local slots) with short runner timeouts for tests.
func hubConfig(data string) server.Config {
	return server.Config{DataDir: data, HubOnly: true, RunnerPollWait: 100 * time.Millisecond, RunnerDeadAfter: time.Second}
}

// release returns -e flags and a shell snippet that waits until the returned func is called.
// Jobs here run unsandboxed on this host, so a file in a test dir works as the signal.
func release(t *testing.T) (env []string, wait string, done func()) {
	p := filepath.Join(t.TempDir(), "release")
	return []string{"-e", "REL=" + p}, `while [ ! -e "$REL" ]; do sleep .01; done`, func() { os.WriteFile(p, nil, 0o644) }
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

func TestRunnerJobs(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	ts.startRunner("r1", 2)
	ts.startRunner("r2", 2)
	ts.waitRunners(2)
	if h := ts.health(); h.LocalSlots != 0 || h.MaxJobs != 4 {
		t.Errorf("health: %+v", h)
	}
	if r := ts.tun("", "runners"); r.code != 0 || strings.Count(r.stdout, "\n") != 2 || !strings.Contains(r.stdout, `"name":"r1"`) {
		t.Errorf("tun runners: %+v", r)
	}

	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "sub"), 0o755)
	os.WriteFile(filepath.Join(ws, "sub", "f.txt"), []byte("content"), 0o644)
	r := ts.tun(ws, "run", "--", "sh", "-c", "echo out; echo err >&2; exit 3")
	if r.code != 3 || r.stdout != "out\n" || !strings.Contains(r.stderr, "\nerr\n") {
		t.Errorf("exit/stdout/stderr: %+v", r)
	}
	if !regexp.MustCompile(`\ntun: running on runner r[12]\n`).MatchString(r.stderr) {
		t.Errorf("no running-on line: %+v", r)
	}
	r = ts.tun(ws, "run", "-C", "sub", "-e", "FOO=bar", "--", "sh", "-c", `printf '%s %s ' "$FOO" "$(basename "$PWD")"; cat f.txt; test -n "$TUN_JOB_ID"`)
	if r.code != 0 || r.stdout != "bar sub content" {
		t.Errorf("env/cwd: %+v", r)
	}
	r = ts.tun("", "run", "--", "sh", "-c", "kill -9 $$")
	if r.code != 128+9 {
		t.Errorf("signal: %+v", r)
	}
	r = ts.tun("", "run", "-C", "nope", "--no-workspace", "--", "true")
	if r.code != 125 || !strings.Contains(r.stderr, `cwd "nope" does not exist`) {
		t.Errorf("bad cwd: %+v", r)
	}
	js, err := ts.client().List(context.Background(), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range js {
		if j.Runner != "r1" && j.Runner != "r2" {
			t.Errorf("job %s ran on %q", j.ID, j.Runner)
		}
	}
}

func TestRunnerConcurrency(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	ts.startRunner("r1", 1)
	ts.startRunner("r2", 1)
	ts.waitRunners(2)
	env, wait, done := release(t)
	var ids []string
	for range 3 {
		ids = append(ids, ts.detach("", append(env, "--no-workspace", "--", "sh", "-c", wait+"; echo $TUN_JOB_ID")...))
	}
	eventually(t, "two jobs running at once", func() bool {
		a, b := ts.status(ids[0]), ts.status(ids[1])
		return a.State == api.Running && b.State == api.Running
	})
	a, b, c := ts.status(ids[0]), ts.status(ids[1]), ts.status(ids[2])
	if a.Runner == b.Runner || c.State != api.Queued || c.QueuePosition == nil || *c.QueuePosition != 1 {
		t.Errorf("placement: %s on %s, %s on %s, third %+v", a.ID, a.Runner, b.ID, b.Runner, c)
	}
	if h := ts.health(); h.Running != 2 || h.Queued != 1 {
		t.Errorf("health: %+v", h)
	}
	done()
	for _, id := range ids {
		if j := ts.waitDone(id); j.State != api.Exited || *j.ExitCode != 0 {
			t.Errorf("job %+v", j)
		}
		if r := ts.tun("", "logs", id); r.stdout != id+"\n" {
			t.Errorf("logs of %s: %+v", id, r)
		}
	}
}

func TestRunnerCancelAndTimeout(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	ts.startRunner("r1", 2)
	ts.waitRunners(1)
	id := ts.detach("", "--no-workspace", "--", "sh", "-c", "echo started; exec sleep 60")
	ts.waitFor(id, func(j api.Job) bool { return j.State == api.Running })
	eventually(t, "log shipped", func() bool { return ts.tun("", "logs", id).stdout == "started\n" })
	start := time.Now()
	if r := ts.tun("", "cancel", id); r.code != 0 {
		t.Fatal(r)
	}
	j := ts.waitDone(id)
	if j.State != api.Canceled || j.Signal == nil || *j.Signal != 15 || time.Since(start) > 5*time.Second {
		t.Errorf("canceled: %+v after %v", j, time.Since(start))
	}

	r := ts.tun("", "run", "--no-workspace", "--timeout", "1s", "--", "sleep", "30")
	if r.code != 124 || !strings.Contains(r.stderr, "timed out") {
		t.Errorf("timeout: %+v", r)
	}
	// The runner is free again.
	if r := ts.tun("", "run", "--no-workspace", "--", "echo", "ok"); r.code != 0 || r.stdout != "ok\n" {
		t.Errorf("after: %+v", r)
	}
}

func TestRunnerLost(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	r1 := ts.startRunner("r1", 1)
	ts.waitRunners(1)
	lost := ts.detach("", "--no-workspace", "--", "sh", "-c", "echo started; exec sleep 60")
	queued := ts.detach("", "--no-workspace", "--", "echo", "second")
	ts.waitFor(lost, func(j api.Job) bool { return j.State == api.Running })
	eventually(t, "log shipped", func() bool { return ts.tun("", "logs", lost).stdout == "started\n" })
	r1.kill()
	j := ts.waitDone(lost)
	if j.State != api.Failed || !strings.Contains(j.Error, "runner r1 lost") {
		t.Errorf("lost job: %+v", j)
	}
	if j := ts.status(queued); j.State != api.Queued {
		t.Errorf("queued job: %+v", j)
	}
	if h := ts.health(); len(h.Runners) != 0 {
		t.Errorf("dead runner still listed: %+v", h.Runners)
	}
	ts.startRunner("r2", 1)
	if j := ts.waitDone(queued); j.State != api.Exited || j.Runner != "r2" {
		t.Errorf("queued job after a new runner came: %+v", j)
	}
	if r := ts.tun("", "logs", queued); r.stdout != "second\n" {
		t.Errorf("logs: %+v", r)
	}
	// A restarted runner (same name, new process) takes over at once; its old jobs are lost.
	ts.startRunner("r1", 1)
	ts.waitRunners(2)
}

func TestRunnerOutputs(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	ts.startRunner("r1", 1)
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "in.txt"), []byte("input"), 0o644)
	r := ts.tun(ws, "run", "--out", "out.txt", "--out", "dist/*.js", "--out", "missing.txt", "--",
		"sh", "-c", "cp in.txt out.txt; mkdir dist; echo a > dist/a.js; echo b > dist/b.txt; echo no > other")
	if r.code != 0 || !strings.Contains(r.stderr, "missing.txt: no such file") {
		t.Fatalf("run --out: %+v", r)
	}
	if got := readFile(t, filepath.Join(ws, "out.txt")); got != "input" {
		t.Errorf("out.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(ws, "dist/a.js")); got != "a\n" {
		t.Errorf("dist/a.js = %q", got)
	}
	if exists(filepath.Join(ws, "dist/b.txt")) || exists(filepath.Join(ws, "other")) {
		t.Error("undeclared files were downloaded")
	}
	id := ts.lastID()
	dst := t.TempDir()
	if r := ts.tun(ws, "get", "-o", dst, id, "dist/a.js"); r.code != 0 || readFile(t, filepath.Join(dst, "dist/a.js")) != "a\n" {
		t.Errorf("get declared: %+v", r)
	}
	if r := ts.tun(ws, "get", "-o", dst, id, "other"); r.code != 125 || !strings.Contains(r.stderr, "ran on runner r1") || !strings.Contains(r.stderr, "--out other") {
		t.Errorf("get undeclared: %+v", r)
	}
	if r := ts.tun(ws, "get", "-o", dst, id, "dist/b.txt"); r.code != 125 {
		t.Errorf("get undeclared file in a declared glob's dir: %+v", r)
	}
	// Outputs of a whole directory, and of a job that failed.
	r = ts.tun(ws, "run", "--out", "d", "--", "sh", "-c", "mkdir -p d/e; echo x > d/e/x; ln -s e/x d/link; exit 2")
	if r.code != 2 || readFile(t, filepath.Join(ws, "d/e/x")) != "x\n" {
		t.Errorf("dir output: %+v", r)
	}
	if l, err := os.Readlink(filepath.Join(ws, "d/link")); err != nil || l != "e/x" {
		t.Errorf("symlink output: %q %v", l, err)
	}
}

func TestRunnerOutputsLimit(t *testing.T) {
	t.Parallel()
	cfg := hubConfig("")
	cfg.MaxDownload = 100
	ts := startServer(t, cfg)
	ts.startRunner("r1", 1)
	r := ts.tun("", "run", "--out", "big", "--", "sh", "-c", "head -c 1000 /dev/zero > big")
	if r.code != 125 || !strings.Contains(r.stderr, "--max-download") {
		t.Errorf("over the limit: %+v", r)
	}
}

// fakeRunner speaks the runner protocol by hand.
type fakeRunner struct {
	ts       *testServer
	name, as string // as: X-Tun-Client, which the hub uses as the runner's source
	instance string
	seq      int64 // sent as RunnerPoll.Seq when set
}

func (f *fakeRunner) req(method, path string, body []byte) (int, []byte) {
	f.ts.t.Helper()
	req, _ := http.NewRequest(method, f.ts.url+path, bytes.NewReader(body))
	req.Header.Set("X-Tun-Token", f.ts.srv.Token())
	req.Header.Set(api.HeaderRunner, f.name)
	if f.as != "" {
		req.Header.Set("X-Tun-Client", f.as)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (f *fakeRunner) poll(slots int, running ...string) (int, api.RunnerPollResponse) {
	f.ts.t.Helper()
	if f.seq > 0 {
		f.seq++
	}
	b, _ := json.Marshal(api.RunnerPoll{Name: f.name, Instance: f.instance, Seq: f.seq, Slots: slots, Running: running})
	code, out := f.req("POST", "/v1/runner/poll", b)
	var resp api.RunnerPollResponse
	json.Unmarshal(out, &resp)
	return code, resp
}

func (f *fakeRunner) log(id string, offset int64, data string) (int, int64) {
	f.ts.t.Helper()
	code, out := f.req("POST", fmt.Sprintf("/v1/runner/jobs/%s/stdout?offset=%d", id, offset), []byte(data))
	var ack api.LogAck
	json.Unmarshal(out, &ack)
	return code, ack.Offset
}

func jobIDs(r api.RunnerPollResponse) []string {
	var ids []string
	for _, j := range r.Jobs {
		ids = append(ids, j.ID)
	}
	return ids
}

func TestRunnerProtocol(t *testing.T) {
	t.Parallel()
	cfg := hubConfig("")
	cfg.RunnerBackstop = 200 * time.Millisecond
	ts := startServer(t, cfg)
	f := &fakeRunner{ts: ts, name: "fake", instance: "i1"}
	id := ts.detach("", "--no-workspace", "--", "true")
	code, resp := f.poll(1)
	if code != 200 || len(resp.Jobs) != 1 || resp.Jobs[0].ID != id {
		t.Fatalf("poll: %d %+v", code, resp)
	}
	if j := ts.status(id); j.State != api.Queued || j.Runner != "fake" || j.QueuePosition != nil {
		t.Errorf("assigned: %+v", j)
	}
	if code, _ := f.req("POST", "/v1/runner/jobs/"+id+"/stdout?offset=0", []byte("x")); code != 410 {
		t.Errorf("log before started: %d", code)
	}
	for range 2 { // idempotent
		if code, b := f.req("POST", "/v1/runner/jobs/"+id+"/started", nil); code != 200 {
			t.Fatalf("started: %d %s", code, b)
		}
	}
	if j := ts.status(id); j.State != api.Running || j.StartedAt == nil {
		t.Errorf("started: %+v", j)
	}
	// Log chunks are idempotent by offset: retries and overlaps are skipped, gaps refused.
	for _, c := range []struct {
		off        int64
		data       string
		code       int
		wantOffset int64
	}{{0, "hello ", 200, 6}, {0, "hello ", 200, 6}, {3, "lo world", 200, 11}, {20, "x", 409, 11}, {11, "\n", 200, 12}} {
		if code, off := f.log(id, c.off, c.data); code != c.code || off != c.wantOffset {
			t.Errorf("chunk %q at %d: %d offset %d, want %d offset %d", c.data, c.off, code, off, c.code, c.wantOffset)
		}
	}
	other := &fakeRunner{ts: ts, name: "other", instance: "o1"}
	if code, _ := other.log(id, 12, "intruder"); code != 410 {
		t.Errorf("log from another runner: %d", code)
	}
	done, _ := json.Marshal(api.RunnerDone{State: api.Exited, ExitCode: new(int)})
	if code, b := f.req("POST", "/v1/runner/jobs/"+id+"/done", done); code != 200 {
		t.Fatalf("done: %d %s", code, b)
	}
	if j := ts.status(id); j.State != api.Exited || *j.ExitCode != 0 {
		t.Errorf("done: %+v", j)
	}
	if r := ts.tun("", "logs", id); r.stdout != "hello world\n" {
		t.Errorf("logs: %q", r.stdout)
	}
	if code, _ := f.req("POST", "/v1/runner/jobs/"+id+"/done", done); code != 410 {
		t.Errorf("second done: %d", code)
	}
	// The hub tells the runner to drop jobs it no longer has.
	if _, resp := f.poll(0, id, "nonexistent"); !slices.Equal(resp.Cancel, []string{id, "nonexistent"}) {
		t.Errorf("cancel list: %+v", resp)
	}

	// An assignment whose poll answer got lost is sent again; one the runner can't take
	// goes back to the queue.
	id2 := ts.detach("", "--no-workspace", "--", "true")
	if _, resp := f.poll(1); !slices.Equal(jobIDs(resp), []string{id2}) {
		t.Fatalf("assign: %+v", resp)
	}
	if _, resp := f.poll(1); !slices.Equal(jobIDs(resp), []string{id2}) {
		t.Errorf("reassign: %+v", resp)
	}
	f.poll(0)
	if j := ts.status(id2); j.State != api.Queued || j.Runner != "" || j.QueuePosition == nil {
		t.Errorf("requeued: %+v", j)
	}
	// Canceled while assigned: never started.
	if _, resp := f.poll(1); !slices.Equal(jobIDs(resp), []string{id2}) {
		t.Fatalf("assign: %+v", resp)
	}
	ts.tun("", "cancel", id2)
	if code, _ := f.req("POST", "/v1/runner/jobs/"+id2+"/started", nil); code != 410 {
		t.Errorf("started after cancel: %d", code)
	}
	if j := ts.status(id2); j.State != api.Canceled || j.StartedAt != nil {
		t.Errorf("canceled while assigned: %+v", j)
	}

	// The newest instance with a name wins; the older one is told it was superseded.
	newer := &fakeRunner{ts: ts, name: "fake", instance: "i2", as: "elsewhere"}
	if code, resp := newer.poll(1); code != 200 || resp.Superseded != "" {
		t.Errorf("newer instance: %d %+v", code, resp)
	}
	if code, resp := f.poll(1); code != 200 || !strings.Contains(resp.Superseded, "superseded") {
		t.Errorf("older instance: %d %+v", code, resp)
	}
	f.instance = "i3"
	if code, resp := f.poll(1); code != 200 || resp.Superseded != "" {
		t.Errorf("restart: %d %+v", code, resp)
	}

	// Hub backstop: a runner that keeps polling but never ends a timed-out job.
	r := ts.tun("", "run", "--detach", "--no-workspace", "--timeout", "1s", "--", "sleep", "60")
	id3 := strings.TrimSpace(r.stdout)
	if _, resp := f.poll(1); !slices.Equal(jobIDs(resp), []string{id3}) {
		t.Fatalf("assign: %+v", resp)
	}
	f.req("POST", "/v1/runner/jobs/"+id3+"/started", nil)
	var cancel []string
	eventually(t, "backstop timeout", func() bool {
		_, resp := f.poll(1, id3)
		cancel = resp.Cancel
		return api.Terminal(ts.status(id3).State)
	})
	if j := ts.status(id3); j.State != api.Timeout || !strings.Contains(j.Error, "enforced by the hub") {
		t.Errorf("backstop: %+v", j)
	}
	if _, resp := f.poll(1, id3); !slices.Equal(resp.Cancel, []string{id3}) {
		t.Errorf("cancel after backstop: %+v (before: %v)", resp, cancel)
	}
}

func TestRunnerStalePoll(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	f := &fakeRunner{ts: ts, name: "seq", instance: "s1", seq: 1}
	id := ts.detach("", "--no-workspace", "--", "true")
	if _, resp := f.poll(1); !slices.Equal(jobIDs(resp), []string{id}) {
		t.Fatalf("assign: %+v", resp)
	}
	f.req("POST", "/v1/runner/jobs/"+id+"/started", nil)
	// A poll that was sent before the runner got the job, delivered late, changes nothing.
	f.seq = 1 // sends 2 again
	f.poll(1)
	if j := ts.status(id); j.State != api.Running {
		t.Errorf("after a stale poll: %+v", j)
	}
	// A current poll that doesn't list it means the runner lost it.
	f.poll(1)
	if j := ts.status(id); j.State != api.Failed || !strings.Contains(j.Error, "no longer has the job") {
		t.Errorf("after a current poll: %+v", j)
	}
}

func TestRunnerHubRestart(t *testing.T) {
	data := t.TempDir()
	ts := startServer(t, hubConfig(data))
	addr := strings.TrimPrefix(ts.url, "http://")
	ts.startRunner("r1", 1)
	ts.waitRunners(1)
	env, wait, done := release(t)
	id := ts.detach("", append(env, "--no-workspace", "--", "sh", "-c", "echo before; "+wait+"; echo after")...)
	ts.waitFor(id, func(j api.Job) bool { return j.State == api.Running })
	eventually(t, "log shipped", func() bool { return ts.tun("", "logs", id).stdout == "before\n" })
	ts.stop()

	// A new hub on the same address and data dir adopts the job the runner is still running.
	s, err := server.New(hubConfig(data))
	if err != nil {
		t.Fatal(err)
	}
	var ln net.Listener
	eventually(t, "listening again", func() bool { ln, err = net.Listen("tcp", addr); return err == nil })
	hs := &http.Server{Handler: s.Handler()}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close(); s.Close() })
	ts2 := &testServer{t: t, srv: s, url: ts.url, data: data}
	eventually(t, "runner back", func() bool { return len(ts2.health().Runners) == 1 && ts2.health().Runners[0].Slots == 1 })
	done()
	j := ts2.waitDone(id)
	if j.State != api.Exited || *j.ExitCode != 0 || j.Runner != "r1" {
		t.Errorf("after hub restart: %+v", j)
	}
	if r := ts2.tun("", "logs", id); r.stdout != "before\nafter\n" {
		t.Errorf("logs: %q", r.stdout)
	}
	if r := ts2.tun("", "run", "--no-workspace", "--", "echo", "new"); r.code != 0 || r.stdout != "new\n" {
		t.Errorf("new job: %+v", r)
	}
}

func TestRunnerMixedLocal(t *testing.T) {
	t.Parallel()
	// A hub with a local slot and a runner: both take work from the same FIFO.
	ts := startServer(t, server.Config{MaxJobs: 1, RunnerPollWait: 100 * time.Millisecond, RunnerDeadAfter: time.Second})
	ts.startRunner("r1", 1)
	ts.waitRunners(1)
	if h := ts.health(); h.LocalSlots != 1 || h.MaxJobs != 2 {
		t.Errorf("health: %+v", h)
	}
	env, wait, done := release(t)
	a := ts.detach("", append(env, "--no-workspace", "-c", wait)...)
	b := ts.detach("", append(env, "--no-workspace", "-c", wait)...)
	ts.waitFor(b, func(j api.Job) bool { return j.State == api.Running })
	done()
	ja, jb := ts.waitDone(a), ts.waitDone(b)
	runners := []string{ja.Runner, jb.Runner}
	slices.Sort(runners)
	if !slices.Equal(runners, []string{"local", "r1"}) || ja.State != api.Exited || jb.State != api.Exited {
		t.Errorf("placement: %+v %+v", ja, jb)
	}
}

// Runner jobs get the same private network as hub jobs; the runner serves their egress proxy.
func TestRunnerNetIsolation(t *testing.T) {
	t.Parallel()
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap not installed or unusable on this host")
	}
	if err := sandbox.NetUsable(); err != nil {
		t.Skipf("no private network namespaces here: %v", err)
	}
	ts := startServer(t, hubConfig(""))
	ts.startRunnerCfg(server.RunnerConfig{Name: "net", Slots: 2, Sandbox: sandbox.Bwrap, PrivateNet: true})
	ts.waitRunners(1)
	if h := ts.health(); !h.Runners[0].NetworkIsolation || h.Runners[0].Sandbox != sandbox.Bwrap {
		t.Errorf("healthz runners: %+v", h.Runners)
	}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))
	defer host.Close()
	_, port, _ := net.SplitHostPort(host.Listener.Addr().String())
	r := ts.tun("", "run", "--no-workspace", "-e", "REQ=GET http://127.0.0.1:"+port+"/a HTTP/1.0\\r\\n\\r\\n", "-c",
		`! (exec 3<>/dev/tcp/127.0.0.1/`+port+`) 2>/dev/null || echo direct-reachable; `+
			`exec 3<>/dev/tcp/127.0.0.1/${HTTP_PROXY##*:} && printf "$REQ" >&3 && cat <&3`)
	if r.code != 0 || !strings.Contains(r.stdout, "hello /a") || strings.Contains(r.stdout, "direct-reachable") {
		t.Errorf("egress through the runner's proxy only: %+v", r)
	}
	if j := ts.status(ts.lastID()); j.Runner != "net" || j.Sandbox != sandbox.Bwrap {
		t.Errorf("job: %+v", j)
	}
	if r := ts.tun("", "run", "--no-workspace", "-c", "echo out; exit 3"); r.code != 3 || r.stdout != "out\n" {
		t.Errorf("exit status through the helper: %+v", r)
	}
}

func TestRunnerDetachOutThenGet(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	ts.startRunner("r1", 1)
	r := ts.tun("", "run", "--detach", "--out", "f", "--no-workspace", "-c", "echo x > f; echo y > g")
	if r.code != 0 {
		t.Fatalf("--detach --out: %+v", r)
	}
	id := strings.TrimSpace(r.stdout)
	if j := ts.waitDone(id); !slices.Equal(j.Outputs, []string{"f"}) {
		t.Errorf("outputs: %+v", j)
	}
	dst := t.TempDir()
	if g := ts.tun("", "get", "-o", dst, id, "f"); g.code != 0 || readFile(t, filepath.Join(dst, "f")) != "x\n" {
		t.Errorf("get: %+v", g)
	}
	if g := ts.tun("", "get", "-o", dst, id, "g"); g.code != 125 || !strings.Contains(g.stderr, "tun run --detach --out g") {
		t.Errorf("get of an undeclared path: %+v", g)
	}
}

// Graceful shutdown tells the hub at once; a superseded runner finishes its job and exits.
func TestRunnerLeaveAndSupersede(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	a := ts.startRunner("a", 1)
	ts.waitRunners(1)
	start := time.Now()
	a.r.Shutdown(5 * time.Second)
	<-a.done
	if h := ts.health(); len(h.Runners) != 0 || time.Since(start) > 900*time.Millisecond {
		t.Errorf("after leaving (%v): %+v", time.Since(start), h.Runners)
	}

	old := ts.startRunner("dup", 1)
	ts.waitRunners(1)
	env, wait, done := release(t)
	id := ts.detach("", append(env, "--no-workspace", "-c", wait)...)
	ts.waitFor(id, func(j api.Job) bool { return j.State == api.Running })
	ts.startRunner("dup", 1)
	eventually(t, "superseded", func() bool { return old.r.Superseded() != "" })
	select {
	case <-old.done:
		t.Fatal("superseded runner exited with a job running")
	default:
	}
	// New work goes to the new instance even while the old one finishes.
	if r := ts.tun("", "run", "--no-workspace", "--", "echo", "new"); r.code != 0 || r.stdout != "new\n" {
		t.Errorf("job on the new instance: %+v", r)
	}
	done()
	if j := ts.waitDone(id); j.State != api.Exited {
		t.Errorf("old instance's job: %+v", j)
	}
	<-old.done
}

func TestRunnerDataDirLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := server.RunnerConfig{Hub: "http://127.0.0.1:1", Name: "x", Slots: 1, DataDir: dir, Sandbox: sandbox.None}
	if _, err := server.NewRunner(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := server.NewRunner(cfg); err == nil || !strings.Contains(err.Error(), "in use by another tun runner") {
		t.Errorf("second runner on one data dir: %v", err)
	}
}

func TestRunnerExitedThenLost(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	f := &fakeRunner{ts: ts, name: "x", instance: "i1"}
	id := ts.detach("", "--no-workspace", "--", "true")
	f.poll(1)
	f.req("POST", "/v1/runner/jobs/"+id+"/started", nil)
	three := 3
	ex, _ := json.Marshal(api.RunnerDone{State: api.Exited, ExitCode: &three})
	if code, _ := f.req("POST", "/v1/runner/jobs/"+id+"/exited", ex); code != 200 {
		t.Fatalf("exited: %d", code)
	}
	// The runner dies while uploading: the job keeps the reported result.
	j := ts.waitDone(id)
	if j.State != api.Exited || j.ExitCode == nil || *j.ExitCode != 3 || !strings.Contains(j.Error, "lost") {
		t.Errorf("after the runner was lost: %+v", j)
	}
}

func TestRunnerStartTimeout(t *testing.T) {
	t.Parallel()
	cfg := hubConfig("")
	cfg.RunnerStartTimeout = 300 * time.Millisecond
	ts := startServer(t, cfg)
	f := &fakeRunner{ts: ts, name: "slow", instance: "i1"}
	id := ts.detach("", "--no-workspace", "--", "true")
	f.poll(1)
	var resp api.RunnerPollResponse
	eventually(t, "requeue of a never started job", func() bool {
		_, resp = f.poll(0, id) // the runner holds it but never starts it
		return slices.Contains(resp.Cancel, id)
	})
	if j := ts.status(id); j.State != api.Queued || j.Runner != "" {
		t.Errorf("requeued: %+v", j)
	}
}

func TestRunnerService(t *testing.T) {
	ts := startServer(t, hubConfig(""))
	ts.startRunner("r", 2)
	ts.waitRunners(1)
	id, urls := ts.startService("--port", "8080", "--ready-timeout", "15s")
	j := ts.status(id)
	if j.Runner != "r" || !j.Ports[0].Ready {
		t.Fatalf("placement/readiness: %+v", j)
	}
	u := urls[8080]
	if code, body := ts.get(u+"env", true); code != 200 || !strings.Contains(body, "PORT=") {
		t.Fatalf("proxy: %d %s", code, body)
	}
	if code, _ := ts.get(u, false); code != 401 {
		t.Fatal(code)
	}
	// Upgrade traffic must pass both proxy layers without exposing the tun token.
	if code, body := ts.get(u, true); code != 200 || strings.Contains(body, ts.srv.Token()) {
		t.Fatalf("auth leak: %d %s", code, body)
	}
	// Streaming: the first chunk must arrive before we release the second.
	streamCtx, cancelStream := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStream()
	req, _ := http.NewRequestWithContext(streamCtx, "GET", u+"stream", nil)
	req.Header.Set("Authorization", "Bearer "+ts.srv.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	streamReader := bufio.NewReader(resp.Body)
	line, err := streamReader.ReadString('\n')
	if err != nil || line != "tick1\n" {
		t.Fatalf("stream first chunk %q %v", line, err)
	}
	if code, _ := ts.get(u+"stream-release", true); code != 200 {
		t.Fatalf("release stream: %d", code)
	}
	if line, err := streamReader.ReadString('\n'); err != nil || line != "tick2\n" {
		t.Errorf("stream second chunk %q %v", line, err)
	}
	resp.Body.Close()

	// Websocket-style upgrade passes through and is bidirectional.
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /v1/jobs/%s/ports/8080/ws HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", id, ts.srv.Token())
	br := bufio.NewReader(conn)
	wsResp, err := http.ReadResponse(br, nil)
	if err != nil || wsResp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %+v", err, wsResp)
	}
	fmt.Fprint(conn, "ping\n")
	if echo, err := br.ReadString('\n'); echo != "ping\n" {
		t.Errorf("websocket echo %q %v", echo, err)
	}

	conn.Close()

	r := ts.tun("", "cancel", id)
	if r.code != 0 {
		t.Fatal(r)
	}
	ts.waitFor(id, func(j api.Job) bool { return j.State == api.Canceled })
	if h := ts.health(); h.Services != 0 {
		t.Fatalf("service admission leaked: %+v", h)
	}
}

func TestRunnerServiceHubRestart(t *testing.T) {
	data := t.TempDir()
	ts := startServer(t, hubConfig(data))
	addr := strings.TrimPrefix(ts.url, "http://")
	ts.startRunner("r", 1)
	ts.waitRunners(1)
	id, urls := ts.startService("--port", "8080", "--ready-timeout", "15s")
	if ts.srv.Drain(50 * time.Millisecond) {
		t.Fatal("drain ignored live service")
	}
	ts.stop()
	s, err := server.New(hubConfig(data))
	if err != nil {
		t.Fatal(err)
	}
	var ln net.Listener
	eventually(t, "hub listening again", func() bool { ln, err = net.Listen("tcp", addr); return err == nil })
	hs := &http.Server{Handler: s.Handler()}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close(); s.Close() })
	ts2 := &testServer{t: t, srv: s, url: ts.url, data: data}
	eventually(t, "runner back", func() bool { return len(ts2.health().Runners) == 1 && ts2.health().Runners[0].Slots == 1 })
	if ts2.health().Services != 1 {
		t.Fatalf("service admission lost: %+v", ts2.health())
	}
	if code, body := ts2.get(urls[8080]+"env", true); code != 200 || !strings.Contains(body, "PORT=") {
		t.Fatalf("proxy after restart: %d %s", code, body)
	}
	if r := ts2.tun("", "cancel", id); r.code != 0 {
		t.Fatal(r)
	}
	ts2.waitDone(id)
	if ts2.health().Services != 0 {
		t.Fatal("service count leaked after restart")
	}
}
