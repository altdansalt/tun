package server

// Hub side of runners (tun runner): machines that pull jobs from this server and run them.
// The hub can't connect to runners, so everything is runner-initiated:
//   - POST /v1/runner/poll long-polls for work and is the runner's heartbeat. The body lists
//     the jobs the runner holds; the answer carries new assignments and ids to kill.
//   - An assigned job stays "queued" until the runner calls .../started (the commit point:
//     a job canceled before that is never started). If the runner's next poll doesn't list
//     it, the assignment was lost and the job goes back to the front of the queue.
//   - The runner pushes logs (POST .../stdout?offset=N, idempotent by offset), the declared
//     outputs as a tar (PUT .../outputs), and finally the end state (POST .../done).
//   - A runner that hasn't polled for RunnerDeadAfter is lost: its running jobs fail, its
//     unstarted ones are requeued. Timeouts are enforced by the runner, and by the hub as a
//     backstop.
// Job state, logs and outputs live on the hub, so status, logs, cancel and files work as for
// local jobs; files of a runner job are limited to the outputs it declared (tun run --out).

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/workspace"
)

// runnerState is the hub's view of one runner. Guarded by Server.mu.
type runnerState struct {
	name, instance, sandbox, source string
	privateNet                      bool
	slots                           int
	lastSeen                        time.Time
	jobs                            map[string]*job // assigned to it and not ended
	seq                             int             // polls received; only the latest may assign
	pollSeq                         int64           // highest RunnerPoll.Seq seen from this instance
	waiting                         int             // poll handlers waiting for work
	held                            map[string]bool // job ids the latest poll listed
	pending                         []*job          // assigned while a poll waited; it sends them
	wake                            chan struct{}   // buffered(1): wakes a waiting poll
}

func newRunnerState(name string) *runnerState {
	return &runnerState{name: name, jobs: map[string]*job{}, wake: make(chan struct{}, 1)}
}

func (rn *runnerState) poke() {
	if rn.waiting > 0 {
		select {
		case rn.wake <- struct{}{}:
		default:
		}
	}
}

func isRunnerJob(j *job) bool { return j.Runner != "" && j.Runner != api.LocalRunner }

const outputsFile = "outputs.tar"

// dispatch hands queued jobs to runners whose poll is waiting, so a job given to an idle
// runner leaves the queue at once. Caller holds s.mu.
func (s *Server) dispatch() {
	for _, rn := range s.runners {
		if len(s.queue) == 0 {
			return
		}
		if rn.waiting > 0 {
			if js := s.assign(rn, rn.held); len(js) > 0 {
				rn.pending = append(rn.pending, js...)
				rn.poke()
			}
		}
	}
}

// takeJobs returns the assignments for rn's current poll: those made while it waited (still
// valid) and new ones. Caller holds s.mu.
func (s *Server) takeJobs(rn *runnerState, held map[string]bool) []api.RunnerJob {
	out := []api.RunnerJob{}
	for _, j := range rn.pending {
		if rn.jobs[j.ID] == j && j.State == api.Queued {
			out = append(out, s.assignment(j))
		}
	}
	rn.pending = nil
	for _, j := range s.assign(rn, held) {
		out = append(out, s.assignment(j))
	}
	return out
}

// runnerJobEnded is called by finish (s.mu held): the runner's slot is free again.
func (s *Server) runnerJobEnded(j *job) {
	if rn := s.runners[j.Runner]; rn != nil && rn.jobs[j.ID] == j {
		delete(rn.jobs, j.ID)
		rn.poke()
	}
}

// adopt keeps a job a previous hub process left running on a runner: the runner may still
// be running it and report back. If no runner claims it within RunnerDeadAfter, it fails
// like any job of a lost runner. Called from recover.
func (s *Server) adopt(j *job) {
	rn := s.runners[j.Runner]
	if rn == nil {
		rn = newRunnerState(j.Runner)
		rn.lastSeen = time.Now()
		s.runners[j.Runner] = rn
	}
	rn.jobs[j.ID] = j
}

