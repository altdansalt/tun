package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
	"github.com/altdansalt/tun/internal/workspace"
)

// testServer is a real tun server on a random local port with a temp data dir.
type testServer struct {
	t    *testing.T
	srv  *server.Server
	hs   *http.Server
	url  string
	data string
	puts atomic.Int32
	once sync.Once
}

func startServer(t *testing.T, cfg server.Config) *testServer {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
		dir := cfg.DataDir
		t.Cleanup(func() { workspace.RemoveAll(dir) }) // before TempDir's: jobs may leave read-only dirs
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = sandbox.None
	}
	if cfg.MaxJobs == 0 {
		cfg.MaxJobs = 4
	}
	if cfg.MaxUpload == 0 {
		cfg.MaxUpload = 64 << 20
	}
	cfg.KillGrace = 50 * time.Millisecond
	s, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{t: t, srv: s, url: "http://" + ln.Addr().String(), data: cfg.DataDir}
	h := s.Handler()
	ts.hs = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			ts.puts.Add(1)
		}
		h.ServeHTTP(w, r)
	})}
	go ts.hs.Serve(ln)
	t.Cleanup(ts.stop)
	return ts
}

// stop shuts the server down like a crash: running jobs are killed, state is left as is.
func (ts *testServer) stop() {
	ts.once.Do(func() {
		ts.hs.Close()
		ts.srv.Close()
	})
}

type result struct {
	code           int
	stdout, stderr string
}

func (ts *testServer) env(cwd string, extra map[string]string, out, errw io.Writer, sigs chan os.Signal) Env {
	vars := map[string]string{"TUN_URL": ts.url, "TUN_TOKEN": ts.srv.Token()}
	for k, v := range extra {
		vars[k] = v
	}
	return Env{Stdout: out, Stderr: errw, Cwd: cwd, Signals: sigs, Getenv: func(k string) string { return vars[k] }}
}

// tun runs a client command in cwd (a fresh empty dir if "").
func (ts *testServer) tun(cwd string, args ...string) result {
	return ts.tunEnv(cwd, nil, args...)
}

func (ts *testServer) tunEnv(cwd string, extra map[string]string, args ...string) result {
	ts.t.Helper()
	if cwd == "" {
		cwd = ts.t.TempDir()
	}
	var out, errw bytes.Buffer
	code := Main(args, ts.env(cwd, extra, &out, &errw, nil))
	return result{code, out.String(), errw.String()}
}

func (ts *testServer) client() *Client { return &Client{URL: ts.url, Token: ts.srv.Token()} }

func (ts *testServer) status(id string) api.Job {
	ts.t.Helper()
	j, err := ts.client().Get(context.Background(), id)
	if err != nil {
		ts.t.Fatal(err)
	}
	return j
}

func (ts *testServer) waitFor(id string, ok func(api.Job) bool) api.Job {
	ts.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		j := ts.status(id)
		if ok(j) {
			return j
		}
		if time.Now().After(deadline) {
			ts.t.Fatalf("job %s stuck in %+v", id, j)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (ts *testServer) waitDone(id string) api.Job {
	return ts.waitFor(id, func(j api.Job) bool { return api.Terminal(j.State) })
}

func (ts *testServer) detach(cwd string, args ...string) string {
	ts.t.Helper()
	r := ts.tun(cwd, append([]string{"run", "--detach"}, args...)...)
	if r.code != 0 {
		ts.t.Fatalf("detach: %+v", r)
	}
	return strings.TrimSpace(r.stdout)
}

// cmdStderr removes CLI diagnostics, preserving the command's stderr bytes.
func cmdStderr(s string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		if !strings.HasPrefix(line, "tun: job ") && !strings.HasPrefix(line, "tun: workspace ") && !strings.HasPrefix(line, "tun: uploaded ") {
			out.WriteString(line)
		}
	}
	return out.String()
}

func TestExitCodes(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"run", "-c", "exit 3"}, 3, ""},
		{[]string{"run", "--", "true"}, 0, ""},
		{[]string{"run", "--", "false"}, 1, ""},
		{[]string{"run", "--", "sh", "-c", "kill -9 $$"}, 137, ""},
		{[]string{"run", "--", "/nonexistent/cmd"}, 125, "tun: job"},
		{[]string{"run"}, 125, "tun: run: nothing to run"},
		{[]string{"run", "-c", "x", "--", "y"}, 125, "tun: run:"},
		{[]string{"run", "--bogus"}, 125, "flag provided but not defined"},
		{[]string{"run", "-C", "../x", "--", "true"}, 125, "tun: run: -C"},
		{[]string{"run", "-C", "missing", "--", "true"}, 125, "does not exist"},
		{[]string{"frob"}, 125, "tun: unknown command"},
		{[]string{"status", "nope"}, 125, "tun: server: no such job"},
	}
	for _, c := range cases {
		r := ts.tun("", c.args...)
		if r.code != c.code || !strings.Contains(r.stderr, c.msg) {
			t.Errorf("tun %q = %d, stderr %q; want %d with %q", c.args, r.code, r.stderr, c.code, c.msg)
		}
	}
	r := Main([]string{"run", "--", "true"}, Env{Stdout: io.Discard, Stderr: &bytes.Buffer{}, Cwd: t.TempDir(),
		Getenv: func(k string) string { return map[string]string{"TUN_URL": "http://127.0.0.1:1"}[k] }})
	if r != 125 {
		t.Errorf("unreachable server: exit %d, want 125", r)
	}
	var errb bytes.Buffer
	Main([]string{"ls"}, Env{Stdout: io.Discard, Stderr: &errb, Cwd: t.TempDir(),
		Getenv: func(k string) string { return map[string]string{"TUN_URL": ts.url, "TUN_TOKEN": "wrong"}[k] }})
	if !strings.HasPrefix(errb.String(), "tun: unauthorized") {
		t.Errorf("bad token: %q", errb.String())
	}
	if r := ts.tun("", "help"); r.code != 0 || !strings.Contains(r.stdout, "tun run") {
		t.Errorf("help: %+v", r)
	}
}

