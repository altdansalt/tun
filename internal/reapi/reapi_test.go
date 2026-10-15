package reapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testServer(t *testing.T) (*Handler, string, string) {
	t.Helper()
	s, e := Open(t.TempDir(), 16<<20)
	if e != nil {
		t.Fatal(e)
	}
	h := &Handler{Store: s, Token: "secret", AllowActionWrites: true}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	hs := &http.Server{Handler: h, Protocols: Protocols()}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
	socketDir := t.TempDir()
	dir, e := os.Open(socketDir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { dir.Close() })
	socket := fmt.Sprintf("/proc/%d/fd/%d/c", os.Getpid(), dir.Fd())
	ul, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	us := &http.Server{Handler: h, Protocols: Protocols()}
	go us.Serve(ul)
	t.Cleanup(func() { us.Close() })
	return h, ln.Addr().String(), socket
}
func rpc(t *testing.T, addr, method, token string, b []byte) ([]byte, string) {
	t.Helper()
	frame := make([]byte, 5)
	binary.BigEndian.PutUint32(frame[1:], uint32(len(b)))
	frame = append(frame, b...)
	r, _ := http.NewRequest("POST", "http://"+addr+method, bytes.NewReader(frame))
	r.Header.Set("Content-Type", "application/grpc")
	r.Header.Set("X-Tun-Token", token)
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: p}
	defer tr.CloseIdleConnections()
	resp, e := tr.RoundTrip(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	out, e := frameRead(resp.Body)
	if e != nil && resp.Trailer.Get("grpc-status") == "0" {
		t.Fatal(e)
	} // Read EOF to populate trailers.
	var buf [1]byte
	resp.Body.Read(buf[:])
	return out, resp.Trailer.Get("grpc-status")
}
func TestWire(t *testing.T) {
	b := msg(intField(1, 300), bytesField(2, []byte("hello")))
	fs, e := decode(b)
	if e != nil || len(fs) != 2 || fs[0].v != 300 || string(fs[1].b) != "hello" {
		t.Fatalf("%v %v", fs, e)
	}
	for _, b := range [][]byte{{0}, {10, 255}, {8, 128}, {15}, {9, 1}} {
		if _, e := decode(b); e == nil {
			t.Fatalf("accepted %x", b)
		}
	}
}
func TestCacheServices(t *testing.T) {
	h, addr, _ := testServer(t)
	prefix := "/build.bazel.remote.execution.v2."
	d := digest([]byte("hello"))
	req := bytesField(2, d)
	if _, c := rpc(t, addr, prefix+"ActionCache/GetActionResult", "bad", req); c != "16" {
		t.Fatal(c)
	}
	if _, c := rpc(t, addr, prefix+"ActionCache/GetActionResult", "secret", req); c != "5" {
		t.Fatal(c)
	}
	b, c := rpc(t, addr, prefix+"ContentAddressableStorage/FindMissingBlobs", "secret", req)
	if c != "0" || len(all(b, 2)) != 1 {
		t.Fatalf("%x %s", b, c)
	}
	update := bytesField(2, msg(bytesField(1, d), bytesField(2, []byte("hello"))))
	b, c = rpc(t, addr, prefix+"ContentAddressableStorage/BatchUpdateBlobs", "secret", update)
	if c != "0" || get(get(get(b, 1).b, 2).b, 1).v != 0 {
		t.Fatalf("%x %s", b, c)
	}
	b, c = rpc(t, addr, prefix+"ContentAddressableStorage/BatchReadBlobs", "secret", req)
	if c != "0" || string(get(get(b, 1).b, 2).b) != "hello" {
		t.Fatalf("%x %s", b, c)
	}
	if e := h.Store.put("cas-", d, []byte("wrong")); e == nil {
		t.Fatal("accepted mismatch")
	}
	result := intField(4, 0)
	_, c = rpc(t, addr, prefix+"ActionCache/UpdateActionResult", "secret", msg(req, bytesField(3, result)))
	if c != "0" {
		t.Fatal(c)
	}
	b, c = rpc(t, addr, prefix+"ActionCache/GetActionResult", "secret", req)
	if c != "0" || !bytes.Equal(b, result) {
		t.Fatalf("%x %s", b, c)
	}
	h.AllowActionWrites = false
	_, c = rpc(t, addr, prefix+"ActionCache/UpdateActionResult", "secret", msg(req, bytesField(3, result)))
	if c != "7" {
		t.Fatal(c)
	}
}
func TestLRU(t *testing.T) {
	s, e := Open(t.TempDir(), 6)
	if e != nil {
		t.Fatal(e)
	}
	a, b := []byte("aaa"), []byte("bbb")
	s.put("cas-", digest(a), a)
	s.put("cas-", digest(b), b)
	s.read("cas-", digest(a))
	s.put("cas-", digest([]byte("ccc")), []byte("ccc"))
	if _, e := s.read("cas-", digest(b)); e == nil {
		t.Fatal("LRU not evicted")
	}
	if _, e := s.read("cas-", digest(a)); e != nil {
		t.Fatal(e)
	}
}

