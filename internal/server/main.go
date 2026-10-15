package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/altdansalt/tun/internal/reapi"
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
	"github.com/altdansalt/tun/internal/workspace"
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
a queued/running job for --keep-blobs. A duration of 0 keeps forever. When free space on
the data dir's filesystem is under --min-free, finished jobs' work dirs are deleted before
--keep-work, oldest first, then idle tun bazel sessions, least recently used first (checked
before each job starts and every minute; healthz "disk" reports it). Each job's stdout and
stderr are capped at --max-log; the workspace's unpacked size at --max-workspace.

Shutdown: SIGINT/SIGTERM reject new submissions with retryable 503 responses and finish
admitted jobs, including the queue, for up to --drain-timeout (default 15m). Then unfinished
work is killed. Under systemd use KillMode=mixed and TimeoutStopSec=17min; see docs/deploy.md.

Runners: other machines can run this server's jobs with 'tun runner --hub URL' (they poll
it; it never connects to them). Queued jobs go FIFO to any free slot: one of the
--local-slots or a runner's. --local-slots 0 makes a pure hub. Services always run here.
A runner job keeps only the files declared with tun run --out (uploaded when it ends).

Bazel sessions (tun bazel): each (client, workspace, session name) gets a persistent dir
under $DATA/sessions with the synced workspace and Bazel's output base, and a long-lived
sandbox in which the Bazel server stays up between commands. A session is deleted after
--session-idle without a command; at most --max-sessions exist (a Bazel server takes 1GB+ of
memory): a new one evicts the least recently used idle one. tun bazel commands take local
slots like other jobs. Each client has its own Bazel caches (bazelisk downloads, repository
cache, disk cache) in $DATA/bazel.

