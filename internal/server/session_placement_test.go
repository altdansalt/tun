package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	request := func(credential string) int {
		req := httptest.NewRequest("POST", "/v1/session-cache/build.bazel.remote.execution.v2.Capabilities/GetCapabilities", bytes.NewReader(nil))
		req.Header.Set("Content-Type", "application/grpc")
		req.Header.Set("X-Tun-Session", "x")
		req.Header.Set("X-Tun-Session-Credential", credential)
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
	if code := request("wrong"); code != 403 {
		t.Fatalf("wrong credential: %d", code)
	}
	if code := request("secret"); code == 403 {
		t.Fatal("valid lease refused")
	}
	s.mu.Lock()
	delete(s.sessions, "x")
	s.mu.Unlock()
	if code := request("secret"); code != 403 {
		t.Fatalf("revoked credential: %d", code)
	}
}

func TestNewSessionPrefersFreeSlot(t *testing.T) {
	s := &Server{cfg: Config{HubOnly: true, MaxSessions: 2, RunnerDeadAfter: time.Minute}, sessions: map[string]*session{}, runners: map[string]*runnerState{}}
	for _, name := range []string{"busy", "idle"} {
		rn := newRunnerState(name)
		rn.slots = 1
		rn.sessionSupport = true
		rn.lastSeen = time.Now()
		rn.memoryAvailable = 2 << 30
		s.runners[name] = rn
	}
	s.runners["busy"].memoryAvailable = 100 << 30
	s.runners["busy"].jobs["running"] = &job{}
	j := &job{Job: api.Job{SessionID: "new"}}
	if h := s.sessionHost(j); h != "idle" {
		t.Fatalf("chose %s", h)
	}
	s.sessions["new"] = &session{Session: api.Session{Host: "busy"}}
	if h := s.sessionHost(j); h != "busy" {
		t.Fatalf("lost affinity: %s", h)
	}
}

func TestRunnerCacheProxyRejectsPathsAndRedirects(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if strings.Contains(req.URL.String(), "SECRET") || req.Header.Get("X-Tun-Session-Credential") != "SECRET" {
			t.Error("credential transport")
		}
		w.Header().Set("Location", "/SECRET")
		w.WriteHeader(307)
	}))
	upstream.Config.Protocols = new(http.Protocols)
	upstream.Config.Protocols.SetUnencryptedHTTP2(true)
	upstream.Start()
	defer upstream.Close()
	r := &Runner{cfg: RunnerConfig{Hub: upstream.URL, DataDir: t.TempDir(), Sandbox: "none", KeepBlobs: time.Hour}}
	if err := r.newSessionHost(); err != nil {
		t.Fatal(err)
	}
	defer r.sessions.Close()
	proxy := r.sessions.sessionProxy(&session{Session: api.Session{ID: "id"}, credential: "SECRET"})
	for _, path := range []string{"//x", "/x/..", "/a/../b", "/x/.", "/svc/%4Dethod"} {
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != 400 || w.Header().Get("Location") != "" {
			t.Fatalf("%s: %d %v", path, w.Code, w.Header())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid path forwarded")
	}
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, httptest.NewRequest("POST", "/service.Name/Method", nil))
	if w.Code != 307 || w.Header().Get("Location") != "" || calls.Load() != 1 {
		t.Fatalf("redirect: %d %v", w.Code, w.Header())
	}
}

func TestHubOnlyDiscardsLocalSessions(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "sessions", "old")
	os.MkdirAll(dir, 0700)
	b, _ := json.Marshal(api.Session{ID: "old", Host: api.LocalRunner})
	os.WriteFile(filepath.Join(dir, "session.json"), b, 0600)
	s, err := New(Config{DataDir: data, HubOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.sessions) != 0 {
		t.Fatal("loaded unavailable local session")
	}
}

func TestRemoteEvictionWaitsForAcknowledgment(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), HubOnly: true, MaxSessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	rn := s.register("r", api.RunnerPoll{Instance: "i", Slots: 1, SessionSupport: true, MemoryAvailable: 4 << 30}, "", time.Now())
	old := &session{Session: api.Session{ID: "old", Host: "r", Commands: 1}, runner: rn}
	s.sessions["old"] = old
	j := &job{Job: api.Job{SessionID: "new"}, bz: &api.BazelJob{}}
	if s.claimSessionOn(j, "r") || !old.stopping {
		t.Fatal("did not initiate eviction")
	}
	other := s.register("other", api.RunnerPoll{Instance: "other", Slots: 1, SessionSupport: true, MemoryAvailable: 100 << 30}, "", time.Now())
	if host := s.sessionHost(j); host != rn.name {
		t.Fatalf("eviction switched to %s", host)
	}
	delete(s.runners, other.name)

	s.register("r", api.RunnerPoll{Instance: "i", Slots: 1, Sessions: []string{"old"}}, "", time.Now())
	if s.claimSessionOn(j, "r") {
		t.Fatal("overlapped JVM leases")
	}
	s.register("r", api.RunnerPoll{Instance: "i", Slots: 1}, "", time.Now())
	if !s.claimSessionOn(j, "r") {
		t.Fatal("did not release capacity")
	}
}

