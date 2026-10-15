package server

// Local execution of one job, shared by the server's own job slots and by tun runner. Both
// keep the same layout under their data dir: jobs/<id>/{stdout,stderr,sandbox/{work,home,tmp}}
// and the shared caches in cache/.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
)

// sharedCaches are the dirs under $DATA/cache that every job gets read-write, and the env var
// pointing tools at each. Only tools that are safe under concurrent use belong here.
var sharedCaches = []struct{ env, dir string }{
	{"GOCACHE", "go-build"},
	{"GOMODCACHE", "gomod"},
	{"XDG_CACHE_HOME", "xdg"}, // pip, uv, yarn, ...
	{"npm_config_cache", "npm"},
}

// cacheDirs are the shared cache dirs, relative to the data dir.
func cacheDirs() []string {
	var dirs []string
	for _, c := range sharedCaches {
		dirs = append(dirs, filepath.Join("cache", c.dir))
	}
	return dirs
}

// prepareJobDir creates dataDir/jobs/<id> with empty stdout/stderr logs and an empty sandbox
// (work, home, tmp). The job only ever gets sandbox/; the logs (and the server's job.json)
// live beside it.
func prepareJobDir(dataDir, id string) error {
	dir := filepath.Join(dataDir, "jobs", id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	for _, d := range []string{"", "work", "home", "tmp"} {
		if err := os.Mkdir(filepath.Join(dir, "sandbox", d), 0o700); err != nil {
			return err
		}
	}
	for _, f := range []string{"stdout", "stderr"} {
		f, err := openOwn(filepath.Join(dir, f), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		f.Close()
	}
	return nil
}

// jobEnv is a job's environment: a small base, the shared caches, then the job's own vars.
func jobEnv(dataDir, id string, extra map[string]string) map[string]string {
	sb := filepath.Join(dataDir, "jobs", id, "sandbox")
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "C.UTF-8"
	}
	m := map[string]string{
		"PATH": pathEnv, "LANG": lang, "TERM": "dumb", "USER": os.Getenv("USER"),
		"HOME": filepath.Join(sb, "home"), "TMPDIR": filepath.Join(sb, "tmp"), "TUN_JOB_ID": id,
		// npm writes its debug logs into its cache by default; keep them private to the job.
		"npm_config_logs_dir": filepath.Join(sb, "home", ".npm", "_logs"),
	}
	for _, c := range sharedCaches {
		m[c.env] = filepath.Join(dataDir, "cache", c.dir)
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func envList(m map[string]string) []string {
	env := make([]string, 0, len(m))
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	return env
}

// execSpec describes one job to run in its prepared job dir.
type execSpec struct {
	DataDir    string
	ID         string
	Argv       []string // or Script
	Script     string
	Cwd        string // relative to the workspace root
	Env        []string
	TimeoutSec int // 0: none
	Sandbox    string
	PrivateNet bool // bwrap only: own network namespace with an egress proxy (see sandbox/net.go)
	MaxLog     int64
	KillGrace  time.Duration
}

// proc is one job's command: newProc checks and opens everything, start starts it, kill
// asks it to end (whole process group: SIGTERM, then SIGKILL after the grace period), and
// wait collects the result. Without start, abort releases what newProc opened.
type proc struct {
	spec    sandbox.Spec
	logs    *jobLogs
	cmd     *exec.Cmd
	net     *sandbox.Net // with PrivateNet, once started; the caller closes it after wait
	timeout time.Duration
	grace   time.Duration

	mu       sync.Mutex
	endState string // set by the first kill
	exited   bool
	killed   chan struct{}
}

func newProc(x execSpec) (*proc, error) {
	dir := filepath.Join(x.DataDir, "jobs", x.ID)
	sb := filepath.Join(dir, "sandbox")
	argv := x.Argv
	if x.Script != "" {
		argv = []string{"bash", "-c", x.Script}
	}
	p := &proc{grace: x.KillGrace, killed: make(chan struct{}), spec: sandbox.Spec{
		Mode: x.Sandbox, Argv: argv, Env: x.Env, Dir: filepath.Join(sb, "work", x.Cwd),
		Root: sb, Tmp: filepath.Join(sb, "tmp"), DataDir: x.DataDir, PrivateNet: x.PrivateNet,
	}}
	for _, c := range cacheDirs() {
		p.spec.Caches = append(p.spec.Caches, filepath.Join(x.DataDir, c))
	}
	p.timeout = time.Duration(x.TimeoutSec) * time.Second
	if p.timeout <= 0 {
		p.timeout = 1<<63 - 1 // none (services)
	}
	if p.grace <= 0 {
		p.grace = 10 * time.Second
	}
	if fi, err := os.Stat(p.spec.Dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("cwd %q does not exist in the workspace", x.Cwd)
	}
	var err error
	if p.logs, err = openLogs(dir, x.MaxLog); err != nil {
		return nil, err
	}
	return p, nil
}

// start starts the command. On error everything is released.
func (p *proc) start() error {
	cmd, n, err := sandbox.Start(p.spec, p.logs.Stdout, p.logs.Stderr)
	p.logs.started()
	if err != nil {
		p.logs.close()
		return err
	}
	p.cmd, p.net = cmd, n
	return nil
}

func (p *proc) abort() { p.logs.close() }

// kill asks the started command to end; the job then ends in state ("" means "crash": the
// caller records nothing). Only the first call counts.
func (p *proc) kill(state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited || p.endState != "" {
		return
	}
	p.endState = state
	if state == "" {
		p.endState = "crash"
	}
	syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	close(p.killed)
}

// procResult is how a job's command ended.
type procResult struct {
	State     string // exited, timeout, canceled, failed, or "crash"
	Code, Sig *int
	Msg       string
}

// wait waits for the started command, enforcing the timeout, and drains its logs.
func (p *proc) wait() procResult {
	pid := p.cmd.Process.Pid
	waitDone := make(chan struct{})
	go func() {
		select {
		case <-waitDone:
			return
		case <-p.killed:
		}
		select {
		case <-waitDone:
		case <-time.After(p.grace):
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
	timer := time.AfterFunc(p.timeout, func() { p.kill(api.Timeout) })
	err := p.cmd.Wait()
	timer.Stop()
	close(waitDone)
	syscall.Kill(-pid, syscall.SIGKILL) // reap stragglers left in the group
	p.logs.wait()

	var res procResult
	if ps := p.cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			n := int(ws.Signal())
			res.Sig = &n
		} else {
			n := ps.ExitCode()
			res.Code = &n
		}
	}
	p.mu.Lock()
	p.exited = true
	res.State = p.endState
	p.mu.Unlock()
	switch res.State {
	case "":
		res.State = api.Exited
	case api.Timeout:
		res.Msg = fmt.Sprintf("timed out after %ds", int(p.timeout/time.Second))
	case api.Canceled:
		res.Msg = "canceled"
	}
	if err != nil && res.Code == nil && res.Sig == nil {
		res.Msg = err.Error()
	}
	return res
}
