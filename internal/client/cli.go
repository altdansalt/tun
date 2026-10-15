package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
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
  tun run --service [--port N]... -- <argv...>
                                     start a long-running service, proxied at
                                     <server>/v1/jobs/<id>/ports/N/; exits once it's ready
  tun bazel [--session NAME] [--cold] <bazel command> [args...]
                                     run Bazel in a warm session on the server: the
                                     workspace is synced (only changed files sent) and
                                     the Bazel server stays up between commands (tun bazel -h)
  tun sessions [--mine]              the server's Bazel sessions as JSON lines
  tun status <id>                    print the job as one JSON object
  tun logs [-f] [--stderr] [--tail N] <id>
                                     print a job's stdout (or stderr); -f follows until it
                                     ends; --tail N: only the last N lines (then follow, with -f)
  tun cancel <id>                    kill a job (whole process group) or drop it from the queue
  tun ls [--limit N] [--state S] [--client NAME | --mine]
                                     recent jobs as JSON lines, newest first
  tun get [-o DIR] <id> PATH...      download files/dirs from a finished job's work dir
  tun runners                        live runners of the server as JSON lines
  tun serve [flags]                  run the server (tun serve -h; --ephemeral for testing)
  tun runner --hub URL [flags]       run jobs for a server from another machine (tun runner -h)
  tun help                           this text; 'tun <cmd> -h' for per-command flags

tun run ships the workspace (the git toplevel of the cwd, else the cwd; tracked + untracked
non-ignored files, no .git) to the server, runs the command there in the same relative
directory, streams stdout/stderr, and exits with the command's exit status.
Stderr reports compressed workspace bytes (uploaded or cached) and final job run/queue times.

exit status of tun run: the command's exit code; 128+N if it was killed by signal N;
124 if it hit --timeout; 125 for tun's own errors (bad flags, server unreachable, upload
failed, job failed to start), always with a one-line message on stderr starting "tun:".

