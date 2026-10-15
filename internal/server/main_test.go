package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
)

func TestEphemeral(t *testing.T) {
	stdin, input := io.Pipe()
	output, stdout := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- serve([]string{"--ephemeral", "--exit-on-eof", "--sandbox", "none"}, stdin, stdout, &stderr)
		stdout.Close()
	}()
	defer input.Close()
	lines := bufio.NewReader(output)
	url, err := lines.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	token, err := lines.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "TUN_URL=http://127.0.0.1:") || !strings.HasPrefix(token, "TUN_TOKEN=") {
		t.Fatalf("environment: %q %q", url, token)
	}
	runnerToken, err := lines.ReadString('\n')
	if err != nil || !strings.HasPrefix(runnerToken, "TUN_RUNNER_TOKEN=") {
		t.Fatal(runnerToken, err)
	}
	base := strings.TrimSpace(strings.TrimPrefix(url, "TUN_URL="))
	req, _ := http.NewRequest("GET", base+"/v1/jobs", nil)
	req.Header.Set("X-Tun-Token", strings.TrimSpace(strings.TrimPrefix(token, "TUN_TOKEN=")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	input.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, &stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EOF did not stop server")
	}
	rest, _ := io.ReadAll(lines)
	if len(rest) != 0 {
		t.Fatalf("extra stdout: %q", rest)
	}
	// The diagnostic includes the temporary path, allowing cleanup to be checked.
	diagnostic := stderr.String()
	start := strings.Index(diagnostic, "(data ")
	if start < 0 {
		t.Fatalf("missing data diagnostic: %s", diagnostic)
	}
	end := strings.Index(diagnostic[start:], ", sandbox")
	if end < 0 {
		t.Fatalf("missing data diagnostic: %s", diagnostic)
	}
	dir := diagnostic[start+6 : start+end]
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temporary dir remains: %s: %v", dir, err)
	}
}

func TestEphemeralConflicts(t *testing.T) {
	for _, flag := range []string{"--addr", "--data"} {
		var out, diagnostic bytes.Buffer
		code := serve([]string{"--ephemeral", flag, "unused"}, strings.NewReader(""), &out, &diagnostic)
		if code != 125 || !strings.Contains(diagnostic.String(), "cannot be combined") || out.Len() != 0 {
			t.Fatalf("%s: code %d, %s", flag, code, &diagnostic)
		}
	}
}

func TestNetFlag(t *testing.T) {
	for _, args := range [][]string{{"--sandbox", "none", "--net", "private"}, {"--net", "bogus"}} {
		var out, diagnostic bytes.Buffer
		code := serve(append([]string{"--ephemeral"}, args...), strings.NewReader(""), &out, &diagnostic)
		if code != 125 || !strings.Contains(diagnostic.String(), "--net") {
			t.Errorf("%q: code %d, %s", args, code, &diagnostic)
		}
	}
	if !sandbox.BwrapUsable() || sandbox.NetUsable() != nil {
		t.Skip("no bwrap with private networks here")
	}
	for _, net := range []string{"auto", "host"} {
		stdin, input := io.Pipe()
		output, stdout := io.Pipe()
		var stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- serve([]string{"--ephemeral", "--exit-on-eof", "--net", net}, stdin, stdout, &stderr)
			stdout.Close()
		}()
		url, err := bufio.NewReader(output).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(strings.TrimSpace(strings.TrimPrefix(url, "TUN_URL=")) + "/v1/healthz")
		if err != nil {
			t.Fatal(err)
		}
		var h api.Health
		json.NewDecoder(resp.Body).Decode(&h)
		resp.Body.Close()
		input.Close()
		go io.Copy(io.Discard, output)
		<-done
		if want := net == "auto"; h.NetworkIsolation != want || !strings.Contains(stderr.String(), "net "+map[bool]string{true: "private", false: "host"}[want]) {
			t.Errorf("--net %s: healthz %+v, stderr %s", net, h, &stderr)
		}
		if net == "host" && !strings.Contains(stderr.String(), "warning: --net host") {
			t.Errorf("--net host: no warning: %s", &stderr)
		}
	}
}

func TestServeCache(t *testing.T) {
	stdin, input := io.Pipe()
	output, stdout := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	dir := t.TempDir()
	fd, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	socket := fmt.Sprintf("/proc/%d/fd/%d/cache.sock", os.Getpid(), fd.Fd())
	go func() {
		done <- serve([]string{"--ephemeral", "--exit-on-eof", "--sandbox=none", "--cache-dir=reapi", "--cache-max-size=1MB", "--cache-socket=" + socket, "--cache-action-writes=false"}, stdin, stdout, &stderr)
		stdout.Close()
	}()
	defer input.Close()
	lines := bufio.NewReader(output)
	url, err := lines.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	token, err := lines.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(strings.TrimPrefix(url, "TUN_URL="))
	auth := strings.TrimSpace(strings.TrimPrefix(token, "TUN_TOKEN="))
	for _, unix := range []bool{false, true} {
		p := new(http.Protocols)
		p.SetUnencryptedHTTP2(true)
		tr := &http.Transport{Protocols: p}
		if unix {
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}
		}
		for _, tok := range []string{auth, "wrong"} {
			req, _ := http.NewRequest("POST", base+"/build.bazel.remote.execution.v2.Capabilities/GetCapabilities", bytes.NewReader(make([]byte, 5)))
			req.Header.Set("Content-Type", "application/grpc")
			req.Header.Set("X-Tun-Token", tok)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := "0"
			if tok != auth {
				want = "16"
			}
			if resp.Trailer.Get("grpc-status") != want {
				t.Fatalf("unix=%v status=%v body=%x", unix, resp.Trailer, body)
			}
		}
		tr.CloseIdleConnections()
	}
	// gRPC content types on service routes must remain in the HTTP router.
	req, _ := http.NewRequest("POST", base+"/v1/jobs/missing/ports/8080/service.Method", bytes.NewReader(make([]byte, 5)))
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("X-Tun-Token", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusNotFound || strings.HasPrefix(resp.Header.Get("Content-Type"), "application/grpc") {
		t.Fatalf("service routing: status=%d headers=%v body=%s err=%v", resp.StatusCode, resp.Header, body, err)
	}
	input.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, &stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
}