// testBazel is the Bazel binary: under bazel test the pinned @bazel_binary (TUN_TEST_BAZEL, see
// BUILD.bazel), run directly, since bazelisk inside a test sandbox has no download cache and no
// network; else bazel (bazelisk) from PATH, or "" if there is none.
func testBazel() string {
	if p := os.Getenv("TUN_TEST_BAZEL"); p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(os.Getenv("TEST_SRCDIR"), p)
		}
		return p
	}
	p, _ := exec.LookPath("bazel")
	return p
}

func TestBazelCache(t *testing.T) {
	bazel := testBazel()
	if bazel == "" {
		t.Skip("bazel missing")
	}
	_, addr, socket := testServer(t)
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "MODULE.bazel"), nil, 0600)
	os.WriteFile(filepath.Join(work, "BUILD.bazel"), []byte(`genrule(name="leaf", outs=["leaf.txt"], cmd="head -c 3145728 /dev/zero > $@")
genrule(name="top", srcs=[":leaf"], outs=["top.txt"], cmd="cat $(location :leaf) > $@")`), 0600)
	root := filepath.Join(t.TempDir(), "root")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bazel, "--output_user_root="+root, "shutdown")
		cmd.Dir = work
		cmd.WaitDelay = time.Second
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("bazel shutdown: %v: %s", err, out)
		}
	})
	run := func(extra ...string) string {
		t.Helper()
		args := []string{"--output_user_root=" + root, "--host_jvm_args=-Xmx512m", "--batch", "build", "//:top", "--jobs=2", "--remote_cache=grpc://" + addr, "--remote_header=x-tun-token=secret", "--remote_timeout=10", "--enable_bzlmod=false", "--repository_cache=" + t.TempDir()}
		args = append(args, extra...)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bazel, args...)
		cmd.WaitDelay = 3 * time.Second
		cmd.Dir = work
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("bazel %v: %v\n%s", extra, e, out)
		}
		return string(out)
	}
	run()
	for _, flags := range [][]string{nil, {"--remote_proxy=unix:" + socket}, {"--remote_download_minimal"}} {
		os.RemoveAll(root)
		out := run(flags...)
		if !strings.Contains(out, "remote cache hit") {
			t.Fatalf("no remote cache hit:\n%s", out)
		}
	}
	if _, e := os.Stat(filepath.Join(work, "bazel-bin", "leaf.txt")); !os.IsNotExist(e) {
		t.Fatalf("minimal download materialized intermediate: %v", e)
	}
	fmt.Fprintln(os.Stderr, "Bazel cache verified over TCP, unix proxy, and minimal downloads")
}

func TestByteStream(t *testing.T) {
	h, addr, _ := testServer(t)
	data := []byte("streaming content")
	d := digest(data)
	name := "instance/uploads/id/blobs/" + string(get(d, 1).b) + fmt.Sprintf("/%d", len(data))
	prefix := "/google.bytestream.ByteStream/"
	// An interrupted stream retains its committed offset.
	if _, e := h.Store.appendUpload(name, d, 0, data[:4], false); e != nil {
		t.Fatal(e)
	}
	b, c := rpc(t, addr, prefix+"QueryWriteStatus", "secret", bytesField(1, []byte(name)))
	if c != "0" || get(b, 1).v != 4 {
		t.Fatalf("%x %s", b, c)
	}
	b, c = rpc(t, addr, prefix+"Write", "secret", msg(bytesField(1, []byte(name)), intField(2, 4), intField(3, 1), bytesField(10, data[4:])))
	if c != "0" || get(b, 1).v != uint64(len(data)) {
		t.Fatalf("%x %s", b, c)
	}
	b, c = rpc(t, addr, prefix+"QueryWriteStatus", "secret", bytesField(1, []byte(name)))
	if c != "0" || get(b, 2).v != 1 {
		t.Fatalf("%x %s", b, c)
	}
	readName := "instance/blobs/" + string(get(d, 1).b) + fmt.Sprintf("/%d", len(data))
	b, c = rpc(t, addr, prefix+"Read", "secret", msg(bytesField(1, []byte(readName)), intField(2, 4), intField(3, 3)))
	if c != "0" || string(get(b, 10).b) != string(data[4:7]) {
		t.Fatalf("%x %s", b, c)
	}
	bad := msg(bytesField(1, []byte(name)), intField(3, 1), bytesField(10, []byte("wrong")))
	_, c = rpc(t, addr, prefix+"Write", "secret", bad)
	if c != "3" {
		t.Fatal(c)
	}
}
func TestGetTree(t *testing.T) {
	h, addr, _ := testServer(t)
	leaf := bytesField(1, bytesField(1, []byte("file")))
	ld := digest(leaf)
	root := bytesField(2, msg(bytesField(1, []byte("dir")), bytesField(2, ld)))
	rd := digest(root)
	h.Store.put("cas-", ld, leaf)
	h.Store.put("cas-", rd, root)
	// Direct streaming handler invocation is covered by real HTTP/2 requests too;
	// rpc reads the first frame, then drain the rest so trailers become available.
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: p}
	defer tr.CloseIdleConnections()
	body := bytesField(2, rd)
	fr := make([]byte, 5)
	binary.BigEndian.PutUint32(fr[1:], uint32(len(body)))
	fr = append(fr, body...)
	r, _ := http.NewRequest("POST", "http://"+addr+"/build.bazel.remote.execution.v2.ContentAddressableStorage/GetTree", bytes.NewReader(fr))
	r.Header.Set("X-Tun-Token", "secret")
	r.Header.Set("Content-Type", "application/grpc")
	resp, e := tr.RoundTrip(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	for _, want := range [][]byte{root, leaf} {
		b, e := frameRead(resp.Body)
		if e != nil || !bytes.Equal(get(b, 1).b, want) {
			t.Fatalf("%x %v", b, e)
		}
	}
	var end [1]byte
	resp.Body.Read(end[:])
	if resp.Trailer.Get("grpc-status") != "0" {
		t.Fatal(resp.Trailer)
	}
}