environment:
  TUN_URL     server address (default http://127.0.0.1:7070)
  TUN_TOKEN   bearer token; else ~/.config/tun/token; else ~/.local/share/tun/token
              (the default server data dir, so a local client needs no config)

job objects (status, ls, cancel) have fields: id, state (queued|running|exited|failed|timeout|
canceled), exit_code, signal, created_at, started_at, ended_at, argv, script, cwd,
timeout_sec (0 = none), workspace, sandbox, error, work_deleted (true once the server's retention
deleted the job's files; status and logs remain), client (who submitted it: see tun ls -h),
queue_sec (seconds from creation to start, counting while queued), run_sec (seconds from start
to end, counting while running), queue_position (only while queued: 1 = next to start),
runner (where it runs: "local" = the server itself, else a runner's name), outputs (the --out
paths). A job that ran on a runner keeps only its --out paths: tun get of anything else is an
error saying so. tun bazel jobs also have session and session_id, and while they wait,
queue_reason (e.g. their session is busy with another command).
Server load (no token needed): curl $TUN_URL/v1/healthz -> {running, queued, max_jobs,
services, max_services, network_isolation, ...}.
Services also have service: true and ports: [{port, host_port, ready, path, url}].
`

const runUsage = `usage: tun run [flags] -- <argv...>
       tun run [flags] -c '<shell script>'

Upload the workspace (git toplevel of the cwd, else the cwd), run the command on the tun
server in a fresh copy of it, stream stdout/stderr, and exit with the command's status:
its exit code; 128+N if killed by signal N; 124 on timeout; 125 for tun errors.
The job id is printed to stderr first as "tun: job <id>"; if this client is killed without a
chance to cancel, the job keeps running and 'tun logs -f <id>' reattaches. SIGINT/SIGTERM
cancel the job and exit 128+signal.
If the server is busy (it runs --max-jobs jobs at once, FIFO), tun run prints once
"tun: queued (N ahead, M running)" and, when the job starts, "tun: started after Xs in queue".
Then "tun: running on hub" (the server itself) or "tun: running on runner NAME" says where.
On completion stderr ends with "tun: job <id> <state> exit=<code> after <run>s (<queue>s queued)".
Workspace diagnostics show compressed bytes uploaded or already on the server.
The job object's queue_sec and run_sec (tun status, and in --json) show where time went.

The job's environment is minimal: PATH, LANG, TERM=dumb, USER, HOME and TMPDIR (private to
the job), GOCACHE/GOMODCACHE/XDG_CACHE_HOME (caches shared across jobs), TUN_JOB_ID, plus -e.

Network: on a server with network isolation (healthz "network_isolation": true) each job has
its own network namespace with only loopback. Outbound HTTP(S) goes through a proxy the server
runs (HTTP_PROXY, HTTPS_PROXY, http_proxy, https_proxy=http://127.0.0.1:3128, the same in every
job, and NO_PROXY=localhost,127.0.0.1,::1 are set unless given with -e), which go, git, curl, pip and npm honor; raw TCP/UDP (ssh, DNS
lookups) can't leave the job. Ports a job listens on are reachable only inside the job (and,
for services, through the server's proxy). Without isolation jobs share the server's network.

--out PATH (repeatable) gets build outputs back: after the job ends, in any state, PATH
(relative to the workspace root; file, directory or glob) is copied from the job's work dir
to the same path in the local workspace, replacing existing files. It copies whatever the
job left there: '--out .' also pulls job-created files such as .git/hooks into your checkout,
so name the outputs you want. A PATH that matched
nothing is a "tun: warning:" on stderr and the exit status stays the command's; if the
download itself fails (e.g. over the server's size limit), a 0 exit becomes 125.
Later: tun get <id> PATH... (for any path if the job ran on the server itself; if it ran on
a runner, only paths within the --out paths given here exist). With --detach, --out only
declares the paths (nothing is downloaded): tun run --detach --out dist -- make, then
tun get <id> dist once it has ended.

--tail N streams nothing while the job runs; when it ends, tun prints the last N lines of its
stdout (to stdout) and of its stderr (to stderr), then a "tun: job <id>: showed the last ..."
line with line counts and the commands for the full logs (tun logs <id>; tun logs --stderr <id>).
The exit status is the same as without --tail. N must be 0 or more.

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
		"run": runCmd, "status": statusCmd, "logs": logsCmd, "cancel": cancelCmd, "ls": lsCmd, "get": getCmd, "runners": runnersCmd,
		"bazel": bazelCmd, "sessions": sessionsCmd,
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
	name, _ := os.Hostname()
	return &Client{URL: url, Token: token, Name: name}
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
	timeout := fs.Duration("timeout", 30*time.Minute, "kill the job's whole process group after this long (exit 124; max 168h)")
	noWS := fs.Bool("no-workspace", false, "upload nothing; run in an empty dir")
	detach := fs.Bool("detach", false, "print the job id to stdout and exit 0 immediately")
	jsonOut := fs.Bool("json", false, "don't stream; when the job ends print one JSON object {id, state, exit_code, signal, error,\nduration (seconds), stdout, stderr, stdout_truncated, stderr_truncated} (output: last 64KiB of each)")
	tailN := fs.Int("tail", -1, "don't stream; when the job ends print only the last `N` lines of its stdout and of its\nstderr (full logs: tun logs <id> [--stderr])")
	script := fs.String("c", "", "run this `script` with bash -c instead of argv")
	var outs envFlag
	fs.Var(&outs, "out", "after the job ends, copy `PATH` (file, dir or glob, relative to the workspace root)\nfrom its work dir into the local workspace (repeatable; with --detach: only declare it, for tun get)")
	svc := serviceFlags(fs)
	fs.Usage = func() { fmt.Fprint(e.Stderr, runUsage); fs.PrintDefaults(); fmt.Fprint(e.Stderr, serviceUsage) }
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
	case *timeout > api.MaxTimeoutSec*time.Second:
		return errorf(e, "run: --timeout %v is over the server maximum of %v", *timeout, api.MaxTimeoutSec*time.Second)
	}
	if err := checkPaths(outs); err != nil {
		return errorf(e, "run: --out: %v", err)
	}
	if negativeTail(fs, *tailN) {
		return errorf(e, "run: --tail must not be negative")
	}
	if *tailN >= 0 && (*jsonOut || *detach) {
		return errorf(e, "run: --tail can't be combined with --json or --detach")
	}
	var outDir string
	if len(outs) > 0 && !*detach { // with --detach, --out only declares the outputs
		var err error
		if outDir, err = localRoot(e.Cwd); err != nil {
			return errorf(e, "run: --out: %v", err)
		}
	}
	req.TimeoutSec = int((*timeout + time.Second - 1) / time.Second)
	req.Outputs = outs // a runner uploads exactly these when the job ends
	if err := svc.apply(fs, &req); err != nil {
		return errorf(e, "%v", err)
	}
	if req.Service && *tailN >= 0 {
		return errorf(e, "run: --tail can't be used with --service; read its logs with tun logs --tail N <id>")
	}
	if req.Service && len(outs) > 0 {
		return errorf(e, "run: --out can't be used with --service (a service doesn't end on its own); stop it with tun cancel, then tun get <id> <path>")
	}
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
	var uploadNote bytes.Buffer
	if !*noWS {
		ws, cwd, err := upload(ctx, c, e.Cwd, &uploadNote)
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
		if *tailN >= 0 {
			fmt.Fprintf(e.Stderr, "tun: job %s (--tail %d: output is printed when the job ends; full logs: tun logs %s, tun logs --stderr %s)\n", j.ID, *tailN, j.ID, j.ID)
		} else {
			fmt.Fprintf(e.Stderr, "tun: job %s\n", j.ID)
		}
		if !*noWS {
			e.Stderr.Write(uploadNote.Bytes())
			if root, err := localRoot(e.Cwd); err == nil {
				fmt.Fprintf(e.Stderr, "tun: workspace %s\n", root)
			}
		}
	}
	if j.Service {
		return waitReady(ctx, c, j, *svc.readyTimeout, *jsonOut, e)
	}
	code, final := wait(ctx, cancel, c, j, *jsonOut, *tailN, e)
	if len(outs) > 0 {
		code = fetchOuts(c, j.ID, outs, outDir, code, e)
	}
	if final != nil && !*jsonOut {
		printCompletion(e.Stderr, *final, code)
	}
	return code
}

