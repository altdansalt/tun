package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

// TestServiceHelper is not a real test: with TUN_TEST_SERVICE=1 the test binary becomes the
// service under test, an HTTP server on every $TUN_PORT_* (optionally after SERVICE_DELAY).
func TestServiceHelper(t *testing.T) {
	if os.Getenv("TUN_TEST_SERVICE") != "1" {
		t.Skip("helper process for service tests")
	}
	if d, err := time.ParseDuration(os.Getenv("SERVICE_DELAY")); err == nil {
		time.Sleep(d)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		var vars []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "PORT=") || strings.HasPrefix(kv, "TUN_PORT_") {
				vars = append(vars, kv)
			}
		}
		sort.Strings(vars)
		fmt.Fprint(w, strings.Join(vars, " "))
	})
	streamRelease := make(chan struct{})
	mux.HandleFunc("/stream-release", func(w http.ResponseWriter, r *http.Request) { close(streamRelease) })
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "tick1")
		w.(http.Flusher).Flush()
		select {
		case <-streamRelease:
		case <-r.Context().Done():
			return
		}
		fmt.Fprintln(w, "tick2")
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "want upgrade", 400)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		io.Copy(conn, rw) // echo
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s query=%s auth=%q prefix=%s", r.URL.EscapedPath(), r.URL.RawQuery,
			r.Header.Get("Authorization")+r.Header.Get("X-Tun-Token"), r.Header.Get("X-Forwarded-Prefix"))
	})
	var wg sync.WaitGroup
	for _, kv := range os.Environ() {
		if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "TUN_PORT_") {
			ln, err := net.Listen("tcp", "127.0.0.1:"+v)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			wg.Add(1)
			go func() { defer wg.Done(); http.Serve(ln, mux) }()
		}
	}
	fmt.Println("service listening")
	wg.Wait()
	os.Exit(0)
}

func helperArgs(t *testing.T) []string {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{"--no-workspace", "-e", "TUN_TEST_SERVICE=1", "--", exe, "-test.run=^TestServiceHelper$"}
}

// startService runs tun run --service and returns the job id and proxy URLs by port.
func (ts *testServer) startService(flags ...string) (string, map[int]string) {
	ts.t.Helper()
	args := append([]string{"run", "--service"}, flags...)
	args = append(args, helperArgs(ts.t)...)
	r := ts.tun("", args...)
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) < 1 {
		ts.t.Fatalf("tun run --service: %+v", r)
	}
	urls := map[int]string{}
	for _, l := range lines[1:] {
		p, u, _ := strings.Cut(l, " ")
		n, _ := strconv.Atoi(p)
		urls[n] = u
	}
	return lines[0], urls
}

