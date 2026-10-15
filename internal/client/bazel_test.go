package client

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

// fakeBazel stands in for bazel: it skips the startup options tun adds, keeps a "server"
// (a sleep in its own session, like Bazel's daemon) whose pid is in the output base's server
// dir, and prints what it was asked to do.
const fakeBazel = `#!/bin/bash
for a; do
  case $a in --output_base=*) ob=${a#--output_base=} ;; --*) ;; *) break ;; esac
  shift
done
cmd=$1; shift
mkdir -p "$ob/server"
pid=$(cat "$ob/server/pid" 2>/dev/null)
if [ "$cmd" = shutdown ]; then
  [ -n "$pid" ] && kill "$pid"
  rm -f "$ob/server/pid"
  exit 0
fi
if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
  echo "warm server $pid" >&2
else
  echo "Starting local Bazel server" >&2
  setsid sleep 60 </dev/null >/dev/null 2>&1 &
  pid=$!
  # Like Bazel's client, return only once the daemon is detached (its own session).
  while [ "$(ps -o sid= -p $pid | tr -d ' ')" != "$pid" ]; do sleep 0.01; done
  echo $pid > "$ob/server/pid"
fi
echo "cmd=$cmd args=$*"
echo "pwd=$PWD home=$HOME tmp=$TMPDIR"
case $cmd in
  fail) exit 3 ;;
  sleep) sleep "$1" ;;
  cat) cat "$@" ;;
esac
`

// bazelServer starts a server whose bazel is fakeBazel (or the real one with real=true).
func bazelServer(t *testing.T, cfg server.Config, real bool) *testServer {
	t.Helper()
	if !real {
		p := filepath.Join(t.TempDir(), "fake-bazel")
		if err := os.WriteFile(p, []byte(fakeBazel), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.BazelCommand = p
		if cfg.Sandbox == sandbox.Bwrap {
			cfg.BazelCommand = "/work/fake-bazel" // the host's /tmp is not visible in the session
		}
	}
	return startServer(t, cfg)
}

// bazelWS creates a git workspace (so its subdirs belong to it) with files and fakeBazel
// (for bwrap sessions) in it, untracked.
func bazelWS(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}
	files["fake-bazel"] = fakeBazel
	for name, data := range files {
		mode := os.FileMode(0o644)
		if name == "fake-bazel" {
			mode = 0o755
		}
		write(t, filepath.Join(dir, name), data, mode)
	}
	return dir
}