func (s *Server) runnerPoll(w http.ResponseWriter, r *http.Request) {
	var p api.RunnerPoll
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&p); err != nil {
		writeErr(w, 400, "bad poll JSON: "+err.Error())
		return
	}
	name := cleanName(p.Name)
	switch {
	case name == "" || name != p.Name || name == api.LocalRunner:
		writeErr(w, 400, fmt.Sprintf("runner name %q must be printable ASCII without spaces, and not %q", p.Name, api.LocalRunner))
		return
	case p.Slots < 0 || p.Slots > 1024:
		writeErr(w, 400, fmt.Sprintf("slots %d must be 0-1024", p.Slots))
		return
	}
	held := map[string]bool{}
	for _, id := range p.Running {
		held[id] = true
	}
	arrival := time.Now()
	s.mu.Lock()
	rn, err := s.register(name, p, clientOf(r), arrival)
	if err != nil {
		s.mu.Unlock()
		writeErr(w, 409, err.Error())
		return
	}
	if p.Seq > 0 && p.Seq <= rn.pollSeq {
		s.mu.Unlock()
		writeJSON(w, 200, api.RunnerPollResponse{Jobs: []api.RunnerJob{}, Cancel: []string{}})
		return // stale: a newer poll has been handled
	}
	rn.pollSeq = max(rn.pollSeq, p.Seq)
	rn.seq++
	seq := rn.seq
	rn.poke() // an older poll of this runner (its client gave up on it) should end
	rn.held = held
	resp := api.RunnerPollResponse{Cancel: s.reconcile(rn, held, arrival)}
	resp.Jobs = s.takeJobs(rn, held)
	deadline := time.NewTimer(s.cfg.RunnerPollWait)
	defer deadline.Stop()
	for len(resp.Jobs) == 0 && len(resp.Cancel) == 0 && !s.closed && rn.seq == seq {
		rn.waiting++
		s.mu.Unlock()
		timedOut := false
		select {
		case <-rn.wake:
		case <-deadline.C:
			timedOut = true
		case <-r.Context().Done():
			timedOut = true
		case <-s.quit:
		}
		s.mu.Lock()
		rn.waiting--
		if s.runners[name] != rn || rn.seq != seq {
			break // dropped or superseded meanwhile
		}
		resp.Cancel = s.reconcile(rn, held, arrival)
		resp.Jobs = s.takeJobs(rn, held)
		if timedOut {
			break
		}
	}
	if s.runners[name] == rn {
		rn.lastSeen = time.Now()
	}
	s.mu.Unlock()
	writeJSON(w, 200, resp)
}

// register records a poll from runner name. A different instance under a known name is the
// same runner restarted if it comes from the same source (or the old one is dead); its old
// jobs are then lost. Otherwise two live runners share a name, which is refused. Caller
// holds s.mu.
func (s *Server) register(name string, p api.RunnerPoll, source string, now time.Time) (*runnerState, error) {
	rn := s.runners[name]
	if rn != nil && rn.instance != "" && rn.instance != p.Instance {
		if rn.source != source && now.Sub(rn.lastSeen) < s.cfg.RunnerDeadAfter {
			return nil, fmt.Errorf("runner name %q is in use by a live runner at %s (last poll %.0fs ago); give each runner its own --name",
				name, rn.source, now.Sub(rn.lastSeen).Seconds())
		}
		s.dropRunner(rn, fmt.Sprintf("runner %s restarted while the job was running", name))
		rn = nil
	}
	if rn == nil {
		rn = newRunnerState(name)
		s.runners[name] = rn
	}
	rn.instance, rn.slots, rn.sandbox, rn.source, rn.lastSeen = p.Instance, p.Slots, cleanName(p.Sandbox), source, now
	rn.privateNet = p.NetworkIsolation
	return rn, nil
}

