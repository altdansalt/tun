package server

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/workspace"
)

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) blobPath(sha string) string {
	return filepath.Join(s.cfg.DataDir, "blobs", sha+".tar.gz")
}

func (s *Server) headBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !shaRE.MatchString(sha) || !s.useBlob(sha) {
		w.WriteHeader(404)
		return
	}
	w.WriteHeader(200)
}

func (s *Server) putBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !shaRE.MatchString(sha) {
		writeErr(w, 400, "blob name must be a lowercase hex sha256")
		return
	}
	if r.ContentLength > s.cfg.MaxUpload {
		writeErr(w, 413, s.tooBig(r.ContentLength))
		return
	}
	tmp, err := os.CreateTemp(filepath.Join(s.cfg.DataDir, "blobs"), "upload-*")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r.Body, s.cfg.MaxUpload+1))
	switch {
	case err != nil:
		writeErr(w, 400, "reading upload: "+err.Error())
	case n > s.cfg.MaxUpload:
		writeErr(w, 413, s.tooBig(n))
	case hex.EncodeToString(h.Sum(nil)) != sha:
		writeErr(w, 400, fmt.Sprintf("uploaded bytes hash to %x, not %s", h.Sum(nil), sha))
	default:
		if err := tmp.Close(); err != nil {
			writeErr(w, 500, err.Error())
		} else if err := os.Rename(tmp.Name(), s.blobPath(sha)); err != nil {
			writeErr(w, 500, err.Error())
		} else {
			w.WriteHeader(201)
		}
	}
}

func (s *Server) tooBig(n int64) string {
	return fmt.Sprintf("workspace upload is %d+ bytes, over the server limit of %d bytes (tun serve --max-upload); shrink it (gitignore build outputs) or use --no-workspace", n, s.cfg.MaxUpload)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var req api.CreateJob
	body, limit := io.Reader(http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload)), int64(8<<20)
	if r.Header.Get("Content-Encoding") == "gzip" { // tun bazel's manifests
		z, err := gzip.NewReader(body)
		if err != nil {
			writeErr(w, 400, "bad job JSON: "+err.Error())
			return
		}
		body, limit = z, 256<<20
	}
	dec := json.NewDecoder(io.LimitReader(body, limit))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, 400, "bad job JSON: "+err.Error())
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		writeErr(w, 400, "bad job JSON: unexpected data after the job object")
		return
	}
	if req.Bazel != nil {
		if code, err := s.checkBazelJob(&req, clientOf(r)); err != nil {
			writeErr(w, code, err.Error())
			return
		}
		if missing := s.missingBlobs(req.Bazel.Manifest); len(missing) > 0 {
			writeJSON(w, 409, api.MissingBlobs{Error: fmt.Sprintf("%d file contents are not on the server: upload them (POST /v1/cas) and create the job again", len(missing)), Missing: missing})
			return
		}
	} else if (len(req.Argv) == 0) == (req.Script == "") {
		writeErr(w, 400, "exactly one of argv or script is required")
		return
	}
	if err := checkStrings(req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cwd := path.Clean("/" + req.Cwd)[1:]
	if strings.HasPrefix(req.Cwd, "/") || strings.Contains("/"+req.Cwd+"/", "/../") {
		writeErr(w, 400, fmt.Sprintf("cwd %q must be relative to the workspace root, without ..", req.Cwd))
		return
	}
	if cwd == "" {
		cwd = "."
	}
	if err := checkService(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.TimeoutSec <= 0 && !req.Service {
		req.TimeoutSec = 30 * 60
	}
	if req.TimeoutSec < 0 {
		req.TimeoutSec = 0 // none (services)
	}
	if req.TimeoutSec > api.MaxTimeoutSec {
		writeErr(w, 400, fmt.Sprintf("timeout_sec %d is over the maximum of %d (7 days)", req.TimeoutSec, api.MaxTimeoutSec))
		return
	}
	if len(req.Outputs) > api.MaxOutputs {
		writeErr(w, 400, fmt.Sprintf("at most %d outputs, got %d", api.MaxOutputs, len(req.Outputs)))
		return
	}
	for _, p := range req.Outputs {
		if err := api.ValidFilePath(p); err != nil {
			writeErr(w, 400, "outputs: "+err.Error())
			return
		}
	}
	if req.Workspace != "" && !shaRE.MatchString(req.Workspace) {
		writeErr(w, 400, "workspace must be a sha256 of an uploaded blob")
		return
	}
	j := &job{done: make(chan struct{}), Env: req.Env, Job: api.Job{
		ID: newID(), State: api.Queued, CreatedAt: time.Now().UTC(), Argv: req.Argv, Script: req.Script,
		Cwd: cwd, TimeoutSec: req.TimeoutSec, Workspace: req.Workspace, Sandbox: s.cfg.Sandbox,
		Service: req.Service, Client: clientOf(r), Outputs: req.Outputs,
	}}
	if b := req.Bazel; b != nil {
		j.bz, j.Argv = b, append([]string{"bazel"}, b.Args...)
		j.Session, j.SessionID = b.Session, sessionID(j.Client, b.Root, b.Session)
	}
	if j.Service {
		j.Runner = api.LocalRunner // services always run on the hub
		// Cheap early check so a full server doesn't unpack a workspace it will reject.
		s.mu.Lock()
		err := s.serviceSlotFree()
		s.mu.Unlock()
		if err != nil {
			writeErr(w, 429, err.Error())
			return
		}
	}
	if code, err := s.prepare(j); err != nil {
		workspace.RemoveAll(s.jobDir(j.ID))
		writeErr(w, code, err.Error())
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		writeErr(w, 503, "server is shutting down")
		return
	}
	if j.Service {
		if code, err := s.admitService(j, req.Ports); err != nil {
			s.mu.Unlock()
			workspace.RemoveAll(s.jobDir(j.ID))
			writeErr(w, code, err.Error())
			return
		}
	}
	if err := s.persist(j); err != nil {
		if j.Service {
			s.releaseService(j)
			s.svc.running--
		}
		s.mu.Unlock()
		writeErr(w, 500, err.Error())
		return
	}
	s.jobs[j.ID] = j
	if j.Service {
		s.startService(j)
	} else {
		s.queue = append(s.queue, j)
		s.schedule()
	}
	snap := s.view(j, time.Now())
	s.mu.Unlock()
	writeJSON(w, 201, snap)
}

// checkStrings rejects NUL bytes (they can't be passed to exec) and malformed env names.
func checkStrings(req api.CreateJob) error {
	for _, a := range req.Argv {
		if strings.Contains(a, "\x00") {
			return fmt.Errorf("argv %q contains a NUL byte", a)
		}
	}
	if strings.Contains(req.Script, "\x00") || strings.Contains(req.Cwd, "\x00") {
		return fmt.Errorf("script and cwd must not contain NUL bytes")
	}
	for k, v := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.Contains(v, "\x00") {
			return fmt.Errorf("env %q: names must be non-empty without = or NUL, values without NUL", k)
		}
	}
	return nil
}