func write(t *testing.T, p, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func mtimeOf(t *testing.T, p string) time.Time {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func (ts *testServer) sessions() []api.Session {
	ts.t.Helper()
	r := ts.tun("", "sessions")
	if r.code != 0 {
		ts.t.Fatalf("tun sessions: %+v", r)
	}
	var out []api.Session
	for _, l := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if l == "" {
			continue
		}
		var s api.Session
		if err := json.Unmarshal([]byte(l), &s); err != nil {
			ts.t.Fatalf("%v: %q", err, l)
		}
		out = append(out, s)
	}
	return out
}

var jobLineRE = regexp.MustCompile(`tun: job (\S+) \(session (\S+)\)`)

func jobOf(t *testing.T, r result) string {
	t.Helper()
	m := jobLineRE.FindStringSubmatch(r.stderr)
	if m == nil {
		t.Fatalf("no job line in %q", r.stderr)
	}
	return m[1]
}

func TestBazelSession(t *testing.T) {
	ts := bazelServer(t, server.Config{}, false)
	ws := bazelWS(t, map[string]string{"in.txt": "one\n", "sub/BUILD": "", "same1": "x", "same2": "x"})

	r := ts.tun(ws, "bazel", "build", "//:a b", "--config=x")
	if r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") || !strings.Contains(r.stdout, "cmd=build args=//:a b --config=x") {
		t.Fatalf("first: %+v", r)
	}
	if !strings.Contains(r.stderr, "tun: sync: 6 manifest entries") || !strings.Contains(r.stderr, "uploaded 4 of 4 files") ||
		!regexp.MustCompile(`tun: job \S+ exited exit=0 after`).MatchString(r.stderr) {
		t.Fatalf("first: diagnostics: %s", r.stderr)
	}
	id := jobOf(t, r)
	j := ts.status(id)
	if j.Session != "default" || j.SessionID == "" || j.Runner != api.LocalRunner || strings.Join(j.Argv, " ") != "bazel build //:a b --config=x" {
		t.Fatalf("job: %+v", j)
	}
	workDir := filepath.Join(ts.data, "sessions", j.SessionID, "sandbox", "work")
	if !strings.Contains(r.stdout, "pwd="+workDir+" ") {
		t.Fatalf("pwd: %s", r.stdout)
	}
	old := mtimeOf(t, filepath.Join(workDir, "in.txt"))
	time.Sleep(10 * time.Millisecond) // a rewrite would have a later mtime

	// No-op: nothing uploaded, nothing written, the server is warm.
	r = ts.tun(ws, "bazel", "build", "//...")
	if r.code != 0 || !strings.Contains(r.stderr, "warm server") || strings.Contains(r.stderr, "Starting local") ||
		!strings.Contains(r.stderr, "uploaded 0 of 4 files (0B sent)") || !strings.Contains(r.stderr, "0 added, 0 updated, 0 deleted, 6 unchanged (0B written)") {
		t.Fatalf("no-op: %+v", r)
	}
	// One changed file: one blob uploaded, one file rewritten, the other untouched.
	write(t, filepath.Join(ws, "sub/BUILD"), "changed\n", 0o644)
	r = ts.tun(filepath.Join(ws, "sub"), "bazel", "fail")
	if r.code != 3 || !strings.Contains(r.stderr, "uploaded 1 of 4 files") || !strings.Contains(r.stderr, "0 added, 1 updated, 0 deleted, 5 unchanged (8B written)") ||
		!strings.Contains(r.stdout, "pwd="+workDir+"/sub ") {
		t.Fatalf("change: %+v", r)
	}
	if !mtimeOf(t, filepath.Join(workDir, "in.txt")).Equal(old) {
		t.Fatal("sync touched an unchanged file")
	}
	if b, _ := os.ReadFile(filepath.Join(workDir, "sub/BUILD")); string(b) != "changed\n" {
		t.Fatalf("sub/BUILD = %q", b)
	}
	// Deleted locally: deleted in the session.
	os.Remove(filepath.Join(ws, "same2"))
	if r = ts.tun(ws, "bazel", "cat", "same1"); r.code != 0 || !strings.Contains(r.stderr, "1 deleted") {
		t.Fatalf("delete: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(workDir, "same2")); !os.IsNotExist(err) {
		t.Fatal("same2 not deleted")
	}

	ss := ts.sessions()
	root, _ := filepath.EvalSymlinks(ws)
	if len(ss) != 1 || ss[0].Name != "default" || ss[0].Commands != 4 || ss[0].State != "idle" || !ss[0].Server ||
		ss[0].Root != root || ss[0].ExpiresAt.Sub(ss[0].LastUsed) != 30*time.Minute {
		t.Fatalf("sessions: %+v", ss)
	}

	// --cold: a fresh session (new server, workspace copied again).
	write(t, filepath.Join(workDir, "server-made"), "x", 0o644)
	r = ts.tun(ws, "bazel", "--cold", "build")
	if r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") || !strings.Contains(r.stderr, "--cold: discarded") ||
		!strings.Contains(r.stderr, "5 added") {
		t.Fatalf("cold: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(workDir, "server-made")); !os.IsNotExist(err) {
		t.Fatal("--cold kept the old workspace copy")
	}
	if ss := ts.sessions(); len(ss) != 1 || ss[0].Commands != 1 {
		t.Fatalf("after cold: %+v", ss)
	}

	// Another session name: its own server.
	if r = ts.tun(ws, "bazel", "--session", "other", "build"); r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") {
		t.Fatalf("other: %+v", r)
	}
	if ss := ts.sessions(); len(ss) != 2 || ss[0].Name != "other" {
		t.Fatalf("two sessions: %+v", ss)
	}
	for _, bad := range [][]string{{"bazel"}, {"bazel", "--host_jvm_args=x", "build"}, {"bazel", "--session", "a/b", "build"}} {
		if r := ts.tun(ws, bad...); r.code != 125 || !strings.HasPrefix(r.stderr, "tun: ") {
			t.Errorf("%v: %+v", bad, r)
		}
	}
}

func TestBazelSessionBwrap(t *testing.T) {
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap unusable")
	}
	cfg := server.Config{Sandbox: sandbox.Bwrap}
	if sandbox.NetUsable() == nil {
		cfg.PrivateNet = true
	}
	ts := bazelServer(t, cfg, false)
	ws := bazelWS(t, map[string]string{"x": "1"})
	r := ts.tun(ws, "bazel", "build")
	if r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") || !strings.Contains(r.stdout, "pwd=/work home=/tun/home tmp=/tmp") {
		t.Fatalf("first: %+v", r)
	}
	// The daemon survives between commands (same pid namespace); the session can't see the
	// server's data dir.
	r = ts.tun(ws, "bazel", "cat", "/tun/tun.bazelrc", ts.data+"/token")
	if r.code == 0 || !strings.Contains(r.stderr, "warm server") || !strings.Contains(r.stdout, "--disk_cache=/tun/cache/disk") ||
		strings.Contains(r.stdout, ts.srv.Token()) {
		t.Fatalf("second: %+v", r)
	}
	if ss := ts.sessions(); len(ss) != 1 || !ss[0].Server {
		t.Fatalf("sessions: %+v", ss)
	}
}

