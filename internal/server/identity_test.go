package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"github.com/altdansalt/tun/internal/reapi"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
)

func TestClientTokens(t *testing.T) {
	data := t.TempDir()
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, MaxJobs: 1, BazelCommand: "true"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := IssueClientToken(data, "alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := IssueClientToken(data, "bob")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	request := func(token, method, path, body string, want int) []byte {
		t.Helper()
		r, _ := http.NewRequest(method, hs.URL+path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Tun-Client", "alice")
		r.Header.Set("X-Exedev-Source-Vm", "alice")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, resp.StatusCode, want, out)
		}
		return out
	}
	var j api.Job
	json.Unmarshal(request(a, "POST", "/v1/jobs", `{"argv":["true"]}`, 201), &j)
	if j.Client != "alice" {
		t.Fatal(j.Client)
	}
	request(b, "GET", "/v1/jobs/"+j.ID, "", 200)
	request(b, "GET", "/v1/jobs", "", 200)
	for _, suffix := range []string{"stdout", "stderr", "files", "bep", "ports/1234"} {
		request(b, "GET", "/v1/jobs/"+j.ID+"/"+suffix, "", 403)
	}
	request(b, "POST", "/v1/jobs/"+j.ID+"/cancel", "", 403)
	request(s.Token(), "GET", "/v1/jobs/"+j.ID+"/stdout", "", 200)
	var own api.Job
	json.Unmarshal(request(b, "POST", "/v1/jobs", `{"bazel":{"root":"/ws","args":["build"],"session":"same"}}`, 201), &own)
	if own.Client != "bob" {
		t.Fatal(own.Client)
	}
	if got := s.cacheIdentity(func() *http.Request {
		r, _ := http.NewRequest("POST", "http://x", nil)
		r.Header.Set("X-Tun-Token", b)
		r.Header.Set("X-Tun-Client", "alice")
		return r
	}()); got != "bob" {
		t.Fatal(got)
	}
	request("wrong", "GET", "/v1/jobs", "", 401)
}

// Exercise the real HTTP/2 gRPC boundary, including forged identity metadata.
func TestClientTokenActionCache(t *testing.T) {
	data := t.TempDir()
	store, err := reapi.Open(filepath.Join(data, "reapi"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, MaxJobs: 1, Cache: store})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := IssueClientToken(data, "alice")
	b, _ := IssueClientToken(data, "bob")
	hs := httptest.NewUnstartedServer(s.Handler())
	hs.Config.Protocols = reapi.Protocols()
	hs.Start()
	defer hs.Close()
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: p}
	defer tr.CloseIdleConnections()
	// Digest {hash: sha256("action"), size_bytes: 6}.
	sum := sha256.Sum256([]byte("action"))
	hash := hex.EncodeToString(sum[:])
	digest := append([]byte{10, 64}, []byte(hash)...)
	digest = append(digest, 16, 6)
	req := append([]byte{18, byte(len(digest))}, digest...)
	call := func(token, method string, body []byte, bearer bool, want string) {
		t.Helper()
		frame := make([]byte, 5)
		binary.BigEndian.PutUint32(frame[1:], uint32(len(body)))
		frame = append(frame, body...)
		r, _ := http.NewRequest("POST", hs.URL+"/build.bazel.remote.execution.v2.ActionCache/"+method, bytes.NewReader(frame))
		r.Header.Set("Content-Type", "application/grpc")
		r.Header.Set("X-Tun-Client", "alice")
		r.Header.Set("X-Exedev-Source-Vm", "alice")
		if bearer {
			r.Header.Set("Authorization", "Bearer "+token)
		} else {
			r.Header.Set("X-Tun-Token", token)
		}
		resp, err := tr.RoundTrip(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := resp.Trailer.Get("grpc-status"); got != want {
			t.Fatalf("%s: grpc %s want %s (%s)", method, got, want, resp.Trailer.Get("grpc-message"))
		}
	}
	update := append(append([]byte{}, req...), 26, 2, 32, 1) // ActionResult exit_code=1
	call(a, "UpdateActionResult", update, false, "0")
	call(b, "GetActionResult", req, true, "5")
	call(b, "UpdateActionResult", update, true, "0")
	call(a, "GetActionResult", req, false, "0")
	call("wrong", "GetActionResult", req, false, "16")
}