func printCompletion(w io.Writer, j api.Job, code int) {
	fmt.Fprintf(w, "tun: job %s %s exit=%d after %.3fs (%.3fs queued)\n", j.ID, j.State, code, j.RunSec, j.QueueSec)
}

// upload packs the workspace containing cwd and uploads it unless the server already has it.
// It returns the blob sha and cwd relative to the workspace root.
func upload(ctx context.Context, c *Client, cwd string, report io.Writer) (sha, rel string, err error) {
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
	var size int64
	sha, size, err = workspace.Pack(root, files, tmp)
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
	if has {
		fmt.Fprintf(report, "tun: workspace %dB (already on server)\n", size)
	} else {
		fmt.Fprintf(report, "tun: uploaded %dB workspace\n", size)
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

// lineTail keeps the last n lines written to it (at most 1MiB of them).
type lineTail struct {
	n       int
	lines   int64 // newlines seen
	partial bool  // the last byte seen was not a newline
	cut     bool  // buf lost bytes to the size cap
	buf     []byte
	mutex   sync.Mutex
}

func (t *lineTail) Write(p []byte) (int, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	t.lines += int64(bytes.Count(p, []byte("\n")))
	t.partial = p[len(p)-1] != '\n'
	t.buf = append(t.buf, p...)
	t.buf = t.buf[lastLines(t.buf, t.n):]
	if len(t.buf) > 1<<20 {
		t.buf, t.cut = t.buf[len(t.buf)-1<<20:], true
	}
	return len(p), nil
}

// counts returns the number of lines kept and seen; a final unterminated line counts.
func (t *lineTail) counts() (kept, total int64) {
	kept, total = int64(bytes.Count(t.buf, []byte("\n"))), t.lines
	if t.partial {
		total++
		if len(t.buf) > 0 { // with --tail 0 the partial line wasn't kept
			kept++
		}
	}
	return kept, total
}

// negativeTail reports whether --tail was given a negative value (-1, the default, means unset).
func negativeTail(fs *flag.FlagSet, n int) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "tail" })
	return set && n < 0
}

