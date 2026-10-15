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
	disk                            *api.Disk
	serviceSupport                  bool
	tunnels                         map[string]*serviceTunnel
	sessionSupport                  bool
	memoryAvailable                 int64
	load                            float64
	liveSessions                    int
	slots                           int
	lastSeen                        time.Time
	retiredAt                       time.Time       // when a newer instance took the name (or it was dropped)
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
	if rn := j.rn; rn != nil && rn.jobs[j.ID] == j {
		delete(rn.jobs, j.ID)
		if sess := s.sessions[j.SessionID]; sess != nil && sess.job == j {
			sess.job = nil
			if s.runners[rn.name] != rn {
				s.forgetSession(sess, fmt.Sprintf("its runner %s restarted", rn.name))
			}
		}
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
	j.rn = rn
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
	// A retired instance (superseded by a newer one, or dropped) only heartbeats for the
	// jobs it still holds; nothing it says touches the current instance.
	if old := s.retired[p.Instance]; old != nil && p.Instance != "" {
		resp := api.RunnerPollResponse{Jobs: []api.RunnerJob{}, Cancel: []string{}, Superseded: fmt.Sprintf(
			"runner %s: this instance was superseded by a newer one with the same name (another tun runner --name %s, or a restart); it gets no more work",
			old.name, old.name)}
		if old.name == name && (p.Seq == 0 || p.Seq > old.pollSeq) {
			old.pollSeq, old.lastSeen = max(old.pollSeq, p.Seq), arrival
			resp.Cancel = s.reconcile(old, held, arrival)
		}
		s.mu.Unlock()
		writeJSON(w, 200, resp)
		return
	}
	if rn := s.runners[name]; rn != nil && rn.instance == p.Instance && p.Seq > 0 && p.Seq <= rn.pollSeq {
		s.mu.Unlock()
		writeJSON(w, 200, api.RunnerPollResponse{Jobs: []api.RunnerJob{}, Cancel: []string{}})
		return // stale: a newer poll of this instance has been handled
	}
	rn := s.register(name, p, clientOf(r), arrival)
	rn.pollSeq = max(rn.pollSeq, p.Seq)
	rn.seq++
	seq := rn.seq
	rn.poke() // an older poll of this runner (its client gave up on it) should end
	rn.held = held
	resp := api.RunnerPollResponse{Cancel: s.reconcile(rn, held, arrival)}
	resp.Jobs = s.takeJobs(rn, held)
	deadline := time.NewTimer(s.cfg.RunnerPollWait)
	defer deadline.Stop()
	for len(resp.Jobs) == 0 && len(resp.Cancel) == 0 && !rn.pendingTunnels() && !s.runnerNeedsRevocation(rn, p.Sessions) && !s.closed && rn.seq == seq {
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
	for _, t := range rn.tunnels {
		if !t.sent {
			resp.Tunnels = append(resp.Tunnels, t.spec)
			t.sent = true
		}
	}
	resp.Sessions = s.runnerSessionLeases(rn)
	s.mu.Unlock()
	writeJSON(w, 200, resp)
}

// register records a poll from runner name. A new instance under a known name takes it
// over (newest wins): if the old instance is gone (overdue, or the new one says it replaces
// it) its jobs fail now; if it may still be alive it is retired: it gets no more work but
// can finish (and report) the jobs it holds. Caller holds s.mu.
func (s *Server) register(name string, p api.RunnerPoll, source string, now time.Time) *runnerState {
	if prev := s.retired[p.Previous]; prev != nil && p.Previous != "" && prev.name == name && len(prev.jobs) > 0 {
		s.dropRunner(prev, fmt.Sprintf("runner %s restarted while the job was running", name))
	}
	rn := s.runners[name]
	if rn != nil && rn.instance != "" && rn.instance != p.Instance {
		if p.Previous == rn.instance || now.Sub(rn.lastSeen) > s.cfg.RunnerDeadAfter {
			s.dropRunner(rn, fmt.Sprintf("runner %s restarted while the job was running", name))
		} else {
			s.retire(rn, now)
		}
		rn = nil
	}
	if rn == nil {
		rn = newRunnerState(name)
		s.runners[name] = rn
	}
	rn.instance, rn.slots, rn.sandbox, rn.source, rn.lastSeen = p.Instance, p.Slots, cleanName(p.Sandbox), source, now
	rn.serviceSupport = p.ServiceSupport
	rn.privateNet, rn.disk = p.NetworkIsolation, p.Disk
	rn.sessionSupport, rn.memoryAvailable, rn.load, rn.liveSessions = p.SessionSupport, p.MemoryAvailable, p.Load, len(p.Sessions)
	for id, sess := range s.sessions {
		if sess.runner != rn {
			continue
		}
		if sess.stopping && !slices.Contains(p.Sessions, id) {
			delete(s.sessions, id)
		}
		if p.Slots == 0 && sess.job == nil {
			s.forgetSession(sess, fmt.Sprintf("its runner %s has no slots now (draining or --slots 0)", name))
		}
	}
	s.schedule()

	return rn
}

// retire moves rn out of the way of a newer instance: its unstarted jobs are requeued, its
// running ones stay with it, and its later polls are told it was superseded. Caller holds
// s.mu.
func (s *Server) retire(rn *runnerState, now time.Time) {
	if s.runners[rn.name] == rn {
		delete(s.runners, rn.name)
	}
	if rn.instance != "" {
		s.retired[rn.instance] = rn
	}
	rn.retiredAt = now
	for _, j := range rn.jobs {
		if j.State == api.Queued {
			s.requeue(j)
		}
	}
	for _, sess := range s.sessions {
		if sess.runner == rn && sess.job == nil {
			s.forgetSession(sess, fmt.Sprintf("its runner %s restarted or left", rn.name))
		}
	}
	rn.poke()
	s.schedule()
}

// dropRunner gives up on a runner instance: it is retired, and its running jobs end (failed
// with msg, or with the result it already reported via .../exited). Caller holds s.mu.
func (s *Server) dropRunner(rn *runnerState, msg string) {
	for _, sess := range s.sessions {
		if sess.runner == rn {
			s.forgetSession(sess, fmt.Sprintf("its runner %s was lost (%s)", rn.name, msg))
		}
	}
	s.retire(rn, time.Now())
	for _, j := range rn.jobs {
		if d := j.exited; d != nil {
			s.finishRunnerJob(j, *d, msg+" before it finished shipping the job's logs and outputs")
		} else {
			s.finish(j, api.Failed, nil, nil, msg)
		}
	}
	rn.jobs = map[string]*job{}
	s.schedule()
}

// finishRunnerJob ends j as the runner reported (a kill the hub requested decides the state),
// with note appended to the message. Caller holds s.mu.
func (s *Server) finishRunnerJob(j *job, d api.RunnerDone, note string) {
	state, msg := d.State, d.Error
	if j.killReq != "" && state != j.killReq {
		state, msg = j.killReq, j.killReq
	}
	if note != "" {
		msg = strings.TrimPrefix(msg+"; "+note, "; ")
	}
	if j.bz != nil {
		if sess := s.sessions[j.SessionID]; sess != nil && sess.job == j {
			sess.LastUsed = time.Now().UTC()
			sess.LastJob = j.ID
			sess.Commands++
			if d.Session != nil {
				sess.Commands = d.Session.Commands
				sess.CreatedAt = d.Session.CreatedAt
			}
		}
		j.Summary = d.Summary
		if len(d.BEP) > 0 {
			if err := writeOwn(filepath.Join(s.jobDir(j.ID), "bep.json"), d.BEP); err != nil {
				msg = joinMsg(msg, err.Error())
			}
		}
	}
	if j.bz != nil && !d.SessionReady {
		if sess := s.sessions[j.SessionID]; sess != nil && sess.job == j {
			sess.stopping = true
			if sess.runner != nil {
				sess.runner.poke()
			}
		}
	}
	s.finish(j, state, d.ExitCode, d.Signal, msg)
	if j.bz != nil {
		s.schedule()
	}
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
		if j == nil || api.Terminal(j.State) || j.rn != rn || j.killReq != "" {
			cancel = append(cancel, id)
		}
	}
	sort.Strings(cancel)
	return cancel
}