// dropRunner forgets a runner: its running jobs fail with msg, unstarted ones are requeued.
// Caller holds s.mu.
func (s *Server) dropRunner(rn *runnerState, msg string) {
	delete(s.runners, rn.name)
	for _, j := range rn.jobs {
		if j.State == api.Queued {
			s.requeue(j)
		} else {
			s.finish(j, api.Failed, nil, nil, msg)
		}
	}
	rn.jobs = map[string]*job{}
	rn.poke()
}

// reconcile compares what runner rn holds (held, as of a poll that arrived at arrival) with
// the hub's books, and returns the held job ids the runner must kill and drop. Caller holds
// s.mu.
func (s *Server) reconcile(rn *runnerState, held map[string]bool, arrival time.Time) []string {
	for id, j := range rn.jobs {
		switch {
		case held[id]:
		case j.State == api.Queued:
			if j.assignedAt.Before(arrival) {
				s.requeue(j) // the poll answer carrying it never reached the runner
			}
		default:
			s.finish(j, api.Failed, nil, nil, fmt.Sprintf("runner %s no longer has the job (did it restart?)", rn.name))
		}
	}
	cancel := []string{}
	for id := range held {
		j := s.jobs[id]
		if j == nil || api.Terminal(j.State) || j.Runner != rn.name || j.killReq != "" {
			cancel = append(cancel, id)
		}
	}
	sort.Strings(cancel)
	return cancel
}

// requeue puts an assigned but unstarted job back in the queue, in creation order. Caller
// holds s.mu.
func (s *Server) requeue(j *job) {
	if rn := s.runners[j.Runner]; rn != nil {
		delete(rn.jobs, j.ID)
	}
	j.Runner, j.assignedAt = "", time.Time{}
	i := sort.Search(len(s.queue), func(i int) bool { return s.queue[i].ID > j.ID })
	s.queue = slices.Insert(s.queue, i, j)
	s.schedule()
}

// assign hands queued jobs to rn up to its free slots. Caller holds s.mu.
func (s *Server) assign(rn *runnerState, held map[string]bool) []*job {
	busy := len(rn.jobs)
	for id := range held {
		if rn.jobs[id] == nil {
			busy++ // still being killed
		}
	}
	var out []*job
	for free := rn.slots - busy; free > 0 && len(s.queue) > 0 && !s.closed; free-- {
		j := s.queue[0]
		s.queue = s.queue[1:]
		j.Runner, j.assignedAt = rn.name, time.Now()
		rn.jobs[j.ID] = j
		out = append(out, j)
	}
	return out
}

// assignment is what a runner gets to run j.
func (s *Server) assignment(j *job) api.RunnerJob {
	return api.RunnerJob{
		ID: j.ID, Argv: j.Argv, Script: j.Script, Cwd: j.Cwd, Env: j.Env, TimeoutSec: j.TimeoutSec,
		Workspace: j.Workspace, Outputs: j.Outputs,
		MaxLog: s.cfg.MaxLog, MaxWorkspace: s.cfg.MaxWorkspace, MaxDownload: s.maxDownload(),
	}
}

func (s *Server) maxDownload() int64 {
	if s.cfg.MaxDownload > 0 {
		return s.cfg.MaxDownload
	}
	return DefaultMaxDownload
}

// runnerJob looks up the job of a /v1/runner/jobs/{id}/... request and checks it is
// assigned to the requesting runner (and, with running, started). It writes 410 Gone
// otherwise: the runner should kill and drop the job.
func (s *Server) runnerJob(w http.ResponseWriter, r *http.Request, running bool) *job {
	name := r.Header.Get(api.HeaderRunner)
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[r.PathValue("id")]
	switch {
	case j == nil:
		writeErr(w, 410, fmt.Sprintf("no such job %q", r.PathValue("id")))
	case j.Runner != name:
		writeErr(w, 410, fmt.Sprintf("job %s is not assigned to runner %q", j.ID, name))
	case api.Terminal(j.State):
		writeErr(w, 410, fmt.Sprintf("job %s has already ended (%s)", j.ID, j.State))
	case running && j.State != api.Running:
		writeErr(w, 410, fmt.Sprintf("job %s is %s, not running", j.ID, j.State))
	default:
		return j
	}
	return nil
}