func (ts *testServer) get(u string, auth bool) (int, string) {
	ts.t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	if auth {
		req.Header.Set("Authorization", "Bearer "+ts.srv.Token())
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err == nil {
		c.Close()
	}
	return err == nil
}

func TestService(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{MaxJobs: 1, MaxServices: 1})
	start := time.Now()
	id, urls := ts.startService("--port", "8080", "-e", "SERVICE_DELAY=700ms")
	if time.Since(start) < 700*time.Millisecond {
		t.Errorf("tun run --service returned before the service listened")
	}
	u := urls[8080]
	if want := ts.url + "/v1/jobs/" + id + "/ports/8080/"; u != want {
		t.Fatalf("url %q, want %q", u, want)
	}

	// Proxying: path (escaped) and query pass through, the tun token does not.
	code, body := ts.get(u+"a/b%2Fc?x=1&y=2", true)
	if want := fmt.Sprintf(`path=/a/b%%2Fc query=x=1&y=2 auth="" prefix=/v1/jobs/%s/ports/8080`, id); code != 200 || body != want {
		t.Errorf("proxy GET = %d %q, want %q", code, body, want)
	}
	if code, body := ts.get(strings.TrimSuffix(u, "/"), true); code != 200 || !strings.HasPrefix(body, "path=/ ") {
		t.Errorf("proxy GET without trailing slash = %d %q", code, body)
	}
	// Client bearer credentials must be stripped just like the legacy admin token.
	clientToken, err := server.IssueClientToken(ts.data, ts.status(id).Client)
	if err != nil {
		t.Fatal(err)
	}
	reqClient, _ := http.NewRequest("GET", u, nil)
	reqClient.Header.Set("Authorization", "Bearer "+clientToken)
	respClient, err := http.DefaultClient.Do(reqClient)
	if err != nil {
		t.Fatal(err)
	}
	clientBody, err := io.ReadAll(respClient.Body)
	respClient.Body.Close()
	if err != nil || respClient.StatusCode != 200 || !strings.Contains(string(clientBody), `auth=""`) {
		t.Fatalf("client token leaked to service: %d %q %v", respClient.StatusCode, clientBody, err)
	}
	// Via X-Tun-Token (as the tun client sends it): dropped, and the service's own
	// Authorization passes through.
	req0, _ := http.NewRequest("GET", u, nil)
	req0.Header.Set("X-Tun-Token", ts.srv.Token())
	req0.Header.Set("Authorization", "Basic c2VydmljZQ==")
	if resp, err := http.DefaultClient.Do(req0); err != nil {
		t.Error(err)
	} else {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), `auth="Basic c2VydmljZQ=="`) {
			t.Errorf("X-Tun-Token request: service saw %q", b)
		}
	}
	if code, _ := ts.get(u, false); code != 401 {
		t.Errorf("proxy without token = %d, want 401", code)
	}
	if code, body := ts.get(ts.url+"/v1/jobs/"+id+"/ports/9999/", true); code != 404 || !strings.Contains(body, "[8080]") {
		t.Errorf("unknown port = %d %q", code, body)
	}

	// Env: PORT and TUN_PORT_8080 are the allocated host port.
	j := ts.status(id)
	if !j.Service || j.TimeoutSec != 0 || len(j.Ports) != 1 || !j.Ports[0].Ready || j.Ports[0].HostPort == 0 {
		t.Fatalf("job %+v", j)
	}
	hp := j.Ports[0].HostPort
	if _, body := ts.get(u+"env", true); body != fmt.Sprintf("PORT=%d TUN_PORT_8080=%d", hp, hp) {
		t.Errorf("env %q", body)
	}
	r := ts.tun("", "status", id)
	if r.code != 0 || !strings.Contains(r.stdout, `"url":"`+u+`"`) || !strings.Contains(r.stdout, `"ready":true`) {
		t.Errorf("status: %+v", r)
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

	// Services don't take max-jobs slots, but max-services is enforced.
	if r := ts.tun("", "run", "--timeout", "20s", "--", "true"); r.code != 0 {
		t.Errorf("job alongside a service (max-jobs 1): %+v", r)
	}
	r = ts.tun("", append([]string{"run", "--service", "--port", "1"}, helperArgs(t)...)...)
	if r.code != 125 || !strings.Contains(r.stderr, "max-services") {
		t.Errorf("over max-services: %+v", r)
	}

	// Cancel kills it; the proxy then says so.
	if r := ts.tun("", "cancel", id); r.code != 0 {
		t.Fatalf("cancel: %+v", r)
	}
	if j := ts.waitDone(id); j.State != api.Canceled {
		t.Errorf("after cancel: %+v", j)
	}
	if code, body := ts.get(u, true); code != 409 || !strings.Contains(body, "canceled") {
		t.Errorf("proxy after cancel = %d %q", code, body)
	}
	if listening(hp) {
		t.Errorf("host port %d still listening after cancel", hp)
	}
	// The slot is free again.
	id2, _ := ts.startService("--port", "1")
	ts.tun("", "cancel", id2)
}

