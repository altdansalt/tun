package client

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/workspace"
)

// Usage is the top-level help (`tun help`).
const Usage = `tun: run commands on a tun server, in a fresh sandboxed copy of your working tree.

usage:
  tun run [flags] -- <argv...>       run argv (no shell) and stream its output
  tun run [flags] -c '<script>'      run a script via bash -c
  tun status <id>                    print the job as one JSON object
  tun logs [-f] [--stderr] <id>      print a job's stdout (or stderr); -f follows until it ends
  tun cancel <id>                    kill a job (whole process group) or drop it from the queue
  tun ls [--limit N]                 recent jobs as JSON lines, newest first
  tun get [-o DIR] <id> PATH...      download files/dirs from a finished job's work dir
  tun serve [flags]                  run the server (tun serve -h)
  tun help                           this text; 'tun <cmd> -h' for per-command flags

tun run ships the workspace (the git toplevel of the cwd, else the cwd; tracked + untracked
non-ignored files, no .git) to the server, runs the command there in the same relative
directory, streams stdout/stderr, and exits with the command's exit status.

exit status of tun run: the command's exit code; 128+N if it was killed by signal N;
124 if it hit --timeout; 125 for tun's own errors (bad flags, server unreachable, upload
failed, job failed to start), always with a one-line message on stderr starting "tun:".

environment:
  TUN_URL     server address (default http://127.0.0.1:7070)
  TUN_TOKEN   bearer token; else ~/.config/tun/token; else ~/.local/share/tun/token
              (the default server data dir, so a local client needs no config)

job objects (status, ls, cancel) have fields: id, state (queued|running|exited|failed|timeout|
canceled), exit_code, signal, created_at, started_at, ended_at, argv, script, cwd,
timeout_sec, workspace, sandbox, error.
`

const runUsage = `usage: tun run [flags] -- <argv...>
       tun run [flags] -c '<shell script>'

Upload the workspace (git toplevel of the cwd, else the cwd), run the command on the tun
server in a fresh copy of it, stream stdout/stderr, and exit with the command's status:
its exit code; 128+N if killed by signal N; 124 on timeout; 125 for tun errors.
The job id is printed to stderr first as "tun: job <id>"; if this client is killed without a
chance to cancel, the job keeps running and 'tun logs -f <id>' reattaches. SIGINT/SIGTERM
cancel the job and exit 128+signal.

The job's environment is minimal: PATH, LANG, TERM=dumb, USER, HOME and TMPDIR (private to
the job), GOCACHE/GOMODCACHE/XDG_CACHE_HOME (caches shared across jobs), TUN_JOB_ID, plus -e.

--out PATH (repeatable) gets build outputs back: after the job ends, in any state, PATH
(relative to the workspace root; file, directory or glob) is copied from the job's work dir
to the same path in the local workspace, replacing existing files. A PATH that matched
nothing is a "tun: warning:" on stderr and the exit status stays the command's; if the
download itself fails (e.g. over the server's size limit), a 0 exit becomes 125.
Later, or for other paths: tun get <id> PATH...

flags:
`

// Env is the process context for the CLI, injectable for tests.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Cwd            string
	Signals        <-chan os.Signal // SIGINT/SIGTERM delivered to the client
}

// Main runs a client command (everything except serve) and returns the exit code.
func Main(args []string, e Env) int {
	if len(args) == 0 {
		fmt.Fprint(e.Stderr, Usage)
		return 125
	}
	cmd, args := args[0], args[1:]
	run := map[string]func([]string, Env) int{
		"run": runCmd, "status": statusCmd, "logs": logsCmd, "cancel": cancelCmd, "ls": lsCmd, "get": getCmd,
	}[cmd]
	switch {
	case cmd == "help" || cmd == "-h" || cmd == "--help":
		fmt.Fprint(e.Stdout, Usage)
		return 0
	case run == nil:
		return errorf(e, "unknown command %q; run 'tun help'", cmd)
	}
	return run(args, e)
}

func errorf(e Env, format string, a ...any) int {
	fmt.Fprintf(e.Stderr, "tun: "+format+"\n", a...)
	return 125
}

