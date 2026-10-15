package sandbox

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A tun server that itself runs inside a private-network job (scripts/dev-tun.sh under tun)
// reaches out through its own HTTPS_PROXY, with a CONNECT tunnel.
func TestEgressChainsThroughUpstreamProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan *http.Request, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				got <- req
				if req.Host == "refused.invalid:443" {
					io.WriteString(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
					return
				}
				io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
				io.Copy(c, br) // echo
			}()
		}
	}()
	t.Setenv("HTTPS_PROXY", "http://u:pw@"+ln.Addr().String()) // before the first ProxyFromEnvironment
	c, err := dialOut(context.Background(), "example.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("tunnel echo: %q %v", buf, err)
	}
	req := <-got
	if req.Method != "CONNECT" || req.Host != "example.invalid:443" ||
		req.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("u:pw")) {
		t.Errorf("upstream got %s %s %v", req.Method, req.Host, req.Header)
	}
	if _, err := dialOut(context.Background(), "refused.invalid:443"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("refused CONNECT: %v", err)
	}
}

// startNetJob starts a sleeping private-network job and waits for its egress listener.
func startNetJob(t *testing.T) *Net {
	t.Helper()
	if !BwrapUsable() {
		t.Skip("bwrap not usable here")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd, n, err := Start(Spec{Mode: Bwrap, PrivateNet: true, Argv: []string{"sleep", "60"}, Env: []string{"PATH=/usr/bin:/bin"}, Dir: dir, Root: dir, Tmp: dir, DataDir: dir}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	if err := n.egressReady(); err != nil {
		t.Skipf("private network not usable here: %v: %s", err, out.String())
	}
	return n
}

func nonblocking(t *testing.T, ln *net.TCPListener) bool {
	t.Helper()
	rc, err := ln.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fl int
	rc.Control(func(fd uintptr) {
		r, _, _ := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		fl = int(r)
	})
	return fl&syscall.O_NONBLOCK != 0
}

// The helper used to send descriptors via (*os.File).Fd, which switches the open file
// description (shared with the server's copy) to blocking mode: a later request flipped the
// server's egress listener, its Accept then blocked in the syscall and Close hung forever
// (seen in TestRunnerNetIsolation via NetUsable, which asked for the listener a second time).
func TestHelperRequestsKeepEgressNonblocking(t *testing.T) {
	n := startNetJob(t)
	defer n.Close()
	port := n.ln.Addr().(*net.TCPAddr).Port
	for i := 0; i < 3; i++ {
		f, err := n.call("egress")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		c, err := n.Dial(port)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if !nonblocking(t, n.ln) {
		t.Fatal("the server's egress listener is in blocking mode after helper requests")
	}
}

// Whatever happens to the shared listener's mode, Close must not hang.
func TestNetCloseWithBlockingEgressListener(t *testing.T) {
	n := startNetJob(t)
	rc, _ := n.ln.SyscallConn()
	rc.Control(func(fd uintptr) { syscall.SetNonblock(int(fd), false) })
	// One connection wakes the parked Accept; the next accept4 then blocks in the kernel.
	c, err := n.Dial(n.ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(200 * time.Millisecond)
	done := make(chan struct{})
	go func() { n.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Net.Close hangs on a blocking egress listener")
	}
}

// Many private-network jobs at once (NetUsable starts one each, like test servers do).
func TestNetUsableConcurrent(t *testing.T) {
	if !BwrapUsable() || NetUsable() != nil {
		t.Skip("private network not usable here")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- NetUsable()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("NetUsable hangs")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