// lastLines returns the index in b where its last n lines start.
func lastLines(b []byte, n int) int {
	if n == 0 {
		return len(b)
	}
	for i := len(b) - 2; i >= 0; i-- {
		if b[i] == '\n' {
			if n--; n == 0 {
				return i + 1
			}
		}
	}
	return 0
}

// printTails prints what --tail kept, then a line saying how much that was and where the rest is.
func printTails(e Env, id string, tOut, tErr *lineTail) {
	e.Stdout.Write(tOut.buf)
	e.Stderr.Write(tErr.buf)
	if tErr.partial && len(tErr.buf) > 0 {
		fmt.Fprintln(e.Stderr)
	}
	ko, no := tOut.counts()
	ke, ne := tErr.counts()
	note := ""
	if tOut.cut || tErr.cut {
		note = " (lines cut to the last 1MiB)"
	}
	fmt.Fprintf(e.Stderr, "tun: job %s: showed the last %d of %d stdout lines and %d of %d stderr lines%s; full logs: tun logs %s, tun logs --stderr %s\n",
		id, ko, no, ke, ne, note, id, id)
}

// cancelOnSignal cancels job id because this client got sig, and returns tun run's exit code.
func cancelOnSignal(c *Client, id string, sig os.Signal, e Env) int {
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	if _, err := c.Cancel(cctx, id); err != nil {
		return errorf(e, "received %v but could not cancel job %s: %v", sig, id, err)
	}
	fmt.Fprintf(e.Stderr, "tun: received %v; canceled job %s\n", sig, id)
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 125
}

// queuePoll is how often tun run checks whether its queued job has started.
var queuePoll = 500 * time.Millisecond

// waitQueued reports on stderr that job j is queued, then polls until it leaves the queue
// and reports how long it waited. done is true if a signal canceled the job meanwhile.
func waitQueued(ctx context.Context, c *Client, j api.Job, e Env) (_ api.Job, code int, done bool) {
	msg := fmt.Sprintf("%d ahead", *j.QueuePosition-1)
	if h, err := c.Health(ctx); err == nil {
		msg += fmt.Sprintf(", %d running", h.Running)
	}
	if j.QueueReason != "" {
		msg += "; " + j.QueueReason
	}
	fmt.Fprintf(e.Stderr, "tun: queued (%s)\n", msg)
	for j.State == api.Queued {
		select {
		case sig := <-e.Signals:
			return j, cancelOnSignal(c, j.ID, sig, e), true
		case <-time.After(queuePoll):
		}
		next, err := c.Get(ctx, j.ID)
		if err != nil {
			return j, 0, false // the log streams retry and report errors
		}
		j = next
	}
	if j.StartedAt != nil {
		fmt.Fprintf(e.Stderr, "tun: started after %.1fs in queue\n", j.QueueSec)
	}
	return j, 0, false
}

// whereLine says where job j runs: "tun: running on hub" (the server itself) or "tun: running
// on runner NAME"; "" while that isn't decided.
func whereLine(j api.Job) string {
	switch j.Runner {
	case "":
		return ""
	case "local":
		return "tun: running on hub\n"
	}
	return "tun: running on runner " + j.Runner + "\n"
}

// newlineWriter records whether streamed stderr needs a newline before diagnostics.
type newlineWriter struct {
	io.Writer
	partial bool
}

func (w *newlineWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.partial = p[n-1] != '\n'
	}
	return n, err
}

