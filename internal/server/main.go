package server

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/sandbox"
)

const serveUsage = `usage: tun serve [flags]

Run the tun server: accepts workspace uploads and jobs over HTTP (see SPEC.md for the API),
runs each job in a fresh per-job directory and sandbox, and keeps job state and logs under
the data dir. On first start it writes a bearer token to $DATA/token (0600); local clients
using the default data dir find it automatically. Jobs left queued/running by a previous
server process are marked failed on startup.

Sandboxes: bwrap (used by auto when it works) gives each job a read-only root, its own
$DATA/jobs/<id>/sandbox dir and the shared caches, and hides the rest of the data dir (token,
other jobs, its own logs/state) and the server user's home. none runs jobs as plain
subprocesses of this server user: NO ISOLATION - a job can read the token and every other
job's files. Use none only if every job is as trusted as whoever runs tun serve.

Network (--net): private (used by auto with bwrap when it works) gives each job its own
network namespace with only loopback: jobs can't reach each other, services, or the host's
ports. Outbound access goes through an HTTP proxy run by this server (HTTP_PROXY/HTTPS_PROXY
are set in the job, which most tools honor: go, git, curl, pip, npm), so HTTP(S) works but raw
TCP/UDP and DNS lookups from the job don't. Services are reached only through the server's
authenticated proxy. host: jobs share this host's network (any job can connect to any
service's port without the token). healthz reports "network_isolation".

Retention: a sweep at startup and periodically deletes finished jobs' sandbox dir (work,
home, tmp) after --keep-work (the job then shows "work_deleted": true; status and logs still
work), whole job records (job.json + logs) after --keep-jobs, and workspace blobs not used by
a queued/running job for --keep-blobs. A duration of 0 keeps forever. Each job's stdout and
stderr are capped at --max-log; the workspace's unpacked size at --max-workspace.

Runners: other machines can run this server's jobs with 'tun runner --hub URL' (they poll
it; it never connects to them). Queued jobs go FIFO to any free slot: one of the
--local-slots or a runner's. --local-slots 0 makes a pure hub. Services always run here.
A runner job keeps only the files declared with tun run --out (uploaded when it ends).

flags:
`

// DefaultDataDir is ~/.local/share/tun.
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tun"
	}
	return filepath.Join(home, ".local", "share", "tun")
}

// Main implements `tun serve`. It returns the process exit code.
func Main(args []string, stderr io.Writer) int {
	return serve(args, os.Stdin, os.Stdout, stderr)
}

