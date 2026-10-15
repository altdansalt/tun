package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/reapi"
)

func TestRunnerCredentialBoundary(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{DataDir: dir, HubOnly: true, Sandbox: "none"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := s.Handler()
	call := func(path, token, instance string, body []byte) int {
		r := httptest.NewRequest("POST", path, bytes.NewReader(body))
		r.Header.Set("X-Tun-Token", token)
		r.Header.Set(api.HeaderRunnerInstance, instance)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, path := range []string{"/v1/runner/poll", "/v1/runner/cas", "/v1/runner/jobs/id/done", "/v1/runner/jobs/id/started", "/v1/runner/leave"} {
		if c := call(path, s.Token(), "fake", []byte("{}")); c != 401 {
			t.Fatalf("%s: %d", path, c)
		}
	}
	if c := call("/v1/jobs", s.RunnerToken(), "x", []byte("{}")); c != 401 {
		t.Fatal(c)
	}
	if c := call("/v1/runner/poll", s.RunnerToken(), "", []byte("{}")); c != 400 {
		t.Fatal(c)
	}
	if s.Token() == s.RunnerToken() {
		t.Fatal("shared credential")
	}
	info, err := os.Stat(filepath.Join(dir, "runner-token"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	// Runner instance is mandatory even if the assignment's name matches.
	j := &job{Job: api.Job{ID: "test", State: api.Queued}, rn: &runnerState{name: "runner", instance: "real"}}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
	req := httptest.NewRequest("POST", "/v1/runner/jobs/test/started", nil)
	req.SetPathValue("id", "test")
	req.Header.Set(api.HeaderRunner, "runner")
	w := httptest.NewRecorder()
	s.mu.Lock()
	got := s.runnerJob(w, req, false)
	delete(s.jobs, j.ID)
	s.mu.Unlock()
	if got != nil || w.Code != 410 {
		t.Fatal(w.Code, got)
	}
}

func TestLargeRunnerDoneAndCASLimit(t *testing.T) {
	body, _ := json.Marshal(api.RunnerDone{State: api.Exited, ActionResult: make([]byte, 5<<20)})
	w := httptest.NewRecorder()
	d, ok := decodeDone(w, httptest.NewRequest("POST", "/", bytes.NewReader(body)))
	if !ok || len(d.ActionResult) != 5<<20 {
		t.Fatal(w.Code, len(d.ActionResult))
	}
	s, err := New(Config{DataDir: t.TempDir(), HubOnly: true, Sandbox: "none"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.actionStore, err = reapi.Open(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	// Digest field 2 is the declared size; an oversized read must fail before disk I/O.
	body, _ = json.Marshal(casTransfer{Digest: []byte{0x10, 0x81, 0x80, 0x80, 0x30}})
	w = httptest.NewRecorder()
	s.runnerCAS(w, httptest.NewRequest("POST", "/", bytes.NewReader(body)))
	if w.Code != 413 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestActionCancellationAndNoJobRecords(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), HubOnly: true, Sandbox: "none", RunnerPollWait: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.executeAction(ctx, []byte{1}, nil); done <- err }()
	deadline := time.Now().Add(time.Second)
	var j *job
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, v := range s.jobs {
			j = v
		}
		s.mu.Unlock()
		if j != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if j == nil {
		t.Fatal("not queued")
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	s.mu.Lock()
	kill := j.killReq
	s.mu.Unlock()
	if kill != api.Canceled {
		t.Fatal(kill)
	}
	entries, err := os.ReadDir(filepath.Join(s.cfg.DataDir, "jobs"))
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	// Finish the canceled queue item so shutdown has nothing to wait for.
	s.mu.Lock()
	s.queue = nil
	s.finish(j, api.Canceled, nil, nil, "canceled")
	s.mu.Unlock()
}