// runnerStarted serves POST /v1/runner/jobs/{id}/started: the runner is about to start the
// command. Idempotent; 410 if the job was canceled or reassigned meanwhile.
func (s *Server) runnerStarted(w http.ResponseWriter, r *http.Request) {
	j := s.runnerJob(w, r, false)
	if j == nil {
		return
	}
	s.mu.Lock()
	if j.State == api.Queued {
		now := time.Now().UTC()
		j.State, j.StartedAt = api.Running, &now
		if rn := s.runners[j.Runner]; rn != nil && rn.sandbox != "" {
			j.Sandbox = rn.sandbox
		}
		s.persist(j)
		// The hub's unpacked copy of the workspace (if any) is no longer needed.
		s.wg.Add(1)
		go func() { defer s.wg.Done(); workspace.RemoveAll(filepath.Join(s.sandboxDir(j.ID), "work")) }()
	}
	s.mu.Unlock()
	writeJSON(w, 200, struct{}{})
}

// maxChunk bounds one log chunk from a runner.
const maxChunk = 8 << 20

// runnerLog serves POST /v1/runner/jobs/{id}/{stdout|stderr}?offset=N: append the body, which
// starts at byte N of the runner's log. Bytes the hub already has are skipped, so a retried
// chunk is harmless; a chunk past the end is a 409 whose body says where to resume.
func (s *Server) runnerLog(w http.ResponseWriter, r *http.Request) {
	stream := r.PathValue("stream")
	if stream != "stdout" && stream != "stderr" {
		writeErr(w, 404, "stream must be stdout or stderr")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeErr(w, 400, "offset must be a non-negative integer")
		return
	}
	j := s.runnerJob(w, r, true)
	if j == nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxChunk+1))
	if err != nil {
		writeErr(w, 400, "reading chunk: "+err.Error())
		return
	}
	if len(body) > maxChunk {
		writeErr(w, 413, fmt.Sprintf("log chunks are at most %d bytes", maxChunk))
		return
	}
	j.logMu.Lock()
	defer j.logMu.Unlock()
	f, err := openOwn(filepath.Join(s.jobDir(j.ID), stream), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	size, end := fi.Size(), offset+int64(len(body))
	// The runner caps logs itself (at MaxLog plus a marker); this is the hub's backstop.
	limit := int64(1<<63 - 1)
	if s.cfg.MaxLog > 0 {
		limit = s.cfg.MaxLog + 4096
	}
	switch {
	case size >= limit:
		writeJSON(w, 200, api.LogAck{Offset: max(end, size)})
		return
	case offset > size:
		writeJSON(w, 409, api.LogAck{Offset: size, Error: fmt.Sprintf("chunk starts at %d but the hub has %d bytes of %s", offset, size, stream)})
		return
	case end <= size:
		writeJSON(w, 200, api.LogAck{Offset: size})
		return
	}
	data := body[size-offset:]
	data = data[:min(int64(len(data)), limit-size)]
	if _, err := f.Write(data); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, api.LogAck{Offset: end})
}

// runnerOutputs serves PUT /v1/runner/jobs/{id}/outputs: a tar of the job's declared outputs,
// built by the runner with the same rules as the files endpoint.
func (s *Server) runnerOutputs(w http.ResponseWriter, r *http.Request) {
	j := s.runnerJob(w, r, true)
	if j == nil {
		return
	}
	limit := s.maxDownload() + 64<<20 // tar headers and padding
	tmp, err := os.CreateTemp(s.jobDir(j.ID), ".outputs-*")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, io.LimitReader(r.Body, limit+1))
	switch {
	case err != nil:
		writeErr(w, 400, "reading outputs: "+err.Error())
	case n > limit:
		writeErr(w, 413, fmt.Sprintf("outputs are over %d bytes", limit))
	default:
		if err := tmp.Close(); err != nil {
			writeErr(w, 500, err.Error())
		} else if err := os.Rename(tmp.Name(), filepath.Join(s.jobDir(j.ID), outputsFile)); err != nil {
			writeErr(w, 500, err.Error())
		} else {
			writeJSON(w, 200, struct{}{})
		}
	}
}

