// Shared test helpers (:testlib): the test binary as a service (TestServiceHelper) and
// talking to services through the server's proxy.

package client

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
