package server

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/sandbox"
)

const runnerUsage = `usage: tun runner --hub URL [flags]

Run jobs for a tun hub (a tun serve this machine can reach; the hub never connects to the
runner). The runner long-polls the hub for work (the poll is also its heartbeat), downloads
each job's workspace (cached by sha), runs it in a fresh per-job dir and sandbox exactly as
tun serve would, streams stdout/stderr to the hub, and when the job ends uploads the paths the
client declared with tun run --out, then reports the exit status. Clients see no difference
except the job's "runner" field (and that only declared --out paths can be fetched with
tun get). tun runners (or the hub's /v1/healthz) lists live runners.

If the hub hears nothing from a runner for its --runner-timeout (default 30s), the runner's
running jobs fail and unstarted ones are requeued. On SIGINT/SIGTERM the runner stops taking
work, kills its jobs, reports them failed and tells the hub it left.

Names: the newest runner with a given --name wins. When another runner process starts with
the same name, the hub stops giving this one work; it finishes the jobs it has and exits
with status 1. A runner holds a lock on its --data dir, so two can't share one.

The token is the hub's client token (for now; a separate runner credential is future work):
--token-file, else $TUN_TOKEN, else ~/.config/tun/token. With --sandbox none a job can read
it, like on the hub. With --net private (auto picks it when it works) each job has its own
network namespace and reaches out only over HTTP(S), through a proxy run by this runner (the
job's traffic leaves from the runner host).

flags:
`

// DefaultRunnerDataDir is ~/.local/share/tun-runner.
func DefaultRunnerDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tun-runner"
	}
	return filepath.Join(home, ".local", "share", "tun-runner")
}

// resolveSandbox turns auto into bwrap or none and checks bwrap works if asked for.
func resolveSandbox(mode string) (string, error) {
	switch mode {
	case "auto":
		if sandbox.BwrapUsable() {
			return sandbox.Bwrap, nil
		}
		return sandbox.None, nil
	case sandbox.Bwrap:
		if !sandbox.BwrapUsable() {
			return "", fmt.Errorf("--sandbox bwrap: bwrap is not installed or cannot create namespaces here (try --sandbox none)")
		}
		return mode, nil
	case sandbox.None:
		return mode, nil
	}
	return "", fmt.Errorf("--sandbox must be auto, bwrap or none, got %q", mode)
}

// resolveNet decides whether jobs get a private network namespace (--net auto|private|host
// with the resolved sandbox mode), warning on stderr where isolation is missing.
func resolveNet(netMode, mode string, stderr io.Writer) (bool, error) {
	switch netMode {
	case "auto", "private":
		if mode != sandbox.Bwrap {
			if netMode == "private" {
				return false, fmt.Errorf("--net private needs --sandbox bwrap")
			}
			return false, nil
		}
		err := sandbox.NetUsable()
		if err != nil && netMode == "private" {
			return false, fmt.Errorf("--net private: cannot give jobs a private network namespace: %v (try --net host)", err)
		}
		if err != nil {
			fmt.Fprintf(stderr, "tun: warning: no network isolation (bwrap --unshare-net with the tun helper failed: %v): jobs share the host network, so any job can reach services' ports without the token (healthz: \"network_isolation\": false)\n", err)
		}
		return err == nil, nil
	case "host":
		if mode == sandbox.Bwrap {
			fmt.Fprintln(stderr, "tun: warning: --net host: jobs share the host network, so any job can reach services' ports without the token")
		}
		return false, nil
	}
	return false, fmt.Errorf("--net must be auto, private or host, got %q", netMode)
}

// RunnerMain implements `tun runner`. It returns the process exit code.
func RunnerMain(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hub := fs.String("hub", os.Getenv("TUN_URL"), "hub URL (default $TUN_URL)")
	tokenFile := fs.String("token-file", "", "file with the hub's token (default $TUN_TOKEN, else ~/.config/tun/token)")
	slots := fs.Int("slots", runtime.NumCPU(), "jobs run at once")
	host, _ := os.Hostname()
	name := fs.String("name", host, "runner name, unique per hub (shown as the job's \"runner\")")
	data := fs.String("data", DefaultRunnerDataDir(), "data dir (blob cache, job dirs, build caches); not a tun serve data dir")
	mode := fs.String("sandbox", "auto", "auto|bwrap|none, as for tun serve")
	netMode := fs.String("net", "auto", "auto|private|host, as for tun serve: private gives each job its own network namespace, with outbound HTTP(S) through a proxy this runner serves")
	fs.Usage = func() { fmt.Fprint(stderr, runnerUsage); fs.PrintDefaults() }
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
	switch {
	case fs.NArg() > 0:
		return fail("runner takes no arguments, got %q", fs.Args())
	case *hub == "":
		return fail("runner: --hub URL is required (or set $TUN_URL)")
	}
	token := os.Getenv("TUN_TOKEN")
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fail("runner: --token-file: %v", err)
		}
		token = strings.TrimSpace(string(b))
	} else if home, _ := os.UserHomeDir(); token == "" && home != "" {
		if b, err := os.ReadFile(filepath.Join(home, ".config/tun/token")); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	if token == "" {
		fmt.Fprintln(stderr, "tun: warning: runner: no token (--token-file, $TUN_TOKEN, ~/.config/tun/token); requests go without one")
	}
	sb, err := resolveSandbox(*mode)
	if err != nil {
		return fail("%v", err)
	}
	if sb == sandbox.None {
		fmt.Fprintln(stderr, "tun: warning: sandbox none: jobs run unisolated as this user and can read the token and other jobs' files")
	}
	private, err := resolveNet(*netMode, sb, stderr)
	if err != nil {
		return fail("%v", err)
	}
	logf := func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }
	r, err := NewRunner(RunnerConfig{Hub: *hub, Token: token, Name: *name, Slots: *slots, DataDir: *data, Sandbox: sb, PrivateNet: private, Logf: logf})
	if err != nil {
		return fail("runner: %v", err)
	}
	netDesc := "host"
	if private {
		netDesc = "private"
	}
	logf("tun runner: %s polling %s (slots %d, sandbox %s, net %s, data %s)", *name, *hub, *slots, sb, netDesc, r.cfg.DataDir)
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		sig := <-sigs
		logf("tun runner: %v: killing jobs and reporting them failed (again to quit at once)", sig)
		go func() { <-sigs; r.Kill() }()
		r.Shutdown(30 * time.Second)
	}()
	r.Run()
	if msg := r.Superseded(); msg != "" {
		fmt.Fprintf(stderr, "tun: runner: %s; exiting\n", msg)
		return 1
	}
	return 0
}