func TestSessionBatchFetch(t *testing.T) {
	hub, err := New(Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	var calls atomic.Int64
	gate := make(chan struct{})
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 3 {
			close(gate)
		}
		select {
		case <-gate:
		case <-time.After(3 * time.Second):
			t.Error("batch requests were serialized")
		}
		hub.Handler().ServeHTTP(w, r)
	}))
	defer h.Close()
	r := &Runner{cfg: RunnerConfig{HTTP: http.DefaultClient, Hub: h.URL, Token: hub.token, DataDir: t.TempDir(), Sandbox: "none", KeepBlobs: time.Hour}}
	if err := r.newSessionHost(); err != nil {
		t.Fatal(err)
	}
	defer r.sessions.Close()
	b := &api.BazelJob{}
	for i := range 600 {
		data := []byte(fmt.Sprintf("file %d", i))
		sha := fmt.Sprintf("%x", sha256.Sum256(data))
		os.WriteFile(hub.casPath(sha), data, 0600)
		b.Manifest = append(b.Manifest, api.ManifestEntry{Type: api.EntryFile, Path: fmt.Sprint(i), Sha: sha, Size: int64(len(data)), Mode: 0644})
	}
	if err := r.fetchSessionBlobs(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("600 blobs took %d requests, want 3", n)
	}
	if missing := r.sessions.missingBlobs(b.Manifest); len(missing) != 0 {
		t.Fatalf("%d missing", len(missing))
	}
}

func TestLostSessionHostSchedulesQueuedCommand(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(fmt.Sprint(retire), func(t *testing.T) {
			s, err := New(Config{DataDir: t.TempDir(), Sandbox: "none", BazelCommand: "true", MaxJobs: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id := sessionID("owner", "/ws", DefaultSession)
			s.mu.Lock()
			rn := s.register("r", api.RunnerPoll{Instance: "i", Slots: 1, SessionSupport: true}, "", time.Now())
			s.sessions[id] = &session{Session: api.Session{ID: id, Host: "r", Client: "owner"}, runner: rn}
			s.mu.Unlock()
			body, _ := json.Marshal(api.CreateJob{Bazel: &api.BazelJob{Root: "/ws", Args: []string{"build"}}, TimeoutSec: 10})
			req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader(body))
			req.Header.Set("X-Tun-Token", s.Token())
			req.Header.Set("X-Tun-Client", "owner")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			var v api.Job
			if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != 201 {
				t.Fatalf("submit %d %s", w.Code, w.Body.String())
			}
			s.mu.Lock()
			j := s.jobs[v.ID]
			if len(s.queue) != 1 || j.Runner != "" {
				s.mu.Unlock()
				t.Fatal("command was not queued before loss")
			}
			if retire {
				s.retire(rn, time.Now())
			} else {
				s.dropRunner(rn, "lost")
			}
			host := j.Runner
			s.mu.Unlock()
			if host != api.LocalRunner {
				t.Fatalf("stranded command: host=%q", host)
			}
			select {
			case <-j.done:
			case <-time.After(10 * time.Second):
				t.Fatal("command did not finish")
			}
		})
	}
}

func TestRunnerLeaseWaitCancellationAndCleanup(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) }))
	defer hub.Close()
	for _, state := range []string{api.Canceled, api.Timeout} {
		t.Run(state, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &Runner{ctx: ctx, cfg: RunnerConfig{HTTP: http.DefaultClient, Hub: hub.URL, DataDir: t.TempDir(), Sandbox: "none", KeepBlobs: time.Hour}}
			if err := r.newSessionHost(); err != nil {
				t.Fatal(err)
			}
			defer r.sessions.Close()
			r.sessions.mu.Lock()
			r.sessions.sessions["old"] = &session{Session: api.Session{ID: "old"}, stopping: true}
			r.sessions.mu.Unlock()
			// The old sandbox deliberately never acknowledges stopping.
			rj := &runnerJob{spec: api.RunnerJob{ID: "wait", TimeoutSec: 1, Bazel: &api.RunnerBazel{Session: api.Session{ID: "old"}}}}
			if state == api.Canceled {
				rj.killState = state
			}
			start := time.Now()
			result, _ := r.executeBazel(rj)
			if result.State != state {
				t.Fatalf("result %+v", result)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("lease wait ignored cancellation/deadline")
			}
			r.sessions.mu.Lock()
			n := len(r.sessions.jobs)
			r.sessions.mu.Unlock()
			if n != 0 {
				t.Fatal("leaked inner jobs")
			}
			if _, err := os.Stat(r.sessions.jobDir("wait")); !os.IsNotExist(err) {
				t.Fatalf("leaked job dir: %v", err)
			}
		})
	}
}

