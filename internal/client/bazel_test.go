package client

import (
	"context"
	"encoding/json"
	"fmt"
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
export AGENT_PID=$PPID
for a; do
  case $a in --output_base=*) ob=${a#--output_base=} ;; --*) ;; *) break ;; esac
  shift
done
cmd=$1; shift
bep=; args=()
for a; do case $a in --build_event_json_file=*) bep=${a#*=} ;; *) args+=("$a") ;; esac; done
set -- "${args[@]}"
if [ -n "$bep" ]; then # a target (or for test, a failed test) per non-flag arg
  for t; do
    case $t in -*) ;;
      *) if [ "$cmd" = test ]; then echo "{\"id\":{\"testSummary\":{\"label\":\"$t\"}},\"testSummary\":{\"overallStatus\":\"FAILED\"}}"
         else echo "{\"id\":{\"targetCompleted\":{\"label\":\"$t\"}},\"completed\":{\"success\":true}}"; fi ;;
    esac
  done >"$bep"
fi
mkdir -p "$ob/server"
pid=$(cat "$ob/server/server.pid.txt" 2>/dev/null)
if [ "$cmd" = shutdown ]; then
  [ -n "$pid" ] && kill "$pid"
  rm -f "$ob/server/server.pid.txt"
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
  echo $pid > "$ob/server/server.pid.txt"
fi
echo "cmd=$cmd args=$*"
echo "pwd=$PWD home=$HOME tmp=$TMPDIR"
case $cmd in
  fail|test) exit 3 ;;
  sleep) sleep "$1" ;;
  cat) cat "$@" ;;
  sh) bash -c "$1" ;;
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
	cfg.DataDir = t.TempDir()
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
	// The sandbox controls out/: a link planted there must not make the server delete
	// a host dir when it clears a dead server's files.
	outside := t.TempDir()
	write(t, filepath.Join(outside, "server", "keep"), "x", 0o644)
	base := filepath.Join(ts.data, "sessions", ts.sessions()[0].ID, "sandbox", "out", "base")
	os.RemoveAll(base)
	if err := os.Symlink(outside, base); err != nil {
		t.Fatal(err)
	}
	// A server restart ends the sandbox (and the daemon in it); the session dir stays.
	ts.stop()
	ts = bazelServer(t, cfg, false)
	r = ts.tun(ws, "bazel", "build")
	if r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") || !strings.Contains(r.stderr, "2 unchanged") {
		t.Fatalf("after restart: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(outside, "server", "keep")); err != nil {
		t.Fatalf("server deleted a dir outside the session through a link: %v", err)
	}
}

// TestBazelSessionMounts: what a session mounts can't be redirected by a job, the Bazel
// caches are per client and out of reach of v0 jobs, and the sandbox root is read-only.
func TestBazelSessionMounts(t *testing.T) {
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap unusable")
	}
	cfg := server.Config{Sandbox: sandbox.Bwrap, DataDir: t.TempDir()}
	ts := bazelServer(t, cfg, false)
	ws := bazelWS(t, map[string]string{"x": "1"})
	// A v0 job owns the shared XDG cache: it used to be able to point bazelisk's home (and
	// so a session's read-write mount) at the data dir.
	if r := ts.tun("", "run", "--no-workspace", "-c", `mkdir -p "$XDG_CACHE_HOME" && rm -rf "$XDG_CACHE_HOME/bazelisk" && ln -s "$XDG_CACHE_HOME/../.." "$XDG_CACHE_HOME/bazelisk"`); r.code != 0 {
		t.Fatalf("v0 job: %+v", r)
	}
	r := ts.tun(ws, "bazel", "sh", `ls /tun/cache/bazelisk; touch /tun/cache/bazelisk/mine /tun/cache/repo/mine /tun/cache/disk/mine; touch /newfile || echo root-ro`)
	if r.code != 0 || strings.Contains(r.stdout, "token") || !strings.Contains(r.stdout, "root-ro") {
		t.Fatalf("session: %+v", r)
	}
	ents, _ := os.ReadDir(filepath.Join(ts.data, "bazel"))
	if len(ents) != 1 {
		t.Fatalf("per-client cache dirs: %v", ents)
	}
	caches := filepath.Join(ts.data, "bazel", ents[0].Name())
	for _, d := range []string{"bazelisk", "repo", "disk"} {
		if _, err := os.Stat(filepath.Join(caches, d, "mine")); err != nil {
			t.Errorf("%s is not the client's own dir: %v", d, err)
		}
	}
	// Even a server-owned mount source that became a symlink is refused, not followed.
	os.RemoveAll(filepath.Join(caches, "disk"))
	if err := os.Symlink(ts.data, filepath.Join(caches, "disk")); err != nil {
		t.Fatal(err)
	}
	r = ts.tun(ws, "bazel", "--cold", "sh", "ls /tun/cache/disk")
	if r.code == 0 || strings.Contains(r.stdout, "token") || !strings.Contains(r.stderr, "is a symlink") {
		t.Fatalf("symlinked mount source: %+v", r)
	}
}

