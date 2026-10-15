package sandbox

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
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
	// Not t.Setenv: ProxyFromEnvironment reads the environment once per process, so a second
	// run (-count) would dial the first run's closed listener.
	pu, _ := url.Parse("http://u:pw@" + ln.Addr().String())
	orig := upstreamProxy
	upstreamProxy = http.ProxyURL(pu)
	defer func() { upstreamProxy = orig }()
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

// The helper hands out the egress listener without changing its blocking mode, which is
// shared by every copy of the descriptor. It used File().Fd(), which made it blocking: a
// second "egress" request (NetUsable makes one while serveEgress makes its own) could leave
// the server's proxy in a blocking accept, and Close then hung forever (seen under load:
// TestNetHelperExitStatus stuck in NetUsable for 96 minutes).
func TestHelperKeepsListenerNonblocking(t *testing.T) {
	if !BwrapUsable() {
		t.Skip("bwrap not installed or unusable on this host")
	}
	if err := NetUsable(); err != nil {
		t.Skipf("no private network namespaces here: %v", err)
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
	closed := make(chan struct{})
	defer func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
		go func() { n.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Error("Net.Close hung (the egress proxy is stuck in accept)")
		}
	}()
	f, err := n.call("egress")
	if err != nil {
		t.Fatalf("%v: %s", err, &out)
	}
	defer f.Close()
	rc, _ := f.SyscallConn()
	nonblocking := func() (nb bool) {
		rc.Control(func(fd uintptr) {
			flags, _, _ := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
			nb = flags&syscall.O_NONBLOCK != 0
		})
		return nb
	}
	rc.Control(func(fd uintptr) { syscall.SetNonblock(int(fd), true) })
	for range 3 {
		g, err := n.call("egress")
		if err != nil {
			t.Fatal(err)
		}
		g.Close()
	}
	// serveEgress' net.FileListener briefly makes it blocking too, then nonblocking again.
	for deadline := time.Now().Add(2 * time.Second); !nonblocking(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("egress requests left the shared listener in blocking mode")
		}
	}
}
