// Package server implements the tun HTTP API: blob upload, job creation, a FIFO scheduler
// bounded by MaxJobs, log streaming, cancel, and recovery of jobs across restarts.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
)

// Config configures a Server.
type Config struct {
	DataDir     string
	MaxJobs     int
	MaxServices int    // service jobs run at once, separately from MaxJobs; more are rejected
	Sandbox     string // sandbox.Bwrap or sandbox.None (already resolved)
	PrivateNet  bool   // bwrap only: a network namespace per job (check sandbox.NetUsable first)
	MaxUpload   int64  // bytes
	KillGrace   time.Duration

	MaxDownload int64 // max total file bytes per files request; 0 = DefaultMaxDownload
	// Retention (see gc.go); zero keeps forever.
	KeepWork, KeepJobs, KeepBlobs time.Duration
	GCInterval                    time.Duration // 0: derived from the Keep* durations
	MaxWorkspace                  int64         // max unpacked workspace bytes; 0: unlimited
	MaxLog                        int64         // max bytes kept per stdout/stderr; 0: unlimited
	MinFree                       MinFree       // free space to keep on the data dir's filesystem (see disk.go)

	// Runners (see hub.go). HubOnly: no local job slots (tun serve --local-slots 0), so
	// non-service jobs run only on runners; MaxJobs is then ignored.
	HubOnly         bool
	RunnerPollWait  time.Duration // how long a runner's poll waits for work; default 25s
	RunnerDeadAfter time.Duration // a runner silent for this long is lost; default 30s
	// RunnerBackstop: how long past its timeout (or a requested kill) plus KillGrace a live
	// runner's command may still be running before the hub ends the job itself; default 5m.
	RunnerBackstop time.Duration
	// RunnerStartTimeout: an assigned job not started by then is requeued; default 10m.
	RunnerStartTimeout time.Duration

	// Bazel sessions (see session.go).
	MaxSessions  int           // sessions at once (each may hold a Bazel server); default 2
	SessionIdle  time.Duration // a session without a command for this long is deleted; default 30m
	BazelCommand string        // default "bazel" (tests use a fake; relative = in the workspace)
	// BazelCaches holds each client's Bazel caches (bazelisk downloads, repository cache,
	// disk cache) in <dir>/<sha256(client)[:16 hex]>/; default $DATA/bazel. Never inside a
	// tree a job can write (bind mount sources must not be replaceable by a job).
	BazelCaches string
	// BazeliskSeed: a server-owned bazelisk home whose downloads/ are copied into a client's
	// new bazelisk home, so new clients don't download Bazel again; "" for none.
	BazeliskSeed string
	DiskCacheMax string // --experimental_disk_cache_gc_max_size of each client's disk cache; default 5G
}

// Server is the tun server. Create with New, serve with Handler, stop with Close.
type Server struct {
	cfg   Config
	token string

	mu       sync.Mutex
	jobs     map[string]*job
	queue    []*job // FIFO of queued jobs
	running  int    // local non-service jobs
	svc      services
	runners  map[string]*runnerState // the current instance of each runner name
	retired  map[string]*runnerState // by instance id: superseded or dropped instances
	sessions map[string]*session     // tun bazel sessions by id
	closed   bool
	quit     chan struct{} // closed by Close
	wg       sync.WaitGroup
	gc       gcState
}

type job struct {
	api.Job
	Env  map[string]string  `json:"env,omitempty"`
	done chan struct{}      // closed when the job reaches a terminal state
	kill func(state string) // set while running; requests termination with the given end state
	net  *sandbox.Net       // set while running with a private network namespace
	// cancelReq is set by a cancel that arrives after the job left the queue (or, for a
	// service, was admitted) but before run started it; run then finishes it as canceled.
	cancelReq bool

	// Runner jobs (see hub.go).
	assignedAt time.Time       // given to a runner, which has not started it yet (state queued)
	killReq    string          // end state the hub asked the runner for (canceled)
	killReqAt  time.Time       //
	rn         *runnerState    // the runner instance it is assigned to
	exitedAt   time.Time       // the runner reported the command ended (.../exited)
	exited     *api.RunnerDone // what it reported then
	logMu      sync.Mutex      // serializes log chunk writes
	bz         *api.BazelJob   // a tun bazel job (kept in memory only: after a restart it has failed)
	// Outcome of the runner collecting the declared outputs.
	OutMissing []string `json:"out_missing,omitempty"`
	OutSkipped []string `json:"out_skipped,omitempty"`
	OutError   string   `json:"out_error,omitempty"`
}

