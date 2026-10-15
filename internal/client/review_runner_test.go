package client

// Review tests for the runner feature (review/runners). Each one fails on 44a8e50.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

// rawReq is fakeRunner.req without t.Fatal, for use from goroutines.
func (f *fakeRunner) rawReq(method, path string, body []byte) int {
	req, _ := http.NewRequest(method, f.ts.url+path, bytes.NewReader(body))
	req.Header.Set("X-Tun-Token", f.ts.srv.Token())
	req.Header.Set(api.HeaderRunner, f.name)
	if f.as != "" {
		req.Header.Set("X-Tun-Client", f.as)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func (f *fakeRunner) createJob() string {
	f.ts.t.Helper()
	b, _ := json.Marshal(api.CreateJob{Argv: []string{"true"}})
	req, _ := http.NewRequest("POST", f.ts.url+"/v1/jobs", bytes.NewReader(b))
	req.Header.Set("X-Tun-Token", f.ts.srv.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	var j api.Job
	json.NewDecoder(resp.Body).Decode(&j)
	if j.ID == "" {
		f.ts.t.Fatalf("create job: HTTP %d", resp.StatusCode)
	}
	return j.ID
}

// Two runner processes with the same --name on the same host (the default name is the
// hostname, so e.g. two runners on one VM, or two VMs that share a hostname such as "ubuntu",
// without the exe.dev proxy) both look like "a restart from the same source" to each other on
// every poll: each poll drops the other instance and fails its running jobs. Either the second
// should be refused (409) or the superseded instance's polls should be; never an endless
// mutual takeover that kills jobs.
func TestReviewRunnerSameNameSameHost(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	ts.startRunner("dup", 1)
	ts.waitRunners(1)
	env, wait, done := release(t)
	id := ts.detach("", append(env, "--no-workspace", "-c", wait)...)
	ts.waitFor(id, func(j api.Job) bool { return j.State == api.Running })
	ts.startRunner("dup", 1) // a second process, same name, same host
	time.Sleep(1500 * time.Millisecond)
	done()
	if j := ts.waitDone(id); j.State != api.Exited {
		t.Errorf("job on the first runner after a same-name runner started: %s %q", j.State, j.Error)
	}
}

// A poll from an old instance delayed in the network (seq protects only within one
// instance) arrives after the runner restarted: the hub takes it as yet another restart and
// fails the new instance's running job. SPEC: "a delayed request must not undo newer state".
func TestReviewRunnerDelayedPollOldInstance(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	old := &fakeRunner{ts: ts, name: "rr", instance: "old", as: "hostA", seq: 1}
	old.poll(0)
	cur := &fakeRunner{ts: ts, name: "rr", instance: "new", as: "hostA", seq: 1}
	id := ts.detach("", "--no-workspace", "--", "true")
	if _, resp := cur.poll(1); len(resp.Jobs) != 1 {
		t.Fatalf("assign: %+v", resp)
	}
	if code, _ := cur.req("POST", "/v1/runner/jobs/"+id+"/started", nil); code != 200 {
		t.Fatalf("started: %d", code)
	}
	old.seq = 1 // the old instance's poll #2, sent before it died, delivered now
	old.poll(0)
	if j := ts.status(id); j.State != api.Running {
		t.Errorf("new instance's job after a delayed poll of the old instance: %s %q", j.State, j.Error)
	}
}

// runnerStarted checks the job (runnerJob) and marks it running under two separate s.mu
// sections. A cancel landing in between finishes the job, and /started still answers 200:
// the runner then starts a job that was canceled before it started (SPEC: "a job canceled
// before [started] ... the call is a 410, so the runner never starts it").
func TestReviewRunnerStartedCancelRace(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	f := &fakeRunner{ts: ts, name: "race", instance: "i1"}
	f.poll(0)
	bad := 0
	for i := 0; i < 10000 && bad == 0; i++ {
		id := f.createJob()
		if _, resp := f.poll(1); len(resp.Jobs) != 1 || resp.Jobs[0].ID != id {
			t.Fatalf("assign: %+v", resp)
		}
		var started, canceled int
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); started = f.rawReq("POST", "/v1/runner/jobs/"+id+"/started", nil) }()
		go func() { defer wg.Done(); canceled = f.rawReq("POST", "/v1/jobs/"+id+"/cancel", nil) }()
		wg.Wait()
		j := ts.status(id)
		if started == 200 && j.State == api.Canceled && j.StartedAt == nil {
			bad++
			t.Errorf("iteration %d: /started answered 200 (cancel %d) for job %s that is %s %q, never started",
				i, canceled, id, j.State, j.Error)
		}
		// Clean up: end whatever is left (the next poll's assignment then has a free slot).
		d, _ := json.Marshal(api.RunnerDone{State: api.Exited, ExitCode: new(int)})
		f.rawReq("POST", "/v1/runner/jobs/"+id+"/done", d)
	}
}

// The hub backstop counts from /started to /done, but the runner ships the remaining logs and
// uploads the declared outputs (up to --max-download, with 10-minute retries) between the
// command's exit and /done. A job that exits 0 well within its timeout but whose upload
// takes longer than kill grace + --runner-timeout is recorded as "timeout (enforced by the
// hub)", and its outputs and exit status are refused with 410.
func TestReviewRunnerBackstopDuringUpload(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	f := &fakeRunner{ts: ts, name: "slow", instance: "i1"}
	r := ts.tun("", "run", "--detach", "--no-workspace", "--timeout", "1s", "--", "true")
	id := strings.TrimSpace(r.stdout)
	if _, resp := f.poll(1); len(resp.Jobs) != 1 {
		t.Fatalf("assign: %+v", resp)
	}
	f.req("POST", "/v1/runner/jobs/"+id+"/started", nil)
	// The command exited at once; the runner, alive and polling, uploads outputs for ~3s.
	f.log(id, 0, "built\n")
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); {
		f.poll(1, id)
	}
	code, _ := f.req("PUT", "/v1/runner/jobs/"+id+"/outputs", nil)
	d, _ := json.Marshal(api.RunnerDone{State: api.Exited, ExitCode: new(int)})
	dcode, _ := f.req("POST", "/v1/runner/jobs/"+id+"/done", d)
	if j := ts.status(id); j.State != api.Exited {
		t.Errorf("job that exited 0 at once, then uploaded outputs: %s %q (outputs %d, done %d)", j.State, j.Error, code, dcode)
	}
}

// tun run --detach refuses --out ("fetch later with tun get <id> PATH..."), but on a runner
// only declared --out paths are kept, so a detached runner job's files can never be fetched:
// the advice in the error is wrong for runner jobs and there is no way to declare outputs
// for a detached job.
func TestReviewRunnerDetachThenGet(t *testing.T) {
	t.Parallel()
	ts := startServer(t, hubConfig(""))
	ts.startRunner("r1", 1)
	ts.waitRunners(1)
	r := ts.tun("", "run", "--detach", "--out", "f", "--no-workspace", "-c", "echo x > f")
	if r.code == 0 {
		t.Skip("--detach --out now accepted")
	}
	advice := r.stderr
	id := ts.detach("", "--no-workspace", "-c", "echo x > f")
	ts.waitDone(id)
	dst := t.TempDir()
	if g := ts.tun("", "get", "-o", dst, id, "f"); g.code != 0 {
		t.Errorf("following %q: tun get %s f: %d %s", strings.TrimSpace(advice), id, g.code, g.stderr)
	}
}