func newClient(e Env) *Client {
	url := e.Getenv("TUN_URL")
	if url == "" {
		url = "http://127.0.0.1:7070"
	}
	token := e.Getenv("TUN_TOKEN")
	if home := e.Getenv("HOME"); token == "" && home != "" {
		for _, p := range []string{filepath.Join(home, ".config/tun/token"), filepath.Join(home, ".local/share/tun/token")} {
			if b, err := os.ReadFile(p); err == nil {
				token = strings.TrimSpace(string(b))
				break
			}
		}
	}
	return &Client{URL: url, Token: token}
}

func newFlags(name, usage string, e Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	fs.Usage = func() {
		fmt.Fprint(e.Stderr, usage)
		fs.PrintDefaults()
	}
	return fs
}

// parse parses flags allowing them to be interspersed with positional args.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func flagErr(e Env, err error) int {
	if err == flag.ErrHelp {
		return 0
	}
	return 125 // flag package already printed the problem
}

type envFlag []string

func (f *envFlag) String() string     { return strings.Join(*f, ",") }
func (f *envFlag) Set(v string) error { *f = append(*f, v); return nil }

func runCmd(args []string, e Env) int {
	fs := newFlags("run", runUsage, e)
	dir := fs.String("C", "", "working dir relative to the workspace root (default: the client's cwd relative to the root)")
	var envs envFlag
	fs.Var(&envs, "e", "`K=V` env var for the job (repeatable); -e K copies K from this environment")
	timeout := fs.Duration("timeout", 30*time.Minute, "kill the job's whole process group after this long (exit 124)")
	noWS := fs.Bool("no-workspace", false, "upload nothing; run in an empty dir")
	detach := fs.Bool("detach", false, "print the job id to stdout and exit 0 immediately")
	jsonOut := fs.Bool("json", false, "don't stream; when the job ends print one JSON object {id, state, exit_code, signal, error,\nduration (seconds), stdout, stderr, stdout_truncated, stderr_truncated} (output: last 64KiB of each)")
	script := fs.String("c", "", "run this `script` with bash -c instead of argv")
	var outs envFlag
	fs.Var(&outs, "out", "after the job ends, copy `PATH` (file, dir or glob, relative to the workspace root)\nfrom its work dir into the local workspace (repeatable)")
	if err := fs.Parse(args); err != nil {
		return flagErr(e, err)
	}
	req := api.CreateJob{Argv: fs.Args(), Script: *script, Env: map[string]string{}}
	switch {
	case req.Script != "" && len(req.Argv) > 0:
		return errorf(e, "run: give either -c '<script>' or -- <argv...>, not both")
	case req.Script == "" && len(req.Argv) == 0:
		return errorf(e, "run: nothing to run; usage: tun run [flags] -- <argv...> | tun run [flags] -c '<script>'")
	case *timeout <= 0:
		return errorf(e, "run: --timeout must be positive")
	}
	if err := checkPaths(outs); err != nil {
		return errorf(e, "run: --out: %v", err)
	}
	if *detach && len(outs) > 0 {
		return errorf(e, "run: --out does not work with --detach; fetch later with tun get <id> PATH...")
	}
	var outDir string
	if len(outs) > 0 {
		var err error
		if outDir, err = localRoot(e.Cwd); err != nil {
			return errorf(e, "run: --out: %v", err)
		}
	}
	req.TimeoutSec = int((*timeout + time.Second - 1) / time.Second)
	for _, kv := range envs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			v = e.Getenv(k)
			if v == "" {
				return errorf(e, "run: -e %s: %s is not set in this environment (use -e %s=VALUE)", k, k, k)
			}
		}
		if k == "" {
			return errorf(e, "run: -e %q: empty variable name", kv)
		}
		req.Env[k] = v
	}
	if *dir != "" {
		if path.IsAbs(*dir) || strings.Contains("/"+*dir+"/", "/../") {
			return errorf(e, "run: -C %q must be relative to the workspace root, without ..", *dir)
		}
		req.Cwd = path.Clean(*dir)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newClient(e)
	if !*noWS {
		ws, cwd, err := upload(ctx, c, e.Cwd)
		if err != nil {
			return errorf(e, "%v", err)
		}
		req.Workspace = ws
		if *dir == "" {
			req.Cwd = cwd
		}
	}
	j, err := c.Create(ctx, req)
	if err != nil {
		return errorf(e, "creating job: %v", err)
	}
	if *detach {
		fmt.Fprintln(e.Stdout, j.ID)
		return 0
	}
	if !*jsonOut {
		fmt.Fprintf(e.Stderr, "tun: job %s\n", j.ID)
	}
	code := wait(ctx, cancel, c, j, *jsonOut, e)
	if len(outs) > 0 {
		code = fetchOuts(c, j.ID, outs, outDir, code, e)
	}
	return code
}