func TestStdoutStderrAndEnv(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	r := ts.tun("", "run", "-c", "echo out; echo err >&2; echo out2")
	if r.code != 0 || r.stdout != "out\nout2\n" || cmdStderr(r.stderr) != "err\n" || !strings.HasPrefix(r.stderr, "tun: job ") {
		t.Fatalf("got %+v", r)
	}
	r = ts.tunEnv("", map[string]string{"FROM_CLIENT": "copied"}, "run", "-e", "FOO=a=b", "-e", "FROM_CLIENT", "-e", "TERM=xterm",
		"-c", `echo "$FOO|$FROM_CLIENT|$TERM|$HOME|$TMPDIR|$GOCACHE|$TUN_JOB_ID|$SECRET"`)
	parts := strings.Split(strings.TrimSpace(r.stdout), "|")
	if r.code != 0 || len(parts) != 8 {
		t.Fatalf("got %+v", r)
	}
	id := parts[6]
	jobDir := filepath.Join(ts.data, "jobs", id)
	want := []string{"a=b", "copied", "xterm", jobDir + "/sandbox/home", jobDir + "/sandbox/tmp", ts.data + "/cache/go-build", id, ""}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("env field %d = %q, want %q", i, parts[i], want[i])
		}
	}
	if r := ts.tun("", "run", "-e", "UNSET_VAR", "--", "true"); r.code != 125 || !strings.Contains(r.stderr, "not set") {
		t.Errorf("-e unset: %+v", r)
	}
}

func TestSharedCacheEnv(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	// Every shared cache var points at a dir under $DATA/cache that a later job sees again;
	// npm's debug logs stay in the job's own home.
	r := ts.tun("", "run", "-c", `echo $TUN_JOB_ID; for d in "$GOCACHE" "$GOMODCACHE" "$XDG_CACHE_HOME" "$npm_config_cache"; do
		echo "$d"; echo $TUN_JOB_ID > "$d/from-first"; done; echo "$npm_config_logs_dir"`)
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 6 {
		t.Fatalf("got %+v", r)
	}
	id := lines[0]
	for i, dir := range []string{"go-build", "gomod", "xdg", "npm"} {
		if want := filepath.Join(ts.data, "cache", dir); lines[i+1] != want {
			t.Errorf("cache var %d = %q, want %q", i, lines[i+1], want)
		}
	}
	if want := filepath.Join(ts.data, "jobs", id, "sandbox", "home", ".npm", "_logs"); lines[5] != want {
		t.Errorf("npm_config_logs_dir = %q, want %q", lines[5], want)
	}
	r2 := ts.tun("", "run", "-e", "npm_config_cache=/elsewhere", "-c",
		`cat "$GOCACHE/from-first" "$GOMODCACHE/from-first" "$XDG_CACHE_HOME/from-first"; echo "$npm_config_cache"`)
	if want := strings.Repeat(id+"\n", 3) + "/elsewhere\n"; r2.code != 0 || r2.stdout != want {
		t.Errorf("second job: %+v, want stdout %q", r2, want)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	files := map[string]string{".gitignore": "*.log\n", "sub/dir/f.txt": "f\n", "other/g.txt": "g", "x.log": "ignored"}
	for name, data := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755)
		os.WriteFile(filepath.Join(root, name), []byte(data), 0o644)
	}
	return root
}