func serve(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ephemeral := fs.Bool("ephemeral", false, "use random loopback port and temporary data; print TUN_URL and TUN_TOKEN on stdout when ready")
	exitOnEOF := fs.Bool("exit-on-eof", false, "shut down when stdin closes")
	addr := fs.String("addr", "127.0.0.1:7070", "listen address")
	data := fs.String("data", DefaultDataDir(), "data dir (token, blobs, jobs, caches)")
	maxJobs := fs.Int("max-jobs", runtime.NumCPU(), "jobs run at once on this host; the rest queue FIFO (same as --local-slots)")
	localSlots := fs.Int("local-slots", -1, "jobs run at once on this host (default --max-jobs); 0: none, every non-service job runs on a runner (tun runner)")
	runnerTimeout := fs.Duration("runner-timeout", 30*time.Second, "a runner that hasn't polled for this long is lost: its running jobs fail, unstarted ones are requeued")
	maxServices := fs.Int("max-services", 4, "service jobs (tun run --service) run at once, not counted in --max-jobs; more are rejected")
	netMode := fs.String("net", "auto", "auto|private|host: private gives each bwrap job its own network namespace (outbound via an HTTP(S) proxy; services only through the server); auto uses it when it works")
	mode := fs.String("sandbox", "auto", "auto|bwrap|none: auto uses bwrap if it works on this host; none gives jobs no isolation from the server")
	maxUpload := fs.String("max-upload", "512MB", "max workspace upload size (bytes, or with KB/MB/GB suffix)")
	maxDownload := fs.String("max-download", "1GB", "max total file bytes per files request (tun run --out, tun get)")
	maxWorkspace := fs.String("max-workspace", "2GB", "max total size of a job's unpacked workspace files (bytes, or with KB/MB/GB suffix)")
	maxLog := fs.String("max-log", "256MB", "max bytes kept of each job's stdout and of its stderr; past it the server appends a marker and discards the rest")
	keepWork := fs.Duration("keep-work", time.Hour, "delete a finished job's sandbox dir (work, home, tmp) this long after it ends (0: never)")
	keepJobs := fs.Duration("keep-jobs", 7*24*time.Hour, "delete a finished job's record and logs this long after it ends (0: never)")
	keepBlobs := fs.Duration("keep-blobs", 24*time.Hour, "delete workspace blobs unused for this long (0: never)")
	fs.Usage = func() { fmt.Fprint(stderr, serveUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 125
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "tun: "+format+"\n", a...)
		return 125
	}
	if fs.NArg() > 0 {
		return fail("serve takes no arguments, got %q", fs.Args())
	}
	if *ephemeral {
		conflict := ""
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "addr" || f.Name == "data" {
				conflict = f.Name
			}
		})
		if conflict != "" {
			return fail("--ephemeral cannot be combined with --%s", conflict)
		}
		*addr = "127.0.0.1:0"
		dir, err := os.MkdirTemp("", "tun-ephemeral-")
		if err != nil {
			return fail("temporary data dir: %v", err)
		}
		defer os.RemoveAll(dir)
		*data = dir
	}
	limit, err := parseSize(*maxUpload)
	if err != nil {
		return fail("--max-upload: %v", err)
	}
	dlimit, err := parseSize(*maxDownload)
	if err != nil {
		return fail("--max-download: %v", err)
	}
	wsLimit, err := parseSize(*maxWorkspace)
	if err != nil {
		return fail("--max-workspace: %v", err)
	}
	logLimit, err := parseSize(*maxLog)
	if err != nil {
		return fail("--max-log: %v", err)
	}
	if *keepWork < 0 || *keepJobs < 0 || *keepBlobs < 0 {
		return fail("--keep-work, --keep-jobs and --keep-blobs must not be negative")
	}
	if *mode, err = resolveSandbox(*mode); err != nil {
		return fail("%v", err)
	}
	if *mode == sandbox.None {
		fmt.Fprintln(stderr, "tun: warning: sandbox none: jobs run unisolated as this user and can read the token and other jobs' files")
	}
	private := false
	switch *netMode {
	case "auto", "private":
		if *mode != sandbox.Bwrap {
			if *netMode == "private" {
				return fail("--net private needs --sandbox bwrap")
			}
			break
		}
		err := sandbox.NetUsable()
		if err != nil && *netMode == "private" {
			return fail("--net private: cannot give jobs a private network namespace: %v (try --net host)", err)
		}
		private = err == nil
		if err != nil {
			fmt.Fprintf(stderr, "tun: warning: no network isolation (bwrap --unshare-net with the tun helper failed: %v): jobs share the host network, so any job can reach services' ports without the token (healthz: \"network_isolation\": false)\n", err)
		}
	case "host":
		if *mode == sandbox.Bwrap {
			fmt.Fprintln(stderr, "tun: warning: --net host: jobs share the host network, so any job can reach services' ports without the token")
		}
	default:
		return fail("--net must be auto, private or host, got %q", *netMode)
	}
	if *localSlots >= 0 {
		*maxJobs = *localSlots
	} else if *maxJobs < 1 {
		return fail("--max-jobs must be at least 1 (for a hub that only dispatches to runners: --local-slots 0)")
	}
	if *runnerTimeout < time.Second {
		return fail("--runner-timeout must be at least 1s")
	}
	s, err := New(Config{DataDir: *data, MaxJobs: *maxJobs, HubOnly: *maxJobs == 0, RunnerDeadAfter: *runnerTimeout, MaxServices: *maxServices, Sandbox: *mode, PrivateNet: private, MaxUpload: limit, MaxDownload: dlimit,
		MaxWorkspace: wsLimit, MaxLog: logLimit, KeepWork: *keepWork, KeepJobs: *keepJobs, KeepBlobs: *keepBlobs})
	if err != nil {
		return fail("opening data dir %s: %v", *data, err)
	}
	defer s.Close()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fail("listen: %v", err)
	}
	hs := &http.Server{Handler: s.Handler()}
	netDesc := "host"
	if private {
		netDesc = "private"
	}
	fmt.Fprintf(stderr, "tun: serving on http://%s (data %s, sandbox %s, net %s, local slots %d)\n", ln.Addr(), s.cfg.DataDir, *mode, netDesc, *maxJobs)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	eof := make(chan struct{})
	if *exitOnEOF {
		go func() { _, _ = io.Copy(io.Discard, stdin); close(eof) }()
	}
	if *ephemeral {
		if _, err := fmt.Fprintf(stdout, "TUN_URL=http://%s\nTUN_TOKEN=%s\n", ln.Addr(), s.Token()); err != nil {
			hs.Close()
			return fail("write environment: %v", err)
		}
	}
	select {
	case <-eof:
	case sig := <-sigs:
		fmt.Fprintf(stderr, "tun: %v: shutting down; running jobs are killed and will show as failed\n", sig)
	case err := <-errc:
		fmt.Fprintf(stderr, "tun: serve: %v\n", err)
	}
	hs.Close()
	return 0
}

func parseSize(v string) (int64, error) {
	mult := int64(1)
	u := strings.ToUpper(strings.TrimSpace(v))
	for _, suf := range []struct {
		s string
		m int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(u, suf.s) {
			u, mult = strings.TrimSuffix(u, suf.s), suf.m
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(u), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad size %q (want e.g. 512MB)", v)
	}
	return n * mult, nil
}