func TestRunnerSessionStorageRetention(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{Hub: "http://unused", DataDir: t.TempDir(), Sandbox: "none", KeepBlobs: time.Hour}}
	if err := r.newSessionHost(); err != nil {
		t.Fatal(err)
	}
	defer r.sessions.Close()
	s := r.sessions
	if s.cfg.KeepJobs == 0 || s.cfg.KeepBlobs != time.Hour {
		t.Fatal("retention not passed to engine")
	}
	old := time.Now().Add(-3 * time.Hour)
	paths := []string{s.casPath(strings.Repeat("a", 64)), s.casPath(".fetch-abandoned")}
	for _, p := range paths {
		os.WriteFile(p, []byte("x"), 0600)
		os.Chtimes(p, old, old)
	}
	orphan := s.jobDir("orphan")
	os.MkdirAll(orphan, 0700)
	os.Chtimes(orphan, old, old)
	s.sweep(time.Now())
	for _, p := range append(paths, orphan) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("retained %s: %v", p, err)
		}
	}
}

func TestDrainingRunnerReleasesIdleLease(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), HubOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	rn := s.register("r", api.RunnerPoll{Instance: "i", Slots: 1}, "", time.Now())
	s.sessions["idle"] = &session{Session: api.Session{ID: "idle", Host: "r"}, runner: rn}
	s.sessions["busy"] = &session{Session: api.Session{ID: "busy", Host: "r"}, runner: rn, job: &job{}}
	s.register("r", api.RunnerPoll{Instance: "i", Slots: 0, Sessions: []string{"idle", "busy"}}, "", time.Now())
	if s.sessions["idle"] != nil || s.sessions["busy"] == nil {
		t.Fatal("drain must release idle leases and preserve running work")
	}
	if !s.runnerNeedsRevocation(rn, []string{"idle", "busy"}) {
		t.Fatal("drain revocation would long-poll")
	}
}

func TestSessionResourceSamplesBoundedByTime(t *testing.T) {
	old := diskStat
	defer func() { diskStat = old }()
	calls := 0
	diskStat = func(string) (int64, int64, error) { calls++; return 1000, 2000, nil }
	s := &Server{cfg: Config{MaxJobs: 1, MaxSessions: 2}, sessions: map[string]*session{}}
	for range 100 {
		s.sessionHost(&job{Job: api.Job{SessionID: "new"}})
		s.sessionCapacity(api.LocalRunner)
	}
	if calls != 1 {
		t.Fatalf("sampled disk %d times for 100 queued jobs", calls)
	}
	s.resourceSample = time.Now().Add(-2 * time.Second)
	s.sessionCapacity(api.LocalRunner)
	if calls != 2 {
		t.Fatal("sample did not refresh")
	}
}

func TestRunnerReclaimsSessionCASUnderMinFree(t *testing.T) {
	data := t.TempDir()
	cas := filepath.Join(data, "session-host", "cas")
	os.MkdirAll(cas, 0700)
	blob := filepath.Join(cas, strings.Repeat("a", 64))
	os.WriteFile(blob, []byte("contents"), 0600)
	old := diskStat
	defer func() { diskStat = old }()
	diskStat = func(string) (int64, int64, error) {
		if _, err := os.Stat(blob); err == nil {
			return 1, 100, nil
		}
		return 100, 100, nil
	}
	r := &Runner{cfg: RunnerConfig{DataDir: data, MinFree: MinFree{Bytes: 50}, Logf: func(string, ...any) {}}}
	r.reclaimBlobs()
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatalf("CAS retained under min-free: %v", err)
	}
}

func TestFailedFirstRemoteCommandReleasesReservation(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), HubOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	rn := s.register("r", api.RunnerPoll{Instance: "i", Slots: 1}, "", time.Now())
	j := &job{Job: api.Job{ID: "failed", SessionID: "new", Runner: "r", State: api.Running}, rn: rn, bz: &api.BazelJob{}, done: make(chan struct{})}
	if err := prepareJobDir(s.cfg.DataDir, j.ID); err != nil {
		t.Fatal(err)
	}
	s.jobs[j.ID] = j
	rn.jobs[j.ID] = j
	sess := &session{Session: api.Session{ID: "new", Host: "r"}, runner: rn, job: j}
	s.sessions["new"] = sess
	s.finishRunnerJob(j, api.RunnerDone{State: api.Failed, Error: "failed before sandbox startup", Session: &api.Session{ID: "new"}}, "")
	if !sess.stopping {
		t.Fatal("failed startup kept a memory reservation")
	}
	s.register("r", api.RunnerPoll{Instance: "i", Slots: 1}, "", time.Now())
	if s.hostSessions("r") != 0 {
		t.Fatal("failed startup reservation survived acknowledgment")
	}
}
