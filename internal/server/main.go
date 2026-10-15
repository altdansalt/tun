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

Retention: a sweep at startup and periodically deletes finished jobs' sandbox dir (work,
home, tmp) after --keep-work (the job then shows "work_deleted": true; status and logs still
work), whole job records (job.json + logs) after --keep-jobs, and workspace blobs not used by
a queued/running job for --keep-blobs. A duration of 0 keeps forever. Each job's stdout and
stderr are capped at --max-log; the workspace's unpacked size at --max-workspace.

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
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:7070", "listen address")
	data := fs.String("data", DefaultDataDir(), "data dir (token, blobs, jobs, caches)")
	maxJobs := fs.Int("max-jobs", runtime.NumCPU(), "jobs run at once; the rest queue FIFO")
	mode := fs.String("sandbox", "auto", "auto|bwrap|none: auto uses bwrap if it works on this host; none gives jobs no isolation from the server")
	maxUpload := fs.String("max-upload", "512MB", "max workspace upload size (bytes, or with KB/MB/GB suffix)")
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
	limit, err := parseSize(*maxUpload)
	if err != nil {
		return fail("--max-upload: %v", err)
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
	switch *mode {
	case "auto":
		*mode = sandbox.None
		if sandbox.BwrapUsable() {
			*mode = sandbox.Bwrap
		}
	case sandbox.Bwrap:
		if !sandbox.BwrapUsable() {
			return fail("--sandbox bwrap: bwrap is not installed or cannot create namespaces here (try --sandbox none)")
		}
	case sandbox.None:
	default:
		return fail("--sandbox must be auto, bwrap or none, got %q", *mode)
	}
	if *mode == sandbox.None {
		fmt.Fprintln(stderr, "tun: warning: sandbox none: jobs run unisolated as this user and can read the token and other jobs' files")
	}
	s, err := New(Config{DataDir: *data, MaxJobs: *maxJobs, Sandbox: *mode, MaxUpload: limit,
		MaxWorkspace: wsLimit, MaxLog: logLimit, KeepWork: *keepWork, KeepJobs: *keepJobs, KeepBlobs: *keepBlobs})
	if err != nil {
		return fail("opening data dir %s: %v", *data, err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fail("listen: %v", err)
	}
	hs := &http.Server{Handler: s.Handler()}
	fmt.Fprintf(stderr, "tun: serving on http://%s (data %s, sandbox %s, max-jobs %d)\n", ln.Addr(), s.cfg.DataDir, *mode, *maxJobs)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case sig := <-sigs:
		fmt.Fprintf(stderr, "tun: %v: shutting down; running jobs are killed and will show as failed\n", sig)
	case err := <-errc:
		fmt.Fprintf(stderr, "tun: serve: %v\n", err)
	}
	hs.Close()
	s.Close()
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