// prepare creates the job dir and extracts the workspace into sandbox/work. A hub without
// local slots only checks the blob exists: the runner unpacks it (and enforces the limit).
func (s *Server) prepare(j *job) (int, error) {
	if err := prepareJobDir(s.cfg.DataDir, j.ID); err != nil {
		return 500, err
	}
	if j.Workspace == "" {
		return 0, nil
	}
	if s.cfg.HubOnly && !j.Service {
		if !s.useBlob(j.Workspace) {
			return 400, fmt.Errorf("workspace blob %s not uploaded (PUT /v1/blobs/%s first)", j.Workspace, j.Workspace)
		}
		return 0, nil
	}
	f, err := s.openBlob(j.Workspace)
	if err != nil {
		return 400, fmt.Errorf("workspace blob %s not uploaded (PUT /v1/blobs/%s first)", j.Workspace, j.Workspace)
	}
	defer f.Close()
	if err := workspace.UnpackLimit(f, filepath.Join(s.sandboxDir(j.ID), "work"), s.cfg.MaxWorkspace); err != nil {
		return s.unpackErr(err)
	}
	return 0, nil
}

// openOwn opens one of the server's own files in a job dir. It never follows a symlink, and
// refuses anything but a regular file with a single link, so even a job that could write
// the job dir (sandbox none) can't point the server at another file.
func openOwn(name string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(name, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
	} else if st, ok := fi.Sys().(*syscall.Stat_t); err == nil && ok && st.Nlink != 1 {
		err = fmt.Errorf("%s has %d hard links, want 1", name, st.Nlink)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// schedule starts queued jobs while there are free local slots, and wakes runners waiting
// for work. Caller holds s.mu.
func (s *Server) schedule() {
	for i := 0; !s.closed && s.running < s.cfg.MaxJobs && i < len(s.queue); {
		j := s.queue[i]
		if j.bz != nil && !s.claimSession(j) {
			i++ // its session is busy (or full): later jobs may go first
			continue
		}
		s.queue = slices.Delete(s.queue, i, i+1)
		j.Runner = api.LocalRunner
		s.running++
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if j.bz != nil {
				s.runBazel(j)
			} else {
				s.run(j)
			}
			s.mu.Lock()
			s.running--
			s.schedule()
			s.mu.Unlock()
		}()
	}
	s.dispatch()
}

func (s *Server) env(j *job) []string {
	m := jobEnv(s.cfg.DataDir, j.ID, j.Env)
	portEnv(j, m)
	return envList(m)
}

// run executes a job that has been taken off the queue (or admitted as a service).
func (s *Server) run(j *job) {
	p, err := newProc(execSpec{
		DataDir: s.cfg.DataDir, ID: j.ID, Argv: j.Argv, Script: j.Script, Cwd: j.Cwd, Env: s.env(j),
		TimeoutSec: j.TimeoutSec, Sandbox: s.cfg.Sandbox, PrivateNet: s.cfg.PrivateNet, MaxLog: s.cfg.MaxLog, KillGrace: s.cfg.KillGrace,
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.finish(j, api.Failed, nil, nil, err.Error())
		return
	}
	if s.closed {
		p.abort()
		return
	}
	if j.cancelReq { // canceled after leaving the queue, before starting
		p.abort()
		s.finish(j, api.Canceled, nil, nil, "canceled before it started")
		return
	}
	if err := p.start(); err != nil {
		s.finish(j, api.Failed, nil, nil, "starting command: "+err.Error())
		return
	}
	j.net = p.net
	now := time.Now().UTC()
	j.State, j.StartedAt = api.Running, &now
	s.persist(j)
	j.kill = p.kill // called with s.mu held
	s.mu.Unlock()
	res := p.wait()
	if p.net != nil {
		s.mu.Lock()
		j.net = nil
		s.mu.Unlock()
		p.net.Close()
	}
	s.mu.Lock()
	if res.State == "crash" {
		j.kill = nil // leave state as running: the next server marks it failed
		return
	}
	s.finish(j, res.State, res.Code, res.Sig, res.Msg)
}

// logs serves GET /v1/jobs/{id}/stdout|stderr?offset=N&follow=1&tail=N. With tail, the
// response starts at the last N lines (instead of offset); X-Tun-Offset is the start offset.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	stream := r.PathValue("stream")
	if stream != "stdout" && stream != "stderr" {
		writeErr(w, 404, "stream must be stdout or stderr")
		return
	}
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	var offset int64
	q := r.URL.Query()
	if v := q.Get("offset"); v != "" {
		var err error
		if offset, err = strconv.ParseInt(v, 10, 64); err != nil || offset < 0 {
			writeErr(w, 400, fmt.Sprintf("offset %q must be a non-negative integer", v))
			return
		}
	}
	tail := -1
	if v := q.Get("tail"); v != "" {
		var err error
		if tail, err = strconv.Atoi(v); err != nil || tail < 0 {
			writeErr(w, 400, fmt.Sprintf("tail %q must be a non-negative integer", v))
			return
		}
		if q.Has("offset") {
			writeErr(w, 400, "give offset or tail, not both")
			return
		}
	}
	follow := false
	if v := q.Get("follow"); v != "" {
		var err error
		if follow, err = strconv.ParseBool(v); err != nil {
			writeErr(w, 400, fmt.Sprintf("follow %q must be 1, 0, true or false", v))
			return
		}
	}
	f, err := openOwn(filepath.Join(s.jobDir(j.ID), stream), os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		writeErr(w, 404, s.noSuchJob(j.ID)) // expired since the lookup
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer f.Close()
	if tail >= 0 {
		if offset, err = tailStart(f, tail); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("X-Tun-Offset", strconv.FormatInt(offset, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	for {
		var finished bool
		select {
		case <-j.done:
			finished = true
		default:
		}
		if _, err := io.Copy(w, f); err != nil {
			return
		}
		rc.Flush()
		if finished || !follow {
			return
		}
		select {
		case <-j.done:
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
}

// tailStart returns the offset where the last n lines of f start. A final line without a
// trailing newline counts as a line.
func tailStart(f *os.File, n int) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	end := fi.Size()
	if n == 0 {
		return end, nil
	}
	buf := make([]byte, 32<<10)
	for pos := end; pos > 0; {
		k := min(int64(len(buf)), pos)
		pos -= k
		if _, err := f.ReadAt(buf[:k], pos); err != nil {
			return 0, err
		}
		for i := k - 1; i >= 0; i-- {
			if buf[i] != '\n' || pos+i == end-1 {
				continue // the newline ending the last line doesn't start one
			}
			if n--; n == 0 {
				return pos + i + 1, nil
			}
		}
	}
	return 0, nil
}