// requeue puts an assigned but unstarted job back in the queue, in creation order. Caller
// holds s.mu.
func (s *Server) requeue(j *job) {
	if j.rn != nil {
		delete(j.rn.jobs, j.ID)
		if j.bz != nil {
			if sess := s.sessions[j.SessionID]; sess != nil && sess.job == j {
				sess.job = nil
			}
		}
	}
	j.Runner, j.rn, j.assignedAt = "", nil, time.Time{}
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
	for free := rn.slots - busy; free > 0 && !s.closed; free-- {
		i := slices.IndexFunc(s.queue, func(j *job) bool {
			return (!j.Service || rn.serviceSupport) && (j.bz == nil || (s.sessionHost(j) == rn.name && s.claimSessionOn(j, rn.name)))
		})
		if i < 0 {
			break
		}
		j := s.queue[i]
		s.queue = slices.Delete(s.queue, i, i+1)
		j.Runner, j.rn, j.assignedAt = rn.name, rn, time.Now()
		rn.jobs[j.ID] = j
		out = append(out, j)
	}
	return out
}

// assignment is what a runner gets to run j.
func (s *Server) assignment(j *job) api.RunnerJob {
	out := api.RunnerJob{
		ID: j.ID, Argv: j.Argv, Script: j.Script, Cwd: j.Cwd, Env: j.Env, TimeoutSec: j.TimeoutSec,
		Workspace: j.Workspace, Outputs: j.Outputs, Ports: j.Ports,
		MaxLog: s.cfg.MaxLog, MaxWorkspace: s.cfg.MaxWorkspace, MaxDownload: s.maxDownload(),
	}
	if j.bz != nil {
		sess := s.sessions[j.SessionID]
		out.Bazel = &api.RunnerBazel{MaxUpload: s.cfg.MaxUpload, DiskCacheMax: s.cfg.DiskCacheMax, Command: *j.bz, Session: sess.Session, CacheReadOnly: s.cfg.CacheReadOnly, SessionIdleSec: int(s.cfg.SessionIdle.Seconds())}
		if s.cache != nil {
			out.Bazel.CacheCredential = sess.credential
		}
		if sess.Commands == 0 { // the hub's reasons for a cold start (see bazelCommand)
			out.Bazel.StartNote = sess.startNote
		}
		out.Bazel.Evicted = j.evicted
	}
	return out
}

