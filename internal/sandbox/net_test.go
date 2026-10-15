package sandbox

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
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
