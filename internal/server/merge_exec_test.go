package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/reapi"
	"github.com/altdansalt/tun/internal/sandbox"
)

// A runner disconnecting mid-report must not hold the hub mutex while reading.
func TestStalledRunnerReportsAllowHealth(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), HubOnly: true, Sandbox: sandbox.None})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.actionStore, err = reapi.Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, path := range []string{"/v1/runner/jobs/x/done", "/v1/runner/jobs/x/exited", "/v1/runner/leave", "/v1/runner/poll", "/v1/runner/cas"} {
		t.Run(path, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			entered := make(chan struct{})
			finished := make(chan struct{})
			req := httptest.NewRequest("POST", path, &enteredReader{Reader: reader, entered: entered})
			req.Header.Set("X-Tun-Token", s.RunnerToken())
			req.Header.Set(api.HeaderRunnerInstance, "instance")
			go func() { defer close(finished); h.ServeHTTP(httptest.NewRecorder(), req) }()
			<-entered
			health := make(chan int, 1)
			go func() {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/healthz", nil))
				health <- w.Code
			}()
			select {
			case code := <-health:
				if code != http.StatusOK {
					t.Error(code)
				}
			case <-time.After(time.Second):
				t.Error("health blocked by incomplete runner body")
			}
			writer.Close()
			<-finished
		})
	}
}

type enteredReader struct {
	io.Reader
	entered chan struct{}
}

func (r *enteredReader) Read(p []byte) (int, error) {
	if r.entered != nil {
		close(r.entered)
		r.entered = nil
	}
	return r.Reader.Read(p)
}

func TestRunnerActionsDiskAndSessionScheduling(t *testing.T) {
	s, err := New(Config{DataDir: t.TempDir(), HubOnly: true, Sandbox: sandbox.None})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	disk := &api.Disk{FreeBytes: 123, Low: true}
	rn := s.register("runner", api.RunnerPoll{Instance: "one", Slots: 3, Sandbox: "bwrap", Actions: true, Disk: disk}, "test", time.Now())
	if !rn.actions || rn.disk != disk {
		t.Fatal("lost action capability or disk report")
	}
	session := &job{bz: &api.BazelJob{}}
	action := &job{Action: []byte{1}}
	ordinary := &job{}
	s.queue = []*job{session, action, ordinary}
	assigned := s.assign(rn, map[string]bool{})
	if len(assigned) != 2 || assigned[0] != action || assigned[1] != ordinary || len(s.queue) != 1 || s.queue[0] != session {
		t.Fatal("incorrect runner assignment")
	}
	// Neither unsupported actions nor sessions may block ordinary runner jobs.
	rn.actions = false
	rn.jobs = map[string]*job{}
	s.queue = []*job{session, action, ordinary}
	assigned = s.assign(rn, map[string]bool{})
	if len(assigned) != 1 || assigned[0] != ordinary {
		t.Fatal("non-action runner assignment")
	}
	s.queue = nil
	rn.jobs = map[string]*job{}
}