func (s *Server) maxDownload() int64 {
	if s.cfg.MaxDownload > 0 {
		return s.cfg.MaxDownload
	}
	return DefaultMaxDownload
}

// runnerJob looks up the job of a /v1/runner/jobs/{id}/... request and checks it is
// assigned to the requesting runner instance (and, with running, started). It writes 410
// Gone otherwise: the runner should kill and drop the job. Caller holds s.mu, and keeps
// holding it across whatever state change the check is for.
func (s *Server) runnerJob(w http.ResponseWriter, r *http.Request, running bool) *job {
	name, inst := r.Header.Get(api.HeaderRunner), r.Header.Get(api.HeaderRunnerInstance)
	j := s.jobs[r.PathValue("id")]
	switch {
	case j == nil:
		writeErr(w, 410, fmt.Sprintf("no such job %q", r.PathValue("id")))
	case j.Runner != name || j.rn == nil:
		writeErr(w, 410, fmt.Sprintf("job %s is not assigned to runner %q", j.ID, name))
	case inst != "" && j.rn.instance != "" && j.rn.instance != inst:
		writeErr(w, 410, fmt.Sprintf("job %s belongs to another instance of runner %q", j.ID, name))
	case api.Terminal(j.State):
		writeErr(w, 410, fmt.Sprintf("job %s has already ended (%s)", j.ID, j.State))
	case running && j.State != api.Running:
		writeErr(w, 410, fmt.Sprintf("job %s is %s, not running", j.ID, j.State))
	default:
		return j
	}
	return nil
}

// lockedRunnerJob is runnerJob for handlers that then work without the lock (log chunks and
// outputs: if the job ends meanwhile, the bytes are just not looked at).
func (s *Server) lockedRunnerJob(w http.ResponseWriter, r *http.Request, running bool) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runnerJob(w, r, running)
}

// runnerStarted serves POST /v1/runner/jobs/{id}/started: the runner is about to start the
// command. Idempotent; 410 if the job was canceled or reassigned meanwhile. The check and
// the transition happen under one lock, so a cancel lands either before (410: never started)
// or after (the job is running and the cancel is delivered as a kill).
func (s *Server) runnerStarted(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.runnerJob(w, r, false)
	if j == nil {
		return
	}
	if j.State == api.Queued {
		now := time.Now().UTC()
		j.State, j.StartedAt = api.Running, &now
		if j.Service {
			go s.probe(j)
		}
		if j.rn.sandbox != "" {
			j.Sandbox = j.rn.sandbox
		}
		s.persist(j)
		// The hub's unpacked copy of the workspace (if any) is no longer needed.
		s.wg.Add(1)
		go func() { defer s.wg.Done(); workspace.RemoveAll(filepath.Join(s.sandboxDir(j.ID), "work")) }()
	}
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
	j := s.lockedRunnerJob(w, r, true)
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
	j := s.lockedRunnerJob(w, r, true)
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

func decodeDone(w http.ResponseWriter, r *http.Request) (api.RunnerDone, bool) {
	var d api.RunnerDone
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&d); err != nil {
		writeErr(w, 400, "bad done JSON: "+err.Error())
		return d, false
	}
	if !slices.Contains([]string{api.Exited, api.Failed, api.Timeout, api.Canceled}, d.State) {
		writeErr(w, 400, fmt.Sprintf("state %q must be exited, failed, timeout or canceled", d.State))
		return d, false
	}
	return d, true
}