func TestServiceMultiPortAndJSON(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	r := ts.tun("", append([]string{"run", "--service", "--json", "--port", "80", "--port", "5432"}, helperArgs(t)...)...)
	var j api.Job
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &j) != nil || len(j.Ports) != 2 {
		t.Fatalf("--json: %+v", r)
	}
	defer ts.tun("", "cancel", j.ID)
	_, body := ts.get(j.Ports[1].URL+"env", true)
	want := fmt.Sprintf("TUN_PORT_5432=%d TUN_PORT_80=%d", j.Ports[1].HostPort, j.Ports[0].HostPort)
	if body != want || j.Ports[0].URL != ts.url+"/v1/jobs/"+j.ID+"/ports/80/" {
		t.Errorf("env %q (want %q), ports %+v", body, want, j.Ports)
	}
	// A service with an explicit --timeout keeps it.
	r = ts.tun("", "run", "--service", "--timeout", "1s", "--no-workspace", "--", "sleep", "30")
	id := strings.TrimSpace(r.stdout)
	if r.code != 0 {
		t.Fatalf("portless service: %+v", r)
	}
	if j := ts.waitDone(id); j.State != api.Timeout {
		t.Errorf("service with --timeout: %+v", j)
	}
}

func TestServiceFailures(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	r := ts.tun("", "run", "--service", "--port", "80", "--no-workspace", "-c", "echo starting; echo boom >&2; exit 3")
	if r.code != 3 || r.stdout != "starting\n" || !strings.Contains(r.stderr, "boom\n") || !strings.Contains(r.stderr, "before it was ready") {
		t.Errorf("early exit: %+v", r)
	}
	r = ts.tun("", "run", "--service", "--port", "80", "--no-workspace", "--", "true")
	if r.code != 125 || !strings.Contains(r.stderr, "exited with code 0 before it was ready") {
		t.Errorf("early exit 0: %+v", r)
	}
	r = ts.tun("", "run", "--service", "--port", "80", "--ready-timeout", "1s", "--no-workspace", "-c", "echo waiting; exec sleep 60")
	if r.code != 125 || !strings.Contains(r.stderr, "not accepting connections on port(s) 80") || !strings.Contains(r.stdout, "waiting") {
		t.Errorf("ready timeout: %+v", r)
	}
	js, _ := ts.client().List(context.Background(), 1, nil)
	if j := ts.waitDone(js[0].ID); j.State != api.Canceled {
		t.Errorf("not canceled after ready timeout: %+v", j)
	}
	for _, c := range []struct{ args, msg string }{
		{"--service --out f -- true", "--out can't be used with --service"},
		{"--port 80 -- true", "--port needs --service"},
		{"--service --port 0 -- true", "invalid value"},
		{"--service --port 80 --port 80 -- true", "port 80 given twice"},
	} {
		r := ts.tun("", append([]string{"run", "--no-workspace"}, strings.Fields(c.args)...)...)
		if r.code != 125 || !strings.Contains(r.stderr, c.msg) {
			t.Errorf("tun run %s: %+v, want %q", c.args, r, c.msg)
		}
	}
}

func TestServiceKilledOnShutdown(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	ts := startServer(t, server.Config{DataDir: data})
	id, _ := ts.startService("--port", "8080")
	hp := ts.status(id).Ports[0].HostPort
	ts.stop()
	if listening(hp) {
		t.Errorf("service still listening on %d after server shutdown", hp)
	}
	ts2 := startServer(t, server.Config{DataDir: data})
	if j := ts2.status(id); j.State != api.Failed {
		t.Errorf("after restart: %+v", j)
	}
}

// TestServiceBwrap runs a python3 http.server under the real sandbox.
func TestServiceBwrap(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap not installed or unusable on this host")
	}
	ts := startServer(t, server.Config{Sandbox: sandbox.Bwrap})
	ws := t.TempDir()
	os.WriteFile(ws+"/hello.txt", []byte("hi from the workspace"), 0o644)
	r := ts.tun(ws, "run", "--service", "--port", "8000", "-c", `exec python3 -m http.server --bind 127.0.0.1 "$PORT"`)
	lines := strings.Fields(r.stdout)
	if r.code != 0 || len(lines) != 3 {
		t.Fatalf("python service: %+v", r)
	}
	defer ts.tun("", "cancel", lines[0])
	u, _ := url.JoinPath(lines[2], "hello.txt")
	if code, body := ts.get(u, true); code != 200 || body != "hi from the workspace" {
		t.Errorf("GET %s = %d %q", u, code, body)
	}
}

