package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/workspace"
)

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) blobPath(sha string) string {
	return filepath.Join(s.cfg.DataDir, "blobs", sha+".tar.gz")
}

func (s *Server) headBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if _, err := os.Stat(s.blobPath(sha)); !shaRE.MatchString(sha) || err != nil {
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
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, 400, "bad job JSON: "+err.Error())
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		writeErr(w, 400, "bad job JSON: unexpected data after the job object")
		return
	}
	if (len(req.Argv) == 0) == (req.Script == "") {
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
	if req.TimeoutSec <= 0 {
		req.TimeoutSec = 30 * 60
	}
	if req.TimeoutSec > api.MaxTimeoutSec {
		writeErr(w, 400, fmt.Sprintf("timeout_sec %d is over the maximum of %d (7 days)", req.TimeoutSec, api.MaxTimeoutSec))
		return
	}
	if req.Workspace != "" && !shaRE.MatchString(req.Workspace) {
		writeErr(w, 400, "workspace must be a sha256 of an uploaded blob")
		return
	}
	j := &job{done: make(chan struct{}), Env: req.Env, Job: api.Job{
		ID: newID(), State: api.Queued, CreatedAt: time.Now().UTC(), Argv: req.Argv, Script: req.Script,
		Cwd: cwd, TimeoutSec: req.TimeoutSec, Workspace: req.Workspace, Sandbox: s.cfg.Sandbox,
	}}
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
	if err := s.persist(j); err != nil {
		s.mu.Unlock()
		writeErr(w, 500, err.Error())
		return
	}
	s.jobs[j.ID] = j
	s.queue = append(s.queue, j)
	s.schedule()
	snap := j.Job
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

// prepare creates the job dir and extracts the workspace into sandbox/work. The job only
// ever gets sandbox/ (see sandboxDir); stdout, stderr and job.json live beside it.
func (s *Server) prepare(j *job) (int, error) {
	dir, sb := s.jobDir(j.ID), s.sandboxDir(j.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return 500, err
	}
	for _, d := range []string{"", "work", "home", "tmp"} {
		if err := os.Mkdir(filepath.Join(sb, d), 0o700); err != nil {
			return 500, err
		}
	}
	for _, f := range []string{"stdout", "stderr"} {
		f, err := openOwn(filepath.Join(dir, f), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return 500, err
		}
		f.Close()
	}
	if j.Workspace == "" {
		return 0, nil
	}
	f, err := os.Open(s.blobPath(j.Workspace))
	if err != nil {
		return 400, fmt.Errorf("workspace blob %s not uploaded (PUT /v1/blobs/%s first)", j.Workspace, j.Workspace)
	}
	defer f.Close()
	if err := workspace.Unpack(f, filepath.Join(sb, "work")); err != nil {
		return 400, fmt.Errorf("unpacking workspace: %v", err)
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

// schedule starts queued jobs while there is capacity. Caller holds s.mu.
func (s *Server) schedule() {
	for !s.closed && s.running < s.cfg.MaxJobs && len(s.queue) > 0 {
		j := s.queue[0]
		s.queue = s.queue[1:]
		s.running++
		s.wg.Add(1)
		go s.run(j)
	}
}

func (s *Server) env(j *job) []string {
	dir := s.sandboxDir(j.ID)
	cache := filepath.Join(s.cfg.DataDir, "cache")
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "C.UTF-8"
	}
	user := os.Getenv("USER")
	m := map[string]string{
		"PATH": pathEnv, "LANG": lang, "TERM": "dumb", "USER": user,
		"HOME": filepath.Join(dir, "home"), "TMPDIR": filepath.Join(dir, "tmp"),
		"GOCACHE": filepath.Join(cache, "go-build"), "GOMODCACHE": filepath.Join(cache, "gomod"),
		"XDG_CACHE_HOME": filepath.Join(cache, "xdg"), "TUN_JOB_ID": j.ID,
	}
	for k, v := range j.Env {
		m[k] = v
	}
	env := make([]string, 0, len(m))
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	return env
}

// run executes a job that has been taken off the queue.
func (s *Server) run(j *job) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		s.running--
		s.schedule()
		s.mu.Unlock()
	}()
	dir, sb := s.jobDir(j.ID), s.sandboxDir(j.ID)
	argv := j.Argv
	if j.Script != "" {
		argv = []string{"bash", "-c", j.Script}
	}
	cache := filepath.Join(s.cfg.DataDir, "cache")
	spec := sandbox.Spec{
		Mode: s.cfg.Sandbox, Argv: argv, Env: s.env(j), Dir: filepath.Join(sb, "work", j.Cwd),
		Root: sb, Tmp: filepath.Join(sb, "tmp"), DataDir: s.cfg.DataDir,
		Caches: []string{filepath.Join(cache, "go-build"), filepath.Join(cache, "gomod"), filepath.Join(cache, "xdg")},
	}
	fail := func(msg string) {
		s.mu.Lock()
		s.finish(j, api.Failed, nil, nil, msg)
		s.mu.Unlock()
	}
	if fi, err := os.Stat(spec.Dir); err != nil || !fi.IsDir() {
		fail(fmt.Sprintf("cwd %q does not exist in the workspace", j.Cwd))
		return
	}
	stdout, err1 := openOwn(filepath.Join(dir, "stdout"), os.O_WRONLY|os.O_APPEND, 0)
	stderr, err2 := openOwn(filepath.Join(dir, "stderr"), os.O_WRONLY|os.O_APPEND, 0)
	if err := errors.Join(err1, err2); err != nil {
		fail(err.Error())
		return
	}
	defer stdout.Close()
	defer stderr.Close()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	cmd, err := sandbox.Start(spec, stdout, stderr)
	if err != nil {
		s.finish(j, api.Failed, nil, nil, "starting command: "+err.Error())
		s.mu.Unlock()
		return
	}
	pid := cmd.Process.Pid
	now := time.Now().UTC()
	j.State, j.StartedAt = api.Running, &now
	s.persist(j)
	endState := api.Exited
	killed := make(chan struct{})
	j.kill = func(state string) { // called with s.mu held
		if endState != api.Exited {
			return // already being killed
		}
		endState = state
		if state == "" {
			endState = "crash"
		}
		syscall.Kill(-pid, syscall.SIGTERM)
		close(killed)
	}
	s.mu.Unlock()

	waitDone := make(chan struct{})
	go func() {
		select {
		case <-waitDone:
			return
		case <-killed:
		}
		select {
		case <-waitDone:
		case <-time.After(s.cfg.KillGrace):
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
	timer := time.AfterFunc(time.Duration(j.TimeoutSec)*time.Second, func() {
		s.mu.Lock()
		if j.kill != nil {
			j.kill(api.Timeout)
		}
		s.mu.Unlock()
	})
	err = cmd.Wait()
	timer.Stop()
	close(waitDone)
	syscall.Kill(-pid, syscall.SIGKILL) // reap stragglers left in the group

	var code, sig *int
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			n := int(ws.Signal())
			sig = &n
		} else {
			n := ps.ExitCode()
			code = &n
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := ""
	switch endState {
	case "crash":
		j.kill = nil // leave state as running: the next server marks it failed
		return
	case api.Timeout:
		msg = fmt.Sprintf("timed out after %ds", j.TimeoutSec)
	case api.Canceled:
		msg = "canceled"
	}
	if err != nil && code == nil && sig == nil {
		msg = err.Error()
	}
	s.finish(j, endState, code, sig, msg)
}

// logs serves GET /v1/jobs/{id}/stdout|stderr?offset=N&follow=1.
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
	follow := false
	if v := q.Get("follow"); v != "" {
		var err error
		if follow, err = strconv.ParseBool(v); err != nil {
			writeErr(w, 400, fmt.Sprintf("follow %q must be 1, 0, true or false", v))
			return
		}
	}
	f, err := openOwn(filepath.Join(s.jobDir(j.ID), stream), os.O_RDONLY, 0)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
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
