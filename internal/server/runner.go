package server

// tun runner: pulls jobs from a hub (a tun serve this machine can reach over HTTP(S); the hub
// can't connect back), runs them with the same executor and sandbox as the hub's local
// slots, and pushes logs, declared outputs and the end state back. See hub.go for the
// protocol.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/workspace"
)

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	Hub            string // hub base URL
	BazelCommand   string
	BazeliskSeed   string
	MaxClientCache int64
	Token          string // sent as X-Tun-Token (the hub's client token, for now)
	Name           string
	Slots          int
	DataDir        string // blob cache, job dirs, shared build caches
	Sandbox        string // sandbox.Bwrap or sandbox.None (already resolved)
	// PrivateNet (bwrap only): a network namespace per job, egress through an HTTP(S) proxy
	// this runner serves (check sandbox.NetUsable first).
	PrivateNet bool
	KillGrace  time.Duration
	KeepBlobs  time.Duration // cached workspace blobs unused this long are deleted; default 24h
	MinFree    MinFree       // when free space is under it, cached blobs are deleted (see disk.go)
	HTTP       *http.Client
	Logf       func(format string, a ...any) // default: discard
}

// Runner is a running tun runner. Create with NewRunner, run with Run; stop with Shutdown
// (graceful: running jobs finish within a deadline) or Kill (like a crash).
type Runner struct {
	sessionTransport *http.Transport
	serviceHTTP      *http.Client
	serviceTransport *http.Transport
	sessions         *Server
	cfg              RunnerConfig
	instance         string
	host             string
	previous         string          // the instance that used the data dir before
	lock             *os.File        // flock on the data dir: one runner process per data dir
	seq              int64           // polls sent (only the Run goroutine touches it)
	ctx              context.Context // canceled by Kill: no more requests, jobs abandoned
	kill             context.CancelFunc
	pollCtx          context.Context // canceled to stop taking work
	stopPoll         context.CancelFunc

	mu         sync.Mutex
	jobs       map[string]*runnerJob
	draining   bool
	superseded string // the hub's message once a newer instance took this runner's name
	wg         sync.WaitGroup
	blobMu     sync.Mutex
	blobLock   map[string]*sync.Mutex
	reclaimed  atomic.Int64 // blobs deleted because disk space was low
}

type runnerJob struct {
	sessionJob *job
	spec       api.RunnerJob
	proc       *proc  // once started
	killState  string // a kill requested before the command started
}

var errGone = errors.New("the hub no longer wants this job")

// NewRunner checks the config and prepares the data dir. Job dirs left by a previous runner
// process are deleted (the hub has failed or will fail those jobs).
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.Name == "" || cleanName(cfg.Name) != cfg.Name || cfg.Name == api.LocalRunner {
		return nil, fmt.Errorf("runner name %q must be printable ASCII without spaces, and not %q", cfg.Name, api.LocalRunner)
	}
	if cfg.Slots < 1 {
		return nil, fmt.Errorf("slots must be at least 1")
	}
	if cfg.KeepBlobs <= 0 {
		cfg.KeepBlobs = 24 * time.Hour
	}
	if cfg.HTTP == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 2 * time.Minute // polls are answered within ~25s
		cfg.HTTP = &http.Client{Transport: tr}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	cfg.Hub = strings.TrimRight(cfg.Hub, "/")
	if cfg.Sandbox != sandbox.Bwrap {
		cfg.PrivateNet = false
	}
	dir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = dir
	if _, err := os.Stat(filepath.Join(dir, "token")); err == nil {
		return nil, fmt.Errorf("%s looks like a tun serve data dir (it has a token); give the runner its own --data", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Mount sources must have no symlink in their path (sandbox.openMountSource).
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, err
	}
	cfg.DataDir = dir
	// Two runners on one data dir would wipe and reuse each other's job dirs.
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("data dir %s is in use by another tun runner (%v); give each runner its own --data (and --name)", dir, err)
	}
	instance := randHex(8)
	prev, _ := os.ReadFile(filepath.Join(dir, "instance"))
	if err := os.WriteFile(filepath.Join(dir, "instance"), []byte(instance+"\n"), 0o600); err != nil {
		lock.Close()
		return nil, err
	}
	if err := workspace.RemoveAll(filepath.Join(dir, "jobs")); err != nil {
		return nil, fmt.Errorf("cleaning old job dirs: %v", err)
	}
	for _, d := range append([]string{"blobs", "jobs"}, cacheDirs...) {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	host, _ := os.Hostname()
	r := &Runner{cfg: cfg, instance: instance, previous: strings.TrimSpace(string(prev)), lock: lock, host: host, jobs: map[string]*runnerJob{}, blobLock: map[string]*sync.Mutex{}}
	r.ctx, r.kill = context.WithCancel(context.Background())
	r.pollCtx, r.stopPoll = context.WithCancel(r.ctx)
	if err := r.newSessionHost(); err != nil {
		lock.Close()
		return nil, err
	}
	return r, nil
}

