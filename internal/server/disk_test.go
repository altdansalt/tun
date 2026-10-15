package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
)

func TestParseMinFree(t *testing.T) {
	for in, want := range map[string]MinFree{"0": {}, "5GB": {Bytes: 5 << 30}, "15%": {Percent: 15}, "2.5%": {Percent: 2.5}} {
		if got, err := ParseMinFree(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"x", "100%", "-1%", "5XB"} {
		if _, err := ParseMinFree(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// fakeDisk makes every job sandbox dir and session dir under data cost 100 of base bytes.
func fakeDisk(t *testing.T, data string, base *atomic.Int64) {
	old := diskStat
	t.Cleanup(func() { diskStat = old })
	diskStat = func(string) (int64, int64, error) {
		n := 0
		jobs, _ := os.ReadDir(filepath.Join(data, "jobs"))
		for _, j := range jobs {
			if _, err := os.Stat(filepath.Join(data, "jobs", j.Name(), "sandbox")); err == nil {
				n++
			}
		}
		sessions, _ := os.ReadDir(filepath.Join(data, "sessions"))
		for _, s := range sessions {
			if !strings.HasPrefix(s.Name(), ".trash") {
				n++
			}
		}
		return base.Load() - 100*int64(n), 1000, nil
	}
}

// When disk space is low, finished jobs' work dirs go first (oldest first), then idle sessions.
func TestReclaimDisk(t *testing.T) {
	data := t.TempDir()
	var base atomic.Int64
	base.Store(1 << 40)
	fakeDisk(t, data, &base)
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, BazelCommand: "true", MaxJobs: 1, MinFree: MinFree{Bytes: 700}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	do := func(method, path string, body any, out any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, hs.URL+path, bytes.NewReader(b))
		req.Header.Set("X-Tun-Token", s.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
		}
		json.NewDecoder(resp.Body).Decode(out)
	}
	run := func(req api.CreateJob) string {
		t.Helper()
		var j api.Job
		do("POST", "/v1/jobs", req, &j)
		for deadline := time.Now().Add(20 * time.Second); !api.Terminal(j.State); do("GET", "/v1/jobs/"+j.ID, nil, &j) {
			if time.Now().After(deadline) {
				t.Fatalf("job stuck: %+v", j)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return j.ID
	}
	var ids []string
	for range 3 {
		ids = append(ids, run(api.CreateJob{Argv: []string{"true"}}))
	}
	ids = append(ids, run(api.CreateJob{Bazel: &api.BazelJob{Root: "/ws", Args: []string{"build"},
		Manifest: []api.ManifestEntry{{Path: "d", Type: api.EntryDir, Mode: 0o755}}}}))
	keep := run(api.CreateJob{Argv: []string{"true"}}) // the newest finished job

	// 5 job dirs + 1 session = 400 free; the next job's dir makes it 300. Keeping 700 free
	// takes the 4 oldest work dirs, not the newest job's nor the session.
	base.Store(1000)
	run(api.CreateJob{Argv: []string{"true"}})
	for _, id := range ids {
		var j api.Job
		do("GET", "/v1/jobs/"+id, nil, &j)
		if !j.WorkDeleted {
			t.Errorf("job %s: work dir kept", id)
		}
	}
	var j api.Job
	if do("GET", "/v1/jobs/"+keep, nil, &j); j.WorkDeleted {
		t.Errorf("newest job's work dir deleted too early")
	}
	var h api.Health
	do("GET", "/v1/healthz", nil, &h)
	if h.Sessions != 1 || h.Disk == nil || h.Disk.ReclaimedWorkDirs != 4 || h.Disk.ReclaimedSessions != 0 || h.Disk.Low || h.Disk.MinFreeBytes != 700 {
		t.Fatalf("healthz: %+v %+v", h, h.Disk)
	}
	// 4 dirs (keep, the last job, the next one, the session) = 450 free: both finished work
	// dirs go, then the idle session.
	base.Store(850)
	run(api.CreateJob{Argv: []string{"true"}})
	do("GET", "/v1/healthz", nil, &h)
	if h.Sessions != 0 || h.Disk.ReclaimedWorkDirs != 6 || h.Disk.ReclaimedSessions != 1 || h.Disk.Low {
		t.Fatalf("healthz: %+v %+v", h, h.Disk)
	}
}

func TestRunnerReclaimBlobs(t *testing.T) {
	data := t.TempDir()
	r, err := NewRunner(RunnerConfig{Hub: "http://127.0.0.1:1", Name: "r", Slots: 1, DataDir: data, Sandbox: sandbox.None, MinFree: MinFree{Bytes: 800}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.lock.Close()
	defer r.Kill()
	// This test replaces the process-wide disk sampler below. Stop the inner
	// engine's background retention worker before installing that fixture.
	r.sessions.Close()
	blobs := filepath.Join(data, "blobs")
	for i, name := range []string{"old", "mid", "new"} {
		p := filepath.Join(blobs, name)
		os.WriteFile(p, nil, 0o600)
		at := time.Now().Add(time.Duration(i-3) * time.Hour)
		os.Chtimes(p, at, at)
	}
	old := diskStat
	defer func() { diskStat = old }()
	diskStat = func(string) (int64, int64, error) {
		ents, _ := os.ReadDir(blobs)
		return 1000 - 100*int64(len(ents)), 1000, nil
	}
	r.reclaimBlobs()
	ents, _ := os.ReadDir(blobs)
	if len(ents) != 2 || ents[0].Name() != "mid" || ents[1].Name() != "new" || r.reclaimed.Load() != 1 {
		t.Fatalf("left %v", ents)
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n += len(p); f.n > 10 {
		return 0, syscall.ENOSPC
	}
	return len(p), nil
}

// A log that can't be written (full disk) is drained anyway and the error reported: the job
// must neither block on its output nor end without a word about what was lost.
func TestCopyCappedWriteError(t *testing.T) {
	for _, max := range []int64{0, 1 << 20} {
		src := strings.NewReader(strings.Repeat("x", 100000))
		if err := copyCapped(&failWriter{}, src, max, "stdout"); err != syscall.ENOSPC || src.Len() != 0 {
			t.Errorf("max %d: err %v, %d bytes left unread", max, err, src.Len())
		}
	}
}
