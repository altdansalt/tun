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
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

// Config configures a Server.
type Config struct {
	DataDir   string
	MaxJobs   int
	Sandbox   string // sandbox.Bwrap or sandbox.None (already resolved)
	MaxUpload int64  // bytes
	KillGrace time.Duration

	MaxDownload int64 // max total file bytes per files request; 0 = DefaultMaxDownload
}

// Server is the tun server. Create with New, serve with Handler, stop with Close.
type Server struct {
	cfg   Config
	token string

	mu      sync.Mutex
	jobs    map[string]*job
	queue   []*job // FIFO of queued jobs
	running int
	closed  bool
	wg      sync.WaitGroup
}

type job struct {
	api.Job
	Env  map[string]string  `json:"env,omitempty"`
	done chan struct{}      // closed when the job reaches a terminal state
	kill func(state string) // set while running; requests termination with the given end state
}

// New opens (creating if needed) the data dir, loads the token and existing jobs, and marks
// jobs left queued/running by a previous server as failed.
func New(cfg Config) (*Server, error) {
	if cfg.MaxJobs < 1 {
		cfg.MaxJobs = 1
	}
	if cfg.KillGrace == 0 {
		cfg.KillGrace = 10 * time.Second
	}
	dir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = dir
	for _, d := range []string{"", "blobs", "jobs", "cache/go-build", "cache/gomod", "cache/xdg"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	s := &Server{cfg: cfg, jobs: map[string]*job{}}
	if s.token, err = loadToken(filepath.Join(dir, "token")); err != nil {
		return nil, err
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
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
	s.closed = true
	for _, j := range s.jobs {
		if j.kill != nil {
			j.kill("")
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
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
		writeJSON(w, 200, map[string]any{"ok": true, "sandbox": s.cfg.Sandbox, "max_jobs": s.cfg.MaxJobs})
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeErr(w, 401, "missing or wrong bearer token (the server's token is in its $DATA/token)")
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("HEAD /v1/blobs/{sha}", auth(s.headBlob))
	mux.HandleFunc("PUT /v1/blobs/{sha}", auth(s.putBlob))
	mux.HandleFunc("POST /v1/jobs", auth(s.createJob))
	mux.HandleFunc("GET /v1/jobs", auth(s.listJobs))
	mux.HandleFunc("GET /v1/jobs/{id}", auth(s.getJob))
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", auth(s.cancelJob))
	mux.HandleFunc("GET /v1/jobs/{id}/{stream}", auth(s.logs))
	mux.HandleFunc("GET /v1/jobs/{id}/files", auth(s.files))
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
		writeErr(w, 404, fmt.Sprintf("no such job %q", r.PathValue("id")))
	}
	return j
}

// snapshot copies the public part of j under the lock.
func (s *Server) snapshot(j *job) api.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return j.Job
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if j := s.lookup(w, r); j != nil {
		writeJSON(w, 200, s.snapshot(j))
	}
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		var err error
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			writeErr(w, 400, fmt.Sprintf("limit %q must be a positive integer", v))
			return
		}
	}
	s.mu.Lock()
	list := make([]api.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j.Job)
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
		for i, q := range s.queue {
			if q == j {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				break
			}
		}
		s.finish(j, api.Canceled, nil, nil, "canceled while queued")
	case j.kill != nil:
		j.kill(api.Canceled)
	}
	s.mu.Unlock()
	writeJSON(w, 200, s.snapshot(j))
}

// finish records a terminal state. Caller holds s.mu.
func (s *Server) finish(j *job, state string, code, sig *int, msg string) {
	now := time.Now().UTC()
	j.State, j.ExitCode, j.Signal, j.EndedAt, j.kill = state, code, sig, &now, nil
	if msg != "" {
		j.Error = msg
	}
	if err := s.persist(j); err != nil && j.Error == "" {
		j.Error = "persisting job state: " + err.Error()
	}
	close(j.done)
}