// runnerExited serves POST /v1/runner/jobs/{id}/exited: the command has ended with this
// result; the runner now ships the rest of the logs and the outputs, then sends done. From
// here the hub's timeout backstop no longer applies, and if the runner is lost before done
// the job ends with this result.
func (s *Server) runnerExited(w http.ResponseWriter, r *http.Request) {
	d, ok := decodeDone(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.runnerJob(w, r, true)
	if j == nil {
		return
	}
	if j.exited == nil {
		j.exited, j.exitedAt = &d, time.Now()
	}
	writeJSON(w, 200, struct{}{})
}

// runnerDone serves POST /v1/runner/jobs/{id}/done: the job's end state. A kill the hub
// requested decides the state. Repeating it is harmless (410 once ended).
func (s *Server) runnerDone(w http.ResponseWriter, r *http.Request) {
	d, ok := decodeDone(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.runnerJob(w, r, false)
	if j == nil {
		return
	}
	j.OutMissing, j.OutSkipped, j.OutError = d.OutputsMissing, d.OutputsSkipped, d.OutputsError
	s.finishRunnerJob(j, d, "")
	writeJSON(w, 200, struct{}{})
}

// runnerLeave serves POST /v1/runner/leave: a runner shutting down. Its slots go away at
// once; jobs it still has (it reports them before leaving) end as failed.
func (s *Server) runnerLeave(w http.ResponseWriter, r *http.Request) {
	var l api.RunnerLeave
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&l); err != nil || l.Instance == "" {
		writeErr(w, 400, "bad leave JSON: want {name, instance}")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rn := range []*runnerState{s.runners[l.Name], s.retired[l.Instance]} {
		if rn != nil && rn.instance == l.Instance && rn.name == l.Name {
			s.dropRunner(rn, fmt.Sprintf("runner %s left while the job was running", rn.name))
		}
	}
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
				s.expireSessions(now)
			}
		}
	}()
}

// reap drops runner instances that stopped polling, ends runner jobs the runner should have
// ended long ago (a timeout or kill it never confirmed), requeues assignments that were never
// started, and forgets retired instances an hour after their last job.
func (s *Server) reap(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rn := range s.runners {
		s.reapRunner(rn, now)
	}
	for id, rn := range s.retired {
		if len(rn.jobs) > 0 {
			s.reapRunner(rn, now)
		} else if now.Sub(rn.lastSeen) > time.Hour && now.Sub(rn.retiredAt) > time.Hour {
			delete(s.retired, id)
		}
	}
}

func (s *Server) reapRunner(rn *runnerState, now time.Time) {
	if gone := now.Sub(rn.lastSeen); gone > s.cfg.RunnerDeadAfter {
		s.dropRunner(rn, fmt.Sprintf("runner %s lost: no poll for %.0fs (tun serve --runner-timeout)", rn.name, gone.Seconds()))
		return
	}
	// The backstop only covers the time the command may still be running: until the runner
	// reports it exited. Shipping logs and outputs afterwards is bounded by liveness alone.
	grace := s.cfg.KillGrace + s.cfg.RunnerBackstop
	for _, j := range rn.jobs {
		switch {
		case j.State == api.Queued:
			if now.Sub(j.assignedAt) > s.cfg.RunnerStartTimeout {
				s.requeue(j) // the runner hasn't managed to fetch and unpack it
			}
		case j.exited != nil:
		case j.killReq != "" && now.Sub(j.killReqAt) > grace:
			s.finish(j, j.killReq, nil, nil, fmt.Sprintf("%s; runner %s did not confirm the kill", j.killReq, rn.name))
		case j.TimeoutSec > 0 && j.StartedAt != nil && now.Sub(*j.StartedAt) > time.Duration(j.TimeoutSec)*time.Second+grace:
			s.finish(j, api.Timeout, nil, nil, fmt.Sprintf("timed out after %ds (enforced by the hub: runner %s did not report the end)", j.TimeoutSec, rn.name))
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
			MemoryAvailable: rn.memoryAvailable, Load: rn.load, Sessions: rn.liveSessions, LastSeen: rn.lastSeen.UTC(), Sandbox: rn.sandbox, Source: rn.source, NetworkIsolation: rn.privateNet, Disk: rn.disk})
	}
	for _, rn := range s.retired {
		running += len(rn.jobs) // still finishing on a superseded instance
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
		if j.SessionID != "" {
			break
		}
		if !slices.ContainsFunc(outputs, func(o string) bool { return covers(o, p) }) {
			declared := "none"
			if len(outputs) > 0 {
				declared = strings.Join(outputs, " ")
			}
			writeErr(w, 409, fmt.Sprintf("job %s ran on runner %s, which keeps only the paths declared up front with tun run --out (declared: %s); %q is not among them: rerun with tun run --out %s (with --detach too: tun run --detach --out %s ..., then tun get <id> %s)",
				j.ID, runner, declared, p, p, p, p))
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
	if j.SessionID != "" {
		var err error
		paths, err = resolveSessionPaths(f, paths)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	match := func(name string) bool {
		if name == artifactIndex {
			return false
		}
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