// TestBazelSummary: build/test/coverage/run commands end with a summary line from the BEP
// (also the job's "summary"); --json prints the summary object on stdout instead.
func TestBazelSummary(t *testing.T) {
	ts := bazelServer(t, server.Config{}, false)
	ws := bazelWS(t, map[string]string{})
	r := ts.tun(ws, "bazel", "build", "//:a", "//:b", "--config=x")
	if r.code != 0 || !strings.Contains(r.stdout, "cmd=build args=//:a //:b --config=x") ||
		!regexp.MustCompile(`tun: bazel build exit=0 after [0-9.]+s: 2 targets built\ntun: job \S+ exited exit=0`).MatchString(r.stderr) {
		t.Fatalf("build: %+v", r)
	}
	if j := ts.status(jobOf(t, r)); !strings.HasPrefix(j.Summary, "tun: bazel build exit=0") {
		t.Fatalf("job summary: %+v", j)
	}
	r = ts.tun(ws, "bazel", "--json", "test", "//:t1", "//:t2")
	var out struct {
		Job, State, Command string
		ExitCode            *int `json:"exit_code"`
		TestCount           int  `json:"test_count"`
		FailedTests         int  `json:"failed_test_count"`
		Tests               []struct{ Label, Status string }
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || r.code != 3 || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("--json test: %v: %+v", err, r)
	}
	if out.Job != jobOf(t, r) || out.State != api.Exited || out.ExitCode == nil || *out.ExitCode != 3 || out.TestCount != 2 || out.FailedTests != 2 ||
		len(out.Tests) != 2 || out.Tests[0].Status != "FAILED" || !strings.Contains(r.stderr, "cmd=test") || !strings.Contains(r.stderr, "2 of 2 tests failed: //:t1 //:t2") {
		t.Fatalf("--json test: %+v\n%+v", out, r)
	}
	// No BEP for other commands: no line, but --json still has the exit code.
	r = ts.tun(ws, "bazel", "--json", "fail")
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || r.code != 3 || out.ExitCode == nil || *out.ExitCode != 3 || strings.Contains(r.stderr, "tun: bazel fail") {
		t.Fatalf("--json fail: %v: %+v", err, r)
	}
}

// TestBazelSessionSweep: processes a command leaves behind are killed when it ends (all but
// the Bazel server), so they can't tamper with later commands.
func TestBazelSessionSweep(t *testing.T) {
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap unusable")
	}
	ts := bazelServer(t, server.Config{Sandbox: sandbox.Bwrap, DataDir: t.TempDir()}, false)
	ws := bazelWS(t, map[string]string{})
	if r := ts.tun(ws, "bazel", "sh", "setsid sleep 300 </dev/null >/dev/null 2>&1 & echo $! >/tmp/left"); r.code != 0 {
		t.Fatalf("first: %+v", r)
	}
	r := ts.tun(ws, "bazel", "sh", "if kill -0 $(cat /tmp/left); then echo alive; else echo gone; fi")
	if r.code != 0 || !strings.Contains(r.stdout, "gone") || !strings.Contains(r.stderr, "warm server") {
		t.Fatalf("leftover survived the command, or the Bazel server didn't: %+v", r)
	}
}

