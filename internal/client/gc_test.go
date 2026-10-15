package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/server"
)

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// eventually polls cond for up to 20s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !cond(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestRetentionWorkDirs(t *testing.T) {
	ts := startServer(t, server.Config{KeepWork: 300 * time.Millisecond, GCInterval: 50 * time.Millisecond})
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "in.txt"), []byte("hello"), 0o644)
	// Runs well past KeepWork: a running job's dirs must not be swept. The read-only trees
	// (including the sandbox root itself) check the sweep can still delete what jobs leave.
	r := ts.tun(ws, "run", "-c", `mkdir -p ro/x $HOME/h; touch ro/x/f; chmod 500 ro/x ro
		sleep 1; cat in.txt; echo oops >&2; chmod 500 $HOME/..`)
	if r.code != 0 || r.stdout != "hello" {
		t.Fatalf("job: %+v", r)
	}
	id := strings.Fields(r.stderr)[2]
	dir := filepath.Join(ts.data, "jobs", id)
	j := ts.waitFor(id, func(j api.Job) bool { return j.WorkDeleted })
	if j.State != api.Exited || j.ExitCode == nil || *j.ExitCode != 0 {
		t.Errorf("status after sweep: %+v", j)
	}
	if exists(filepath.Join(dir, "sandbox")) {
		t.Error("sandbox/ not deleted")
	}
	if r := ts.tun("", "logs", id); r.code != 0 || r.stdout != "hello" {
		t.Errorf("logs after sweep: %+v", r)
	}
	if r := ts.tun("", "logs", "-f", id, "--stderr"); r.code != 0 || r.stdout != "oops\n" {
		t.Errorf("logs --stderr after sweep: %+v", r)
	}
	if r := ts.tun("", "status", id); !strings.Contains(r.stdout, `"work_deleted":true`) {
		t.Errorf("status: %+v", r)
	}

	// Startup sweep: a server restarted with retention enabled cleans up at once, and the
	// flag survives restarts.
	data := ts.data
	ts.stop()
	ts2 := startServer(t, server.Config{DataDir: data})
	old := ts2.detach("", "--", "true")
	ts2.waitDone(old)
	time.Sleep(400 * time.Millisecond)
	ts2.stop()
	if !exists(filepath.Join(data, "jobs", old, "sandbox")) {
		t.Fatal("work dir deleted without retention configured")
	}
	ts3 := startServer(t, server.Config{DataDir: data, KeepWork: 300 * time.Millisecond, GCInterval: time.Hour})
	eventually(t, "startup sweep", func() bool { return !exists(filepath.Join(data, "jobs", old, "sandbox")) })
	if j := ts3.waitFor(old, func(j api.Job) bool { return j.WorkDeleted }); j.State != api.Exited {
		t.Errorf("after startup sweep: %+v", j)
	}
	if j := ts3.status(id); !j.WorkDeleted {
		t.Errorf("work_deleted lost across restart: %+v", j)
	}
}

func TestRetentionJobs(t *testing.T) {
	ts := startServer(t, server.Config{KeepWork: 100 * time.Millisecond, KeepJobs: 500 * time.Millisecond, GCInterval: 50 * time.Millisecond})
	id := ts.detach("", "-c", "echo hi")
	long := ts.detach("", "-c", "sleep 2; echo still here")
	ts.waitDone(id)
	eventually(t, "job record deleted", func() bool { return !exists(filepath.Join(ts.data, "jobs", id)) })
	r := ts.tun("", "status", id)
	if r.code != 125 || !strings.Contains(r.stderr, "no such job") || !strings.Contains(r.stderr, "--keep-jobs") {
		t.Errorf("status of expired job: %+v", r)
	}
	if r := ts.tun("", "ls"); strings.Contains(r.stdout, id) {
		t.Errorf("expired job still listed: %q", r.stdout)
	}
	// A job that was running all along is kept until it too expires.
	if r := ts.tun("", "logs", "-f", long); r.stdout != "still here\n" {
		t.Errorf("long job logs: %+v", r)
	}
}