// upload packs the workspace containing cwd and uploads it unless the server already has it.
// It returns the blob sha and cwd relative to the workspace root.
func upload(ctx context.Context, c *Client, cwd string) (sha, rel string, err error) {
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", "", err
	}
	root, err := workspace.Root(real)
	if err != nil {
		return "", "", err
	}
	if rel, err = filepath.Rel(root, real); err != nil {
		return "", "", err
	}
	files, err := workspace.Files(root)
	if err != nil {
		return "", "", fmt.Errorf("listing workspace files: %v", err)
	}
	tmp, err := os.CreateTemp("", "tun-ws-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name())
	sha, _, err = workspace.Pack(root, files, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", fmt.Errorf("packing workspace %s: %v", root, err)
	}
	has, err := c.HasBlob(ctx, sha)
	if err == nil && !has {
		err = c.PutBlob(ctx, sha, tmp.Name())
	}
	if err != nil {
		return "", "", fmt.Errorf("uploading workspace %s: %v", root, err)
	}
	return sha, filepath.ToSlash(rel), nil
}

// tail keeps the last max bytes written to it.
type tail struct {
	max   int
	n     int64
	buf   []byte
	mutex sync.Mutex
}

func (t *tail) Write(p []byte) (int, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.n += int64(len(p))
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// wait streams (or collects) the job's output until it ends and maps the result to an exit code.
func wait(ctx context.Context, cancel context.CancelFunc, c *Client, j api.Job, jsonOut bool, e Env) int {
	var out, errw io.Writer = e.Stdout, e.Stderr
	tOut, tErr := &tail{max: 64 << 10}, &tail{max: 64 << 10}
	if jsonOut {
		out, errw = tOut, tErr
	}
	var wg sync.WaitGroup
	var errOut, errErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, errOut = c.Logs(ctx, j.ID, "stdout", 0, true, out) }()
	go func() { defer wg.Done(); _, errErr = c.Logs(ctx, j.ID, "stderr", 0, true, errw) }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case sig := <-e.Signals:
		cancel()
		<-done
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		if _, err := c.Cancel(cctx, j.ID); err != nil {
			return errorf(e, "received %v but could not cancel job %s: %v", sig, j.ID, err)
		}
		fmt.Fprintf(e.Stderr, "tun: received %v; canceled job %s\n", sig, j.ID)
		if s, ok := sig.(syscall.Signal); ok {
			return 128 + int(s)
		}
		return 125
	}
	if err := errors.Join(errOut, errErr); err != nil {
		return errorf(e, "%v; job %s may still be running: reattach with 'tun logs -f %s'", err, j.ID, j.ID)
	}
	final, err := c.Get(ctx, j.ID)
	if err != nil {
		return errorf(e, "getting status of job %s: %v", j.ID, err)
	}
	code, msg := exitCode(final)
	if jsonOut {
		res := map[string]any{
			"id": final.ID, "state": final.State, "exit_code": final.ExitCode, "signal": final.Signal,
			"error": final.Error, "tun_exit": code,
			"stdout": string(tOut.buf), "stderr": string(tErr.buf),
			"stdout_truncated": tOut.n > int64(len(tOut.buf)), "stderr_truncated": tErr.n > int64(len(tErr.buf)),
		}
		if final.StartedAt != nil && final.EndedAt != nil {
			res["duration"] = final.EndedAt.Sub(*final.StartedAt).Seconds()
		}
		json.NewEncoder(e.Stdout).Encode(res)
	} else if msg != "" {
		fmt.Fprintf(e.Stderr, "tun: job %s %s\n", final.ID, msg)
	}
	return code
}