// New opens (creating if needed) the data dir, loads the token and existing jobs, and marks
// jobs left queued/running by a previous server as failed.
func New(cfg Config) (*Server, error) {
	if cfg.MaxJobs < 1 {
		cfg.MaxJobs = 1
	}
	if cfg.HubOnly {
		cfg.MaxJobs = 0
	}
	if cfg.RunnerDeadAfter <= 0 {
		cfg.RunnerDeadAfter = 30 * time.Second
	}
	if cfg.RunnerBackstop <= 0 {
		cfg.RunnerBackstop = 5 * time.Minute
	}
	if cfg.RunnerStartTimeout <= 0 {
		cfg.RunnerStartTimeout = 10 * time.Minute
	}
	if cfg.RunnerPollWait <= 0 {
		cfg.RunnerPollWait = 25 * time.Second
	}
	// A runner is seen at least once per poll wait, so it must be shorter than the timeout.
	cfg.RunnerPollWait = min(cfg.RunnerPollWait, cfg.RunnerDeadAfter*5/6)
	if cfg.MaxServices < 1 {
		cfg.MaxServices = 4
	}
	if cfg.Sandbox != sandbox.Bwrap {
		cfg.PrivateNet = false
	}
	if cfg.KillGrace == 0 {
		cfg.KillGrace = 10 * time.Second
	}
	if cfg.MaxUpload <= 0 {
		cfg.MaxUpload = 512 << 20
	}
	if cfg.MaxSessions < 1 {
		cfg.MaxSessions = 2
	}
	if cfg.SessionIdle <= 0 {
		cfg.SessionIdle = 30 * time.Minute
	}
	if cfg.BazelCommand == "" {
		cfg.BazelCommand = "bazel"
	}
	if cfg.DiskCacheMax == "" {
		cfg.DiskCacheMax = "5G"
	}
	dir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = dir
	for _, d := range []string{"", "blobs", "cas", "jobs", "cache/go-build", "cache/gomod", "cache/xdg"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	// Mount sources must have no symlink in their path (sandbox.openMountSource).
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, err
	}
	cfg.DataDir = dir
	if cfg.BazelCaches == "" {
		cfg.BazelCaches = filepath.Join(dir, "bazel")
	}
	if err := os.MkdirAll(cfg.BazelCaches, 0o700); err != nil {
		return nil, err
	}
	if cfg.BazelCaches, err = filepath.EvalSymlinks(cfg.BazelCaches); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, jobs: map[string]*job{}, runners: map[string]*runnerState{}, retired: map[string]*runnerState{},
		sessions: map[string]*session{}, quit: make(chan struct{})}
	s.gc.fails = map[string]int{}
	if s.token, err = loadToken(filepath.Join(dir, "token")); err != nil {
		return nil, err
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	if err := s.loadSessions(); err != nil {
		return nil, err
	}
	s.startGC()
	s.startReaper()
	return s, nil
}

func loadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	t := randHex(32)
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Token returns the bearer token clients must present.
func (s *Server) Token() string { return s.token }

// recover loads job.json files and fails jobs a previous server left unfinished.
func (s *Server) recover() error {
	ents, err := os.ReadDir(filepath.Join(s.cfg.DataDir, "jobs"))
	if err != nil {
		return err
	}
	for _, e := range ents {
		f, err := openOwn(filepath.Join(s.jobDir(e.Name()), "job.json"), os.O_RDONLY, 0)
		if err != nil {
			continue // half-created job dir
		}
		b, err := io.ReadAll(io.LimitReader(f, 16<<20))
		f.Close()
		if err != nil {
			continue
		}
		j := &job{done: make(chan struct{})}
		if err := json.Unmarshal(b, j); err != nil || j.ID != e.Name() {
			continue
		}
		if j.State == api.Running && isRunnerJob(j) {
			s.jobs[j.ID] = j // the runner may still be running it
			s.adopt(j)
			continue
		}
		if !api.Terminal(j.State) {
			j.Error = fmt.Sprintf("server restarted while job was %s", j.State)
			j.State = api.Failed
			now := time.Now().UTC()
			j.EndedAt = &now
			if err := s.persist(j); err != nil {
				return err
			}
		}
		close(j.done)
		s.jobs[j.ID] = j
	}
	return nil
}

// Close stops scheduling and kills running jobs without recording an end state, as a crash
// would; the next server on this data dir marks them failed.
func (s *Server) Close() {
	s.mu.Lock()
	if !s.closed {
		close(s.quit)
	}
	s.closed = true
	for _, j := range s.jobs {
		if j.kill != nil {
			j.kill("")
		}
	}
	s.mu.Unlock()
	s.closeSessions()
	s.stopGC()
	s.wg.Wait()
	s.closeSessions() // any a stopping session's goroutine started meanwhile
}

// jobDir holds the server's files for a job (job.json, stdout, stderr) and its sandbox dir.
// The job itself can write only inside sandboxDir.
func (s *Server) jobDir(id string) string { return filepath.Join(s.cfg.DataDir, "jobs", id) }