func TestRetentionBlobs(t *testing.T) {
	ts := startServer(t, server.Config{MaxJobs: 1, KeepBlobs: 500 * time.Millisecond, GCInterval: 50 * time.Millisecond})
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "f"), []byte("blob"), 0o644)
	blocker := ts.detach("", "--", "sleep", "2")
	// Queued behind blocker, then running: its blob must survive well past KeepBlobs.
	id := ts.detach(ws, "-c", "sleep 1; cat f")
	blob := filepath.Join(ts.data, "blobs", ts.status(id).Workspace+".tar.gz")
	ts.waitDone(blocker)
	ts.waitFor(id, func(j api.Job) bool { return j.State == api.Running })
	time.Sleep(300 * time.Millisecond) // the blob was last used >2s ago, past KeepBlobs
	if !exists(blob) {
		t.Fatal("blob of a running job deleted")
	}
	if j := ts.waitDone(id); j.State != api.Exited {
		t.Fatalf("job: %+v", j)
	}
	eventually(t, "unused blob deleted", func() bool { return !exists(blob) })
	// The client re-uploads a swept workspace transparently.
	puts := ts.puts.Load()
	if r := ts.tun(ws, "run", "--", "cat", "f"); r.code != 0 || r.stdout != "blob" {
		t.Fatalf("rerun after blob sweep: %+v", r)
	}
	if ts.puts.Load() != puts+1 {
		t.Errorf("expected one re-upload, got %d", ts.puts.Load()-puts)
	}
}

func TestMaxWorkspace(t *testing.T) {
	ts := startServer(t, server.Config{MaxWorkspace: 64 << 10})
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "a"), make([]byte, 32<<10), 0o644)
	os.WriteFile(filepath.Join(ws, "b"), make([]byte, 32<<10), 0o644) // exactly at the limit
	if r := ts.tun(ws, "run", "--", "true"); r.code != 0 {
		t.Fatalf("workspace at the limit: %+v", r)
	}
	// 1 byte over; compresses to almost nothing, so only the unpacked size catches it.
	os.WriteFile(filepath.Join(ws, "c"), []byte("x"), 0o644)
	r := ts.tun(ws, "run", "--", "true")
	if r.code != 125 || !strings.HasPrefix(r.stderr, "tun: ") || !strings.Contains(r.stderr, "64KB") ||
		!strings.Contains(r.stderr, "--max-workspace") || !strings.Contains(r.stderr, "HTTP 413") {
		t.Fatalf("over the limit: %+v", r)
	}
	ents, _ := os.ReadDir(filepath.Join(ts.data, "jobs"))
	if len(ents) != 1 {
		t.Errorf("rejected job left a dir behind: %d job dirs", len(ents))
	}
}

// From review: a job that makes its sandbox root read-only must not make it undeletable.
func TestRetentionSandboxRootReadOnly(t *testing.T) {
	ts := startServer(t, server.Config{KeepWork: 200 * time.Millisecond, GCInterval: 50 * time.Millisecond})
	id := ts.detach("", "-c", "echo data > big; chmod 500 ..; chmod 500 $HOME $TMPDIR")
	ts.waitDone(id)
	ts.waitFor(id, func(j api.Job) bool { return j.WorkDeleted })
	if exists(filepath.Join(ts.data, "jobs", id, "sandbox")) {
		t.Error("sandbox/ still there")
	}
}

func TestLogsOfExpiredJob(t *testing.T) {
	ts := startServer(t, server.Config{})
	id := ts.detach("", "-c", "echo hi")
	ts.waitDone(id)
	// As if the sweep deleted the job dir between the handler's lookup and its open.
	os.Remove(filepath.Join(ts.data, "jobs", id, "stdout"))
	if r := ts.tun("", "logs", id); r.code != 125 || !strings.Contains(r.stderr, "no such job") || !strings.Contains(r.stderr, "HTTP 404") {
		t.Errorf("logs of a deleted job: %+v", r)
	}
}

func TestMaxLog(t *testing.T) {
	ts := startServer(t, server.Config{MaxLog: 1 << 10})
	// 1MB on stdout must neither block the job nor land on disk; stderr exactly at the cap is kept whole.
	r := ts.tun("", "run", "--json", "-c", `head -c 1048576 /dev/zero | tr '\0' a; head -c 1024 /dev/zero | tr '\0' b >&2`)
	if r.code != 0 {
		t.Fatalf("job: %+v", r)
	}
	id := ts.lastID()
	out := ts.tun("", "logs", id).stdout
	if !strings.HasPrefix(out, strings.Repeat("a", 1024)+"\n[tun: stdout truncated") || !strings.Contains(out, "--max-log") || len(out) > 2048 {
		t.Errorf("stdout: %d bytes, %.1200q", len(out), out)
	}
	if e := ts.tun("", "logs", "--stderr", id).stdout; e != strings.Repeat("b", 1024) {
		t.Errorf("stderr at the cap: %d bytes, %.100q", len(e), e)
	}
	fi, err := os.Stat(filepath.Join(ts.data, "jobs", id, "stdout"))
	if err != nil || fi.Size() > 2048 {
		t.Errorf("stdout file: %v %v", fi, err)
	}
}

func (ts *testServer) lastID() string {
	ts.t.Helper()
	js, err := ts.client().List(context.Background(), 1)
	if err != nil || len(js) == 0 {
		ts.t.Fatalf("list: %v %v", js, err)
	}
	return js[0].ID
}
