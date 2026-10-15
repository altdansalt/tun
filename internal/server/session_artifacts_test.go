package server

import (
	"archive/tar"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/bep"
)

func TestSessionArtifactsRetained(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), Sandbox: "none", MaxJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	token, err := IssueClientToken(s.cfg.DataDir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	other, err := IssueClientToken(s.cfg.DataDir, "bob")
	if err != nil {
		t.Fatal(err)
	}
	j := &job{Job: api.Job{ID: newID(), SessionID: "session", Client: "alice", State: api.Exited, Runner: "runner"}}
	if err := prepareJobDir(s.cfg.DataDir, j.ID); err != nil {
		t.Fatal(err)
	}
	s.jobs[j.ID] = j
	work := filepath.Join(s.sessionDir(j.SessionID), "sandbox", "work")
	out := filepath.Join(s.sessionDir(j.SessionID), "sandbox", "out")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "base", "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(out, "base", "bin", "hello"), []byte("original output"), 0700)
	os.WriteFile(filepath.Join(out, "test.log"), []byte("test passed"), 0600)
	os.Symlink(filepath.Join(out, "base", "bin"), filepath.Join(work, "bazel-bin"))
	sum := bep.Summary{Targets: []bep.Target{{Label: "//pkg:hello", Outputs: []bep.File{{Path: "bazel-bin/hello"}}}}, Tests: []bep.Test{{Label: "//pkg:test", Attempts: []bep.Attempt{{Run: 1, Shard: 1, Attempt: 1, Logs: []bep.File{{Path: filepath.Join(out, "test.log")}}}}}}}
	if err := s.stageSessionArtifacts(j, out, sum); err != nil {
		t.Fatal(err)
	}
	// Subsequent commands/eviction cannot overwrite old command results.
	os.RemoveAll(s.sessionDir(j.SessionID))
	for _, tc := range []struct{ path, body string }{{"//pkg:hello", "original output"}, {"//pkg:test", "test passed"}, {"//pkg:test.log", "test passed"}, {"bazel-bin/hello", "original output"}} {
		req := httptest.NewRequest("GET", "/v1/jobs/"+j.ID+"/files?path="+tc.path, nil)
		req.Header.Set("X-Tun-Token", token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		tr := tar.NewReader(w.Body)
		if _, err := tr.Next(); err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		if string(b) != tc.body {
			t.Fatalf("%q", b)
		}
	}
	req := httptest.NewRequest("GET", "/v1/jobs/"+j.ID+"/files?path=//pkg:test", nil)
	req.Header.Set("X-Tun-Token", other)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
	// Absolute test log paths outside the output mount must never be staged.
	os.MkdirAll(out, 0700)
	sum = bep.Summary{Tests: []bep.Test{{Label: "//bad:test", Attempts: []bep.Attempt{{Logs: []bep.File{{Path: "/etc/passwd"}}}}}}}
	if err := s.stageSessionArtifacts(j, out, sum); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("escape: %v", err)
	}
}