// sandboxDir is the job's own tree (work, home, tmp): the only part of the data dir, besides
// the shared caches, that a job can see or write.
func (s *Server) sandboxDir(id string) string { return filepath.Join(s.jobDir(id), "sandbox") }

// persist writes job.json atomically, via a fresh temp file (O_EXCL, random name) so a
// pre-existing file or symlink can't redirect the write. Caller holds s.mu (or owns j
// exclusively).
func (s *Server) persist(j *job) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	dir := s.jobDir(j.ID)
	tmp, err := os.CreateTemp(dir, ".job.json-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, "job.json"))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// newID returns a sortable unique id: UTC time to the microsecond plus random suffix.
func newID() string {
	t := time.Now().UTC()
	return t.Format("20060102T150405") + fmt.Sprintf("%06d", t.Nanosecond()/1000) + "-" + randHex(4)
}

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		slots, running, runners := s.runnerInfo()
		h := api.Health{OK: true, Sandbox: s.cfg.Sandbox, NetworkIsolation: s.cfg.PrivateNet, MaxJobs: s.cfg.MaxJobs + slots,
			MaxServices: s.cfg.MaxServices, Running: s.running + running, Queued: len(s.queue), Services: s.svc.running,
			LocalSlots: s.cfg.MaxJobs, Runners: runners, Sessions: len(s.sessions), MaxSessions: s.cfg.MaxSessions}
		s.mu.Unlock()
		if h.Disk = diskInfo(s.cfg.DataDir, s.cfg.MinFree); h.Disk != nil {
			h.Disk.ReclaimedWorkDirs, h.Disk.ReclaimedSessions = s.gc.reclaimedWork.Load(), s.gc.reclaimedSessions.Load()
		}
		writeJSON(w, 200, h)
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// X-Tun-Token first: proxies in front of tun (e.g. exe.dev's) may consume Authorization.
			got, ok := r.Header.Get("X-Tun-Token"), true
			if got == "" {
				got, ok = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeErr(w, 401, "missing or wrong bearer token (the server's token is in its $DATA/token)")
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("HEAD /v1/blobs/{sha}", auth(s.headBlob))
	mux.HandleFunc("PUT /v1/blobs/{sha}", auth(s.putBlob))
	mux.HandleFunc("GET /v1/blobs/{sha}", auth(s.getBlob))
	mux.HandleFunc("POST /v1/runner/poll", auth(s.runnerPoll))
	mux.HandleFunc("POST /v1/runner/jobs/{id}/started", auth(s.runnerStarted))
	mux.HandleFunc("POST /v1/runner/jobs/{id}/done", auth(s.runnerDone))
	mux.HandleFunc("POST /v1/runner/jobs/{id}/exited", auth(s.runnerExited))
	mux.HandleFunc("POST /v1/runner/leave", auth(s.runnerLeave))
	mux.HandleFunc("PUT /v1/runner/jobs/{id}/outputs", auth(s.runnerOutputs))
	mux.HandleFunc("POST /v1/runner/jobs/{id}/{stream}", auth(s.runnerLog))
	mux.HandleFunc("POST /v1/cas", auth(s.putCAS))
	mux.HandleFunc("GET /v1/sessions", auth(s.listSessions))
	mux.HandleFunc("POST /v1/jobs", auth(s.createJob))
	mux.HandleFunc("GET /v1/jobs", auth(s.listJobs))
	mux.HandleFunc("GET /v1/jobs/{id}", auth(s.getJob))
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", auth(s.cancelJob))
	mux.HandleFunc("GET /v1/jobs/{id}/{stream}", auth(s.logs))
	mux.HandleFunc("GET /v1/jobs/{id}/files", auth(s.files))
	mux.HandleFunc("/v1/jobs/{id}/ports/{port}", auth(s.proxy))
	mux.HandleFunc("/v1/jobs/{id}/ports/{port}/{path...}", auth(s.proxy))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 404, fmt.Sprintf("no such endpoint: %s %s", r.Method, r.URL.Path))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.Error{Error: msg})
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) *job {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	s.mu.Unlock()
	if j == nil {
		writeErr(w, 404, s.noSuchJob(r.PathValue("id")))
	}
	return j
}

// snapshot copies the public part of j, with computed fields, under the lock.
func (s *Server) snapshot(j *job) api.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(j, time.Now())
}