func TestBazelSessionQueueAndCancel(t *testing.T) {
	ts := bazelServer(t, server.Config{MaxJobs: 4}, false)
	ws := bazelWS(t, map[string]string{})
	c := ts.client()
	start := func(session string, args ...string) string {
		t.Helper()
		_, _, m, err := bazelManifest(ts.env(ws, nil, nil, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		j, _, err := c.createBazel(context.Background(), api.CreateJob{Bazel: &api.BazelJob{Session: session, Root: ws, Manifest: m, Args: args}}, ws)
		if err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	a := start("", "sleep", "30")
	ts.waitFor(a, func(j api.Job) bool { return j.State == api.Running })
	b := start("", "build") // same session: waits for a
	other := start("other", "build")
	if j := ts.waitDone(other); j.State != api.Exited { // another session runs meanwhile
		t.Fatalf("other: %+v", j)
	}
	if j := ts.status(b); j.State != api.Queued || j.QueuePosition == nil || *j.QueuePosition != 1 {
		t.Fatalf("b should wait for its session: %+v", j)
	}
	if ss := ts.sessions(); len(ss) != 2 || ss[1].State != "busy" || ss[1].Job != a {
		t.Fatalf("sessions: %+v", ss)
	}
	if r := ts.tun("", "cancel", a); r.code != 0 {
		t.Fatal(r)
	}
	if j := ts.waitDone(a); j.State != api.Canceled {
		t.Fatalf("a: %+v", j)
	}
	if j := ts.waitDone(b); j.State != api.Exited || *j.ExitCode != 0 {
		t.Fatalf("b: %+v", j)
	}
	if r := ts.tun("", "logs", "--stderr", b); !strings.Contains(r.stdout, "warm server") {
		t.Fatalf("b after a canceled command: %+v", r)
	}
	r := ts.tun(ws, "bazel", "--timeout", "1s", "sleep", "30")
	if r.code != 124 {
		t.Fatalf("timeout: %+v", r)
	}
}

func TestBazelSessionLimits(t *testing.T) {
	ts := bazelServer(t, server.Config{MaxSessions: 1, SessionIdle: 2 * time.Second}, false)
	ws := bazelWS(t, map[string]string{})
	ts.tun(ws, "bazel", "build")
	first := ts.sessions()[0]
	pid, _ := os.ReadFile(filepath.Join(ts.data, "sessions", first.ID, "sandbox/out/base/server/pid"))
	if r := ts.tun(ws, "bazel", "--session", "b", "build"); r.code != 0 {
		t.Fatal(r)
	}
	if ss := ts.sessions(); len(ss) != 1 || ss[0].Name != "b" {
		t.Fatalf("LRU eviction: %+v", ss)
	}
	if _, err := os.Stat(filepath.Join(ts.data, "sessions", first.ID)); !os.IsNotExist(err) {
		t.Fatal("evicted session dir kept")
	}
	if exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run() == nil {
		t.Fatal("evicted session's server still running (no bazel shutdown)")
	}
	eventually(t, "idle session expires", func() bool { return len(ts.sessions()) == 0 })
}

func TestBazelSessionRestart(t *testing.T) {
	data := t.TempDir()
	ts := bazelServer(t, server.Config{DataDir: data}, false)
	ws := bazelWS(t, map[string]string{"f": "1"})
	ts.tun(ws, "bazel", "build")
	ts.stop()
	ts = bazelServer(t, server.Config{DataDir: data}, false)
	ss := ts.sessions()
	if len(ss) != 1 || ss[0].Server || ss[0].Commands != 1 {
		t.Fatalf("after restart: %+v", ss)
	}
	r := ts.tun(ws, "bazel", "build")
	if r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") || !strings.Contains(r.stderr, "0 added, 0 updated, 0 deleted, 2 unchanged") {
		t.Fatalf("after restart: %+v", r)
	}
}

func TestBazelNeedsLocalSlots(t *testing.T) {
	ts := startServer(t, server.Config{HubOnly: true})
	r := ts.tun(bazelWS(t, map[string]string{}), "bazel", "build")
	if r.code != 125 || !strings.Contains(r.stderr, "--local-slots 0") {
		t.Fatalf("%+v", r)
	}
}

// TestBazelReal runs the real Bazel (skipped without bazel and bwrap): a genrule workspace
// needs no downloads. The second command must find the server warm and its analysis cached.
func TestBazelReal(t *testing.T) {
	if _, err := exec.LookPath("bazel"); err != nil {
		t.Skip("bazel not on PATH")
	}
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap unusable (without it a Bazel server would outlive the test)")
	}
	cfg := server.Config{Sandbox: sandbox.Bwrap}
	if sandbox.NetUsable() == nil {
		cfg.PrivateNet = true
	}
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" { // in a tun job: its shared bazelisk downloads
		cfg.BazeliskHome = filepath.Join(x, "bazelisk")
	}
	ts := bazelServer(t, cfg, true)
	ws := bazelWS(t, map[string]string{
		"MODULE.bazel":  "module(name = \"t\")\n",
		"BUILD.bazel":   "genrule(name = \"hello\", srcs = [\"in.txt\"], outs = [\"hello.txt\"], cmd = \"cat $< > $@\")\n",
		"in.txt":        "one\n",
		".bazelversion": "9.3.0\n", // what the tun hosts have cached
	})
	out := func() string {
		t.Helper()
		ss := ts.sessions()
		b, err := os.ReadFile(filepath.Join(ts.data, "sessions", ss[0].ID, "sandbox/out/base/execroot/_main/bazel-out/k8-fastbuild/bin/hello.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	r := ts.tun(ws, "bazel", "build", "//:hello")
	if r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") || out() != "one\n" {
		t.Fatalf("first: %+v", r)
	}
	r = ts.tun(ws, "bazel", "build", "//:hello")
	if r.code != 0 || strings.Contains(r.stderr, "Starting local Bazel server") || !strings.Contains(r.stderr, "(0 packages loaded, 0 targets configured)") {
		t.Fatalf("second (warm): %+v", r)
	}
	write(t, filepath.Join(ws, "in.txt"), "two\n", 0o644)
	r = ts.tun(ws, "bazel", "build", "//:hello")
	if r.code != 0 || strings.Contains(r.stderr, "Starting local Bazel server") || out() != "two\n" {
		t.Fatalf("third (changed input): %+v", r)
	}
	if r = ts.tun(ws, "bazel", "build", "//:nope"); r.code == 0 || !strings.Contains(r.stderr, "no such target") {
		t.Fatalf("bad target: %+v", r)
	}
}