// wait streams (or collects) the job's output until it ends and maps the result to an exit code.
// With tailN >= 0 it prints only the last tailN lines of each stream once the job has ended.
func wait(ctx context.Context, cancel context.CancelFunc, c *Client, j api.Job, jsonOut bool, tailN int, e Env) (int, *api.Job) {
	if j.QueuePosition != nil && !jsonOut {
		var code int
		var done bool
		if j, code, done = waitQueued(ctx, c, j, e); done {
			return code, nil
		}
	}
	if !jsonOut {
		io.WriteString(e.Stderr, whereLine(j))
	}
	stderr := &newlineWriter{Writer: e.Stderr}
	var out, errw io.Writer = e.Stdout, stderr
	tOut, tErr := &tail{max: 64 << 10}, &tail{max: 64 << 10}
	lOut, lErr := &lineTail{n: tailN}, &lineTail{n: tailN}
	if jsonOut {
		out, errw = tOut, tErr
	} else if tailN >= 0 {
		out, errw = lOut, lErr
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
		if tailN >= 0 {
			printTails(e, j.ID, lOut, lErr)
		}
		return cancelOnSignal(c, j.ID, sig, e), nil
	}
	if tailN >= 0 {
		printTails(e, j.ID, lOut, lErr)
	}
	if err := errors.Join(errOut, errErr); err != nil {
		return errorf(e, "%v; job %s may still be running: reattach with 'tun logs -f %s'", err, j.ID, j.ID), nil
	}
	final, err := c.Get(ctx, j.ID)
	if err != nil {
		return errorf(e, "getting status of job %s: %v", j.ID, err), nil
	}
	if !jsonOut && tailN < 0 && stderr.partial {
		fmt.Fprintln(e.Stderr)
	}
	code, msg := exitCode(final)
	if jsonOut {
		res := map[string]any{
			"id": final.ID, "state": final.State, "exit_code": final.ExitCode, "signal": final.Signal,
			"error": final.Error, "tun_exit": code, "queue_sec": final.QueueSec, "run_sec": final.RunSec,
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
	return code, &final
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
	case j.ExitCode != nil && j.Error != "":
		return *j.ExitCode, "exited; server error: " + j.Error // e.g. its logs could not be written
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
	withURLs(newClient(e), &j)
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
	fs := newFlags("logs", "usage: tun logs [-f] [--stderr] [--tail N] <id>\n\nPrint a job's stdout (or stderr) so far; with -f, keep streaming until the job ends.\n\nflags:\n", e)
	follow := fs.Bool("f", false, "follow until the job ends")
	stderr := fs.Bool("stderr", false, "print stderr instead of stdout")
	tailN := fs.Int("tail", -1, "print only the last `N` lines (with -f: start there, then follow)")
	pos, err := parse(fs, args)
	if err != nil {
		return flagErr(e, err)
	}
	if negativeTail(fs, *tailN) {
		return errorf(e, "logs: --tail must not be negative")
	}
	id, ok := oneID("logs", pos, e)
	if !ok {
		return 125
	}
	stream := "stdout"
	if *stderr {
		stream = "stderr"
	}
	c := newClient(e)
	if *tailN >= 0 {
		err = c.LogsTail(context.Background(), id, stream, *tailN, *follow, e.Stdout)
	} else {
		_, err = c.Logs(context.Background(), id, stream, 0, *follow, e.Stdout)
	}
	if err != nil {
		return errorf(e, "%v", err)
	}
	return 0
}

const lsUsage = `usage: tun ls [--limit N] [--state S] [--client NAME | --mine]

Print recent jobs, newest first, one JSON object per line. Filters apply before --limit.
Each job's "client" is who submitted it: the exe.dev source VM when the request came through
the exe.dev proxy, else the submitting host's name, else its IP address.

flags:
`

func lsCmd(args []string, e Env) int {
	fs := newFlags("ls", lsUsage, e)
	limit := fs.Int("limit", 20, "max jobs to list")
	state := fs.String("state", "", "only jobs in state `S` (queued|running|exited|failed|timeout|canceled; comma-separated for several)")
	client := fs.String("client", "", "only jobs submitted by `NAME` (a job's \"client\" field)")
	mine := fs.Bool("mine", false, "only jobs submitted from this host (as the server identifies it; see above)")
	pos, err := parse(fs, args)
	if err != nil {
		return flagErr(e, err)
	}
	switch {
	case len(pos) > 0:
		return errorf(e, "usage: tun ls [--limit N] [--state S] [--client NAME | --mine]")
	case *mine && *client != "":
		return errorf(e, "ls: give --client or --mine, not both")
	}
	filter := url.Values{}
	if *state != "" {
		filter.Set("state", *state)
	}
	if *client != "" {
		filter.Set("client", *client)
	}
	if *mine {
		filter.Set("mine", "1")
	}
	c := newClient(e)
	js, err := c.List(context.Background(), *limit, filter)
	if err != nil {
		return errorf(e, "%v", err)
	}
	for _, j := range js {
		withURLs(c, &j)
		printJSON(e, j)
	}
	return 0
}
