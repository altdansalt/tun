package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

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

// A test command keeps its tests' logs with the job: tun get <id> //label fetches them even
// after the session's next command replaced them, and nothing outside bazel-out/*/testlogs/
// (or a symlink in it) is kept, whatever paths the BEP names.
func TestBazelTestLogs(t *testing.T) {
	for _, mode := range []string{"none", "bwrap"} {
		t.Run(mode, func(t *testing.T) {
			cfg := server.Config{}
			if mode == "bwrap" {
				if !sandbox.BwrapUsable() {
					t.Skip("bwrap unusable")
				}
				cfg.Sandbox = sandbox.Bwrap
			}
			ts := bazelServer(t, cfg, false)
			ws := bazelWS(t, map[string]string{})
			r := ts.tun(ws, "bazel", "test", "//pkg/sub:logs_test", "//:top")
			id := jobOf(t, r)
			if r.code != 3 || !strings.Contains(r.stderr, "2 of 2 tests failed: //:top //pkg/sub:logs_test; logs: tun get "+id+" //:top\n") {
				t.Fatalf("test: %+v", r)
			}
			ts.tun(ws, "bazel", "test", "//pkg/sub:logs_test") // overwrites the session's logs
			out := t.TempDir()
			r = ts.tun(ws, "get", "-o", out, id, "//pkg/sub:logs_test", "//:top")
			if r.code != 0 {
				t.Fatalf("get: %+v", r)
			}
			for name, want := range map[string]string{
				"bazel-testlogs/pkg/sub/logs_test/test.log":                    "log of //pkg/sub:logs_test\n",
				"bazel-testlogs/pkg/sub/logs_test/test_attempts/attempt_1.log": "first try of //pkg/sub:logs_test\n",
				"bazel-testlogs/top/test.log":                                  "log of //:top\n",
			} {
				if b, err := os.ReadFile(filepath.Join(out, name)); err != nil || string(b) != want {
					t.Errorf("%s: %q %v", name, b, err)
				}
			}
			var got []string
			filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					got = append(got, strings.TrimPrefix(p, out+"/"))
				}
				return nil
			})
			if len(got) != 4 { // the three above and //:top's attempt_1.log
				t.Errorf("kept files: %v", got)
			}
			// A label for a test the job didn't run; a build job keeps nothing.
			if r = ts.tun(ws, "get", "-o", out, id, "//nope:x_test"); r.code != 1 || !strings.Contains(r.stderr, "bazel-testlogs/nope/x_test") {
				t.Errorf("get missing label: %+v", r)
			}
			r = ts.tun(ws, "bazel", "build", "//:a")
			if r = ts.tun(ws, "get", "-o", out, jobOf(t, r), "//:a"); r.code != 125 || !strings.Contains(r.stderr, "only test and coverage commands keep") {
				t.Errorf("get from a build job: %+v", r)
			}
		})
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
	// A canceled command that hasn't ended after twice the grace gets its sandbox killed: the
	// tests' default 50ms was exceeded under tun bazel test --config=race on a busy hub.
	ts := bazelServer(t, server.Config{KillGrace: time.Second}, false)
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
	time.Sleep(2100 * time.Millisecond) // past the escalation (twice KillGrace)
	if ss := ts.sessions(); len(ss) != 1 || !ss[0].Server {
		t.Fatalf("the session's sandbox was killed: %+v", ss)
	}
}

func TestBazelSessionQueueAndCancel(t *testing.T) {
	// The session sandbox is killed if a canceled command hasn't ended after twice the grace:
	// not 50ms, which a loaded host can exceed.
	ts := bazelServer(t, server.Config{MaxJobs: 4, KillGrace: 500 * time.Millisecond}, false)
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
	// Running is set before the command starts. Wait until the fake daemon has
	// detached and published its pid, so cancellation actually exercises a warm
	// server rather than racing its startup on a loaded host.
	eventually(t, "first Bazel command ready", func() bool {
		r := ts.tun("", "logs", a)
		return r.code == 0 && strings.Contains(r.stdout, "cmd=sleep")
	})
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
	// a is running as soon as its job started; its Bazel server exists once it printed its
	// command (on a loaded host, canceling before that left b without a warm server).
	eventually(t, "a's server started", func() bool { return strings.Contains(ts.tun("", "logs", a).stdout, "cmd=sleep") })
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
	time.Sleep(1100 * time.Millisecond) // past the cancel's escalation (twice KillGrace)
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
	if r := ts.tun(ws, "bazel", "build"); !strings.Contains(r.stderr, "(Bazel server starts with this command; cold: first command of a new session)") || strings.Contains(r.stderr, "; cold start") {
		t.Fatalf("first command's summary: %+v", r)
	}
	first := ts.sessions()[0]
	pid, _ := os.ReadFile(filepath.Join(ts.data, "sessions", first.ID, "sandbox/out/base/server/server.pid.txt"))
	r := ts.tun(ws, "bazel", "--session", "b", "build")
	if r.code != 0 || !regexp.MustCompile(`; evicted client \S+'s session default \(idle for \d+s\) to make room\n`).MatchString(r.stderr) {
		t.Fatalf("evicting command's summary: %+v", r)
	}
	evictor := jobOf(t, r)
	if ss := ts.sessions(); len(ss) != 1 || ss[0].Name != "b" {
		t.Fatalf("LRU eviction: %+v", ss)
	}
	if _, err := os.Stat(filepath.Join(ts.data, "sessions", first.ID)); !os.IsNotExist(err) {
		t.Fatal("evicted session dir kept")
	}
	if exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run() == nil {
		t.Fatal("evicted session's server still running (no bazel shutdown)")
	}
	// The evicted session's next command says why it is cold, on its start line and summary.
	r = ts.tun(ws, "bazel", "build")
	want := regexp.MustCompile(`cold: its session ended \d+s ago: evicted \(least recently used of 1 sessions\) for client \S+'s session b \(job ` + evictor + `\)\); `)
	if r.code != 0 || !want.MatchString(r.stderr) || !strings.Contains(r.stderr, "; cold start: its session ended ") {
		t.Fatalf("evicted session's next command: %+v", r)
	}
	eventually(t, "idle session expires", func() bool { return len(ts.sessions()) == 0 })
	r = ts.tun(ws, "bazel", "build")
	if !strings.Contains(r.stderr, "; cold start: its session ended ") || !strings.Contains(r.stderr, "idle for 2s (tun serve --session-idle)") {
		t.Fatalf("expired session's next command: %+v", r)
	}
}

