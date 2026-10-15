package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