// TestCancelStartRace: plain jobs canceled right as the scheduler hands them to run() (here
// with MaxJobs 1 and a stream of quick jobs) must end canceled and never start.
func TestCancelStartRace(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{MaxJobs: 1})
	c := ts.client()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		j, err := c.Create(ctx, api.CreateJob{Argv: []string{"sleep", "30"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Cancel(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
		if cur := ts.waitDone(j.ID); cur.State != api.Canceled {
			t.Fatalf("iteration %d: %+v", i, cur)
		}
	}
	if r := ts.tun("", "run", "--timeout", "20s", "--", "true"); r.code != 0 {
		t.Errorf("server unusable after cancel loop: %+v", r)
	}
}

func TestServiceJSONFailureAndEarlyLimit(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{MaxServices: 1})
	r := ts.tun("", "run", "--service", "--json", "--port", "80", "--no-workspace", "-c", "echo out; echo err >&2; exit 4")
	var res map[string]any
	if r.code != 4 || json.Unmarshal([]byte(r.stdout), &res) != nil || res["stdout"] != "out\n" || res["stderr"] != "err\n" ||
		res["tun_exit"] != 4.0 || !strings.Contains(fmt.Sprint(res["error"]), "before it was ready") {
		t.Errorf("--json early exit: %+v", r)
	}
	// With a port the helper serves until canceled (without one it exits at once, freeing the
	// slot: the check below then raced its exit).
	id, _ := ts.startService("--port", "8080")
	defer ts.tun("", "cancel", id)
	before, _ := os.ReadDir(ts.data + "/jobs")
	ws := t.TempDir()
	os.WriteFile(ws+"/f", []byte("x"), 0o644)
	r = ts.tun(ws, "run", "--service", "--", "true")
	after, _ := os.ReadDir(ts.data + "/jobs")
	if r.code != 125 || !strings.Contains(r.stderr, "HTTP 429") || len(after) != len(before) {
		t.Errorf("over max-services: %+v, job dirs %d -> %d", r, len(before), len(after))
	}
}

// TestServiceSurvivesGC: the retention sweep must leave a running service (EndedAt nil) alone
// even with retention far shorter than its lifetime, while sweeping services that ended,
// including ones canceled before they started.
func TestServiceSurvivesGC(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{KeepWork: time.Millisecond, GCInterval: 20 * time.Millisecond, MaxServices: 2})
	ws := t.TempDir()
	os.WriteFile(ws+"/f", []byte("x"), 0o644)
	r := ts.tun(ws, append([]string{"run", "--service", "--port", "8080"}, helperArgs(t)[1:]...)...) // with a workspace
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 2 {
		t.Fatalf("service: %+v", r)
	}
	id := lines[0]
	_, u, _ := strings.Cut(lines[1], " ")
	for i := 0; i < 20; i++ {
		c := ts.client()
		j, err := c.Create(context.Background(), api.CreateJob{Argv: []string{"sleep", "30"}, Service: true})
		if err != nil {
			t.Fatal(err)
		}
		c.Cancel(context.Background(), j.ID)
		ts.waitDone(j.ID)
	}
	time.Sleep(300 * time.Millisecond) // many sweeps
	if code, body := ts.get(u+"env", true); code != 200 || !strings.Contains(body, "PORT=") {
		t.Errorf("service after sweeps: %d %q", code, body)
	}
	if j := ts.status(id); j.State != api.Running || j.WorkDeleted {
		t.Errorf("running service touched by gc: %+v", j)
	}
	if _, err := os.Stat(ts.data + "/jobs/" + id + "/sandbox/work/f"); err != nil {
		t.Errorf("running service's workspace deleted: %v", err)
	}
	ts.tun("", "cancel", id)
	ts.waitFor(id, func(j api.Job) bool { return j.WorkDeleted })
}