func TestStoreConcurrent(t *testing.T) {
	s, e := Open(t.TempDir(), 512)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				b := []byte(fmt.Sprintf("blob %d %d", i, j))
				d := digest(b)
				if e := s.put("cas-", d, b); e != nil {
					t.Error(e)
				}
				f, e := s.openBlob(d)
				if e == nil {
					got, e := io.ReadAll(f)
					f.Close()
					if e != nil || !bytes.Equal(got, b) {
						t.Errorf("pinned read: %q %v", got, e)
					}
				}
			}
		}(i)
	}
	wg.Wait()
	var total int64
	es, _ := os.ReadDir(s.dir)
	for _, e := range es {
		i, _ := e.Info()
		total += i.Size()
	}
	if total > s.max {
		t.Fatalf("size %d > %d", total, s.max)
	}
}
func TestActionEvictedReferences(t *testing.T) {
	h, addr, _ := testServer(t)
	data := []byte("output")
	d := digest(data)
	action := digest([]byte("action"))
	result := bytesField(2, msg(bytesField(1, []byte("out")), bytesField(2, d)))
	h.Store.put("cas-", d, data)
	h.Store.put("ac-", action, result)
	req := bytesField(2, action)
	path := "/build.bazel.remote.execution.v2.ActionCache/GetActionResult"
	if _, c := rpc(t, addr, path, "secret", req); c != "0" {
		t.Fatal(c)
	}
	os.Remove(filepath.Join(h.Store.dir, "cas-"+string(get(d, 1).b)))
	if _, c := rpc(t, addr, path, "secret", req); c != "5" {
		t.Fatal(c)
	}
}
func TestFrames(t *testing.T) {
	for _, b := range [][]byte{{1, 0, 0, 0, 0}, {0, 255, 255, 255, 255}, {0, 0, 0, 0, 2, 8}, {0, 0, 0, 0, 1, 0}} {
		if _, e := frameRead(bytes.NewReader(b)); e == nil {
			t.Fatalf("accepted %x", b)
		}
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint8(0))
	binary.Write(&buf, binary.BigEndian, uint32(0))
	if _, e := frameRead(&buf); e != nil {
		t.Fatal(e)
	}
}

func TestStoreRestartBudget(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir, 100)
	if e != nil {
		t.Fatal(e)
	}
	unrelated := filepath.Join(dir, "keep-me")
	os.WriteFile(unrelated, bytes.Repeat([]byte("x"), 200), 0600)
	data := []byte("resumable")
	d := digest(data)
	name := "uploads/id/blobs/" + string(get(d, 1).b) + "/9"
	if _, e = s.appendUpload(name, d, 0, data[:3], false); e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir, 100)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.appendUpload(name, d, 3, data[3:], true); e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir, 1)
	if e != nil {
		t.Fatal(e)
	}
	if s.has(d) {
		t.Fatal("startup did not enforce smaller budget")
	}
	if _, e = os.Stat(unrelated); e != nil {
		t.Fatalf("evicted unrelated file: %v", e)
	}
}