func TestBazelSessionStop(t *testing.T) {
	ts := bazelServer(t, server.Config{MaxSessions: 8}, false)
	ws := bazelWS(t, map[string]string{})
	for _, name := range []string{"default", "b", "c"} {
		if r := ts.tun(ws, "bazel", "--session", name, "build"); r.code != 0 {
			t.Fatal(r)
		}
	}
	other := bazelWS(t, map[string]string{})
	if r := ts.tun(other, "bazel", "--session", "b", "build"); r.code != 0 {
		t.Fatal(r)
	}
	names := func() (n []string) {
		for _, s := range ts.sessions() {
			n = append(n, s.Name+"@"+map[bool]string{true: "ws", false: "other"}[s.Root == ws])
		}
		sort.Strings(n)
		return n
	}
	// NAME: only this workspace's session of that name.
	r := ts.tun(ws, "sessions", "stop", "b")
	var stopped api.Session
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &stopped) != nil || stopped.Name != "b" || stopped.Root != ws || stopped.State != "stopping" {
		t.Fatalf("stop b: %+v", r)
	}
	eventually(t, "session b stopped", func() bool { return fmt.Sprint(names()) == "[b@other c@ws default@ws]" })
	if r := ts.tun(ws, "sessions", "stop", "b"); r.code != 125 || !strings.Contains(r.stderr, "no such session") {
		t.Fatalf("stop b again: %+v", r)
	}
	r = ts.tun(ws, "bazel", "--session", "b", "build")
	if !strings.Contains(r.stderr, "; cold start: its session ended ") || !strings.Contains(r.stderr, "stopped by client ") {
		t.Fatalf("stopped session's next command: %+v", r)
	}
	// --id; a busy session is left alone.
	c := ts.sessions()
	idOf := map[string]string{}
	for _, s := range c {
		idOf[s.Name+"@"+map[bool]string{true: "ws", false: "other"}[s.Root == ws]] = s.ID
	}
	if r := ts.tun("", "sessions", "stop", "--id", idOf["c@ws"]); r.code != 0 || !strings.Contains(r.stdout, idOf["c@ws"]) {
		t.Fatalf("stop --id: %+v", r)
	}
	done := make(chan result)
	go func() { done <- ts.tun(ws, "bazel", "sleep", "30") }()
	eventually(t, "default busy", func() bool {
		for _, s := range ts.sessions() {
			if s.Name == "default" && s.State == "busy" {
				return true
			}
		}
		return false
	})
	// No arguments: all of this client's sessions (any workspace), except the busy one.
	r = ts.tun("", "sessions", "stop")
	if r.code != 1 || strings.Count(r.stdout, "\n") != 2 || !strings.Contains(r.stderr, "session default ("+idOf["default@ws"]+") is busy (job ") {
		t.Fatalf("stop all: %+v", r)
	}
	eventually(t, "only the busy session left", func() bool { return fmt.Sprint(names()) == "[default@ws]" })
	ts.tun("", "cancel", ts.lastID())
	<-done
	if r := ts.tun("", "sessions", "stop", "extra", "args"); r.code != 125 {
		t.Fatalf("usage: %+v", r)
	}
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
	if r.code != 0 || !strings.Contains(r.stderr, "started a new session sandbox") || !strings.Contains(r.stderr, "0 added, 0 updated, 0 deleted, 2 unchanged") ||
		!strings.Contains(r.stderr, "; cold start: the tun server restarted since the session's last command") {
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
