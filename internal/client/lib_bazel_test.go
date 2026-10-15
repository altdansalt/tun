// Shared test helpers (:testlib): a fake bazel, servers and workspaces for tun bazel tests.

package client

import (
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
      *) if [ "$cmd" = test ]; then
           # Its log in Bazel's layout, named by a testResult event; for //...:logs_* labels
           # also logs that tun must not keep (outside testlogs/, a symlink, a traversal).
           d=$ob/execroot/_main/bazel-out/k8-fastbuild/testlogs/${t#//}; d=${d/://}; d=${d/testlogs\/\//testlogs/}
           mkdir -p "$d/test_attempts"; echo "log of $t" > "$d/test.log"; echo "first try of $t" > "$d/test_attempts/attempt_1.log"
           logs="{\"name\":\"test.log\",\"uri\":\"file://$d/test.log\"}"
           case $t in *:logs_*)
             echo secret > "$ob/secret.txt"; ln -sf /etc/hostname "$d/link.log"
             for u in "$ob/secret.txt" "$d/../../../../../../secret.txt" "$d/link.log" /etc/hostname; do logs+=",{\"name\":\"x\",\"uri\":\"file://$u\"}"; done ;;
           esac
           echo "{\"id\":{\"testResult\":{\"label\":\"$t\",\"run\":1,\"shard\":1,\"attempt\":1}},\"testResult\":{\"status\":\"FAILED\",\"testActionOutput\":[{\"name\":\"test.log\",\"uri\":\"file://$d/test_attempts/attempt_1.log\"}]}}"
           echo "{\"id\":{\"testResult\":{\"label\":\"$t\",\"run\":1,\"shard\":1,\"attempt\":2}},\"testResult\":{\"status\":\"FAILED\",\"testActionOutput\":[$logs]}}"
           echo "{\"id\":{\"testSummary\":{\"label\":\"$t\"}},\"testSummary\":{\"overallStatus\":\"FAILED\"}}"
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