// Run polls the hub and runs jobs until Shutdown or Kill, then waits for job handlers.
func (r *Runner) Run() {
	lastErr, nextSweep := "", time.Time{}
	failures := 0
	for r.pollCtx.Err() == nil {
		if time.Now().After(nextSweep) {
			r.sweepBlobs()
			nextSweep = time.Now().Add(time.Hour)
		}
		r.reclaimBlobs()
		resp, err := r.poll()
		if err != nil {
			if r.pollCtx.Err() != nil {
				break
			}
			if msg := err.Error(); msg != lastErr {
				r.cfg.Logf("tun runner: polling %s: %s (retrying)", r.cfg.Hub, msg)
				lastErr = msg
			}
			failures++
			sleepCtx(r.pollCtx, min(time.Duration(failures)*500*time.Millisecond, 5*time.Second))
			continue
		}
		if lastErr != "" {
			r.cfg.Logf("tun runner: connected to %s", r.cfg.Hub)
			lastErr = ""
		}
		failures = 0
		for _, id := range resp.Cancel {
			r.cancelJob(id, api.Canceled)
		}
		for _, t := range resp.Tunnels {
			go r.connectServiceTunnel(t)
		}
		r.sessionLeases(resp.Sessions)
		for _, j := range resp.Jobs {
			r.startJob(j)
		}
		if resp.Superseded != "" {
			r.mu.Lock()
			if r.superseded == "" {
				r.cfg.Logf("tun runner: %s; finishing %d jobs, then exiting", resp.Superseded, len(r.jobs))
			}
			r.superseded, r.draining = resp.Superseded, true
			left := len(r.jobs)
			r.mu.Unlock()
			if left == 0 {
				break
			}
			sleepCtx(r.pollCtx, 250*time.Millisecond) // the hub answers at once: keep a heartbeat
		}
	}
	r.wg.Wait()
	r.sessions.Close()
	r.sessionTransport.CloseIdleConnections()
	r.serviceTransport.CloseIdleConnections()
	r.lock.Close()
}

// Superseded is the hub's message if Run returned because a newer runner instance took
// this runner's name ("" otherwise).
func (r *Runner) Superseded() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.superseded
}

// Kill stops the runner like a crash: commands are killed, nothing more is reported (the
// hub fails the jobs once the runner is overdue).
func (r *Runner) Kill() {
	r.kill()
	r.mu.Lock()
	for _, j := range r.jobs {
		r.cancelSessionJob(j, "")
		if j.proc != nil {
			j.proc.kill("")
		}
	}
	r.mu.Unlock()
}

// Shutdown advertises zero slots while jobs finish, retaining heartbeats and cancellation.
// After timeout it gives up like Kill.
func (r *Runner) Shutdown(timeout time.Duration) {
	r.mu.Lock()
	r.draining = true
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		r.stopPoll()
		// Tell the hub, so it stops counting this runner's slots now rather than once it is
		// overdue.
		ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
		r.postJSON(ctx, "/v1/runner/leave", api.RunnerLeave{Name: r.cfg.Name, Instance: r.instance}, nil)
		cancel()
	case <-time.After(timeout):
		r.Kill()
		<-done
	}
	r.stopPoll()
	r.kill()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// do sends a request to the hub with the runner's credentials.
