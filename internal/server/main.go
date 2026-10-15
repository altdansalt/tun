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

	"github.com/altdansalt/tun/internal/sandbox"
)

const serveUsage = `usage: tun serve [flags]

Run the tun server: accepts workspace uploads and jobs over HTTP (see SPEC.md for the API),
runs each job in a fresh per-job directory and sandbox, and keeps job state and logs under
the data dir. On first start it writes a bearer token to $DATA/token (0600); local clients
using the default data dir find it automatically. Jobs left queued/running by a previous
server process are marked failed on startup.

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
	mode := fs.String("sandbox", "auto", "auto|bwrap|none: auto uses bwrap if it works on this host")
	maxUpload := fs.String("max-upload", "512MB", "max workspace upload size (bytes, or with KB/MB/GB suffix)")
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
	s, err := New(Config{DataDir: *data, MaxJobs: *maxJobs, Sandbox: *mode, MaxUpload: limit})
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