// view is j's public job object with the computed timing and queue fields. Caller holds s.mu.
func (s *Server) view(j *job, now time.Time) api.Job {
	v := j.Job
	end := now
	if v.EndedAt != nil {
		end = *v.EndedAt
	}
	if v.StartedAt != nil {
		v.QueueSec, v.RunSec = secs(v.StartedAt.Sub(v.CreatedAt)), secs(end.Sub(*v.StartedAt))
	} else {
		v.QueueSec = secs(end.Sub(v.CreatedAt))
	}
	if v.State == api.Queued && j.bz != nil {
		v.QueueReason = s.queueReason(j)
	}
	if v.State == api.Queued {
		for i, q := range s.queue {
			if q == j {
				p := i + 1
				v.QueuePosition = &p
				break
			}
		}
	}
	return v
}

// secs is d in seconds, to the millisecond.
func secs(d time.Duration) float64 {
	return float64(max(d, 0).Milliseconds()) / 1000
}

// clientOf names who sent r: the exe.dev proxy's X-Exedev-Source-Vm header (the proxy sets it
// and doesn't let callers forge it), else the client's self-reported X-Tun-Client (its
// hostname), else the remote IP. Without the proxy in front, both headers are only as
// trustworthy as the token holder: this is for visibility, not access control.
func clientOf(r *http.Request) string {
	for _, h := range []string{"X-Exedev-Source-Vm", "X-Tun-Client"} {
		if v := cleanName(r.Header.Get(h)); v != "" {
			return v
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// cleanName trims a header value to at most 128 printable ASCII characters.
func cleanName(v string) string {
	v = strings.Map(func(r rune) rune {
		if r <= ' ' || r > '~' {
			return -1
		}
		return r
	}, v)
	if len(v) > 128 {
		v = v[:128]
	}
	return v
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if j := s.lookup(w, r); j != nil {
		writeJSON(w, 200, s.snapshot(j))
	}
}

// listJobs serves GET /v1/jobs?limit=N&state=S[,S...]&client=NAME|mine=1, newest first.
// mine=1 means client = clientOf(this request).
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		var err error
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			writeErr(w, 400, fmt.Sprintf("limit %q must be a positive integer", v))
			return
		}
	}
	states := map[string]bool{}
	if v := q.Get("state"); v != "" {
		for _, st := range strings.Split(v, ",") {
			if !slices.Contains(api.States, st) {
				writeErr(w, 400, fmt.Sprintf("state %q must be one of %s (comma-separated for several)", st, strings.Join(api.States, ",")))
				return
			}
			states[st] = true
		}
	}
	client := q.Get("client")
	if v := q.Get("mine"); v != "" {
		mine, err := strconv.ParseBool(v)
		switch {
		case err != nil:
			writeErr(w, 400, fmt.Sprintf("mine %q must be 1, 0, true or false", v))
			return
		case mine && client != "":
			writeErr(w, 400, "give client or mine, not both")
			return
		case mine:
			client = clientOf(r)
		}
	}
	now := time.Now()
	s.mu.Lock()
	list := make([]api.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if (len(states) == 0 || states[j.State]) && (client == "" || j.Client == client) {
			list = append(list, s.view(j, now))
		}
	}
	s.mu.Unlock()
	sort.Slice(list, func(a, b int) bool { return list[a].ID > list[b].ID })
	if len(list) > limit {
		list = list[:limit]
	}
	writeJSON(w, 200, list)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	s.mu.Lock()
	switch {
	case j.State == api.Queued:
		queued := false
		for i, q := range s.queue {
			if q == j {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				queued = true
				break
			}
		}
		if queued {
			s.finish(j, api.Canceled, nil, nil, "canceled while queued")
		} else if isRunnerJob(j) { // assigned; the runner's .../started will be refused
			s.finish(j, api.Canceled, nil, nil, "canceled before it started")
		} else {
			j.cancelReq = true // run() owns it now and will finish it before starting
		}
	case isRunnerJob(j) && j.State == api.Running:
		if j.killReq == "" {
			j.killReq, j.killReqAt = api.Canceled, time.Now()
			if j.rn != nil {
				j.rn.poke()
			}
		}
	case j.kill != nil:
		j.kill(api.Canceled)
	}
	s.mu.Unlock()
	writeJSON(w, 200, s.snapshot(j))
}

// finish records a terminal state, once; later calls are no-ops. Caller holds s.mu.
func (s *Server) finish(j *job, state string, code, sig *int, msg string) {
	select {
	case <-j.done:
		return
	default:
	}
	now := time.Now().UTC()
	j.State, j.ExitCode, j.Signal, j.EndedAt, j.kill = state, code, sig, &now, nil
	if j.bz != nil {
		j.bz.Manifest = nil // possibly large, and only needed until the sync
	}
	if msg != "" {
		j.Error = msg
	}
	if err := s.persist(j); err != nil && j.Error == "" {
		j.Error = "persisting job state: " + err.Error()
	}
	s.serviceDone(j)
	close(j.done)
	if isRunnerJob(j) {
		s.runnerJobEnded(j)
	}
}