// runnerDone serves POST /v1/runner/jobs/{id}/done: the job's end state. A kill the hub
// requested decides the state. Repeating it is harmless.
func (s *Server) runnerDone(w http.ResponseWriter, r *http.Request) {
	var d api.RunnerDone
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&d); err != nil {
		writeErr(w, 400, "bad done JSON: "+err.Error())
		return
	}
	if !slices.Contains([]string{api.Exited, api.Failed, api.Timeout, api.Canceled}, d.State) {
		writeErr(w, 400, fmt.Sprintf("state %q must be exited, failed, timeout or canceled", d.State))
		return
	}
	j := s.runnerJob(w, r, false)
	if j == nil {
		return
	}
	s.mu.Lock()
	state, msg := d.State, d.Error
	if j.killReq != "" && state != j.killReq {
		state, msg = j.killReq, j.killReq
	}
	j.OutMissing, j.OutSkipped, j.OutError = d.OutputsMissing, d.OutputsSkipped, d.OutputsError
	s.finish(j, state, d.ExitCode, d.Signal, msg)
	s.mu.Unlock()
	writeJSON(w, 200, struct{}{})
}

// getBlob serves GET /v1/blobs/{sha} (runners fetch workspaces with it).
func (s *Server) getBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !shaRE.MatchString(sha) {
		writeErr(w, 400, "blob name must be a lowercase hex sha256")
		return
	}
	f, err := s.openBlob(sha)
	if err != nil {
		writeErr(w, 404, fmt.Sprintf("no blob %s", sha))
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(200)
	io.Copy(w, f)
}

// startReaper checks runners and runner jobs periodically until Close.
func (s *Server) startReaper() {
	tick := min(max(s.cfg.RunnerDeadAfter/10, 50*time.Millisecond), time.Second)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-s.quit:
				return
			case now := <-t.C:
				s.reap(now)
			}
		}
	}()
}

// reap drops runners that stopped polling and ends runner jobs the runner should have ended
// by now (a timeout or kill it never confirmed).
func (s *Server) reap(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slack := s.cfg.KillGrace + s.cfg.RunnerDeadAfter
	for _, rn := range s.runners {
		if gone := now.Sub(rn.lastSeen); gone > s.cfg.RunnerDeadAfter {
			s.dropRunner(rn, fmt.Sprintf("runner %s lost: no poll for %.0fs (tun serve --runner-timeout)", rn.name, gone.Seconds()))
			continue
		}
		for _, j := range rn.jobs {
			switch {
			case j.State != api.Running:
			case j.killReq != "" && now.Sub(j.killReqAt) > slack:
				s.finish(j, j.killReq, nil, nil, fmt.Sprintf("%s; runner %s did not confirm the kill", j.killReq, rn.name))
			case j.TimeoutSec > 0 && j.StartedAt != nil && now.Sub(*j.StartedAt) > time.Duration(j.TimeoutSec)*time.Second+slack:
				s.finish(j, api.Timeout, nil, nil, fmt.Sprintf("timed out after %ds (enforced by the hub: runner %s did not report the end)", j.TimeoutSec, rn.name))
			}
		}
	}
}

// runnerInfo lists live runners for healthz. Caller holds s.mu.
func (s *Server) runnerInfo() (slots, running int, list []api.RunnerInfo) {
	list = []api.RunnerInfo{}
	for _, rn := range s.runners {
		slots += rn.slots
		running += len(rn.jobs)
		list = append(list, api.RunnerInfo{Name: rn.name, Slots: rn.slots, Running: len(rn.jobs),
			LastSeen: rn.lastSeen.UTC(), Sandbox: rn.sandbox, Source: rn.source, NetworkIsolation: rn.privateNet})
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Name < list[b].Name })
	return slots, running, list
}