// TestBazelSessionAgentLog: the session sandbox writes its own output (agent.log) only
// through a pipe the server caps, even via /proc/<agent>/fd/1.
func TestBazelSessionAgentLog(t *testing.T) {
	ts := bazelServer(t, server.Config{MaxLog: 64 << 10}, false)
	ws := bazelWS(t, map[string]string{})
	if r := ts.tun(ws, "bazel", "sh", "head -c 1000000 /dev/zero >/proc/$AGENT_PID/fd/1"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	p := filepath.Join(ts.data, "sessions", ts.sessions()[0].ID, "agent.log")
	eventually(t, "agent.log capped", func() bool {
		b, _ := os.ReadFile(p)
		return strings.Contains(string(b), "agent.log truncated")
	})
	if fi, _ := os.Stat(p); fi.Size() > 65<<10 {
		t.Fatalf("agent.log has %d bytes", fi.Size())
	}
}

// TestBazelSessionLongArgs: a command line larger than a socket buffer reaches Bazel.
func TestBazelSessionLongArgs(t *testing.T) {
	ts := bazelServer(t, server.Config{}, false)
	ws := bazelWS(t, map[string]string{})
	args := []string{"bazel", "build"}
	for i := range 12000 {
		args = append(args, fmt.Sprintf("//some/package/path:target_%d", i))
	}
	r := ts.tun(ws, args...)
	if r.code != 0 || !strings.Contains(r.stdout, "target_11999") {
		t.Fatalf("code %d, stderr %s", r.code, r.stderr)
	}
}

// TestBazelSessionCancelLeftover: a cancel the command obeys leaves the session's sandbox
// alone even if a process it started keeps the job's output open for longer than the kill
// escalation.
func TestBazelSessionCancelLeftover(t *testing.T) {
	ts := bazelServer(t, server.Config{}, false)
	ws := bazelWS(t, map[string]string{})
	ts.tun(ws, "bazel", "build")
	c := newClient(ts.env(ws, nil, nil, nil, nil))
	_, _, m, err := bazelManifest(ts.env(ws, nil, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(ws)
	j, _, err := c.createBazel(context.Background(), api.CreateJob{Bazel: &api.BazelJob{Root: root, Manifest: m,
		Args: []string{"sh", "setsid sleep 30 & echo $! >left; sleep 30"}}}, ws)
	if err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(ts.data, "sessions", ts.sessions()[0].ID, "sandbox", "work")
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(workDir, "left")); err == nil {
			exec.Command("kill", strings.TrimSpace(string(b))).Run()
		}
	})
	ts.waitFor(j.ID, func(j api.Job) bool { return j.State == api.Running })
	eventually(t, "leftover started", func() bool { _, err := os.Stat(filepath.Join(workDir, "left")); return err == nil })
	ts.tun("", "cancel", j.ID)
	if j := ts.waitDone(j.ID); j.State != api.Canceled {
		t.Fatalf("%+v", j)
	}
	time.Sleep(200 * time.Millisecond) // past the escalation (KillGrace is 50ms here)
	if ss := ts.sessions(); len(ss) != 1 || !ss[0].Server {
		t.Fatalf("the session's sandbox was killed: %+v", ss)
	}
}

func TestBazelSessionQueueAndCancel(t *testing.T) {
	ts := bazelServer(t, server.Config{MaxJobs: 4}, false)
	ws := bazelWS(t, map[string]string{})
	c := newClient(ts.env(ws, nil, nil, nil, nil)) // the same client name as the CLI's
	start := func(session string, args ...string) string {
		t.Helper()
		_, _, m, err := bazelManifest(ts.env(ws, nil, nil, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		root, _ := filepath.EvalSymlinks(ws)
		j, _, err := c.createBazel(context.Background(), api.CreateJob{Bazel: &api.BazelJob{Session: session, Root: root, Manifest: m, Args: args}}, ws)
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
	if j := ts.status(b); j.State != api.Queued || j.QueuePosition == nil || *j.QueuePosition != 1 || !strings.Contains(j.QueueReason, "is running job "+a) {
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
	time.Sleep(200 * time.Millisecond) // past the cancel's escalation (KillGrace is 50ms here)
	if r := ts.tun(ws, "bazel", "build"); !strings.Contains(r.stderr, "warm server") {
		t.Fatalf("a cancel that Bazel obeyed must not end the session's server: %+v", r)
	}
	if r := ts.tun(ws, "get", b, "x"); r.code != 125 || !strings.Contains(r.stderr, "tun bazel job") {
		t.Fatalf("tun get of a bazel job: %+v", r)
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
	pid, _ := os.ReadFile(filepath.Join(ts.data, "sessions", first.ID, "sandbox/out/base/server/server.pid.txt"))
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
	// Without bwrap the old daemon (a host process) is still valid and reused.
	r := ts.tun(ws, "bazel", "build")
	if r.code != 0 || !strings.Contains(r.stderr, "started a new session sandbox") || !strings.Contains(r.stderr, "0 added, 0 updated, 0 deleted, 2 unchanged") {
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
		cfg.BazeliskSeed = filepath.Join(x, "bazelisk")
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
	if r.code != 0 || !strings.Contains(r.stderr, "Starting local Bazel server") || out() != "one\n" ||
		!regexp.MustCompile(`tun: bazel build exit=0 after [0-9.]+s: 1 targets built\n`).MatchString(r.stderr) {
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
	if r = ts.tun(ws, "bazel", "build", "//:nope"); r.code == 0 || !strings.Contains(r.stderr, "no such target") ||
		!strings.Contains(r.stderr, "tun: bazel build exit=1 after") {
		t.Fatalf("bad target: %+v", r)
	}
	r = ts.tun(ws, "bazel", "--json", "build", "//:hello")
	var sum struct {
		Success bool
		Targets []struct {
			Label   string
			Outputs []struct{ Path string }
		}
	}
	if err := json.Unmarshal([]byte(r.stdout), &sum); err != nil || !sum.Success || len(sum.Targets) != 1 || len(sum.Targets[0].Outputs) != 1 ||
		sum.Targets[0].Outputs[0].Path != "bazel-bin/hello.txt" {
		t.Fatalf("--json: %v: %+v", err, r)
	}
}