func TestCwdAndWorkspace(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	root := gitRepo(t)
	r := ts.tun(filepath.Join(root, "sub", "dir"), "run", "-c", "pwd; cat f.txt; ls -a ../..")
	lines := strings.Split(r.stdout, "\n")
	if r.code != 0 || !strings.HasSuffix(lines[0], "/work/sub/dir") || lines[1] != "f" {
		t.Fatalf("got %+v", r)
	}
	if strings.Contains(r.stdout, "x.log") || strings.Contains(r.stdout, ".git\n") {
		t.Errorf("ignored file or .git shipped: %q", r.stdout)
	}
	r = ts.tun(filepath.Join(root, "sub"), "run", "-C", "other", "--", "cat", "g.txt")
	if r.code != 0 || r.stdout != "g" {
		t.Errorf("-C other: %+v", r)
	}
	if n := ts.puts.Load(); n != 1 {
		t.Errorf("workspace uploaded %d times, want 1 (content addressed)", n)
	}
	r = ts.tun(filepath.Join(root, "sub"), "run", "--no-workspace", "-c", "pwd; ls | wc -l")
	if r.code != 0 || !strings.HasSuffix(strings.Split(r.stdout, "\n")[0], "/work") || strings.TrimSpace(strings.Split(r.stdout, "\n")[1]) != "0" {
		t.Errorf("--no-workspace: %+v", r)
	}
}

func TestUploadLimit(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{MaxUpload: 4 << 10})
	dir := t.TempDir()
	big := make([]byte, 64<<10)
	rand.Read(big)
	os.WriteFile(filepath.Join(dir, "big"), big, 0o644)
	r := ts.tun(dir, "run", "--", "true")
	if r.code != 125 || !strings.HasPrefix(r.stderr, "tun: uploading workspace") || !strings.Contains(r.stderr, "limit") {
		t.Fatalf("got %+v", r)
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	start := time.Now()
	// The trap ignores SIGTERM in the shell; the background sleep proves the whole group dies.
	r := ts.tun("", "run", "--timeout", "1s", "-c", "trap '' TERM; sleep 60 & sleep 60")
	if r.code != 124 || !strings.Contains(r.stderr, "timed out") {
		t.Fatalf("got %+v", r)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("timeout took %v", d)
	}
	id := strings.Fields(r.stderr)[2]
	if j := ts.status(id); j.State != api.Timeout {
		t.Errorf("state %s", j.State)
	}
}

func TestCancel(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	id := ts.detach("", "--", "sleep", "60")
	ts.waitFor(id, func(j api.Job) bool { return j.State == api.Running })
	if r := ts.tun("", "cancel", id); r.code != 0 {
		t.Fatalf("cancel: %+v", r)
	}
	if j := ts.waitDone(id); j.State != api.Canceled || j.Signal == nil || *j.Signal != 15 {
		t.Errorf("after cancel: %+v", j)
	}

	// SIGINT to an attached client cancels the job and exits 128+2.
	sigs := make(chan os.Signal, 1)
	var out, errw bytes.Buffer
	done := make(chan int)
	go func() {
		done <- Main([]string{"run", "--no-workspace", "-c", "echo started; sleep 60"}, ts.env(t.TempDir(), nil, &out, &errw, sigs))
	}()
	var jid string
	for deadline := time.Now().Add(20 * time.Second); jid == ""; {
		js, _ := ts.client().List(context.Background(), 1, nil)
		if len(js) > 0 && js[0].ID != id && js[0].State == api.Running {
			jid = js[0].ID
		}
		if time.Now().After(deadline) {
			t.Fatal("job never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	sigs <- syscall.SIGINT
	if code := <-done; code != 130 {
		t.Errorf("exit %d, want 130; stderr %q", code, errw.String())
	}
	if j := ts.waitDone(jid); j.State != api.Canceled {
		t.Errorf("job state %s", j.State)
	}
}

func TestConcurrencyAndIsolation(t *testing.T) {
	t.Parallel()
	const n = 4
	ts := startServer(t, server.Config{MaxJobs: n})
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "shared.txt"), []byte("x"), 0o644)
	// Each job waits (via the shared cache dir) until all n are running at once, then checks
	// its workspace copy is private.
	script := fmt.Sprintf(`b=$XDG_CACHE_HOME/barrier; mkdir -p $b; touch $b/$TUN_JOB_ID
		for i in $(seq 300); do [ $(ls $b | wc -l) -ge %d ] && break; sleep 0.1; done
		[ $(ls $b | wc -l) -ge %d ] || { echo "not concurrent" >&2; exit 9; }
		echo mine > mine-$TUN_JOB_ID; rm shared.txt; sleep 0.5; ls; pwd`, n, n)
	var wg sync.WaitGroup
	results := make([]result, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = ts.tun(ws, "run", "-c", script) }(i)
	}
	wg.Wait()
	dirs := map[string]bool{}
	for _, r := range results {
		lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
		if r.code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "mine-") {
			t.Fatalf("job output %+v", r)
		}
		dirs[lines[1]] = true
	}
	if len(dirs) != n {
		t.Errorf("jobs shared dirs: %v", dirs)
	}
}

