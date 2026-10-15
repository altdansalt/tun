package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/server"
)

func (ts *testServer) health() api.Health {
	ts.t.Helper()
	h, err := ts.client().Health(context.Background())
	if err != nil {
		ts.t.Fatal(err)
	}
	return h
}

func TestQueueVisibility(t *testing.T) {
	ts := startServer(t, server.Config{MaxJobs: 1})
	a := ts.detach("", "--", "sleep", "2")
	b := ts.detach("", "--", "true")
	c := ts.detach("", "--", "true")
	ja := ts.waitFor(a, func(j api.Job) bool { return j.State == api.Running })
	time.Sleep(300 * time.Millisecond)
	jb, jc := ts.status(b), ts.status(c)
	if ja.QueuePosition != nil || jb.QueuePosition == nil || *jb.QueuePosition != 1 || jc.QueuePosition == nil || *jc.QueuePosition != 2 {
		t.Errorf("queue positions: a=%v b=%v c=%v", ja.QueuePosition, jb.QueuePosition, jc.QueuePosition)
	}
	if ja2 := ts.status(a); ja2.RunSec < 0.3 || ja2.RunSec <= ja.RunSec || jb.QueueSec < 0.3 || jb.RunSec != 0 {
		t.Errorf("live timing: a run_sec %v then %v; b queue_sec %v run_sec %v", ja.RunSec, ja2.RunSec, jb.QueueSec, jb.RunSec)
	}
	if h := ts.health(); h.Running != 1 || h.Queued != 2 || h.MaxJobs != 1 || h.Services != 0 {
		t.Errorf("healthz: %+v", h)
	}

	// tun run behind the three: told it's queued once, then when it started.
	r := ts.tun("", "run", "--", "echo", "hi")
	lines := strings.Split(cmdStderr(r.stderr), "\n")
	if r.code != 0 || r.stdout != "hi\n" || len(lines) != 3 || lines[0] != "tun: queued (2 ahead, 1 running)" ||
		!strings.HasPrefix(lines[1], "tun: started after ") || !strings.HasSuffix(lines[1], "s in queue") {
		t.Errorf("queued run: %+v", r)
	}
	d := ts.status(ts.lastID())
	if d.QueuePosition != nil || d.QueueSec < 1 || d.StartedAt == nil || d.EndedAt == nil ||
		d.RunSec != secsOf(d.EndedAt.Sub(*d.StartedAt)) {
		t.Errorf("finished job timing: %+v", d)
	}
	if ja := ts.status(a); ja.RunSec < 1.9 || ja.QueueSec > 0.5 {
		t.Errorf("a timing: queue %v run %v", ja.QueueSec, ja.RunSec)
	}

	// Not queued: no queue lines. --json carries the timing.
	r = ts.tun("", "run", "--", "true")
	if cmdStderr(r.stderr) != "" {
		t.Errorf("unqueued run printed %q", r.stderr)
	}
	r = ts.tun("", "run", "--json", "--", "sleep", "0.2")
	var res map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || res["run_sec"].(float64) < 0.2 || res["queue_sec"] == nil {
		t.Errorf("json: %v %q", err, r.stdout)
	}

	// A job canceled while queued keeps its queue time and has no run time.
	e := ts.detach("", "--", "sleep", "5")
	f := ts.detach("", "--", "true")
	time.Sleep(200 * time.Millisecond)
	ts.tun("", "cancel", f)
	ts.tun("", "cancel", e)
	if jf := ts.waitDone(f); jf.QueueSec < 0.2 || jf.RunSec != 0 || jf.QueuePosition != nil {
		t.Errorf("canceled queued job: %+v", jf)
	}

	// Services are counted separately.
	svc := ts.detach("", "--service", "--no-workspace", "--", "sleep", "30")
	ts.waitFor(svc, func(j api.Job) bool { return j.State == api.Running })
	if h := ts.health(); h.Services != 1 || h.Running != 0 || h.Queued != 0 {
		t.Errorf("healthz with a service: %+v", h)
	}
	ts.tun("", "cancel", svc)
}

func secsOf(d time.Duration) float64 { return float64(d.Milliseconds()) / 1000 }

