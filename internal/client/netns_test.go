package client

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

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

func TestNetIsolation(t *testing.T) {
	ts := netServer(t)
	if h := ts.health(); !h.NetworkIsolation {
		t.Errorf("healthz: %+v", h)
	}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))
	defer host.Close()
	_, port, _ := net.SplitHostPort(host.Listener.Addr().String())
	// Jobs reach the host (and through it the internet) only via HTTP_PROXY/HTTPS_PROXY.
	proxyReq := `exec 3<>/dev/tcp/127.0.0.1/${HTTP_PROXY##*:} && printf "$REQ" >&3 && cat <&3`
	checks := map[string]struct{ env, script, want string }{
		"host port unreachable": {"", `! (exec 3<>/dev/tcp/127.0.0.1/` + port + `) 2>/dev/null`, ""},
		"proxy env":             {"", `echo "$HTTP_PROXY $HTTPS_PROXY $https_proxy $NO_PROXY"`, ""},
		"proxy env overridable": {"HTTPS_PROXY=x", `echo "$HTTPS_PROXY"`, "x\n"},
		"absolute-URI request":  {"REQ=GET http://127.0.0.1:" + port + "/a HTTP/1.0\\r\\n\\r\\n", proxyReq, "hello /a"},
		"CONNECT tunnel":        {"REQ=CONNECT 127.0.0.1:" + port + " HTTP/1.1\\r\\nHost: x\\r\\n\\r\\nGET /b HTTP/1.0\\r\\n\\r\\n", proxyReq, "hello /b"},
		"CONNECT refused":       {"REQ=CONNECT 127.0.0.1:1 HTTP/1.1\\r\\n\\r\\n", proxyReq, "502 Bad Gateway"},
	}
	// None of the server's up interfaces (other than loopback) exist in the job's namespace.
	r := ts.tun("", "run", "--no-workspace", "--", "cat", "/proc/net/dev")
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&(net.FlagLoopback|net.FlagUp) == net.FlagUp && strings.Contains(r.stdout, " "+i.Name+":") {
			t.Errorf("job sees interface %s: %+v", i.Name, r)
		}
	}
	if r.code != 0 || !strings.Contains(r.stdout, " lo:") {
		t.Errorf("/proc/net/dev: %+v", r)
	}
	for name, c := range checks {
		args := []string{"run", "--no-workspace"}
		if c.env != "" {
			args = append(args, "-e", c.env)
		}
		r := ts.tun("", append(args, "-c", c.script)...)
		if r.code != 0 || !strings.Contains(r.stdout, c.want) {
			t.Errorf("%s: %+v", name, r)
		}
		// The same address in every job (Bazel's test cache keys on --test_env=HTTPS_PROXY).
		if name == "proxy env" && !strings.HasPrefix(r.stdout, "http://127.0.0.1:3128 http://127.0.0.1:3128 ") {
			t.Errorf("%s: %q", name, r.stdout)
		}
	}
}

// The helper between bwrap and the command must not change exit statuses, signals or kills.
func TestNetHelperExitStatus(t *testing.T) {
	ts := netServer(t)
	if r := ts.tun("", "run", "--no-workspace", "-c", "echo out; echo err >&2; exit 3"); r.code != 3 || r.stdout != "out\n" || !strings.Contains(r.stderr, "err\n") {
		t.Errorf("exit 3: %+v", r)
	}
	id := ts.detach("", "--no-workspace", "-c", "kill -KILL $$")
	// bwrap reports a command killed by signal N as exit status 128+N.
	if j := ts.waitDone(id); j.ExitCode == nil || *j.ExitCode != 128+9 {
		t.Errorf("kill -KILL: %+v", j)
	}
	if r := ts.tun("", "run", "--no-workspace", "--", "no-such-command-tun"); r.code == 0 || !strings.Contains(r.stderr, "no-such-command-tun") {
		t.Errorf("missing command: %+v", r)
	}
	if r := ts.tun("", "run", "--no-workspace", "--timeout", "1s", "-c", "trap '' TERM; sleep 60 & sleep 60"); r.code != 124 {
		t.Errorf("timeout: %+v", r)
	}
	if r := ts.tun("", "run", "--no-workspace", "-c", "ls /proc/self/fd"); r.code != 0 || r.stdout != "0\n1\n2\n3\n" {
		t.Errorf("command should get only stdio (3 is ls's own dir fd): %+v", r)
	}
}

// A Go build that downloads a module through the egress proxy, as builds do from
// proxy.golang.org. The module proxy is a local server under a name only the server can
// resolve (via sandbox.EgressDial), so the test needs no internet.
func TestNetGoModuleDownload(t *testing.T) {
	ts := netServer(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	var hits atomic.Int32
	mod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch strings.TrimPrefix(r.URL.Path, "/tun.test/greet/@v/") {
		case "list":
			fmt.Fprintln(w, "v1.0.0")
		case "v1.0.0.info":
			fmt.Fprint(w, `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`)
		case "v1.0.0.mod":
			fmt.Fprint(w, "module tun.test/greet\n\ngo 1.21\n")
		case "v1.0.0.zip":
			z := zip.NewWriter(w)
			for name, body := range map[string]string{
				"go.mod":   "module tun.test/greet\n\ngo 1.21\n",
				"greet.go": "package greet\n\nfunc Hello() string { return \"hello from a module\" }\n",
			} {
				f, _ := z.Create("tun.test/greet@v1.0.0/" + name)
				io.WriteString(f, body)
			}
			z.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	defer mod.Close()
	orig := sandbox.EgressDial
	sandbox.EgressDial = func(ctx context.Context, addr string) (net.Conn, error) {
		if addr == "modproxy.tun.invalid:80" {
			addr = mod.Listener.Addr().String()
		}
		return orig(ctx, addr)
	}
	defer func() { sandbox.EgressDial = orig }()

	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module hello\n\ngo 1.21\n\nrequire tun.test/greet v1.0.0\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nimport (\n\t\"fmt\"\n\n\t\"tun.test/greet\"\n)\n\nfunc main() { fmt.Println(greet.Hello()) }\n"), 0o644)
	r := ts.tun(ws, "run", "-e", "GOTOOLCHAIN=local", "-e", "GOPROXY=http://modproxy.tun.invalid", "-e", "GOSUMDB=off",
		"-e", "GOFLAGS=-mod=mod", "-c", "go build -o hello . && ./hello")
	if r.code != 0 || r.stdout != "hello from a module\n" || hits.Load() == 0 {
		t.Fatalf("go build with a module download (%d proxy requests): %+v", hits.Load(), r)
	}
}

func TestNetServiceThroughProxy(t *testing.T) {
	ts := netServer(t)
	id, urls := ts.startService("--port", "8080", "--port", "9090")
	defer ts.tun("", "cancel", id)
	for _, p := range []int{8080, 9090} {
		if code, body := ts.get(urls[p]+"q?x=1", true); code != 200 || !strings.HasPrefix(body, "path=/q query=x=1") {
			t.Errorf("port %d: %d %q", p, code, body)
		}
	}
	j := ts.status(id)
	ts.tun("", "cancel", id)
	ts.waitDone(id)
	if code, _ := ts.get(urls[8080], true); code != 409 {
		t.Errorf("after cancel: %d", code)
	}
	if j.State != api.Running || !j.Ports[0].Ready {
		t.Errorf("%+v", j)
	}
}