func TestQueueing(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{MaxJobs: 1})
	a := ts.detach("", "--", "sleep", "1")
	b := ts.detach("", "--", "true")
	c := ts.detach("", "--", "true")
	if j := ts.status(b); j.State != api.Queued {
		t.Errorf("b state %s, want queued", j.State)
	}
	ja, jb, jc := ts.waitDone(a), ts.waitDone(b), ts.waitDone(c)
	if jb.State != api.Exited || jb.StartedAt.Before(*ja.EndedAt) || jc.StartedAt.Before(*jb.EndedAt) {
		t.Errorf("not FIFO one-at-a-time: a=%+v b=%+v c=%+v", ja, jb, jc)
	}
	// Canceling a queued job drops it.
	d := ts.detach("", "--", "sleep", "5")
	e := ts.detach("", "--", "true")
	ts.tun("", "cancel", e)
	if j := ts.waitDone(e); j.State != api.Canceled || j.StartedAt != nil {
		t.Errorf("queued cancel: %+v", j)
	}
	ts.tun("", "cancel", d)
	r := ts.tun("", "ls", "--limit", "2")
	if lines := strings.Split(strings.TrimSpace(r.stdout), "\n"); len(lines) != 2 || !strings.Contains(lines[0], e) {
		t.Errorf("ls: %q", r.stdout)
	}
}

func TestLogsReattach(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	id := ts.detach("", "-c", "for i in 1 2 3; do echo line$i; sleep 0.3; done; echo bye >&2")
	if r := ts.tun("", "logs", "-f", id); r.code != 0 || r.stdout != "line1\nline2\nline3\n" {
		t.Fatalf("logs -f: %+v", r)
	}
	if r := ts.tun("", "logs", id, "--stderr"); r.stdout != "bye\n" {
		t.Errorf("logs --stderr: %+v", r)
	}
	var buf bytes.Buffer
	if _, err := ts.client().Logs(context.Background(), id, "stdout", 6, true, &buf); err != nil || buf.String() != "line2\nline3\n" {
		t.Errorf("offset logs: %q %v", buf.String(), err)
	}
	r := ts.tun("", "status", id)
	if r.code != 0 || !strings.Contains(r.stdout, `"state":"exited"`) || !strings.Contains(r.stdout, `"exit_code":0`) {
		t.Errorf("status: %+v", r)
	}
}

func TestJSONOutput(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	r := ts.tun("", "run", "--json", "-c", "echo hi; echo oops >&2; exit 4")
	if r.code != 4 || r.stderr != "" {
		t.Fatalf("got %+v", r)
	}
	for _, want := range []string{`"stdout":"hi\n"`, `"stderr":"oops\n"`, `"exit_code":4`, `"state":"exited"`, `"duration":`} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("json output %s missing %s", r.stdout, want)
		}
	}
}

func TestRestartRecovery(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	ts := startServer(t, server.Config{DataDir: data, MaxJobs: 1})
	running := ts.detach("", "-c", "echo before; sleep 60")
	queued := ts.detach("", "--", "true")
	ts.waitFor(running, func(j api.Job) bool { return j.State == api.Running })
	eventually(t, "job output before restart", func() bool {
		return ts.tun("", "logs", running).stdout == "before\n"
	})
	token := ts.srv.Token()
	ts.stop()

	ts2 := startServer(t, server.Config{DataDir: data, MaxJobs: 1})
	if ts2.srv.Token() != token {
		t.Error("token changed across restart")
	}
	for _, id := range []string{running, queued} {
		j := ts2.status(id)
		if j.State != api.Failed || !strings.Contains(j.Error, "server restarted") || j.EndedAt == nil {
			t.Errorf("after restart: %+v", j)
		}
	}
	if r := ts2.tun("", "logs", "-f", running); r.stdout != "before\n" {
		t.Errorf("logs after restart: %+v", r)
	}
	if r := ts2.tun("", "run", "--", "true"); r.code != 0 {
		t.Errorf("new job after restart: %+v", r)
	}
}