// post creates a job directly over HTTP with the given extra headers and returns it.
func (ts *testServer) post(headers map[string]string) api.Job {
	ts.t.Helper()
	req, _ := http.NewRequest("POST", ts.url+"/v1/jobs", bytes.NewReader([]byte(`{"argv":["true"]}`)))
	req.Header.Set("X-Tun-Token", ts.srv.Token())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	var j api.Job
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil || resp.StatusCode != 201 {
		ts.t.Fatalf("create: %d %v", resp.StatusCode, err)
	}
	ts.waitDone(j.ID)
	return j
}

func ids(t *testing.T, out string) []string {
	t.Helper()
	var r []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var j api.Job
		if err := json.Unmarshal([]byte(l), &j); err != nil {
			t.Fatalf("ls line %q: %v", l, err)
		}
		r = append(r, j.ID)
	}
	return r
}

func TestClientAndListFilters(t *testing.T) {
	ts := startServer(t, server.Config{})
	host, _ := os.Hostname()
	mine := ts.detach("", "--timeout", "1s", "--", "sleep", "5")
	ts.waitDone(mine)
	if j := ts.status(mine); j.Client != host {
		t.Errorf("client %q, want hostname %q", j.Client, host)
	}
	vm := ts.post(map[string]string{"X-Exedev-Source-Vm": "vm1", "X-Tun-Client": "spoof"})
	other := ts.post(map[string]string{"X-Tun-Client": "other host"})
	anon := ts.post(nil)
	for _, c := range []struct{ id, want string }{{vm.ID, "vm1"}, {other.ID, "otherhost"}, {anon.ID, "127.0.0.1"}} {
		if j := ts.status(c.id); j.Client != c.want {
			t.Errorf("job %s client %q, want %q", c.id, j.Client, c.want)
		}
	}

	check := func(want []string, args ...string) {
		t.Helper()
		r := ts.tun("", append([]string{"ls"}, args...)...)
		if got := ids(t, r.stdout); r.code != 0 || strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("ls %v = %v (%+v), want %v", args, got, r, want)
		}
	}
	check([]string{mine}, "--mine")
	check([]string{vm.ID}, "--client", "vm1")
	check([]string{anon.ID, other.ID, vm.ID}, "--state", "exited")
	check([]string{anon.ID, other.ID, vm.ID, mine}, "--state", "exited,timeout")
	check([]string{mine}, "--state", "timeout", "--limit", "1", "--client", host)
	check([]string{anon.ID}, "--state", "exited", "--limit", "1")
	check(nil, "--client", "nobody")

	// mine=1 is relative to the asker, as the server identifies it.
	c := ts.client()
	c.Name = "vm1"
	js, err := c.List(context.Background(), 10, map[string][]string{"mine": {"1"}})
	if err != nil || len(js) != 1 || js[0].ID != vm.ID {
		t.Errorf("mine as vm1: %v %+v", err, js)
	}

	for _, args := range [][]string{{"--state", "bogus"}, {"--mine", "--client", "x"}} {
		if r := ts.tun("", append([]string{"ls"}, args...)...); r.code != 125 || !strings.HasPrefix(r.stderr, "tun: ") {
			t.Errorf("ls %v: %+v", args, r)
		}
	}
}

// SIGINT while tun run waits in the queue cancels the job before it ever starts.
func TestSignalWhileQueued(t *testing.T) {
	ts := startServer(t, server.Config{MaxJobs: 1})
	blocker := ts.detach("", "--", "sleep", "30")
	sigs := make(chan os.Signal, 1)
	var out, errw syncBuffer
	done := make(chan int)
	go func() {
		done <- Main([]string{"run", "--no-workspace", "--", "true"}, ts.env(t.TempDir(), nil, &out, &errw, sigs))
	}()
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(errw.String(), "tun: queued (0 ahead, 1 running)"); {
		if time.Now().After(deadline) {
			t.Fatalf("never reported queued: %q", errw.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	sigs <- syscall.SIGINT
	if code := <-done; code != 130 {
		t.Errorf("exit %d, want 130; stderr %q", code, errw.String())
	}
	if j := ts.status(ts.lastID()); j.State != api.Canceled || j.StartedAt != nil {
		t.Errorf("queued job after SIGINT: %+v", j)
	}
	ts.tun("", "cancel", blocker)
}

// syncBuffer is a bytes.Buffer safe to read while Main writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