Remote cache (--cache-dir, default "reapi" in the data dir; --cache-dir= disables it): Bazel's
remote cache protocol (REAPI gRPC over h2c) on this listener and on $DATA/cache.sock, both
with the token (Bazel: --remote_cache=grpc://HOST:PORT --remote_header=x-tun-token=TOKEN).
Every tun bazel session uses it without asking: tun's bazelrc points Bazel at the session's
own socket, which needs no token. File contents are shared (they are verified); action
results are per client: what a client uploads answers only its own lookups.

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
	localSlots := fs.Int("local-slots", -1, "jobs run at once on this host; -1: --max-jobs; 0: none, every non-service job runs on a runner (tun runner)")
	drainTimeout := fs.Duration("drain-timeout", 15*time.Minute, "finish admitted jobs before shutdown (then kill remaining work)")
	runnerTimeout := fs.Duration("runner-timeout", 30*time.Second, "a runner that hasn't polled for this long is lost: its running jobs fail, unstarted ones are requeued")
	maxServices := fs.Int("max-services", 4, "service jobs (tun run --service) run at once, not counted in --max-jobs; more are rejected")
	netMode := fs.String("net", "auto", "auto|private|host: private gives each bwrap job its own network namespace (outbound via an HTTP(S) proxy; services only through the server); auto uses it when it works")
	mode := fs.String("sandbox", "auto", "auto|bwrap|none: auto uses bwrap if it works on this host; none gives jobs no isolation from the server")
	cacheDir := fs.String("cache-dir", "reapi", "REAPI gRPC remote cache in this directory (relative to the data dir); empty disables it")
	cacheMax := fs.String("cache-max-size", "10GB", "REAPI cache LRU size limit")
	cacheWrites := fs.Bool("cache-action-writes", true, "allow clients to update the REAPI action cache")
	cacheSocket := fs.String("cache-socket", "", "REAPI unix socket path (default DATA/cache.sock when enabled)")
	maxUpload := fs.String("max-upload", "512MB", "max workspace upload size (bytes, or with KB/MB/GB suffix)")
	maxDownload := fs.String("max-download", "1GB", "max total file bytes per files request (tun run --out, tun get)")
	maxWorkspace := fs.String("max-workspace", "2GB", "max total size of a job's unpacked workspace files (bytes, or with KB/MB/GB suffix)")
	maxLog := fs.String("max-log", "256MB", "max bytes kept of each job's stdout and of its stderr; past it the server appends a marker and discards the rest")
	keepWork := fs.Duration("keep-work", time.Hour, "delete a finished job's sandbox dir (work, home, tmp) this long after it ends (0: never)")
	keepJobs := fs.Duration("keep-jobs", 7*24*time.Hour, "delete a finished job's record and logs this long after it ends (0: never)")
	minFree := fs.String("min-free", "10%", "free space to keep on the data dir's filesystem (bytes with KB/MB/GB, a percentage, or 0): under it, finished jobs' work dirs (oldest first), then idle tun bazel sessions (least recently used) are deleted before a job starts")
	keepBlobs := fs.Duration("keep-blobs", 24*time.Hour, "delete workspace blobs unused for this long (0: never)")
	maxSessions := fs.Int("max-sessions", 2, "tun bazel sessions at once (each can hold a Bazel server, 1GB+ of memory); a new one evicts the least recently used idle one")
	bazeliskSeed := fs.String("bazelisk-seed", "", "copy this bazelisk home's downloads/ (e.g. ~/.cache/bazelisk; must not be writable by jobs) into each client's new bazelisk home for tun bazel, so Bazel isn't downloaded again")
	sessionIdle := fs.Duration("session-idle", 30*time.Minute, "delete a tun bazel session (its Bazel server, output base and workspace copy) after this long without a command")
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
	if *cacheSocket != "" && *cacheDir == "" {
		return fail("--cache-socket requires --cache-dir")
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
		defer func() {
			// workspace.RemoveAll: jobs may leave read-only dirs (chmod, Go's module cache).
			if err := workspace.RemoveAll(dir); err != nil {
				fmt.Fprintf(stderr, "tun: could not remove temporary data dir: %v\n", err)
			}
		}()
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
	mf, err := ParseMinFree(*minFree)
	if err != nil {
		return fail("--min-free: %v", err)
	}
	if *mode, err = resolveSandbox(*mode); err != nil {
		return fail("%v", err)
	}
	if *mode == sandbox.None {
		fmt.Fprintln(stderr, "tun: warning: sandbox none: jobs run unisolated as this user and can read the token and other jobs' files")
	}
	private, err := resolveNet(*netMode, *mode, stderr)
	if err != nil {
		return fail("%v", err)
	}
	if *localSlots >= 0 {
		*maxJobs = *localSlots
	} else if *maxJobs < 1 {
		return fail("--max-jobs must be at least 1 (for a hub that only dispatches to runners: --local-slots 0)")
	}
	if *maxSessions < 1 || *sessionIdle <= 0 {
		return fail("--max-sessions must be at least 1 and --session-idle positive")
	}
	if *runnerTimeout < time.Second {
		return fail("--runner-timeout must be at least 1s")
	}
	var store *reapi.Store
	if *cacheDir != "" {
		dir := *cacheDir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(*data, dir)
		}
		limit, err := parseSize(*cacheMax)
		if err != nil {
			return fail("--cache-max-size: %v", err)
		}
		if store, err = reapi.Open(dir, limit); err != nil {
			return fail("cache: %v", err)
		}
	}
	s, err := New(Config{DataDir: *data, MaxJobs: *maxJobs, HubOnly: *maxJobs == 0, RunnerDeadAfter: *runnerTimeout, MaxServices: *maxServices, Sandbox: *mode, PrivateNet: private, MaxUpload: limit, MaxDownload: dlimit,
		MaxWorkspace: wsLimit, MaxLog: logLimit, KeepWork: *keepWork, KeepJobs: *keepJobs, KeepBlobs: *keepBlobs, MinFree: mf,
		MaxSessions: *maxSessions, SessionIdle: *sessionIdle, BazeliskSeed: *bazeliskSeed, Cache: store, CacheReadOnly: !*cacheWrites})
	if err != nil {
		return fail("opening data dir %s: %v", *data, err)
	}
	defer s.Close()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fail("listen: %v", err)
	}
	handler := s.Handler()
	if store != nil {
		cache := s.CacheHandler()
		socket := *cacheSocket
		if socket == "" {
			socket = filepath.Join(s.cfg.DataDir, "cache.sock")
		}
		// Bind through a short directory descriptor path: job TMPDIRs can exceed
		// Linux's 108-byte sockaddr_un limit even for ephemeral servers.
		socketDir, err := os.Open(filepath.Dir(socket))
		if err != nil {
			ln.Close()
			return fail("cache socket directory: %v", err)
		}
		defer socketDir.Close()
		bindPath := fmt.Sprintf("/proc/%d/fd/%d/%s", os.Getpid(), socketDir.Fd(), filepath.Base(socket))
		if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			conn, dialErr := net.DialTimeout("unix", bindPath, time.Second)
			if dialErr == nil {
				conn.Close()
				ln.Close()
				return fail("cache socket already in use: %s", socket)
			}
			if !errors.Is(dialErr, syscall.ECONNREFUSED) {
				ln.Close()
				return fail("cache socket probe: %v", dialErr)
			}
			if err := os.Remove(socket); err != nil {
				ln.Close()
				return fail("remove stale cache socket: %v", err)
			}
		}
		ul, err := net.Listen("unix", bindPath)
		if err != nil {
			ln.Close()
			return fail("cache socket: %v", err)
		}
		if err := os.Chmod(socket, 0600); err != nil {
			ul.Close()
			ln.Close()
			return fail("cache socket permissions: %v", err)
		}
		us := &http.Server{Handler: cache, Protocols: reapi.Protocols(), ReadHeaderTimeout: 10 * time.Second}
		defer os.Remove(socket)
		defer us.Close()
		fmt.Fprintf(stderr, "tun: REAPI cache unix socket %s (alias %s)\n", socket, bindPath)
		go us.Serve(ul)
	}
	hs := &http.Server{Handler: handler, Protocols: reapi.Protocols(), ReadHeaderTimeout: 10 * time.Second}
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
		fmt.Fprintf(stderr, "tun: %v: draining admitted jobs (limit %s)\n", sig, *drainTimeout)
		if !s.Drain(*drainTimeout) {
			fmt.Fprintln(stderr, "tun: drain timeout; unfinished jobs will fail")
		}
		// Allow final status/log responses to finish before closing HTTP.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		hs.Shutdown(ctx)
		cancel()
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
