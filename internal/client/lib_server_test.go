// Shared test helpers (the :testlib target in BUILD.bazel, embedded by every go_test here):
// an in-process tun server, the CLI against it, and waiting on jobs.

package client

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/reapi"
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
	if cfg.KillGrace == 0 {
		cfg.KillGrace = 50 * time.Millisecond
	}
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
	}), Protocols: reapi.Protocols()} // h2c: the REAPI cache's gRPC with Config.Cache
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
		if !strings.HasPrefix(line, "tun: job ") && !strings.HasPrefix(line, "tun: workspace ") && !strings.HasPrefix(line, "tun: uploaded ") && !strings.HasPrefix(line, "tun: running on ") {
			out.WriteString(line)
		}
	}
	return out.String()
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

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// eventually polls cond for up to 20s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !cond(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (ts *testServer) lastID() string {
	ts.t.Helper()
	js, err := ts.client().List(context.Background(), 1, nil)
	if err != nil || len(js) == 0 {
		ts.t.Fatalf("list: %v %v", js, err)
	}
	return js[0].ID
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// release returns -e flags and a shell snippet that waits until the returned func is called.
// Jobs here run unsandboxed on this host, so a file in a test dir works as the signal.
func release(t *testing.T) (env []string, wait string, done func()) {
	p := filepath.Join(t.TempDir(), "release")
	return []string{"-e", "REL=" + p}, `while [ ! -e "$REL" ]; do sleep .01; done`, func() { os.WriteFile(p, nil, 0o644) }
}

func (ts *testServer) health() api.Health {
	ts.t.Helper()
	h, err := ts.client().Health(context.Background())
	if err != nil {
		ts.t.Fatal(err)
	}
	return h
}

// bwrapServer runs jobs under bwrap, with private networks where they work.
func bwrapServer(t *testing.T) *testServer {
	t.Helper()
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap not installed or unusable on this host")
	}
	return startServer(t, server.Config{Sandbox: sandbox.Bwrap, PrivateNet: sandbox.NetUsable() == nil})
}

// netServer runs jobs under bwrap with a private network namespace each.
func netServer(t *testing.T) *testServer {
	t.Helper()
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap not installed or unusable on this host")
	}
	if err := sandbox.NetUsable(); err != nil {
		t.Skipf("no private network namespaces here: %v", err)
	}
	return startServer(t, server.Config{Sandbox: sandbox.Bwrap, PrivateNet: true})
}
