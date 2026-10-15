package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
)

// A finished bazel job must not keep its manifest (all of a workspace's paths and digests)
// in memory for as long as the job is kept (--keep-jobs).
func TestBazelJobDropsManifest(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), Sandbox: sandbox.None, BazelCommand: "true", MaxJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	create := func(args ...string) string {
		t.Helper()
		b, _ := json.Marshal(api.CreateJob{Bazel: &api.BazelJob{Root: "/ws", Args: args,
			Manifest: []api.ManifestEntry{{Path: "d", Type: api.EntryDir, Mode: 0o755}}}})
		req, _ := http.NewRequest("POST", hs.URL+"/v1/jobs", bytes.NewReader(b))
		req.Header.Set("X-Tun-Token", s.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var j api.Job
		if err := json.NewDecoder(resp.Body).Decode(&j); err != nil || resp.StatusCode != 201 {
			t.Fatalf("create: %d %v", resp.StatusCode, err)
		}
		return j.ID
	}
	ran := create("build")
	queued := create("build") // same session: queued behind the first, canceled there
	s.mu.Lock()
	j := s.jobs[queued]
	if j.State == api.Queued {
		for i, q := range s.queue {
			if q == j {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				s.finish(j, api.Canceled, nil, nil, "canceled while queued")
				break
			}
		}
	}
	s.mu.Unlock()
	for _, id := range []string{ran, queued} {
		s.mu.Lock()
		j := s.jobs[id]
		s.mu.Unlock()
		select {
		case <-j.done:
		case <-time.After(20 * time.Second):
			t.Fatalf("job %s did not end", id)
		}
		s.mu.Lock()
		m := j.bz.Manifest
		s.mu.Unlock()
		if m != nil {
			t.Errorf("job %s (%s) still holds its manifest", id, j.State)
		}
	}
}

// TestBazelrc: what tun.bazelrc says with no remote cache, with one, read-only, and with
// executors (remote execution through the same socket, --jobs from their slots).
func TestBazelrc(t *testing.T) {
	s := &Server{cfg: Config{DiskCacheMax: "5G"}}
	in := sessionPaths{repo: "/tun/cache/repo", disk: "/tun/cache/disk"}
	local := s.bazelrc("default", in, remoteSetup{})
	if !strings.Contains(local, "common --repository_cache=/tun/cache/repo\ncommon --disk_cache=/tun/cache/disk\n") || strings.Contains(local, "--remote") {
		t.Fatalf("no remote cache:\n%s", local)
	}
	cache := s.bazelrc("default", in, remoteSetup{socket: "/tun/sock/cache.sock"})
	if !strings.Contains(cache, "common --remote_cache=grpc://tun\ncommon --remote_proxy=unix:/tun/sock/cache.sock\n") ||
		strings.Contains(cache, "--disk_cache") || strings.Contains(cache, "--remote_executor") || strings.Contains(cache, "--jobs") || strings.Contains(cache, "--noremote_upload_local_results") {
		t.Fatalf("remote cache:\n%s", cache)
	}
	if ro := s.bazelrc("default", in, remoteSetup{socket: "/s", readOnly: true}); !strings.Contains(ro, "common --noremote_upload_local_results\n") {
		t.Fatalf("read-only:\n%s", ro)
	}
	exec := s.bazelrc("default", in, remoteSetup{socket: "/s", executors: 12})
	if !strings.Contains(exec, "common --remote_executor=grpc://tun\ncommon --jobs=12\ncommon --remote_local_fallback\n") {
		t.Fatalf("executors:\n%s", exec)
	}
	if note := s.bazelrc("default", in, remoteSetup{note: "no remote cache: why"}); !strings.Contains(note, "# no remote cache: why\n") || strings.Contains(note, "--remote") {
		t.Fatalf("note:\n%s", note)
	}
}

// Session ids, Bazel caches and action cache namespaces are keyed by identityOf: a
// self-reported name never equals a proxy-attested (or address) one.
func TestIdentityOf(t *testing.T) {
	req := func(h map[string]string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "10.0.0.1:5000"
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	for _, c := range []struct {
		h    map[string]string
		want string
	}{
		{map[string]string{"X-Exedev-Source-Vm": "box", "X-Tun-Client": "other"}, "vm:box"},
		{map[string]string{"X-Tun-Client": "box"}, "self:box"},
		{map[string]string{"X-Tun-Client": "vm:box"}, "self:vm:box"},
		{nil, "addr:10.0.0.1"},
	} {
		if got := identityOf(req(c.h)); got != c.want {
			t.Errorf("%v: %q, want %q", c.h, got, c.want)
		}
	}
	if sessionID("vm:box", "/r", "default") == sessionID("self:box", "/r", "default") {
		t.Error("same session id for vm:box and self:box")
	}
}
