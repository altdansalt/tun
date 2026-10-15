package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/reapi"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

// Just enough protobuf for ActionCache requests (remote_execution.proto): a Digest is
// {1: hash, 2: size_bytes}; GetActionResultRequest {2: action_digest};
// UpdateActionResultRequest {2: action_digest, 3: action_result}; ActionResult {4: exit_code}.
func pbVarint(v uint64) []byte { return binary.AppendUvarint(nil, v) }
func pbBytes(n int, b []byte) []byte {
	return append(append(pbVarint(uint64(n<<3|2)), pbVarint(uint64(len(b)))...), b...)
}
func pbInt(n int, v uint64) []byte { return append(pbVarint(uint64(n<<3)), pbVarint(v)...) }
func pbDigest(data string) []byte {
	h := sha256.Sum256([]byte(data))
	return append(pbBytes(1, []byte(hex.EncodeToString(h[:]))), pbInt(2, uint64(len(data)))...)
}

// grpcCall makes one unary gRPC call over h2c, through dial if given (a unix socket), else
// to the test server's TCP address. It returns the response message and grpc-status. client
// is sent as X-Tun-Client, or with a "vm:" prefix as X-Exedev-Source-Vm (the exe.dev proxy's).
func (ts *testServer) grpcCall(dial func() (net.Conn, error), method, token, client string, req []byte) ([]byte, string) {
	ts.t.Helper()
	frame := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(req)))
	r, _ := http.NewRequest("POST", ts.url+"/build.bazel.remote.execution.v2."+method, bytes.NewReader(append(frame, req...)))
	r.Header.Set("Content-Type", "application/grpc")
	if token != "" {
		r.Header.Set("X-Tun-Token", token)
	}
	if vm, ok := strings.CutPrefix(client, "vm:"); ok {
		r.Header.Set("X-Exedev-Source-Vm", vm)
	} else if client != "" {
		r.Header.Set("X-Tun-Client", client)
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: p}
	if dial != nil {
		tr.DialContext = func(context.Context, string, string) (net.Conn, error) { return dial() }
	}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(r)
	if err != nil {
		return nil, "dial: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(body) >= 5 {
		body = body[5:]
	}
	return body, resp.Trailer.Get("grpc-status")
}

// TestBazelSessionRemoteCache: with a REAPI cache, every session gets tun's remote cache in its
// bazelrc through its own socket, bound read-only into the sandbox. The socket needs no token
// and stores action results in the session's client's namespace only; the TCP endpoint
// needs the token and gives each client its own namespace.
func TestBazelSessionRemoteCache(t *testing.T) {
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap unusable")
	}
	store, err := reapi.Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.Config{Sandbox: sandbox.Bwrap, DataDir: t.TempDir(), Cache: store}
	ts := bazelServer(t, cfg, false)
	ws := bazelWS(t, map[string]string{"x": "1"})
	r := ts.tun(ws, "bazel", "sh", `cat /tun/tun.bazelrc; test -S /tun/sock/cache.sock && echo sock-ok; touch /tun/sock/x || echo sock-ro`)
	if r.code != 0 || !strings.Contains(r.stdout, "common --remote_cache=grpc://tun\ncommon --remote_proxy=unix:/tun/sock/cache.sock\n") ||
		!strings.Contains(r.stdout, "sock-ok") || !strings.Contains(r.stdout, "sock-ro") ||
		!strings.Contains(r.stdout, "common --remote_executor=grpc://tun\ncommon --jobs=4\ncommon --remote_local_fallback\n") ||
		!strings.Contains(r.stderr, "remote cache: tun's, through /tun/sock/cache.sock; remote execution on 4 slots") {
		t.Fatalf("session: %+v", r)
	}
	if !ts.health().RemoteCache {
		t.Error("healthz: remote_cache false")
	}
	sess := ts.sessions()[0]
	dial := func() (net.Conn, error) {
		// The data dir's path may be too long for a unix socket address: go through a descriptor.
		d, err := os.Open(filepath.Join(ts.data, "sessions", sess.ID, "sock"))
		if err != nil {
			return nil, err
		}
		defer d.Close()
		return net.Dial("unix", fmt.Sprintf("/proc/self/fd/%d/cache.sock", d.Fd()))
	}
	action := pbBytes(2, pbDigest("action"))
	result := pbInt(4, 7)
	if _, c := ts.grpcCall(dial, "ActionCache/UpdateActionResult", "", "someone-else", append(action, pbBytes(3, result)...)); c != "0" {
		t.Fatalf("update through the session socket (no token): %s", c)
	}
	if b, c := ts.grpcCall(dial, "ActionCache/GetActionResult", "", "", action); c != "0" || !bytes.Equal(b, result) {
		t.Fatalf("lookup through the session socket: %x %s", b, c)
	}
	token := ts.srv.Token()
	if _, c := ts.grpcCall(nil, "ActionCache/GetActionResult", "", sess.Client, action); c != "16" {
		t.Fatalf("TCP without a token: %s", c)
	}
	// The socket's writes went to the session's client, whatever the request claimed.
	if b, c := ts.grpcCall(nil, "ActionCache/GetActionResult", token, sess.Client, action); c != "0" || !bytes.Equal(b, result) {
		t.Fatalf("the session's client over TCP: %x %s", b, c)
	}
	if _, c := ts.grpcCall(nil, "ActionCache/GetActionResult", token, "someone-else", action); c != "5" {
		t.Fatalf("another client got the session's result: %s", c)
	}
	// A proxy-attested name equal to the session's self-reported one is another client.
	if sess.Identity != "self:"+sess.Client {
		t.Fatalf("session identity %q for client %q", sess.Identity, sess.Client)
	}
	if _, c := ts.grpcCall(nil, "ActionCache/GetActionResult", token, "vm:"+sess.Client, action); c != "5" {
		t.Fatalf("an exe.dev VM named like the session's self-reported client got its result: %s", c)
	}
	// --cold discards the session dir, the socket comes back with the new sandbox; the action
	// cache stays (it is the client's, not the session's).
	if r = ts.tun(ws, "bazel", "--cold", "sh", "test -S /tun/sock/cache.sock && echo sock-ok"); r.code != 0 || !strings.Contains(r.stdout, "sock-ok") {
		t.Fatalf("--cold: %+v", r)
	}
	if b, c := ts.grpcCall(dial, "ActionCache/GetActionResult", "", "", action); c != "0" || !bytes.Equal(b, result) {
		t.Fatalf("after --cold: %x %s", b, c)
	}
	ts.stop()
	if _, c := ts.grpcCall(dial, "ActionCache/GetActionResult", "", "", action); !strings.HasPrefix(c, "dial:") {
		t.Fatalf("the session socket outlived the server: %s", c)
	}
}

// tun serve --session-remote-execution=false: sessions keep the remote cache but run their
// actions themselves.
func TestBazelSessionsLocal(t *testing.T) {
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap unusable")
	}
	store, err := reapi.Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ts := bazelServer(t, server.Config{Sandbox: sandbox.Bwrap, DataDir: t.TempDir(), Cache: store, SessionsLocal: true}, false)
	ws := bazelWS(t, map[string]string{"x": "1"})
	r := ts.tun(ws, "bazel", "sh", "cat /tun/tun.bazelrc")
	if r.code != 0 || !strings.Contains(r.stdout, "common --remote_cache=grpc://tun\n") || strings.Contains(r.stdout, "--remote_executor") ||
		strings.Contains(r.stderr, "remote execution") {
		t.Fatalf("%+v", r)
	}
}