// exitCode maps a finished job to tun run's exit status and an optional explanation.
func exitCode(j api.Job) (int, string) {
	switch {
	case j.State == api.Timeout:
		return 124, fmt.Sprintf("timed out after %ds (--timeout)", j.TimeoutSec)
	case j.State == api.Failed:
		return 125, "failed: " + j.Error
	case j.Signal != nil:
		msg := ""
		if j.State == api.Canceled {
			msg = "was canceled"
		}
		return 128 + *j.Signal, msg
	case j.ExitCode != nil:
		return *j.ExitCode, ""
	case j.State == api.Canceled:
		return 125, "was canceled before it started"
	}
	return 125, fmt.Sprintf("ended in state %s", j.State)
}

func oneID(name string, args []string, e Env) (string, bool) {
	if len(args) != 1 {
		errorf(e, "usage: tun %s <id> (job ids: tun ls)", name)
		return "", false
	}
	return args[0], true
}

func printJSON(e Env, v any) {
	json.NewEncoder(e.Stdout).Encode(v)
}

func statusCmd(args []string, e Env) int {
	fs := newFlags("status", "usage: tun status <id>\n\nPrint the job object as one line of JSON.\n", e)
	pos, err := parse(fs, args)
	if err != nil {
		return flagErr(e, err)
	}
	id, ok := oneID("status", pos, e)
	if !ok {
		return 125
	}
	j, err := newClient(e).Get(context.Background(), id)
	if err != nil {
		return errorf(e, "%v", err)
	}
	printJSON(e, j)
	return 0
}

func cancelCmd(args []string, e Env) int {
	fs := newFlags("cancel", "usage: tun cancel <id>\n\nCancel a job: a queued job is dropped; a running job's process group gets SIGTERM,\nthen SIGKILL after 10s. Prints the job object as JSON.\n", e)
	pos, err := parse(fs, args)
	if err != nil {
		return flagErr(e, err)
	}
	id, ok := oneID("cancel", pos, e)
	if !ok {
		return 125
	}
	j, err := newClient(e).Cancel(context.Background(), id)
	if err != nil {
		return errorf(e, "%v", err)
	}
	printJSON(e, j)
	return 0
}

func logsCmd(args []string, e Env) int {
	fs := newFlags("logs", "usage: tun logs [-f] [--stderr] <id>\n\nPrint a job's stdout (or stderr) so far; with -f, keep streaming until the job ends.\n\nflags:\n", e)
	follow := fs.Bool("f", false, "follow until the job ends")
	stderr := fs.Bool("stderr", false, "print stderr instead of stdout")
	pos, err := parse(fs, args)
	if err != nil {
		return flagErr(e, err)
	}
	id, ok := oneID("logs", pos, e)
	if !ok {
		return 125
	}
	stream := "stdout"
	if *stderr {
		stream = "stderr"
	}
	if _, err := newClient(e).Logs(context.Background(), id, stream, 0, *follow, e.Stdout); err != nil {
		return errorf(e, "%v", err)
	}
	return 0
}

func lsCmd(args []string, e Env) int {
	fs := newFlags("ls", "usage: tun ls [--limit N]\n\nPrint recent jobs, newest first, one JSON object per line.\n\nflags:\n", e)
	limit := fs.Int("limit", 20, "max jobs to list")
	pos, err := parse(fs, args)
	if err != nil {
		return flagErr(e, err)
	}
	if len(pos) > 0 {
		return errorf(e, "usage: tun ls [--limit N]")
	}
	js, err := newClient(e).List(context.Background(), *limit)
	if err != nil {
		return errorf(e, "%v", err)
	}
	for _, j := range js {
		printJSON(e, j)
	}
	return 0
}