func (r *Runner) do(ctx context.Context, method, p string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.cfg.Hub+p, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tun-Token", r.cfg.Token)
	req.Header.Set("Authorization", "Bearer "+r.cfg.Token)
	req.Header.Set(api.HeaderRunner, r.cfg.Name)
	req.Header.Set(api.HeaderRunnerInstance, r.instance)
	req.Header.Set("X-Tun-Client", r.host) // the hub tells a restart from a name clash by it
	if strings.Contains(p, "/tunnels/") {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "tun-service")
	}
	if strings.Contains(p, "/tunnels/") {
		return r.serviceHTTP.Do(req)
	}
	return r.cfg.HTTP.Do(req)
}

// hubError reads a non-2xx response's message.
func hubError(resp *http.Response) error {
	var e api.Error
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e) != nil || e.Error == "" {
		e.Error = strings.TrimSpace(string(b))
	}
	if resp.StatusCode == 410 {
		return fmt.Errorf("%w: %s", errGone, e.Error)
	}
	if resp.StatusCode == 401 {
		return fmt.Errorf("hub refused the token (HTTP 401): %s; pass the hub's token with --token-file or $TUN_TOKEN", e.Error)
	}
	return fmt.Errorf("hub: %s (HTTP %d)", e.Error, resp.StatusCode)
}

