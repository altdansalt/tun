package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/reapi"
)

func TestSessionPlacementResources(t *testing.T) {
	s := &Server{cfg: Config{HubOnly: true, MaxSessions: 2, RunnerDeadAfter: time.Minute}, sessions: map[string]*session{}, runners: map[string]*runnerState{}}
	add := func(name string, mem int64, load float64) {
		r := newRunnerState(name)
		r.slots = 2
		r.sessionSupport = true
		r.lastSeen = time.Now()
		r.memoryAvailable = mem
		r.load = load
		s.runners[name] = r
	}
	add("a", 3<<30, 0)
	add("b", 2<<30, 3)
	j := &job{Job: api.Job{SessionID: "new"}}
	if h := s.sessionHost(j); h != "a" {
		t.Fatalf("memory/load: %s", h)
	}
	s.runners["a"].disk = &api.Disk{FreeBytes: 1, MinFreeBytes: 2}
	if h := s.sessionHost(j); h != "b" {
		t.Fatalf("disk: %s", h)
	}
	s.runners["a"].disk = nil
	s.sessions[j.SessionID] = &session{Session: api.Session{Host: "b", ID: j.SessionID}}
	if h := s.sessionHost(j); h != "b" {
		t.Fatalf("affinity: %s", h)
	}
	delete(s.sessions, j.SessionID)
	s.runners["a"].lastSeen = time.Now().Add(-time.Hour)
	if h := s.sessionHost(j); h != "b" {
		t.Fatalf("dead host: %s", h)
	}
	if n := s.sessionCapacity("b"); n != 1 {
		t.Fatalf("2GiB fits one new JVM: %d", n)
	}
	s.runners["b"].memoryAvailable = 4 << 30
	if n := s.sessionCapacity("b"); n != 2 {
		t.Fatalf("max sessions: %d", n)
	}
}

func TestRunnerSessionCredentialScope(t *testing.T) {
	store, err := reapi.Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{DataDir: t.TempDir(), Cache: store})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rn := newRunnerState("r")
	s.mu.Lock()
	s.sessions["x"] = &session{Session: api.Session{ID: "x", Host: "r", Client: "owner"}, runner: rn, credential: "secret"}
	s.mu.Unlock()
	request := func(path string) int {
		req := httptest.NewRequest("POST", path, bytes.NewReader(nil))
		req.Header.Set("Content-Type", "application/grpc")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	// The credential is not an API token.
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("X-Tun-Token", "secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("credential authorized API: %d", w.Code)
	}
	if code := request("/v1/session-cache/wrong/build.bazel.remote.execution.v2.Capabilities/GetCapabilities"); code != 403 {
		t.Fatalf("wrong credential: %d", code)
	}
	if code := request("/v1/session-cache/secret/build.bazel.remote.execution.v2.Capabilities/GetCapabilities"); code == 403 {
		t.Fatal("valid lease refused")
	}
	s.mu.Lock()
	delete(s.sessions, "x")
	s.mu.Unlock()
	if code := request("/v1/session-cache/secret/build.bazel.remote.execution.v2.Capabilities/GetCapabilities"); code != 403 {
		t.Fatalf("revoked credential: %d", code)
	}
}