// covers reports whether a files request for p is within the declared output pattern o:
// p is o, or below something o matches.
func covers(o, p string) bool {
	o, p = path.Clean(o), path.Clean(p)
	return o == p || under(o, p)
}

// under reports whether name is something pattern matches or is inside one: pattern's
// elements match name's leading elements (path.Match, so a literal element matches itself).
func under(pattern, name string) bool {
	if pattern == "." {
		return true
	}
	pc, nc := strings.Split(pattern, "/"), strings.Split(name, "/")
	if len(nc) < len(pc) {
		return false
	}
	for i := range pc {
		if ok, _ := path.Match(pc[i], nc[i]); !ok {
			return false
		}
	}
	return true
}

// runnerFiles serves the files endpoint for a job that ran on a runner, from the outputs
// tar the runner uploaded: only paths within the job's declared outputs are available.
func (s *Server) runnerFiles(w http.ResponseWriter, j *job, paths []string) {
	s.mu.Lock()
	outputs, runner, outErr, skippedAll, gone := j.Outputs, j.Runner, j.OutError, j.OutSkipped, j.WorkDeleted
	s.mu.Unlock()
	for _, p := range paths {
		if !slices.ContainsFunc(outputs, func(o string) bool { return covers(o, p) }) {
			declared := "none"
			if len(outputs) > 0 {
				declared = strings.Join(outputs, " ")
			}
			writeErr(w, 409, fmt.Sprintf("job %s ran on runner %s, which keeps only the paths declared up front with tun run --out (declared: %s); %q is not among them: rerun with --out %s",
				j.ID, runner, declared, p, p))
			return
		}
	}
	if outErr != "" {
		writeErr(w, 413, outErr)
		return
	}
	if gone {
		writeErr(w, 410, fmt.Sprintf("job %s's outputs no longer exist (they are deleted some time after a job ends; see tun serve --keep-work)", j.ID))
		return
	}
	f, err := openOwn(filepath.Join(s.jobDir(j.ID), outputsFile), os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		f, err = nil, nil // never uploaded (the job failed before it ran): nothing matches
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f != nil {
		defer f.Close()
	}
	match := func(name string) bool {
		return slices.ContainsFunc(paths, func(p string) bool { return under(path.Clean(p), name) })
	}
	// First pass: which patterns matched, so missing ones go in the headers.
	found := make([]bool, len(paths))
	if f != nil {
		tr := tar.NewReader(f)
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			name := strings.TrimSuffix(hdr.Name, "/")
			for i, p := range paths {
				if under(path.Clean(p), name) {
					found[i] = true
				}
			}
		}
	}
	for _, sk := range skippedAll {
		name, _, _ := strings.Cut(sk, ": ")
		if match(name) {
			for i, p := range paths {
				if under(path.Clean(p), name) {
					found[i] = true
				}
			}
			w.Header().Add(api.HeaderSkipped, url.PathEscape(sk))
		}
	}
	for i, p := range paths {
		if !found[i] {
			w.Header().Add(api.HeaderMissing, url.PathEscape(path.Clean(p)))
		}
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Trailer", completeTrailer)
	w.WriteHeader(200)
	tw := tar.NewWriter(w)
	if f != nil {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			panic(http.ErrAbortHandler)
		}
		tr := tar.NewReader(f)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				panic(http.ErrAbortHandler)
			}
			if !match(strings.TrimSuffix(hdr.Name, "/")) {
				continue
			}
			if err := tw.WriteHeader(hdr); err != nil {
				panic(http.ErrAbortHandler)
			}
			if _, err := io.Copy(tw, tr); err != nil {
				panic(http.ErrAbortHandler)
			}
		}
	}
	if err := tw.Close(); err != nil {
		panic(http.ErrAbortHandler)
	}
	w.Header().Set(completeTrailer, "ok")
}