// postJSON sends in as JSON and decodes a 2xx answer into out (if not nil).
func (r *Runner) postJSON(ctx context.Context, p string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // a hung hub connection must not stall the runner
	defer cancel()
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := r.do(ctx, "POST", p, bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return hubError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (r *Runner) poll() (api.RunnerPollResponse, error) {
	r.mu.Lock()
	r.seq++
	req := api.RunnerPoll{Name: r.cfg.Name, Instance: r.instance, Previous: r.previous, Seq: r.seq, Slots: r.cfg.Slots, Sandbox: r.cfg.Sandbox,
		NetworkIsolation: r.cfg.PrivateNet, Running: []string{}}
	for id := range r.jobs {
		req.Running = append(req.Running, id)
	}
	if r.draining {
		req.Slots = 0
	}
	r.mu.Unlock()
	req.ServiceSupport = true
	req.SessionSupport = true
	req.MemoryAvailable, req.Load = hostResources()
	r.sessions.mu.Lock()
	for id := range r.sessions.sessions {
		req.Sessions = append(req.Sessions, id)
	}
	r.sessions.mu.Unlock()
	if req.Disk = diskInfo(r.cfg.DataDir, r.cfg.MinFree); req.Disk != nil {
		req.Disk.ReclaimedBlobs = r.reclaimed.Load()
	}
	ctx, cancel := context.WithTimeout(r.pollCtx, 90*time.Second)
	defer cancel()
	var resp api.RunnerPollResponse
	err := r.postJSON(ctx, "/v1/runner/poll", req, &resp)
	return resp, err
}

// retry calls f until it succeeds, the hub says the job is gone, the runner is killed, or
// giveUp has passed.
func (r *Runner) retry(giveUp time.Duration, f func() error) error {
	deadline := time.Now().Add(giveUp)
	for i := 1; ; i++ {
		err := f()
		if err == nil || errors.Is(err, errGone) || r.ctx.Err() != nil || time.Now().After(deadline) {
			return err
		}
		sleepCtx(r.ctx, min(time.Duration(i)*500*time.Millisecond, 10*time.Second))
	}
}

func (r *Runner) jobDir(id string) string { return filepath.Join(r.cfg.DataDir, "jobs", id) }

func (r *Runner) startJob(spec api.RunnerJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	badWS := spec.Workspace != "" && !shaRE.MatchString(spec.Workspace)
	if r.jobs[spec.ID] != nil || r.draining || badWS || spec.ID == "" || strings.ContainsAny(spec.ID, "/.") {
		return // a duplicate (or garbage): the hub requeues what we don't list
	}
	j := &runnerJob{spec: spec}
	r.jobs[spec.ID] = j
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.handle(j)
		workspace.RemoveAll(r.jobDir(spec.ID))
		r.mu.Lock()
		delete(r.jobs, spec.ID)
		r.mu.Unlock()
	}()
}

// cancelJob kills a job (or makes sure it never starts); it then ends in state.
func (r *Runner) cancelJob(id, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	switch {
	case j == nil:
	case j.sessionJob != nil:
		r.cancelSessionJob(j, state)
	case j.proc != nil:
		j.proc.kill(state)
	case j.killState == "":
		j.killState = state
	}
}

// handle runs one job and reports it, unless the hub doesn't want it anymore.
func (r *Runner) handle(j *runnerJob) {
	id := j.spec.ID
	start := time.Now()
	res, done := r.execute(j)
	if res.State == "crash" {
		r.cfg.Logf("tun runner: job %s: dropped", id)
		return
	}
	if res.State == api.Failed && res.Msg == "" { // shut down before the command started
		res.Msg = fmt.Sprintf("runner %s shut down while the job was running", r.cfg.Name)
	}
	done.State, done.ExitCode, done.Signal, done.Error = res.State, res.Code, res.Sig, res.Msg
	err := r.retry(10*time.Minute, func() error {
		return r.postJSON(r.ctx, "/v1/runner/jobs/"+id+"/done", done, nil)
	})
	switch {
	case err != nil && !errors.Is(err, errGone):
		r.cfg.Logf("tun runner: job %s: reporting the end failed: %v", id, err)
	default:
		code := ""
		if res.Code != nil {
			code = fmt.Sprintf(" %d", *res.Code)
		}
		r.cfg.Logf("tun runner: job %s: %s%s after %.1fs", id, res.State, code, time.Since(start).Seconds())
	}
}

func failed(format string, a ...any) procResult {
	return procResult{State: api.Failed, Msg: fmt.Sprintf(format, a...)}
}

// execute prepares, starts and waits for the job, ships its logs and uploads its outputs.
func (r *Runner) execute(j *runnerJob) (procResult, api.RunnerDone) {
	if j.spec.Bazel != nil {
		return r.executeBazel(j)
	}
	spec := j.spec
	var done api.RunnerDone
	if err := prepareJobDir(r.cfg.DataDir, spec.ID); err != nil {
		return failed("runner %s: preparing the job dir: %v", r.cfg.Name, err), done
	}
	if spec.Workspace != "" {
		if err := r.unpack(spec); err != nil {
			return failed("runner %s: %v", r.cfg.Name, err), done
		}
	}
	env := jobEnv(r.cfg.DataDir, spec.ID, spec.Env)
	portEnv(&job{Job: api.Job{Ports: spec.Ports}}, env)
	p, err := newProc(execSpec{
		DataDir: r.cfg.DataDir, ID: spec.ID, Argv: spec.Argv, Script: spec.Script, Cwd: spec.Cwd,
		Env: envList(env), TimeoutSec: spec.TimeoutSec,
		Sandbox: r.cfg.Sandbox, PrivateNet: r.cfg.PrivateNet, MaxLog: spec.MaxLog, KillGrace: r.cfg.KillGrace,
	})
	if err != nil {
		return failed("%v", err), done
	}
	// The commit point: from here on the hub won't hand the job to anyone else.
	err = r.retry(time.Minute, func() error {
		return r.postJSON(r.ctx, "/v1/runner/jobs/"+spec.ID+"/started", struct{}{}, nil)
	})
	if err != nil {
		p.abort()
		return procResult{State: "crash"}, done
	}
	r.mu.Lock()
	if j.killState != "" {
		r.mu.Unlock()
		p.abort()
		res := procResult{State: j.killState, Msg: "canceled before it started"}
		if j.killState == api.Failed {
			res.Msg = ""
		}
		return res, done
	}
	if err := p.start(); err != nil {
		r.mu.Unlock()
		return failed("starting command: %v", err), done
	}
	j.proc = p
	r.mu.Unlock()
	r.cfg.Logf("tun runner: job %s: started", spec.ID)

	var sent [2]int64
	stop, shipped := make(chan struct{}), make(chan struct{})
	go func() { // ship logs while the job runs
		defer close(shipped)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			if err := r.shipLogs(spec.ID, &sent); errors.Is(err, errGone) {
				p.kill("")
			}
		}
	}()
	res := p.wait()
	if p.net != nil {
		p.net.Close()
	}
	close(stop)
	<-shipped
	if res.State == "crash" {
		return res, done
	}
	if res.State == api.Failed && res.Msg == "" {
		res.Msg = fmt.Sprintf("runner %s shut down while the job was running", r.cfg.Name)
	}
	// Report the result now: shipping logs and outputs may take a while, and the hub's
	// timeout backstop must not count that as the command running.
	ex := api.RunnerDone{State: res.State, ExitCode: res.Code, Signal: res.Sig, Error: res.Msg}
	if err := r.retry(time.Minute, func() error {
		return r.postJSON(r.ctx, "/v1/runner/jobs/"+spec.ID+"/exited", ex, nil)
	}); errors.Is(err, errGone) {
		return procResult{State: "crash"}, done
	}
	if err := r.retry(10*time.Minute, func() error { return r.shipLogs(spec.ID, &sent) }); err != nil {
		r.cfg.Logf("tun runner: job %s: shipping logs: %v", spec.ID, err)
		if errors.Is(err, errGone) || r.ctx.Err() != nil {
			return procResult{State: "crash"}, done
		}
	}
	if len(spec.Outputs) > 0 {
		done = r.outputs(spec)
	}
	return res, done
}

