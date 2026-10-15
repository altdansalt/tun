package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/workspace"
)

// The reviewed attack: bob lists alice's job, reuses its workspace sha and cats her secret.
// Content (workspace blobs and tun bazel's CAS) is usable only by clients that uploaded it.
func TestContentOwnership(t *testing.T) {
	data := t.TempDir()
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, MaxJobs: 1, BazelCommand: "true", KeepBlobs: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	alice, _ := IssueClientToken(data, "alice")
	bob, _ := IssueClientToken(data, "bob")
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	request := func(token, method, path string, body []byte, hdr map[string]string) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, hs.URL+path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	expect := func(want int) func(int, []byte) []byte {
		return func(got int, out []byte) []byte {
			t.Helper()
			if got != want {
				t.Fatalf("status %d want %d: %s", got, want, out)
			}
			return out
		}
	}
	wait := func(token, id string) api.Job {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			var j api.Job
			json.Unmarshal(expect(200)(request(token, "GET", "/v1/jobs/"+id, nil, nil)), &j)
			if api.Terminal(j.State) {
				return j
			}
			if time.Now().After(deadline) {
				t.Fatal("job did not finish")
			}
		}
	}

	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "secret.txt"), []byte("hunter2"), 0o600)
	var blob bytes.Buffer
	sha, _, err := workspace.Pack(ws, []string{"secret.txt"}, &blob)
	if err != nil {
		t.Fatal(err)
	}
	expect(404)(request(alice, "HEAD", "/v1/blobs/"+sha, nil, nil))
	expect(201)(request(alice, "PUT", "/v1/blobs/"+sha, blob.Bytes(), nil))
	expect(200)(request(alice, "HEAD", "/v1/blobs/"+sha, nil, nil))
	var aj api.Job
	json.Unmarshal(expect(201)(request(alice, "POST", "/v1/jobs", []byte(`{"argv":["cat","secret.txt"],"workspace":"`+sha+`"}`), nil)), &aj)
	if j := wait(alice, aj.ID); j.State != api.Exited || j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatal("alice's own job failed")
	}

	// Bob sees the job, not its command or workspace; the blob doesn't exist for him.
	var list []api.Job
	json.Unmarshal(expect(200)(request(bob, "GET", "/v1/jobs", nil, nil)), &list)
	var seen api.Job
	json.Unmarshal(expect(200)(request(bob, "GET", "/v1/jobs/"+aj.ID, nil, nil)), &seen)
	for _, v := range append(list, seen) {
		if v.ID == aj.ID && (v.Workspace != "" || v.Argv != nil || v.Script != "") {
			t.Fatalf("job details leaked to another client: %+v", v)
		}
	}
	var own api.Job
	json.Unmarshal(expect(200)(request(alice, "GET", "/v1/jobs/"+aj.ID, nil, nil)), &own)
	if own.Workspace != sha || len(own.Argv) != 2 {
		t.Fatalf("owner's view redacted: %+v", own)
	}
	expect(404)(request(bob, "HEAD", "/v1/blobs/"+sha, nil, nil))
	code, out := request(bob, "POST", "/v1/jobs", []byte(`{"argv":["cat","secret.txt"],"workspace":"`+sha+`"}`), nil)
	if code != 400 || !strings.Contains(string(out), "not uploaded") {
		t.Fatalf("bob used alice's workspace: %d %s", code, out)
	}
	expect(403)(request(bob, "GET", "/v1/blobs/"+sha, nil, nil))
	expect(200)(request(s.Token(), "HEAD", "/v1/blobs/"+sha, nil, nil)) // admin: any blob

	// Uploading the content proves possession: then it is bob's too (stored once).
	expect(201)(request(bob, "PUT", "/v1/blobs/"+sha, blob.Bytes(), nil))
	expect(200)(request(bob, "HEAD", "/v1/blobs/"+sha, nil, nil))
	var bj api.Job
	json.Unmarshal(expect(201)(request(bob, "POST", "/v1/jobs", []byte(`{"argv":["true"],"workspace":"`+sha+`"}`), nil)), &bj)
	wait(bob, bj.ID)
	expect(200)(request(alice, "HEAD", "/v1/blobs/"+sha, nil, nil))

	// tun bazel's CAS: bob can't reference (or probe) content by a digest he didn't upload.
	content := []byte("API_KEY=short")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	cas := func(token string) {
		t.Helper()
		var tb bytes.Buffer
		zw := gzip.NewWriter(&tb)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(&tar.Header{Name: digest, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(content))})
		tw.Write(content)
		tw.Close()
		zw.Close()
		expect(200)(request(token, "POST", "/v1/cas", tb.Bytes(), map[string]string{"Content-Encoding": "gzip"}))
	}
	bazelJob := []byte(fmt.Sprintf(`{"bazel":{"root":"/ws","args":["build"],"manifest":[{"p":".env","t":"f","m":420,"s":%q,"n":%d}]}}`, digest, len(content)))
	expect(409)(request(alice, "POST", "/v1/jobs", bazelJob, nil))
	cas(alice)
	var bz api.Job
	json.Unmarshal(expect(201)(request(alice, "POST", "/v1/jobs", bazelJob, nil)), &bz)
	wait(alice, bz.ID)
	var missing api.MissingBlobs
	json.Unmarshal(expect(409)(request(bob, "POST", "/v1/jobs", bazelJob, nil)), &missing)
	if len(missing.Missing) != 1 || missing.Missing[0] != digest {
		t.Fatalf("bob's manifest used alice's CAS content: %+v", missing)
	}
	expect(201)(request(s.Token(), "POST", "/v1/jobs", bazelJob, nil))
	cas(bob)
	expect(201)(request(bob, "POST", "/v1/jobs", bazelJob, nil))

	// Retention deletes ownership links with the content, so they never pin deleted data.
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(s.blobPath(sha), old, old)
	os.Chtimes(s.casPath(digest), old, old)
	s.sweepBlobs(time.Now(), nil)
	s.sweepCAS(time.Now())
	for _, p := range []string{filepath.Join(s.ownerDir("bob", "blobs"), sha), filepath.Join(s.ownerDir("alice", "cas"), digest)} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("ownership link %s outlived its content: %v", p, err)
		}
	}
	expect(404)(request(alice, "HEAD", "/v1/blobs/"+sha, nil, nil))
}