// unpack extracts the job's workspace blob (cached by sha) into its work dir.
func (r *Runner) unpack(spec api.RunnerJob) error {
	f, err := r.blob(spec.Workspace)
	if err != nil {
		return fmt.Errorf("fetching workspace %s: %v", spec.Workspace, err)
	}
	defer f.Close()
	err = workspace.UnpackLimit(f, filepath.Join(r.jobDir(spec.ID), "sandbox", "work"), spec.MaxWorkspace)
	var tl *workspace.TooLargeError
	if errors.As(err, &tl) {
		return fmt.Errorf("workspace unpacks to more than %s, over the hub's limit (tun serve --max-workspace; crossed at %q); shrink it (gitignore build outputs and large data files) or use --no-workspace", fmtSize(tl.Limit), tl.Entry)
	}
	if err != nil {
		return fmt.Errorf("unpacking workspace: %v", err)
	}
	return nil
}

// blob opens the cached blob sha, downloading it from the hub first if needed.
func (r *Runner) blob(sha string) (*os.File, error) {
	r.blobMu.Lock()
	l := r.blobLock[sha]
	if l == nil {
		l = &sync.Mutex{}
		r.blobLock[sha] = l
	}
	r.blobMu.Unlock()
	l.Lock()
	defer l.Unlock()
	p := filepath.Join(r.cfg.DataDir, "blobs", sha+".tar.gz")
	if f, err := os.Open(p); err == nil {
		now := time.Now()
		os.Chtimes(p, now, now)
		return f, nil
	}
	err := r.retry(time.Minute, func() error {
		resp, err := r.do(r.ctx, "GET", "/v1/blobs/"+sha, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return hubError(resp)
		}
		tmp, err := os.CreateTemp(filepath.Dir(p), "download-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil && hex.EncodeToString(h.Sum(nil)) != sha {
			err = fmt.Errorf("downloaded blob hashes to %x", h.Sum(nil))
		}
		if err == nil {
			err = os.Rename(tmp.Name(), p)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// sweepBlobs deletes cached blobs unused for KeepBlobs.
func (r *Runner) sweepBlobs() {
	dir := filepath.Join(r.cfg.DataDir, "blobs")
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > r.cfg.KeepBlobs {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// shipLogs sends whatever the hub doesn't have yet of the job's stdout and stderr. sent
// holds the hub's offsets; a retried or overlapping chunk is harmless.
func (r *Runner) shipLogs(id string, sent *[2]int64) error {
	return r.shipLogsDir(id, r.jobDir(id), sent)
}

func (r *Runner) shipLogsDir(id, dir string, sent *[2]int64) error {
	buf := make([]byte, 1<<20)
	for i, stream := range []string{"stdout", "stderr"} {
		f, err := os.Open(filepath.Join(dir, stream))
		if err != nil {
			return err
		}
		for {
			n, err := f.ReadAt(buf, sent[i])
			if n == 0 {
				if err != nil && err != io.EOF {
					f.Close()
					return err
				}
				break
			}
			if sent[i], err = r.postLog(id, stream, sent[i], buf[:n]); err != nil {
				f.Close()
				return err
			}
		}
		f.Close()
	}
	return nil
}

// postLog sends one chunk starting at offset and returns the hub's new offset.
func (r *Runner) postLog(id, stream string, offset int64, chunk []byte) (int64, error) {
	ctx, cancel := context.WithTimeout(r.ctx, time.Minute)
	defer cancel()
	q := fmt.Sprintf("/v1/runner/jobs/%s/%s?offset=%d", id, stream, offset)
	resp, err := r.do(ctx, "POST", q, bytes.NewReader(chunk))
	if err != nil {
		return offset, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 409 {
		return offset, hubError(resp)
	}
	var ack api.LogAck
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return offset, fmt.Errorf("bad log ack: %v", err)
	}
	return ack.Offset, nil // on 409 (the hub has less than we thought): resume where it is
}

// outputs collects the job's declared outputs into a tar (same rules as the hub's files
// endpoint) and uploads it. Problems are reported to the hub in the done body.
func (r *Runner) outputs(spec api.RunnerJob) api.RunnerDone {
	var d api.RunnerDone
	fail := func(format string, a ...any) api.RunnerDone {
		d.OutputsError = fmt.Sprintf(format, a...)
		return d
	}
	root, err := openWorkDir(r.cfg.DataDir, spec.ID)
	if err != nil {
		return fail("runner %s: opening job %s's work dir for its outputs: %v", r.cfg.Name, spec.ID, unwrapPathErr(err))
	}
	defer root.Close()
	entries, missing, skipped, err := collect(root, spec.Outputs)
	if err != nil {
		return fail("%v", err)
	}
	d.OutputsMissing, d.OutputsSkipped = missing, skipped
	var total int64
	for _, e := range entries {
		if e.info.Mode().IsRegular() {
			total += e.info.Size()
		}
	}
	if total > spec.MaxDownload {
		return fail("requested files total %d bytes, over the server limit of %d bytes (tun serve --max-download); fetch fewer or narrower paths", total, spec.MaxDownload)
	}
	tmp, err := os.CreateTemp(r.jobDir(spec.ID), ".outputs-*")
	if err != nil {
		return fail("runner %s: %v", r.cfg.Name, err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	tw := tar.NewWriter(tmp)
	for _, e := range entries {
		if err := writeEntry(tw, root, e); err != nil {
			return fail("runner %s: collecting output %s: %v", r.cfg.Name, e.name, unwrapPathErr(err))
		}
	}
	if err := tw.Close(); err != nil {
		return fail("runner %s: %v", r.cfg.Name, err)
	}
	err = r.retry(10*time.Minute, func() error {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		resp, err := r.do(r.ctx, "PUT", "/v1/runner/jobs/"+url.PathEscape(spec.ID)+"/outputs", io.NopCloser(tmp))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return hubError(resp)
		}
		return nil
	})
	if err != nil {
		return fail("runner %s: uploading outputs: %v", r.cfg.Name, err)
	}
	return d
}
